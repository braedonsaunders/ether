// Package prefetch warms the segment cache in the background so the next
// episode and the resume point of in-progress titles start instantly.
//
//   - Next episode: when a timeline update shows an episode past
//     config.Cache.NextEpisodeAt, the next episode is resolved for the same
//     client and its opening PrefetchBytes are pulled.
//   - Keep warm: periodically, a window around each in-progress title's
//     resume point is pulled from the release that last served it.
//
// All work runs on a bounded pool at core.PriorityPrefetch through
// Engine.Prefetch, is deduplicated, and never blocks the caller: OnTimeline
// only enqueues, and a full queue drops work rather than wait.
package prefetch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/playback"
	"github.com/braedonsaunders/ether/internal/schedule"
)

// Hints is what prefetch needs from playback; *playback.Manager implements it.
type Hints interface {
	// ResumePoint is the release that last served a user's title.
	ResumePoint(ctx context.Context, userID int64, key core.RatingKey) (*playback.ResumePoint, error)
	// ResumeFor is the stored release a Prepare from this client would open.
	ResumeFor(ctx context.Context, user *core.User, key core.RatingKey, caps core.ClientCaps) (*playback.ResumePoint, error)
	// Remember stores a prefetched resolution so playback opens the same release.
	Remember(ctx context.Context, user *core.User, key core.RatingKey, caps core.ClientCaps, res *core.Resolution) error
	// ClientCaps returns the caps of the user's session for key (live) or
	// of the user's latest session.
	ClientCaps(userID int64, key core.RatingKey) (caps core.ClientCaps, live bool, ok bool)
}

// Deps are the prefetcher's collaborators. Catalog, Resolver and Engine are
// required; State, Auth and Hints enable keep-warm (and Hints the reuse of
// stored releases).
type Deps struct {
	Catalog  core.Catalog
	Resolver core.Resolver
	Engine   core.Engine
	State    core.State
	Auth     core.Auth
	Hints    Hints
	Logger   *slog.Logger

	// Cache records why background bytes were warmed when it supports
	// provenance (optional).
	Cache core.CacheProvenanceRecorder
	// PostPlay ranks the personalized hook queue (optional; #30). With
	// it set, the end of a movie or season/series finale pre-buffers the
	// #1 pick instead of relying on next-episode prefetch.
	PostPlay core.PostPlayer
	// Prewarmer warms the pick through the playback manager when it
	// implements core.Prewarmer; nil falls back to resolve + Engine.Prefetch.
	Prewarmer core.Prewarmer
	// Markers supplies credits ranges through the markers service API
	// (optional; without it the trigger is duration − 3 minutes).
	Markers MarkerSource
	// PostPlayEnabled reports the owner's switch (nil = on).
	PostPlayEnabled func() bool
}

// Options tune the prefetcher. Zero values take the defaults noted.
type Options struct {
	// Concurrency is the global limit of prefetch jobs running at once (default 2).
	Concurrency int
	// QueueSize bounds queued jobs; more are dropped (default 128).
	QueueSize int
	// WarmInterval is how often in-progress resume points are warmed (default 15m).
	WarmInterval time.Duration
	// WarmBehind is pulled before the resume point, for the client's
	// rewind-on-resume (default 4 MiB). The window after it is
	// a keyframe-aligned 25 seconds of media, capped at 256 MiB and 10% of the cache budget.
	WarmBehind int64
	// InProgressLimit is how many in-progress titles per user are warmed (default 5).
	InProgressLimit int
	// DoneTTL suppresses repeating a finished next-episode job (default 6h).
	DoneTTL time.Duration
	// RetryAfter suppresses retrying a failed job (default 30m).
	RetryAfter time.Duration
	// JobTimeout bounds one job (default 10m).
	JobTimeout time.Duration
	// PostPlayBytes is how much of the #1 post-play pick to pre-buffer
	// (default 40 MiB, inside the 32–48 MiB the countdown needs).
	PostPlayBytes int64
	// Now is the clock (tests).
	Now func() time.Time
}

func (p *Prefetcher) noteCause(releaseID, cause, title string) {
	if releaseID == "" || p.deps.Cache == nil {
		return
	}
	p.deps.Cache.RecordProvenance(core.CacheProvenance{ReleaseID: releaseID, Title: title, Cause: cause})
}

func (p *Prefetcher) setCurrent(title, cause string) {
	p.mu.Lock()
	p.stats.Current = &WarmCurrent{Title: title, Cause: cause}
	p.mu.Unlock()
}

func episodeLabel(t *core.Title) string {
	if t.ShowTitle != "" && t.Season > 0 && t.Episode > 0 {
		return fmt.Sprintf("%s · S%02dE%02d · %s", t.ShowTitle, t.Season, t.Episode, t.Title)
	}
	return t.Title
}

func (o *Options) defaults() {
	if o.Concurrency <= 0 {
		o.Concurrency = 2
	}
	if o.QueueSize <= 0 {
		o.QueueSize = 128
	}
	if o.WarmInterval <= 0 {
		o.WarmInterval = 15 * time.Minute
	}
	if o.WarmBehind <= 0 {
		o.WarmBehind = 4 << 20
	}
	if o.InProgressLimit <= 0 {
		o.InProgressLimit = 5
	}
	if o.DoneTTL <= 0 {
		o.DoneTTL = 6 * time.Hour
	}
	if o.RetryAfter <= 0 {
		o.RetryAfter = 30 * time.Minute
	}
	if o.JobTimeout <= 0 {
		o.JobTimeout = 10 * time.Minute
	}
	if o.PostPlayBytes <= 0 {
		o.PostPlayBytes = 40 << 20
	}
	if o.Now == nil {
		o.Now = time.Now
	}
}

// WarmCurrent names the item being warmed and its cache cause.
type WarmCurrent struct {
	Title string `json:"title"`
	Cause string `json:"cause"`
}

// Stats are counters for the admin dashboard.
type Stats struct {
	Queued    int          `json:"queued"`
	Running   int          `json:"running"`
	Completed int64        `json:"completed"`
	Failed    int64        `json:"failed"`
	Dropped   int64        `json:"dropped"`
	Current   *WarmCurrent `json:"current,omitempty"`
}

type jobState int

const (
	stateQueued jobState = iota
	stateRunning
	stateDone
	stateFailed
)

type entry struct {
	state jobState
	at    time.Time
	ttl   time.Duration // how long a done entry suppresses repeats
}

type job struct {
	id  string // dedupe key
	ttl time.Duration
	run func(ctx context.Context) error
}

// Prefetcher is the background prefetch worker pool.
type Prefetcher struct {
	deps Deps
	opts Options
	log  *slog.Logger
	now  func() time.Time

	cfgMu sync.RWMutex
	cfg   config.Cache

	mu      sync.Mutex
	closed  bool
	seen    map[string]*entry
	running int
	pending int // queued + running
	stats   Stats
	idle    *sync.Cond

	// Post-play sessions (postplay.go), keyed by user and title.
	ppMu       sync.Mutex
	ppSessions map[string]*ppSession

	jobs   chan job
	warmC  chan struct{}
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// New starts a Prefetcher with Options.Concurrency workers and the
// keep-warm loop. Call Close to stop it.
func New(deps Deps, cfg config.Cache, opts Options) (*Prefetcher, error) {
	switch {
	case deps.Catalog == nil:
		return nil, errors.New("prefetch: Catalog is required")
	case deps.Resolver == nil:
		return nil, errors.New("prefetch: Resolver is required")
	case deps.Engine == nil:
		return nil, errors.New("prefetch: Engine is required")
	}
	opts.defaults()
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	p := &Prefetcher{
		deps:       deps,
		opts:       opts,
		log:        deps.Logger.With("component", "prefetch"),
		now:        opts.Now,
		cfg:        cfg,
		seen:       map[string]*entry{},
		ppSessions: map[string]*ppSession{},
		jobs:       make(chan job, opts.QueueSize),
		warmC:      make(chan struct{}, 1),
	}
	p.idle = sync.NewCond(&p.mu)
	p.ctx, p.cancel = context.WithCancel(context.Background())
	for i := 0; i < opts.Concurrency; i++ {
		p.wg.Add(1)
		go p.worker()
	}
	p.wg.Add(1)
	go p.warmLoop()
	return p, nil
}

// Close stops the workers; running jobs are cancelled.
func (p *Prefetcher) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	p.mu.Unlock()
	p.cancel()
	p.wg.Wait()
	p.mu.Lock()
	p.idle.Broadcast()
	p.mu.Unlock()
	return nil
}

// SetConfig applies a new cache config snapshot.
func (p *Prefetcher) SetConfig(cfg config.Cache) {
	p.cfgMu.Lock()
	p.cfg = cfg
	p.cfgMu.Unlock()
}

func (p *Prefetcher) config() config.Cache {
	p.cfgMu.RLock()
	defer p.cfgMu.RUnlock()
	return p.cfg
}

// Stats returns current counters.
func (p *Prefetcher) Stats() Stats {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.stats
	s.Running = p.running
	s.Queued = len(p.jobs)
	return s
}

// OnTimeline is called by the Plex front end for every timeline update. It
// never blocks: past Cache.NextEpisodeAt of an episode it queues the next
// episode's prefetch (once per user and episode).
func (p *Prefetcher) OnTimeline(user *core.User, key core.RatingKey, offset, duration time.Duration, state string) {
	if cfg := p.config(); cfg.DisableSpeculation || cfg.Mode == "off" {
		return
	}
	// Resume warming reuses the played source; permanent failure may replace it.
	if user != nil && offset > 0 && p.deps.Hints != nil && p.config().KeepInProgressWarm && (key.Kind() == core.KindMovie || key.Kind() == core.KindEpisode) {
		u := *user
		p.enqueue(job{
			id:  fmt.Sprintf("resume:%d:%d:%d:%s", u.ID, key, offset/(25*time.Second), state),
			ttl: 25 * time.Second,
			run: func(ctx context.Context) error { return p.warmResume(ctx, u.ID, key, offset, duration) },
		})
	}
	// Post-play hook queue (#30): movies and finales pre-buffer their
	// #1 pick near the end. Runs before the episode-only path below.
	if p.config().SpeculativeWarming != "off" {
		p.onTimelinePostPlay(user, key, offset, duration, state)
	}
	if user == nil || key.Kind() != core.KindEpisode || duration <= 0 || offset <= 0 {
		return
	}
	cfg := p.config()
	if !cfg.PrefetchNextEpisode || cfg.PrefetchBytes <= 0 {
		return
	}
	at := cfg.NextEpisodeAt
	if at <= 0 || at >= 1 {
		at = 0.8
	}
	if float64(offset)/float64(duration) < at {
		return
	}
	u := *user
	p.enqueue(job{
		id:  fmt.Sprintf("next:%d:%d", u.ID, key),
		ttl: p.opts.DoneTTL,
		run: func(ctx context.Context) error { return p.nextEpisode(ctx, &u, key) },
	})
}

// WarmNow triggers a keep-warm pass without waiting for the interval.
func (p *Prefetcher) WarmNow() {
	select {
	case p.warmC <- struct{}{}:
	default:
	}
}

// enqueue adds a job unless the same work is queued, running, recently done
// or recently failed. It never blocks; a full queue drops the job.
func (p *Prefetcher) enqueue(j job) bool {
	now := p.now()
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return false
	}
	if e := p.seen[j.id]; e != nil {
		switch {
		case e.state == stateQueued || e.state == stateRunning:
			p.mu.Unlock()
			return false
		case e.state == stateDone && now.Sub(e.at) < e.ttl:
			p.mu.Unlock()
			return false
		case e.state == stateFailed && now.Sub(e.at) < p.opts.RetryAfter:
			p.mu.Unlock()
			return false
		}
	}
	select {
	case p.jobs <- j:
		p.seen[j.id] = &entry{state: stateQueued, at: now, ttl: j.ttl}
		p.pending++
		p.mu.Unlock()
		return true
	default:
		p.stats.Dropped++
		p.mu.Unlock()
		p.log.Debug("prefetch queue full; dropping", "job", j.id)
		return false
	}
}

func (p *Prefetcher) worker() {
	defer p.wg.Done()
	for {
		select {
		case <-p.ctx.Done():
			return
		case j := <-p.jobs:
			p.runJob(j)
		}
	}
}

func (p *Prefetcher) runJob(j job) {
	p.mu.Lock()
	p.running++
	if e := p.seen[j.id]; e != nil {
		e.state = stateRunning
	}
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.stats.Current = nil
		p.mu.Unlock()
	}()

	ctx, cancel := context.WithTimeout(p.ctx, p.opts.JobTimeout)
	var err error
	if cfg := p.config(); !cfg.DisableSpeculation && cfg.Mode != "off" {
		err = j.run(ctx)
	}
	cancel()

	p.mu.Lock()
	p.running--
	p.pending--
	e := p.seen[j.id]
	if e == nil {
		e = &entry{ttl: j.ttl}
		p.seen[j.id] = e
	}
	e.at = p.now()
	if err != nil {
		e.state = stateFailed
		p.stats.Failed++
	} else {
		e.state = stateDone
		p.stats.Completed++
	}
	p.idle.Broadcast()
	p.mu.Unlock()
	if err != nil && p.ctx.Err() == nil {
		p.log.Warn("prefetch job failed", "job", j.id, "err", err)
	} else if err == nil {
		p.log.Debug("prefetch job done", "job", j.id)
	}
}

// nextEpisode resolves the episode after key for the user's client and
// pulls its opening bytes.
func (p *Prefetcher) nextEpisode(ctx context.Context, user *core.User, key core.RatingKey) error {
	next, err := p.deps.Catalog.NextEpisode(ctx, key)
	if errors.Is(err, core.ErrNotFound) {
		return nil // season or series finale
	}
	if err != nil {
		return fmt.Errorf("next episode of %s: %w", key, err)
	}
	if next == nil || (!next.Released.IsZero() && next.Released.After(p.now())) {
		return nil // not aired yet; don't spend indexer budget
	}
	p.setCurrent(episodeLabel(next), core.CacheCauseNextEpisode)
	var caps core.ClientCaps
	if p.deps.Hints != nil {
		caps, _, _ = p.deps.Hints.ClientCaps(user.ID, key)
	}
	length := p.config().PrefetchBytes
	log := p.log.With("user", user.ID, "episode", next.Key)
	// Reuse the same opened-source/probe preparation as detail and post-play
	// warming. Merely remembering an NZB still leaves container probing on
	// the critical path when the player advances to this episode.
	if p.deps.Prewarmer != nil {
		if p.deps.Prewarmer.Prewarm(ctx, user, next.Key, caps, core.PrewarmOptions{
			Reason: core.PrewarmNextEpisode, PrefetchBytes: length, DuringPlayback: true,
		}) {
			log.Info("preparing next episode for playback")
		} else {
			log.Debug("next episode preparation already present or budget unavailable")
		}
		return nil
	}

	if p.deps.Hints != nil {
		rp, err := p.deps.Hints.ResumeFor(ctx, user, next.Key, caps)
		if err != nil {
			log.Debug("resume lookup failed", "err", err)
		}
		if rp != nil {
			log.Debug("warming stored release of next episode", "release", rp.Release.Name)
			p.noteCause(rp.ReleaseID(), core.CacheCauseNextEpisode, episodeLabel(next))
			err := p.deps.Engine.Prefetch(ctx, rp.ReleaseID(), rp.NZB, 0, clampLen(0, length, rp.Size))
			if err != nil {
				return p.replaceWarmSource(ctx, user.ID, next.Key, rp.Release, 0, next.Runtime, err)
			}
			return nil
		}
	}

	// Nobody waits on a prefetch: fallback queries go one at a time.
	res, err := p.deps.Resolver.Resolve(core.WithIndexerPurpose(core.WithUnattendedSearch(ctx), core.IndexerPurposeWarmup), core.ResolveRequest{Title: next, Client: caps, User: user})
	if errors.Is(err, core.ErrNotAvailable) {
		log.Debug("next episode not available")
		return nil
	}
	if err != nil {
		return fmt.Errorf("resolve %s: %w", next.Key, err)
	}
	if p.deps.Hints != nil {
		if err := p.deps.Hints.Remember(ctx, user, next.Key, caps, res); err != nil {
			log.Warn("remember next episode release failed", "err", err)
		}
	}
	log.Info("prefetching next episode", "release", res.Release.Name, "bytes", length)
	p.noteCause(res.Release.ID, core.CacheCauseNextEpisode, episodeLabel(next))
	err = p.deps.Engine.Prefetch(ctx, res.Release.ID, res.NZB, 0, clampLen(0, length, res.Release.Size))
	if err != nil {
		return p.replaceWarmSource(ctx, user.ID, next.Key, res.Release, 0, next.Runtime, err)
	}
	return nil
}

// clampLen keeps [off, off+length) inside a file of size bytes (when known).
func clampLen(off, length, size int64) int64 {
	if size > 0 && off+length > size {
		length = size - off
	}
	if length < 0 {
		return 0
	}
	return length
}

func (p *Prefetcher) warmLoop() {
	defer p.wg.Done()
	deadline := schedule.New(p.opts.Now(), p.opts.WarmInterval)
	t := time.NewTimer(min(p.opts.WarmInterval, schedule.MaxWait))
	defer t.Stop()
	for {
		select {
		case <-p.ctx.Done():
			return
		case <-t.C:
			if left := deadline.Remaining(p.opts.Now()); left > 0 {
				t.Reset(min(left, schedule.MaxWait))
				continue
			}
		case <-p.warmC:
			t.Stop()
		}
		if p.ctx.Err() != nil {
			return
		}

		ctx, cancel := context.WithTimeout(p.ctx, time.Minute)
		if err := p.warmPass(ctx); err != nil && p.ctx.Err() == nil {
			p.log.Warn("keep-warm pass failed", "err", err)
		}
		cancel()
		p.prune()
		deadline = schedule.New(p.opts.Now(), p.opts.WarmInterval)
		t.Reset(min(p.opts.WarmInterval, schedule.MaxWait))
	}
}

// warmPass queues a prefetch around the resume point of every in-progress
// title of every user that has a stored release and no live session.
func (p *Prefetcher) warmPass(ctx context.Context) error {
	cfg := p.config()
	if cfg.DisableSpeculation || cfg.Mode == "off" || !cfg.KeepInProgressWarm || p.deps.State == nil || p.deps.Auth == nil || p.deps.Hints == nil {
		return nil
	}
	users, err := p.deps.Auth.Users(ctx)
	if err != nil {
		return fmt.Errorf("users: %w", err)
	}
	for _, u := range users {
		list, err := p.deps.State.InProgress(ctx, u.ID, p.opts.InProgressLimit)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			p.log.Warn("in-progress lookup failed", "user", u.ID, "err", err)
			continue
		}
		// State returns most recent first. Old accounts cost no source IO.
		if len(list) == 0 || p.now().Sub(maxTime(list[0].LastViewedAt, list[0].UpdatedAt)) > 7*24*time.Hour {
			continue
		}
		for _, st := range list {
			if st.ViewOffset <= 0 || p.now().Sub(maxTime(st.LastViewedAt, st.UpdatedAt)) > 7*24*time.Hour {
				continue
			}
			if k := st.Key.Kind(); k != core.KindMovie && k != core.KindEpisode {
				continue
			}
			if _, live, _ := p.deps.Hints.ClientCaps(u.ID, st.Key); live {
				continue // the stream engine's read-ahead has it
			}
			st := st
			userID := u.ID
			p.enqueue(job{
				id:  fmt.Sprintf("warm:%d:%d:%d", userID, st.Key, st.ViewOffset/(25*time.Second)),
				ttl: p.opts.WarmInterval / 2,
				run: func(ctx context.Context) error { return p.warmResume(ctx, userID, st.Key, st.ViewOffset, st.Duration) },
			})
		}
	}
	return nil
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// warmResume only resolves after a stored source proves permanently broken.
// An unstarted title or missing source never spends an indexer request.
func (p *Prefetcher) warmResume(ctx context.Context, userID int64, key core.RatingKey, offset, duration time.Duration) error {
	rp, err := p.deps.Hints.ResumePoint(ctx, userID, key)
	if err != nil || rp == nil || rp.Size <= 0 || len(rp.NZB) == 0 {
		return err
	}
	if rp.Duration > 0 {
		duration = rp.Duration
	}
	if duration <= 0 {
		return nil
	}
	tm := playback.TimeMap{Size: rp.Size, Duration: duration, Points: resumeIndexPoints(rp.Keyframes)}
	start := max(int64(0), tm.KeyframeOffsetAt(offset)-p.opts.WarmBehind)
	end := tm.OffsetAt(min(duration, offset+25*time.Second))
	capBytes := int64(256 << 20)
	if budget := p.config().BudgetBytes; budget > 0 {
		capBytes = min(capBytes, budget/10)
	}
	length := clampLen(start, min(max(int64(0), end-start), capBytes), rp.Size)
	if length <= 0 {
		return nil
	}
	p.setCurrent(rp.Release.Name, core.CacheCauseResumeWarm)
	p.noteCause(rp.ReleaseID(), core.CacheCauseResumeWarm, rp.Release.Name)
	err = p.deps.Engine.Prefetch(ctx, rp.ReleaseID(), rp.NZB, start, length)
	if err == nil {
		return nil
	}
	return p.replaceWarmSource(ctx, userID, key, rp.Release, offset, duration, err)
}

func WarmWindowFor(viewOffset time.Duration, tm playback.TimeMap, behind, ahead int64) (start, length int64) {
	off := tm.OffsetAt(viewOffset)
	start = off - behind
	if start < 0 {
		start = 0
	}
	return start, clampLen(start, off-start+ahead, tm.Size)
}

func resumeIndexPoints(points []core.KeyframePoint) []playback.IndexPoint {
	if len(points) == 0 {
		return nil
	}
	out := make([]playback.IndexPoint, len(points))
	for i, p := range points {
		out[i] = playback.IndexPoint{Offset: p.Offset, Time: p.Time}
	}
	return out
}

// prune forgets dedupe entries that no longer suppress anything.
func (p *Prefetcher) prune() {
	now := p.now()
	keep := p.opts.DoneTTL
	if p.opts.RetryAfter > keep {
		keep = p.opts.RetryAfter
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, e := range p.seen {
		if (e.state == stateDone || e.state == stateFailed) && now.Sub(e.at) > keep {
			delete(p.seen, id)
		}
	}
}

var _ Hints = (*playback.Manager)(nil)

func (p *Prefetcher) replaceWarmSource(ctx context.Context, userID int64, key core.RatingKey, old core.Release, offset, duration time.Duration, failure error) error {
	if !(errors.Is(failure, core.ErrArticleMissing) || errors.Is(failure, core.ErrArticleCorrupt) || errors.Is(failure, core.ErrReleaseRejected)) || ctx.Err() != nil {
		return failure
	}
	replacer, ok := p.deps.Hints.(interface {
		ReplaceWarmSource(context.Context, *core.User, core.RatingKey, core.ClientCaps, core.Release, time.Duration, time.Duration, error) error
	})
	if !ok || p.deps.Auth == nil {
		return failure
	}
	users, err := p.deps.Auth.Users(ctx)
	if err != nil {
		return err
	}
	for _, user := range users {
		if user.ID == userID {
			caps, _, _ := p.deps.Hints.ClientCaps(userID, key)
			return replacer.ReplaceWarmSource(ctx, &user, key, caps, old, offset, duration, failure)
		}
	}
	return failure
}
