package playback

import (
	"context"
	"errors"
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
	alt core.Release
	g   *generation
	err error
}

// raceAlternates opens untried alternates concurrently, keeping width in
// flight, and returns the first that opens. Releases that failed are
// returned so the caller does not try them again. Late successes are
// dropped in the background. Add-on stream links that open on demand
// (core.Release.OpensOnDemand) never race: each open makes the add-on fetch
// a release, so the sequential failover tries them one after another.
func (m *Manager) raceAlternates(ctx context.Context, s *session, tried []string, alternates []core.Release, fallback func(int64) core.MediaInfo, width int, accept func(*generation) error, prefer func(*generation) bool) (*generation, []string, error) {
	var cands []core.Release
	for _, alt := range alternates {
		if alt.ID != "" && !alt.OpensOnDemand() && !slices.Contains(tried, alt.ID) && releaseMatchesTitle(alt, s.title) {
			cands = append(cands, alt)
		}
	}
	if m.deps.FetchNZB == nil || len(cands) < 2 {
		return nil, nil, nil
	}
	rctx, cancel := context.WithCancel(ctx)
	results := make(chan raceResult, len(cands))
	next, running := 0, 0
	launch := func() {
		alt := cands[next]
		next++
		running++
		go func() {
			nzb, err := m.deps.FetchNZB(rctx, alt)
			if err != nil {
				results <- raceResult{alt: alt, err: err}
				return
			}
			res := &core.Resolution{Title: s.ident.key, Release: alt, NZB: nzb, ResolvedAt: m.now()}
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
				if next < len(cands) && next < m.opts.MaxAttempts && ctx.Err() == nil {
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
		m.log.Warn("failover candidate failed", "session", s.id, "release", r.alt.Name, "err", r.err)
		if next < len(cands) && (held == nil || next < m.opts.MaxAttempts) {
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
