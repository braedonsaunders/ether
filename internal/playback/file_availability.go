package playback

import (
	"context"
	"errors"

	"github.com/braedonsaunders/ether/internal/core"
)

// File checks are explicit source probes, not playback admissions. Bound their
// background work independently so metadata cannot consume viewer stream slots.
var fileCheckSlots = make(chan struct{}, 8)

// errFileCheckInconclusive is the no-verdict answer: the check could not show
// that this version is unplayable (or playable), so callers keep the offered
// state. Playback-time resolution does the full search and reports its own
// specific error if the title truly cannot play.
var errFileCheckInconclusive = errors.New("playback: file check inconclusive")

// CheckFileAvailability probes one version of a title for Plex's checkFiles.
// It returns a verdict, (live, nil), or an error meaning no verdict; callers
// must not read an error as "missing".
//
// Plex apps hide Play when a Part reads accessible="0", so false is reserved
// for definitive evidence that this exact version cannot play: its release is
// blocked (marked bad or owner-blocked), its source opened with a permanent
// failure (articles gone, provider no longer has the file, release rejected),
// or it opened with zero size. Auto (release "") is never reported missing:
// play time resolves past any one bad candidate, so an Auto check only primes
// the source. A different preferred release, an empty or unfinished warm-up
// search, a timeout or a busy slot are all inconclusive. So, by default, is an
// add-on stream: opening its link makes the add-on fetch the release, which a
// detail page must not cause (core.WithoutOnDemandStreams). The owner may let
// this one check open those links (resolver.openOnDemandAddonStreams); when
// that setting is off the behaviour above is unchanged.
func (m *Manager) CheckFileAvailability(ctx context.Context, user *core.User, key core.RatingKey, caps core.ClientCaps, release string) (bool, error) {
	if user == nil {
		return false, core.ErrUnauthorized
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	openAddon := m.config().Resolver.OpenOnDemandAddonStreams
	if openAddon {
		ctx = core.WithOnDemandStreamsForAvailability(ctx)
	} else {
		ctx = core.WithoutOnDemandStreams(ctx)
	}
	ctx, cancel := context.WithTimeout(ctx, m.config().Plex.FileCheckTimeoutOrDefault())
	defer cancel()
	if release != "" {
		if checker, ok := m.deps.Resolver.(core.ReleaseBlockChecker); ok {
			if blocked, err := checker.ReleaseBlocked(ctx, release); err == nil && blocked {
				return false, nil
			}
		}
	}
	if _, remembered, ok := m.ResumedTracks(ctx, user, key, caps); ok && (release == "" || release == remembered) {
		return true, nil
	}
	if core.LocalResponse(ctx) {
		// Other offered versions have no current local verdict. Keep them
		// accessible rather than reopening every source to paint one page.
		return false, errFileCheckInconclusive
	}
	select {
	case fileCheckSlots <- struct{}{}:
	default:
		return false, errFileCheckInconclusive
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		<-fileCheckSlots
		return false, errFileCheckInconclusive
	}
	m.wg.Add(1)
	m.mu.Unlock()
	type result struct {
		live bool
		err  error
	}
	done := make(chan result, 1)
	u := *user
	go func() {
		defer m.wg.Done()
		defer func() { <-fileCheckSlots }()
		stop := context.AfterFunc(m.ctx, cancel)
		defer stop()
		done <- func() result {
			title, err := m.deps.Catalog.Get(ctx, key)
			if err != nil || title == nil {
				return result{err: errFileCheckInconclusive}
			}
			sctx, _ := core.WithSpeculative(ctx)
			wctx := core.WithIndexerPurpose(sctx, core.IndexerPurposeWarmup)
			res, err := m.deps.Resolver.Resolve(wctx, core.ResolveRequest{Title: title, Client: caps, User: &u, Prefer: release, PreferredOnly: release != ""})
			// An on-demand release is inconclusive when the owner has not let
			// this check open it: without that permission the resolver stops
			// at the link without opening it. With permission it opened and
			// validated the release, so measure it like any other source.
			if err != nil || res == nil || release != "" && !res.MatchesVersion(release) || (res.Release.OpensOnDemand() && !openAddon) {
				// Nothing found yet, or the resolver prefers another release
				// (this one may simply be outside the bounded warm-up search).
				return result{err: errFileCheckInconclusive}
			}
			lifetime := newSourceLifetime(u.ID, string(caps.Protocol), caps.ClientID)
			defer lifetime.seal()
			src, finish, err := m.openSource(wctx, lifetime, res)
			if err != nil {
				if release != "" && permanentSourceFailure(err) {
					return result{}
				}
				return result{err: errFileCheckInconclusive}
			}
			live := src.Size() > 0
			finish(src.Close())
			if !live && release == "" {
				return result{err: errFileCheckInconclusive}
			}
			return result{live: live}
		}()
	}()
	select {
	case r := <-done:
		return r.live, r.err
	case <-ctx.Done():
		return false, ctx.Err()
	}
}
