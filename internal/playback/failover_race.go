package playback

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// failoverRaceWidth is how many alternates a mid-stream failover opens at
// once. Opening one is mostly waiting (an indexer fetch, a first article or
// first range, a header read), and the viewer is stalled until one works,
// so trying them one after another multiplied the stall by every broken
// alternate.
const failoverRaceWidth = 3

type raceResult struct {
	alt   core.Release
	g     *generation
	err   error
	fetch bool
}

// raceAlternates opens untried alternates concurrently, keeping width in
// flight, and returns the first that opens. Releases that failed are
// returned so the caller does not try them again. Late successes are
// dropped in the background. Add-on stream links that open on demand
// (core.Release.OpensOnDemand) never race: each open makes the add-on fetch
// a release, so the sequential failover tries them one after another.
func (m *Manager) raceAlternates(ctx context.Context, s *session, tried []string, alternates []core.Release, fallback func(int64) core.MediaInfo, width int, accept func(*generation) error, prefer func(*generation) bool) (*generation, []string, error) {
	var cands []core.Release
	gate := m.preferenceGate(s, tried)
	for _, alt := range alternates {
		if alt.ID != "" && m.ownerPinAllows(s, alt.ID) && !gate.blocks(alt.ID) && !alt.OpensOnDemand() && !slices.Contains(tried, alt.ID) && !m.titleReleaseRejected(ctx, s.ident.key, alt) && releaseMatchesTitle(alt, s.title) {
			cands = append(cands, alt)
		}
	}
	if m.deps.FetchNZB == nil || len(cands) == 0 {
		return nil, nil, nil
	}
	rctx, cancel := context.WithCancel(ctx)
	results := make(chan raceResult, len(cands))
	altSel, altFallback := s.alternateMeta()
	next, running := 0, 0
	launch := func() {
		alt := cands[next]
		next++
		running++
		go func() {
			nzb, err := m.deps.FetchNZB(rctx, alt)
			if err != nil {
				results <- raceResult{alt: alt, err: err, fetch: true}
				return
			}
			res := &core.Resolution{VersionSelector: altSel, QualityFallback: altFallback, Title: s.ident.key, Release: alt, NZB: nzb, ResolvedAt: m.now()}
			g, err := m.openGeneration(rctx, s, res, fallback)
			if err == nil {
				if perr := m.preflightSwap(rctx, s, g); perr != nil {
					m.dropGeneration(g)
					g, err = nil, perr
				}
			}
			if err == nil && accept != nil {
				if e := accept(g); e != nil {
					m.dropGeneration(g)
					g = nil
					err = e
				}
			}
			results <- raceResult{alt: alt, g: g, err: err}
		}()
	}
	// A cancelled caller (a stopped session) starts no provider requests.
	for running < width && next < len(cands) && ctx.Err() == nil {
		launch()
	}
	var failed []string
	var lastErr error
	var held *generation
	var timer *time.Timer
	var deadline <-chan time.Time
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for running > 0 {
		var r raceResult
		select {
		case r = <-results:
		case <-deadline:
			cancel()
			m.drainRace(s, results, running)
			return held, failed, nil
		case <-ctx.Done():
			cancel()
			m.drainRace(s, results, running)
			if held != nil {
				m.dropGeneration(held)
			}
			return nil, failed, ctx.Err()
		}
		running--
		if r.err == nil {
			if prefer != nil && !prefer(r.g) {
				if held == nil {
					held = r.g
					timer = time.NewTimer(500 * time.Millisecond)
					deadline = timer.C
				} else {
					m.dropGeneration(r.g)
				}
				if next < len(cands) && m.attemptAllowed(next) && ctx.Err() == nil {
					launch()
				}
				continue
			}
			cancel() // the rest are no longer needed
			if held != nil {
				m.dropGeneration(held)
			}
			if running > 0 {
				m.drainRace(s, results, running)
			}
			return r.g, failed, nil
		}
		if ctx.Err() != nil {
			cancel()
			m.drainRace(s, results, running)
			if held != nil {
				m.dropGeneration(held)
			}
			return nil, failed, ctx.Err()
		}
		failed = append(failed, r.alt.ID)
		lastErr = r.err
		if r.fetch {
			m.recordTimeline(s, TimelineEvent{
				Kind:    "candidate",
				Level:   "warn",
				Failing: true,
				Message: "Candidate failed: " + releaseLabel(r.alt),
				Detail:  OwnerError(r.err),
			})
		}
		m.log.Warn("failover candidate failed", "session", s.id, "release", r.alt.Name, "err", r.err)
		if next < len(cands) && (held == nil || m.attemptAllowed(next)) {
			launch()
		}
	}
	cancel()
	if held != nil {
		return held, failed, nil
	}
	return nil, failed, lastErr
}

// drainRace waits for the remaining racers, releases any that opened and
// records the ones that failed so later failovers skip them. Racers that
// were only cancelled are left untried.
func (m *Manager) drainRace(s *session, results chan raceResult, n int) {
	m.mu.Lock()
	track := !m.closed
	if track {
		m.wg.Add(1)
	}
	m.mu.Unlock()
	go func() {
		if track {
			defer m.wg.Done()
		}
		for ; n > 0; n-- {
			r := <-results
			switch {
			case r.err == nil && r.g != nil:
				m.dropGeneration(r.g)
			case r.err != nil && !errors.Is(r.err, context.Canceled):
				s.mu.Lock()
				if !slices.Contains(s.tried, r.alt.ID) {
					s.tried = append(s.tried, r.alt.ID)
				}
				s.mu.Unlock()
			}
		}
	}()
}

type initialRaceResult struct {
	alt         core.Release
	g           *generation
	burned      bool
	preferOK    bool
	audioReject bool
	err         error
	fetch       bool
}

type initialRaceOutcome struct {
	g              *generation
	fallbackGen    *generation
	fallbackBurned bool
	failed         []string
	audioRejected  []core.Release
	attempted      int
	lastErr        error
	allUncached    bool
	remaining      []core.Release
}

// raceInitialAlternates opens untried, non-on-demand alternates concurrently
// after the primary candidate fails during initial Play. Keeping failoverRaceWidth
// opens in flight avoids serialising slow indexer fetches and broken archives
// across a large candidate pool, while leaving the happy path (attempt 0)
// untouched. Any alternates ineligible for racing (on-demand add-on links or
// releases needing unknown-audio probe accounting) are returned in remaining
// for the sequential loop.
func (m *Manager) raceInitialAlternates(openCtx context.Context, s *session, profile core.QualityProfile, tried []string, audioRejected []core.Release, alternates []core.Release, fallback func(int64) core.MediaInfo, width, maxBudget int) initialRaceOutcome {
	if m.deps.FetchNZB == nil || len(alternates) < 2 {
		return initialRaceOutcome{remaining: alternates, allUncached: true}
	}
	m.mu.Lock()
	openFailed := make(map[string]bool, len(m.openFailures[s.ident.key]))
	for id := range m.openFailures[s.ident.key] {
		openFailed[id] = true
	}
	if core.IsExplicitVersionSlotSelector(s.prefer) {
		for _, id := range m.staleReleasesLocked(s.ident.key, m.now()) {
			openFailed[id] = true
		}
	}
	m.mu.Unlock()

	var cands, remaining []core.Release
	var skipped []string
	gate := m.preferenceGate(s, tried)
	for _, alt := range alternates {
		if alt.ID == "" || !m.ownerPinAllows(s, alt.ID) || gate.blocks(alt.ID) || !m.channelSourceAllowed(s, alt) ||
			slices.Contains(tried, alt.ID) || slices.Contains(s.caps.BroadcastExclude, alt.ID) ||
			openFailed[alt.ID] || m.titleReleaseRejected(openCtx, s.ident.key, alt) || audioExcluded(audioRejected, alt) || !releaseMatchesTitle(alt, s.title) {
			continue
		}
		if m.releaseNamesakeRejected(openCtx, s, alt) {
			m.markBad(alt.ID, "release names the other title")
			skipped = append(skipped, alt.ID)
			continue
		}
		if alt.OpensOnDemand() {
			remaining = append(remaining, alt)
			continue
		}
		if !m.ownerChoiceCovers(s, alt.ID) {
			if _, unverified := candidateNeedsAudioVerification(profile, s.title, alt); unverified {
				remaining = append(remaining, alt)
				continue
			}
		}
		if maxBudget > 0 && len(cands) >= maxBudget {
			remaining = append(remaining, alt)
			continue
		}
		cands = append(cands, alt)
	}
	if len(cands) < 2 {
		return initialRaceOutcome{failed: skipped, remaining: alternates, allUncached: true}
	}
	rctx, cancel := context.WithCancel(core.WithIndexerPurpose(openCtx, core.IndexerPurposePlayback))
	results := make(chan initialRaceResult, len(cands))
	altSel, altFallback := s.alternateMeta()
	next, running := 0, 0
	launch := func() {
		alt := cands[next]
		next++
		running++
		go func() {
			nzb, err := m.deps.FetchNZB(rctx, alt)
			if err != nil {
				results <- initialRaceResult{alt: alt, err: err, fetch: true}
				return
			}
			res := &core.Resolution{VersionSelector: altSel, QualityFallback: altFallback, Title: s.ident.key, Release: alt, NZB: nzb, ResolvedAt: m.now()}
			if len(res.NZB) > 0 {
				res.Release.SourceHash = fmt.Sprintf("%x", sha256.Sum256(res.NZB))
			}
			if m.titleReleaseRejected(rctx, s.ident.key, res.Release) || audioExcluded(audioRejected, res.Release) {
				results <- initialRaceResult{alt: res.Release, err: core.ErrReleaseRejected}
				return
			}
			cached := m.persistedProbe(rctx, s.ident.userID, s.ident.key, res.Release.ID, res.NZB)
			g, err := m.openGeneration(rctx, s, res, fallback, cached)
			if err != nil {
				results <- initialRaceResult{alt: res.Release, err: err}
				return
			}
			if m.videoOff(s.caps) {
				if dec, needs := m.probedNeedsVideoTranscode(g.infoSnapshot(), s.caps); needs {
					reason := core.ErrVideoTranscodingDisabled.Error()
					if dec.Reason != "" {
						reason += " (" + dec.Reason + ")"
					}
					m.log.Info("trying next candidate", "session", s.id, "release", res.Release.Name, "reason", reason)
					m.recordTimeline(s, TimelineEvent{
						Kind:    "candidate",
						Level:   "info",
						Message: "Skipped candidate " + releaseLabel(res.Release),
						Detail:  reason,
					})
					m.dropGeneration(g)
					results <- initialRaceResult{alt: res.Release, err: core.ErrVideoTranscodingDisabled}
					return
				}
			}
			burned := m.knownHardSubbed(rctx, g)
			if burned && m.hardSubtitleBlocked(rctx, res.Release) {
				m.dropGeneration(g)
				results <- initialRaceResult{alt: res.Release, err: core.ErrReleaseRejected}
				return
			}
			info := g.infoSnapshot()
			if reqReason := probedRequiredAudioRejection(profile, s.title, res.Release, info, false); reqReason != "" {
				m.noteSelectionProbe(s.ident.key, res, info)
				m.log.Info("trying next candidate", "release", res.Release.Name, "reason", reqReason)
				m.dropGeneration(g)
				results <- initialRaceResult{alt: res.Release, audioReject: true, err: errors.New(reqReason)}
				return
			}
			audioOK := m.probedAudioAcceptable(s, profile, info)
			if !audioOK {
				m.noteSelectionProbe(s.ident.key, res, info)
			}
			preferOK := !burned && audioOK
			results <- initialRaceResult{alt: res.Release, g: g, burned: burned, preferOK: preferOK, audioReject: !audioOK}
		}()
	}
	for running < width && next < len(cands) && openCtx.Err() == nil {
		launch()
	}
	out := initialRaceOutcome{
		failed:      skipped,
		allUncached: true,
		remaining:   remaining,
	}
	var (
		held       *generation
		heldBurned bool
		timer      *time.Timer
		deadline   <-chan time.Time
	)
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for running > 0 {
		var r initialRaceResult
		select {
		case r = <-results:
		case <-deadline:
			cancel()
			m.drainInitialRace(s, results, running)
			out.fallbackGen = held
			out.fallbackBurned = heldBurned
			if next < len(cands) {
				out.remaining = append(slices.Clone(cands[next:]), out.remaining...)
			}
			return out
		case <-openCtx.Done():
			cancel()
			m.drainInitialRace(s, results, running)
			out.fallbackGen = held
			out.fallbackBurned = heldBurned
			out.lastErr = openCtx.Err()
			return out
		}
		running--
		if r.err == nil {
			if !r.preferOK {
				reason := "probed release lacks viewer audio language"
				if r.burned {
					reason = "burned-in subtitles detected"
				}
				m.log.Info("trying next candidate", "session", s.id, "release", r.alt.Name, "reason", reason)
				m.recordTimeline(s, TimelineEvent{
					Kind:    "candidate",
					Level:   "info",
					Message: "Skipped candidate " + releaseLabel(r.alt),
					Detail:  reason,
				})
				out.allUncached = false
				out.failed = append(out.failed, r.alt.ID)
				if r.audioReject {
					out.audioRejected = append(out.audioRejected, r.alt)
				}
				out.attempted++
				if held == nil || (heldBurned && !r.burned) {
					if held != nil {
						m.dropGeneration(held)
					}
					held = r.g
					heldBurned = r.burned
					if timer == nil {
						timer = time.NewTimer(500 * time.Millisecond)
						deadline = timer.C
					}
				} else {
					m.dropGeneration(r.g)
				}
				if next < len(cands) && openCtx.Err() == nil {
					launch()
				}
				continue
			}
			cancel()
			if held != nil {
				m.dropGeneration(held)
			}
			if running > 0 {
				m.drainInitialRace(s, results, running)
			}
			out.g = r.g
			return out
		}
		if openCtx.Err() != nil {
			cancel()
			m.drainInitialRace(s, results, running)
			out.fallbackGen = held
			out.fallbackBurned = heldBurned
			out.lastErr = openCtx.Err()
			return out
		}
		out.failed = append(out.failed, r.alt.ID)
		if r.audioReject {
			out.audioRejected = append(out.audioRejected, r.alt)
		}
		if !errors.Is(r.err, core.ErrWrongContent) {
			out.attempted++
		}
		out.lastErr = r.err
		out.allUncached = out.allUncached && errors.Is(r.err, core.ErrNotCached)
		if r.fetch {
			m.log.Warn("alternate nzb fetch failed", "session", s.id, "release", r.alt.Name, "err", r.err)
			m.recordTimeline(s, TimelineEvent{
				Kind:    "candidate",
				Level:   "warn",
				Failing: true,
				Message: "Candidate failed: " + releaseLabel(r.alt),
				Detail:  OwnerError(r.err),
			})
		} else {
			m.log.Warn("release failed to open", "session", s.id, "release", r.alt.Name, "err", r.err)
		}
		if next < len(cands) && openCtx.Err() == nil {
			launch()
		}
	}
	cancel()
	out.fallbackGen = held
	out.fallbackBurned = heldBurned
	return out
}

func (m *Manager) drainInitialRace(s *session, results chan initialRaceResult, n int) {
	m.mu.Lock()
	track := !m.closed
	if track {
		m.wg.Add(1)
	}
	m.mu.Unlock()
	go func() {
		if track {
			defer m.wg.Done()
		}
		for ; n > 0; n-- {
			r := <-results
			switch {
			case r.err == nil && r.g != nil:
				m.dropGeneration(r.g)
			case r.err != nil && !errors.Is(r.err, context.Canceled) && !errors.Is(r.err, errRuntimeMismatchDeferred):
				s.mu.Lock()
				if !slices.Contains(s.tried, r.alt.ID) {
					s.tried = append(s.tried, r.alt.ID)
				}
				s.mu.Unlock()
			}
		}
	}()
}
