package prefetch

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/markers"
)

// Post-play pre-buffering (roadmap item #30): when a movie or a
// season/series finale enters its last minutes, the personalized hook
// queue is computed once and its #1 pick is warmed — through
// core.Prewarmer when the playback manager offers it, otherwise by
// resolving and pulling the opening bytes like next-episode prefetch —
// so the 10-second post-play countdown launches instantly. Ordinary
// episodes keep the existing next-episode path (Up Next), and the two
// can warm different titles at a season boundary; continuation retains its
// own preparation budget so a recommendation cannot suppress the next episode.
//
// The trigger point is the earlier of duration−3m and the credits marker
// start (the first non-final credits marker when stingers split the
// credits, else the final one), probed once per session when playback
// first comes within postplayProbeWindow of the end.

// MarkerSource is the markers service API (internal/markers.Service
// satisfies it, as does plex.MarkerProvider).
type MarkerSource interface {
	Markers(ctx context.Context, key core.RatingKey, info core.MediaInfo) ([]markers.Marker, error)
}

const (
	// postplayProbeWindow: start caring this far from the end (credits
	// with stingers can begin well before the last 3 minutes).
	postplayProbeWindow = 12 * time.Minute
	// postplayTail is the default trigger: the last 3 minutes.
	postplayTail = 3 * time.Minute
	// postplaySessionTTL bounds stale per-session state.
	postplaySessionTTL  = 6 * time.Hour
	postplayMaxSessions = 256
)

// ppSession is one viewer's playback session of one title.
type ppSession struct {
	at      time.Time // last timeline update
	probed  bool
	skip    bool // not a movie/finale: leave it to next-episode prefetch
	fired   bool
	trigger time.Duration // offset at which to pre-buffer the #1 pick
}

func (p *Prefetcher) postplayEnabled() bool {
	if p.deps.PostPlay == nil {
		return false
	}
	return p.deps.PostPlayEnabled == nil || p.deps.PostPlayEnabled()
}

// onTimelinePostPlay is the timeline half of the hook: cheap checks only,
// all real work goes through the job queue. It never blocks.
func (p *Prefetcher) onTimelinePostPlay(user *core.User, key core.RatingKey, offset, duration time.Duration, state string) {
	if user == nil || duration <= 0 || offset <= 0 || !p.postplayEnabled() {
		return
	}
	if k := key.Kind(); k != core.KindMovie && k != core.KindEpisode {
		return
	}
	if state == "stopped" {
		p.dropPostPlaySession(user.ID, key)
		return
	}
	if duration-offset > postplayProbeWindow {
		return
	}
	id := fmt.Sprintf("%d:%d", user.ID, key)
	now := p.now()
	p.ppMu.Lock()
	s := p.ppSessions[id]
	if s == nil {
		if len(p.ppSessions) >= postplayMaxSessions {
			p.prunePostPlayLocked(now)
		}
		s = &ppSession{trigger: duration - postplayTail}
		p.ppSessions[id] = s
	}
	s.at = now
	probed, skip, fired, trigger := s.probed, s.skip, s.fired, s.trigger
	p.ppMu.Unlock()
	if skip || fired {
		return
	}
	u := *user
	// The session's probed/fired flags dedupe within a session. The jobs
	// carry no done-TTL: a new session for the same title after a stop (a
	// resume, a rewatch) must probe and warm again, not be suppressed for
	// hours by the previous session's finished jobs.
	if !probed {
		p.enqueue(job{
			id:  "ppprobe:" + id,
			run: func(ctx context.Context) error { return p.probePostPlay(ctx, &u, key, duration) },
		})
		return
	}
	if offset >= trigger {
		p.ppMu.Lock()
		if s := p.ppSessions[id]; s != nil && !s.fired {
			s.fired = true
		}
		p.ppMu.Unlock()
		p.enqueue(job{
			id:  "postplay:" + id,
			run: func(ctx context.Context) error { return p.firePostPlay(ctx, &u, key) },
		})
	}
}

func (p *Prefetcher) dropPostPlaySession(userID int64, key core.RatingKey) {
	p.ppMu.Lock()
	delete(p.ppSessions, fmt.Sprintf("%d:%d", userID, key))
	p.ppMu.Unlock()
}

func (p *Prefetcher) prunePostPlayLocked(now time.Time) {
	for id, s := range p.ppSessions {
		if now.Sub(s.at) > postplaySessionTTL {
			delete(p.ppSessions, id)
		}
	}
	// Still full (sessions that never sent "stopped" within the TTL):
	// forget the least recently updated so the map stays bounded.
	for len(p.ppSessions) >= postplayMaxSessions {
		var oldest string
		var at time.Time
		for id, s := range p.ppSessions {
			if oldest == "" || s.at.Before(at) {
				oldest, at = id, s.at
			}
		}
		delete(p.ppSessions, oldest)
	}
}

// probePostPlay decides, once per session, whether this title gets the
// hook queue (movie, or season/series finale) and where its trigger is.
func (p *Prefetcher) probePostPlay(ctx context.Context, user *core.User, key core.RatingKey, duration time.Duration) error {
	skip := false
	if key.Kind() == core.KindEpisode {
		finale, err := p.isFinale(ctx, key)
		if err != nil {
			return err
		}
		skip = !finale
	}
	trigger := duration - postplayTail
	if !skip {
		if cs, ok := p.creditsStart(ctx, key, duration); ok && cs < trigger {
			trigger = cs
		}
		if trigger < 0 {
			trigger = 0
		}
	}
	id := fmt.Sprintf("%d:%d", user.ID, key)
	p.ppMu.Lock()
	if s := p.ppSessions[id]; s != nil {
		s.probed, s.skip, s.trigger = true, skip, trigger
	}
	p.ppMu.Unlock()
	return nil
}

// isFinale reports whether an episode ends its season or series: no next
// episode at all, or the next one opens a new season.
func (p *Prefetcher) isFinale(ctx context.Context, key core.RatingKey) (bool, error) {
	next, err := p.deps.Catalog.NextEpisode(ctx, key)
	if errors.Is(err, core.ErrNotFound) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("next episode of %s: %w", key, err)
	}
	if next == nil {
		return true, nil
	}
	return next.Key.Season() != key.Season(), nil
}

// creditsStart is where pre-buffering should begin according to the
// markers service: the first non-final credits marker when stingers
// split the credits into several ranges, else the (final) credits start.
// Shared with tests as a pure function over marker lists.
func (p *Prefetcher) creditsStart(ctx context.Context, key core.RatingKey, duration time.Duration) (time.Duration, bool) {
	if p.deps.Markers == nil || duration <= 0 {
		return 0, false
	}
	ms, err := p.deps.Markers.Markers(ctx, key, core.MediaInfo{Duration: duration})
	if err != nil {
		p.log.Debug("postplay: markers lookup failed", "key", key, "err", err)
		return 0, false
	}
	return creditsTrigger(ms, duration)
}

// creditsTrigger picks the trigger offset from a marker list.
func creditsTrigger(ms []markers.Marker, duration time.Duration) (time.Duration, bool) {
	var firstNonFinal, final int64 = -1, -1
	for _, m := range ms {
		if m.Type != "credits" || m.StartMS < 0 || m.StartMS >= duration.Milliseconds() {
			continue
		}
		if m.Final {
			if final < 0 || m.StartMS < final {
				final = m.StartMS
			}
		} else if firstNonFinal < 0 || m.StartMS < firstNonFinal {
			firstNonFinal = m.StartMS
		}
	}
	switch {
	case firstNonFinal >= 0:
		return time.Duration(firstNonFinal) * time.Millisecond, true
	case final >= 0:
		return time.Duration(final) * time.Millisecond, true
	}
	return 0, false
}

// firePostPlay computes the hook queue (cached by the engine, so the
// endpoint serves the same #1) and warms its first playable pick.
func (p *Prefetcher) firePostPlay(ctx context.Context, user *core.User, key core.RatingKey) error {
	var caps core.ClientCaps
	if p.deps.Hints != nil {
		caps, _, _ = p.deps.Hints.ClientCaps(user.ID, key)
	}
	now := p.now()
	hc := core.NewHubContext(now, caps.Product, caps.Platform, caps.Device)
	picks, err := p.deps.PostPlay.PostPlay(ctx, user, key, hc, 12)
	if err != nil {
		return fmt.Errorf("postplay picks for %s: %w", key, err)
	}
	if len(picks) == 0 {
		return nil
	}
	playable, err := p.playablePick(ctx, user, &picks[0].Title)
	if err != nil {
		return err
	}
	if playable == nil {
		return nil
	}
	p.setCurrent(episodeLabel(playable), core.CacheCausePostPlay)
	log := p.log.With("user", user.ID, "after", key, "pick", playable.Key, "reason", picks[0].Reason)
	if p.deps.Prewarmer != nil {
		if p.deps.Prewarmer.Prewarm(ctx, user, playable.Key, caps, core.PrewarmOptions{
			Reason: core.PrewarmPostPlay, PrefetchBytes: p.opts.PostPlayBytes, DuringPlayback: true,
		}) {
			log.Info("pre-warming post-play pick")
		} else {
			log.Debug("post-play prewarm skipped by playback budgets")
		}
		return nil
	}
	// Fallback while no Prewarmer is wired: resolve + pull opening bytes,
	// exactly like next-episode prefetch.
	length := p.opts.PostPlayBytes
	if p.deps.Hints != nil {
		if rp, err := p.deps.Hints.ResumeFor(ctx, user, playable.Key, caps); err == nil && rp != nil {
			log.Info("pre-buffering post-play pick (stored release)")
			p.noteCause(rp.ReleaseID(), core.CacheCausePostPlay, episodeLabel(playable))
			return p.deps.Engine.Prefetch(ctx, rp.ReleaseID(), rp.NZB, 0, clampLen(0, length, rp.Size))
		}
	}
	// Nobody waits on a post-play pre-buffer: fallback queries go one at a time.
	res, err := p.deps.Resolver.Resolve(core.WithIndexerPurpose(core.WithUnattendedSearch(ctx), core.IndexerPurposeWarmup), core.ResolveRequest{Title: playable, Client: caps, User: user})
	if errors.Is(err, core.ErrNotAvailable) {
		log.Debug("post-play pick not available")
		return nil
	}
	if err != nil {
		return fmt.Errorf("resolve post-play pick %s: %w", playable.Key, err)
	}
	if p.deps.Hints != nil {
		if err := p.deps.Hints.Remember(ctx, user, playable.Key, caps, res); err != nil {
			log.Warn("remember post-play release failed", "err", err)
		}
	}
	log.Info("pre-buffering post-play pick", "release", res.Release.Name, "bytes", length)
	p.noteCause(res.Release.ID, core.CacheCausePostPlay, episodeLabel(playable))
	return p.deps.Engine.Prefetch(ctx, res.Release.ID, res.NZB, 0, clampLen(0, length, res.Release.Size))
}

// playablePick turns a pick into the title Play would start: a movie
// itself, or a show's next unwatched episode (resume-aware).
func (p *Prefetcher) playablePick(ctx context.Context, user *core.User, t *core.Title) (*core.Title, error) {
	switch t.Type {
	case core.TypeMovie, core.TypeEpisode:
		return t, nil
	case core.TypeShow:
		eps, err := p.deps.Catalog.Grandchildren(ctx, t.Key)
		if err != nil {
			return nil, fmt.Errorf("episodes of %s: %w", t.Key, err)
		}
		var keys []core.RatingKey
		for _, ep := range eps {
			if ep.Season > 0 {
				keys = append(keys, ep.Key)
			}
		}
		statuses := map[core.RatingKey]core.WatchStatus{}
		if p.deps.State != nil && len(keys) > 0 {
			statuses, _ = p.deps.State.GetMany(ctx, user.ID, keys)
		}
		for i := range eps {
			ep := &eps[i]
			if ep.Season == 0 || statuses[ep.Key].ViewCount > 0 {
				continue
			}
			if !ep.Released.IsZero() && ep.Released.After(p.now()) {
				continue
			}
			return ep, nil
		}
	}
	return nil, nil
}
