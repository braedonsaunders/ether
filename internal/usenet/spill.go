package usenet

import (
	"context"
	"slices"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// Cross-tier spillover. Priority tiers are strict while the first usable
// primary provider can start a request at once. When it cannot (every slot
// held or already spoken for by queued work), the request goes to the first
// lower-priority, non-backfill provider that can, instead of queueing behind
// the busy one while a second account idles. Viewer (live) requests spill to
// any such provider with a free slot. Speculative readahead and prefetch
// spill only onto a quiet provider: no viewer work and nothing queued there,
// and at least half its slots still free after this request, so a spill can
// never crowd out the live requests the secondary exists to absorb. Backfill
// (block) accounts are never spill targets: they stay last resort for
// articles the primaries lack.

// serves reports whether a request could be answered here at all, by the
// same checks get makes: idle sockets may be used (no credential lockout or
// command backoff), and either new dials are allowed or an idle socket is
// left (connect backoff pauses dials only).
func (p *pool) serves() bool {
	if p.brk.idleAllowed() != nil {
		return false
	}
	if p.brk.allow() == nil {
		return true
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.idle) > 0
}

// spare is how many more requests at prio this pool could start right now
// without queueing: free semaphore slots minus queued BODY work at the same
// or a more urgent priority. It is zero when the pool cannot serve (see
// serves), and while a handshake backoff would make a fresh connection wait
// and no idle connection is left. until is the readahead burst deadline
// (see BodyPartBurst), zero otherwise.
func (p *pool) spare(prio core.Priority, until time.Time) int {
	if !p.serves() {
		return 0
	}
	prio = clampPriority(prio)
	n := p.sem.available(prio, until)
	if n == 0 {
		return 0
	}
	p.bodyMu.Lock()
	for q := core.PriorityLive; q <= prio; q++ {
		n -= len(p.bodyQueue[q])
	}
	p.bodyMu.Unlock()
	if n <= 0 {
		return 0
	}
	p.mu.Lock()
	if len(p.idle) == 0 && time.Now().Before(p.limit.dialAfter) {
		n = 0
	}
	p.mu.Unlock()
	return n
}

// quiet reports whether speculative work at prio may borrow this pool: it
// serves no live request, nothing waits or is queued on it, and at least
// half its slots stay free after one more request.
func (p *pool) quiet(prio core.Priority, until time.Time) bool {
	s := p.sem.stats()
	if s.Live > 0 || s.Waiting[0]+s.Waiting[1]+s.Waiting[2] > 0 || 2*(s.InUse+1) > s.Size {
		return false
	}
	p.bodyMu.Lock()
	queued := len(p.bodyQueue[0]) + len(p.bodyQueue[1]) + len(p.bodyQueue[2])
	p.bodyMu.Unlock()
	return queued == 0 && p.spare(prio, until) > 0
}

// routeReq describes one request for route.
type routeReq struct {
	prio core.Priority // the slot priority the request will take
	// joinable, when set, reports that a pool already has this article in
	// flight; a request joins that work rather than spill away from it.
	joinable func(*pool) bool
}

// route returns the providers to try for one request: order(), with a
// lower-tier provider moved to the front when the first usable primary is
// saturated (see above). spill is that provider, or nil when tiers stayed
// strict. known lists pool keys already known to lack the article; those
// and providers that cannot serve are not considered usable.
func (f *Fetcher) route(ctx context.Context, req routeReq, known []string) (pools []*pool, spill *pool) {
	prio := req.prio
	pools = f.order()
	usable := func(p *pool) bool {
		return !slices.Contains(known, p.key) && p.serves()
	}
	head := -1
	for i, p := range pools {
		if p.cfg.Backfill {
			break
		}
		if usable(p) {
			head = i
			break
		}
	}
	if head < 0 {
		return pools, nil
	}
	until := burstUntil(ctx)
	first := pools[head]
	if first.spare(prio, until) > 0 || (req.joinable != nil && req.joinable(first)) {
		return pools, nil
	}
	for i := head + 1; i < len(pools); i++ {
		p := pools[i]
		if p.cfg.Backfill {
			break
		}
		if !usable(p) {
			continue
		}
		sameTier := p.cfg.Priority == first.cfg.Priority
		ok := p.spare(prio, until) > 0
		if !sameTier && prio != core.PriorityLive {
			ok = p.quiet(prio, until)
		}
		if !ok {
			continue
		}
		out := make([]*pool, 0, len(pools))
		out = append(out, p)
		out = append(out, pools[:i]...)
		out = append(out, pools[i+1:]...)
		if sameTier {
			// Load balancing inside a tier, not a spill.
			return out, nil
		}
		return out, p
	}
	return pools, nil
}

// routePriority is the slot priority a BODY request will actually take:
// background health checks run at prefetch whatever the caller asked (STAT
// uses commandPriority).
func routePriority(ctx context.Context, prio core.Priority) core.Priority {
	if core.IsBackgroundCheck(ctx) {
		return core.PriorityPrefetch
	}
	return prio
}
