package resolver

import (
	"context"
	"errors"
	"net"
	"os"
	"slices"
	"sync"
	"syscall"

	"github.com/braedonsaunders/ether/internal/core"
)

// unverifiedReason marks a winner committed to after every health check
// timed out: nothing contradicted it, but nothing confirmed it either.
const unverifiedReason = "played unverified after health checks timed out"

// unverifiedPool collects, for one Resolve call, the candidates whose NZB
// was fetched but whose health check timed out. A congested provider can
// time out every check while it is still serving articles; when nothing
// verifies, the best-ranked of these is played rather than nothing, and
// mid-stream failover covers the rest. An unreachable provider is not
// congestion: those checks never qualify, so an outage still fails fast.
type unverifiedPool struct {
	mu   sync.Mutex
	nzbs map[string][]byte // release id -> fetched NZB
}

type unverifiedPoolKey struct{}

func withUnverifiedPool(ctx context.Context) (context.Context, *unverifiedPool) {
	p := &unverifiedPool{nzbs: map[string][]byte{}}
	return context.WithValue(ctx, unverifiedPoolKey{}, p), p
}

// noteValidation keeps a timed-out candidate's NZB for the last resort.
// It is a no-op outside a Resolve call and for every other outcome.
func noteValidation(ctx context.Context, rel core.Release, v validation) {
	p, _ := ctx.Value(unverifiedPoolKey{}).(*unverifiedPool)
	if p == nil || !inconclusiveCheck(v) {
		return
	}
	p.mu.Lock()
	p.nzbs[rel.ID] = v.nzb
	p.mu.Unlock()
}

// drainValidations notes the results of checks still running when a
// validation pass ended early (its budget ran out). Call after joining the
// workers, so every result they will send is already buffered.
func drainValidations(ctx context.Context, cands []candidate, results <-chan validation) {
	for {
		select {
		case v := <-results:
			noteValidation(ctx, cands[v.index].rel, v)
		default:
			return
		}
	}
}

// inconclusiveCheck reports a check that fetched the NZB and then ran out
// of time (see checkTimedOut). Every other failure is either decisive
// (missing or corrupt articles, an archive or PAR2 rejection, a debrid
// miss, an unhealthy sample) or means no provider could be reached.
func inconclusiveCheck(v validation) bool {
	return len(v.nzb) > 0 && v.healthChecked && checkTimedOut(v.err)
}

// checkTimedOut reports a timeout-class check failure: the check's or the
// validation budget's deadline, or an i/o deadline such as a provider's
// idle-read (stall) timeout. Those happen while a provider is slow but
// answering. It is false for anything saying the providers cannot serve
// at all, even when it also carries a timeout: none usable, every one in
// breaker backoff or locked out (core.ErrSourceUnavailable), and failed
// dials, refused connections or DNS errors.
func checkTimedOut(err error) bool {
	if err == nil || permanentFailure(err) || errors.Is(err, core.ErrSourceUnavailable) || errors.Is(err, syscall.ECONNREFUSED) {
		return false
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return false
	}
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "dial" {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// lastResort commits to the best-ranked candidate whose checks were only
// inconclusive, when no candidate verified. ranked is every candidate this
// call considered, best first (a viewer's or owner's preferred release
// leads). The winner is not recorded as verified or as available; the
// other candidates without a confirmed failure stay as failover alternates.
// Background checks (availability validation) never take it: for them an
// unverified release is no answer.
func (s *Service) lastResort(ctx context.Context, p *unverifiedPool, key core.RatingKey, ranked []candidate, tr *Trace) *core.Resolution {
	if ctx.Err() != nil || core.IsBackgroundCheck(ctx) {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range ranked {
		nzb, ok := p.nzbs[c.rel.ID]
		// Another Resolve may have confirmed this release dead meanwhile.
		if !ok || s.recentlyFailedHealth(c.rel.ID) || s.isBad(c.rel.ID) {
			continue
		}
		var keep []candidate
		for _, o := range ranked {
			if !slices.ContainsFunc(keep, func(k candidate) bool { return k.rel.ID == o.rel.ID }) {
				keep = append(keep, o)
			}
		}
		winner := slices.IndexFunc(keep, func(k candidate) bool { return k.rel.ID == c.rel.ID })
		ct := &tr.Candidates[c.trace]
		ct.Reason = unverifiedReason + ": " + ct.Reason
		tr.Winner = c.rel.ID
		tr.Unverified = true
		tr.Err = ""
		s.log.Info("resolver: playing unverified release after health checks timed out", "title", key, "release", c.rel.ID)
		// Inconclusive candidates stay usable alternates; confirmed
		// failures and fetches that produced no NZB do not.
		return s.resolutionSkipping(key, keep, winner, nzb, func(o candidate) bool {
			_, inconclusive := p.nzbs[o.rel.ID]
			return tr.Candidates[o.trace].Rejected && !inconclusive
		})
	}
	return nil
}

// concat joins candidate lists into a new slice, preserving their order.
func concat(lists ...[]candidate) []candidate {
	var out []candidate
	for _, l := range lists {
		out = append(out, l...)
	}
	return out
}
