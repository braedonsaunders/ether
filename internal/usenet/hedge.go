package usenet

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/yenc"
)

// Head-of-line hedging for live article fetches.
//
// A viewer-blocking BODY whose provider stalls would otherwise hold the
// player until the idle read deadline (seconds) fails it over. Once a live
// fetch has gone unanswered for max(500ms, 2×p50) of its provider's recent
// live fetches, or max(250ms, p50) once a live fetch issued after it has
// already completed, one duplicate goes out on a separate, never-coalesced
// connection: to another provider with spare capacity first, else to a fresh
// connection of the same provider while it has spare slots, else nowhere.
// The first CRC-verified result wins and the other side is cancelled.
// Speculative lanes are never hedged, block accounts only once the primary
// path has already fallen back to them, and a small fetcher-wide cap bounds
// concurrent hedges so a slow network cannot double its own load.

// hedgePolicy holds the automatic hedge bounds. There are no settings; tests
// shrink the delays.
type hedgePolicy struct {
	floor          time.Duration // least stall before a hedge
	overtakenFloor time.Duration // least stall once a later live fetch finished
	cold           time.Duration // stall threshold before a provider has samples
	recheck        time.Duration // retry interval while no spare capacity exists
	minSamples     int           // live samples needed to trust a provider's p50
	slotsPerHedge  int           // one concurrent hedge per this many primary slots
	off            bool          // tests: measure the unhedged path
}

var defaultHedgePolicy = hedgePolicy{
	floor:          500 * time.Millisecond,
	overtakenFloor: 250 * time.Millisecond,
	cold:           time.Second,
	recheck:        250 * time.Millisecond,
	minSamples:     4,
	slotsPerHedge:  8,
}

// liveHedger tracks in-flight live fetches (for the overtaken signal) and
// in-flight hedges (for the per-article and global bounds).
type liveHedger struct {
	policy  hedgePolicy
	mu      sync.Mutex
	seq     uint64
	pending map[*liveFetch]struct{}
	hedging map[string]struct{} // message-ids with a hedge in flight
	active  int
}

type liveFetch struct {
	seq       uint64
	overtaken chan struct{} // closed when a later live fetch succeeds first
	flagged   bool
}

func newLiveHedger() *liveHedger {
	return &liveHedger{policy: defaultHedgePolicy, pending: make(map[*liveFetch]struct{}), hedging: make(map[string]struct{})}
}

func (h *liveHedger) begin() *liveFetch {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.seq++
	lf := &liveFetch{seq: h.seq, overtaken: make(chan struct{})}
	h.pending[lf] = struct{}{}
	return lf
}

// end retires lf. A success overtakes every live fetch issued before it that
// is still waiting: the clearest sign that one of them is stuck.
func (h *liveHedger) end(lf *liveFetch, ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.pending, lf)
	if !ok {
		return
	}
	for other := range h.pending {
		if other.seq < lf.seq && !other.flagged {
			other.flagged = true
			close(other.overtaken)
		}
	}
}

// admit reserves the single hedge allowed for id within the global cap.
func (h *liveHedger) admit(id string, limit int) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, busy := h.hedging[id]; busy || h.active >= limit {
		return false
	}
	h.hedging[id] = struct{}{}
	h.active++
	return true
}

func (h *liveHedger) release(id string) {
	h.mu.Lock()
	delete(h.hedging, id)
	h.active--
	h.mu.Unlock()
}

// threshold is how long a live fetch on p may go unanswered before a hedge.
func (h *liveHedger) threshold(p *pool, overtaken bool) time.Duration {
	var p50 time.Duration
	n := 0
	if p != nil {
		p.latMu.Lock()
		p50, n = p.liveLat.median()
		p.latMu.Unlock()
	}
	pol := h.policy
	if overtaken {
		if n < pol.minSamples {
			p50 = 0
		}
		return max(pol.overtakenFloor, p50)
	}
	if n < pol.minSamples {
		return max(pol.floor, pol.cold)
	}
	return max(pol.floor, 2*p50)
}

// latencyWindow keeps the most recent live fetch durations for a median.
type latencyWindow struct {
	buf [32]time.Duration
	n   int
}

func (w *latencyWindow) add(d time.Duration) {
	w.buf[w.n%len(w.buf)] = d
	w.n++
}

func (w *latencyWindow) median() (time.Duration, int) {
	k := min(w.n, len(w.buf))
	if k == 0 {
		return 0, 0
	}
	s := w.buf
	slices.Sort(s[:k])
	return s[k/2], k
}

func (p *pool) noteLiveFetch(d time.Duration) {
	p.latMu.Lock()
	p.liveLat.add(d)
	p.latMu.Unlock()
}

// hedgeRace settles which side of one live fetch reaches the viewer.
type hedgeRace struct {
	mu      sync.Mutex
	settled bool
	target  *pool
}

// claim reports whether the caller is first to settle the race.
func (r *hedgeRace) claim() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.settled {
		return false
	}
	r.settled = true
	return true
}

func (r *hedgeRace) setTarget(p *pool) {
	r.mu.Lock()
	r.target = p
	r.mu.Unlock()
}

// wasted charges bytes a losing primary still delivered to the hedge target.
func (r *hedgeRace) wasted(n int64) {
	r.mu.Lock()
	t := r.target
	r.mu.Unlock()
	if t != nil {
		t.hedgeWaste.Add(n)
	}
}

type hedgeResult struct {
	part *yenc.Part
	err  error
}

// bodyLive is BodyPart's live path: the ordinary provider chain, raced by at
// most one hedge once the fetch looks stalled. Healthy fetches pay one timer
// and goroutine; nothing waits on the hedge machinery.
func (f *Fetcher) bodyLive(ctx context.Context, id string, groups []string) (*yenc.Part, error) {
	h := f.live
	if h.policy.off {
		return f.bodyChain(ctx, id, groups, core.PriorityLive, nil)
	}
	lf := h.begin()
	ok := false
	defer func() { h.end(lf, ok) }()

	race := &hedgeRace{}
	var serving atomic.Pointer[pool]
	pctx, pcancel := context.WithCancel(ctx)
	defer pcancel()
	primary := make(chan hedgeResult, 1)
	go func() {
		part, err := f.bodyChain(pctx, id, groups, core.PriorityLive, &serving)
		if err == nil && !race.claim() {
			race.wasted(int64(len(part.Data)))
			return
		}
		primary <- hedgeResult{part, err}
	}()

	start := time.Now()
	timer := time.NewTimer(min(h.policy.floor, h.policy.overtakenFloor))
	defer timer.Stop()
	timerC := timer.C
	overtaken := lf.overtaken
	isOvertaken := false
	var hedged chan hedgeResult
	var hcancel context.CancelFunc
	defer func() {
		if hcancel != nil {
			hcancel()
		}
	}()
	consider := func() {
		if wait := h.threshold(serving.Load(), isOvertaken) - time.Since(start); wait > 0 {
			timer.Reset(wait)
			timerC = timer.C
			return
		}
		if ch, cancel := f.startHedge(ctx, id, groups, serving.Load(), race); ch != nil {
			hedged, hcancel, timerC = ch, cancel, nil
			return
		}
		timer.Reset(h.policy.recheck)
		timerC = timer.C
	}
	var primaryErr error
	for {
		select {
		case r := <-primary:
			if r.err == nil {
				ok = true
				return r.part, nil
			}
			if hedged == nil {
				return nil, r.err
			}
			primary, primaryErr, timerC = nil, r.err, nil
		case r := <-hedged:
			if r.err == nil {
				ok = true
				return r.part, nil
			}
			if primary == nil {
				return nil, primaryErr
			}
			hedged = nil
		case <-timerC:
			timerC = nil
			consider()
		case <-overtaken:
			overtaken, isOvertaken = nil, true
			if hcancel == nil {
				consider()
			}
		case <-ctx.Done():
			race.claim()
			return nil, ctx.Err()
		}
	}
}

// startHedge sends the one duplicate a stalled live fetch may have. It
// returns nil when the bounds or spare capacity do not allow one now.
func (f *Fetcher) startHedge(ctx context.Context, id string, groups []string, primary *pool, race *hedgeRace) (chan hedgeResult, context.CancelFunc) {
	h := f.live
	if !h.admit(id, f.hedgeLimit()) {
		return nil, nil
	}
	target := f.hedgeTarget(id, primary)
	if target == nil {
		h.release(id)
		return nil, nil
	}
	race.setTarget(target)
	target.hedges.Add(1)
	hctx, hcancel := context.WithCancel(ctx)
	// A provider reload or shutdown abandons the hedge rather than waiting.
	stop := context.AfterFunc(target.ctx, hcancel)
	out := make(chan hedgeResult, 1)
	go func() {
		defer target.bodyWG.Done()
		defer stop()
		defer h.release(id)
		part, wire, err := target.hedgeBody(hctx, id, groups)
		if err == nil && race.claim() {
			target.hedgeWins.Add(1)
			out <- hedgeResult{part: part}
			return
		}
		target.hedgeLosses.Add(1)
		target.hedgeWaste.Add(wire)
		if errors.Is(err, ErrNoSuchArticle) {
			f.misses.add(id, target.key)
		}
		if err != nil {
			out <- hedgeResult{err: err}
		}
	}()
	return out, hcancel
}

// hedgeLimit is the fetcher-wide cap on concurrent hedges: one per
// slotsPerHedge primary connection slots, at least one.
func (f *Fetcher) hedgeLimit() int {
	f.mu.RLock()
	defer f.mu.RUnlock()
	slots := 0
	for _, p := range f.pools {
		if !p.cfg.Backfill {
			slots += p.sem.stats().Size
		}
	}
	return max(1, slots/f.live.policy.slotsPerHedge)
}

// hedgeTarget reserves a spare live slot for a hedge: another provider in the
// usual order first, then the primary's own pool. Block accounts qualify only
// when the primary path has already fallen back to one.
func (f *Fetcher) hedgeTarget(id string, primary *pool) *pool {
	known := f.misses.get(id)
	backfill := primary != nil && primary.cfg.Backfill
	for _, p := range f.order() {
		if p == primary || (p.cfg.Backfill && !backfill) || slices.Contains(known, p.key) {
			continue
		}
		if p.reserveHedge() {
			return p
		}
	}
	if primary != nil && !slices.Contains(known, primary.key) && primary.reserveHedge() {
		return primary
	}
	return nil
}

// reserveHedge takes a spare live slot and registers the hedge with the
// pool's body work so an aborting close joins it. Recovery probes are never
// spent on hedges.
func (p *pool) reserveHedge() bool {
	if p.brk.allow() != nil {
		return false
	}
	p.bodyMu.Lock()
	queued := len(p.bodyQueue[core.PriorityLive])
	p.bodyMu.Unlock()
	if queued > 0 || !p.sem.tryAcquireSpare() {
		return false
	}
	if probe, err := p.brk.admit(); err != nil || probe {
		if probe {
			p.brk.releaseProbe()
		}
		p.sem.Release(core.PriorityLive)
		return false
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		p.sem.Release(core.PriorityLive)
		return false
	}
	p.bodyWG.Add(1) // under mu: closeMode sets closed under mu before it Waits
	p.mu.Unlock()
	return true
}

// hedgeBody fetches one article on a connection of its own, holding the slot
// reserveHedge took. It never joins or feeds the coalesced body flights. It
// also reports the bytes read off the wire, for waste accounting.
func (p *pool) hedgeBody(ctx context.Context, id string, groups []string) (*yenc.Part, int64, error) {
	c, err := p.takeIdle()
	if err == nil && c == nil {
		c, err = p.dialNew(ctx)
	}
	if err != nil {
		p.sem.Release(core.PriorityLive)
		return nil, 0, err
	}
	reused := c.uses > 0 || !c.idleSince.IsZero()
	c.uses++
	before := c.wireBytes()
	start := time.Now()
	if p.o.UseGroup && len(groups) > 0 && c.CurrentGroup() != groups[0] {
		if err = c.Group(ctx, groups[0]); errors.Is(err, ErrNoSuchGroup) {
			err = nil
		}
	}
	var part *yenc.Part
	if err == nil {
		part, err = c.BodyDecode(ctx, id, &yenc.DecodeOptions{MaxSize: p.o.MaxArticleSize})
	}
	wire := c.wireBytes() - before
	rtt := c.lastRTT
	p.put(c, core.PriorityLive)
	if err != nil {
		if errors.Is(err, ErrNoSuchArticle) {
			p.bodyMisses.Add(1)
			var pe *ProtocolError
			if errors.As(err, &pe) && pe.Code == 451 {
				p.bodyTakedowns.Add(1)
			}
		}
		return nil, wire, p.failed(err, reused, ctx)
	}
	p.brk.success()
	p.articles.Add(1)
	took := time.Since(start)
	p.latMu.Lock()
	p.latency.add(rtt)
	p.fetching.add(took)
	p.liveLat.add(took)
	p.latMu.Unlock()
	return part, wire, nil
}
