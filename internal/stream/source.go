package stream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// maxCursors is how many independent sequential readers a source tracks
// (a player reading the video while a prober reads the index, ...).
const maxCursors = 4

// backSlack tolerates small backwards steps of a sequential reader.
const backSlack = 1 << 20

// cursor follows one sequential reader.
type cursor struct {
	requestBegin int64 // foreground start; next includes bytes still being read
	next         int64 // offset after the last byte read
	window       int64 // current read-ahead window
	frontier     int64 // read-ahead scheduled up to here
	used         time.Time
	verifying    bool // one archive-layout verification per cursor
	blocked      bool // failed verification waits for a new foreground read
}

// source implements core.MediaSource.
type source struct {
	recovery  repairObserver
	bulk      *bulkReader
	e         *Engine
	rel       *release
	plan      *plan
	releaseID string
	fo        fetchOpts

	ctx    context.Context
	cancel context.CancelFunc

	mu      sync.Mutex
	cursors []*cursor
	closed  bool

	damage damageRecovery

	raSem     chan struct{}
	raWG      sync.WaitGroup
	raPumping bool // protected by mu; one source-local scheduling pump
	raPending bool // coalesced foreground/completion wakeup
}

func newSource(e *Engine, rel *release, p *plan, releaseID string) *source {
	ctx, cancel := context.WithCancel(e.ctx)
	e.active.Add(1)
	s := &source{
		e: e, rel: rel, plan: p, releaseID: releaseID,
		bulk:   &bulkReader{e: e, r: rel, prio: core.PriorityReadahead, fo: fetchOpts{cacheID: releaseID}},
		fo:     fetchOpts{cacheID: releaseID},
		ctx:    ctx,
		cancel: cancel,
		raSem:  make(chan struct{}, e.opts.ReadAheadParallel),
	}
	s.fo.repairProgress = s.reportRepair
	s.bulk.fo = s.fo
	return s
}

func (s *source) Size() int64       { return s.plan.size }
func (s *source) Name() string      { return s.plan.name }
func (s *source) ReleaseID() string { return s.releaseID }

// Close stops read-ahead and releases the index. Safe to call twice.
func (s *source) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()
	s.cancel()
	s.raWG.Wait()
	s.e.active.Add(-1)
	s.e.release(s.rel)
	return nil
}

// ReadAt implements io.ReaderAt. It is safe for concurrent use. A segment
// missing on every provider surfaces as an error wrapping
// core.ErrArticleMissing.
func (s *source) ReadAt(p []byte, off int64) (int, error) {
	return s.readAt(s.ctx, p, off, true)
}

// ReadAtContext lets HTTP readers cancel an in-flight segment read.
func (s *source) ReadAtContext(ctx context.Context, p []byte, off int64) (int, error) {
	return s.readAt(ctx, p, off, true)
}

// ReadAtPrefetchContext reads without speculative read-ahead and yields to
// live playback by assigning every missing article background priority.
func (s *source) ReadAtPrefetchContext(ctx context.Context, p []byte, off int64) (int, error) {
	return s.readAtPriority(ctx, p, off, core.PriorityPrefetch)
}

// ReadMetadataAtContext avoids speculative read-ahead for bounded index reads.
func (s *source) ReadMetadataAtContext(ctx context.Context, p []byte, off int64) (int, error) {
	return s.readAt(ctx, p, off, false)
}

func (s *source) readAt(parent context.Context, p []byte, off int64, ahead bool) (int, error) {
	return s.readAtWithPriority(parent, p, off, ahead, core.PriorityLive)
}

func (s *source) readAtPriority(parent context.Context, p []byte, off int64, prio core.Priority) (int, error) {
	return s.readAtWithPriority(parent, p, off, false, prio)
}

func (s *source) readAtWithPriority(parent context.Context, p []byte, off int64, ahead bool, prio core.Priority) (int, error) {
	if off < 0 {
		return 0, fmt.Errorf("stream: negative offset %d", off)
	}
	if s.ctx.Err() != nil {
		return 0, ErrClosed
	}
	size := s.plan.size
	if off >= size {
		return 0, io.EOF
	}
	want := p
	if rem := size - off; int64(len(want)) > rem {
		want = want[:rem]
	}
	if ahead {
		s.readAhead(off, int64(len(want)))
	}

	ctx, cancel := context.WithTimeout(core.WithStreamKey(parent, s), s.e.opts.ReadTimeout)
	stop := context.AfterFunc(s.ctx, cancel)
	defer stop()
	defer cancel()
	n, err := s.read(ctx, want, off, prio)
	if err != nil {
		if errors.Is(err, context.Canceled) && s.ctx.Err() != nil {
			err = ErrClosed
		} else if errors.Is(err, context.DeadlineExceeded) {
			err = fmt.Errorf("stream: read %d bytes at %d of %s timed out: %w", len(want), off, s.plan.name, err)
		} else if IsRejection(err) {
			err = fmt.Errorf("%w: %w", core.ErrReleaseRejected, err)
		}
		return n, err
	}
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// read fills p (within the video) at off.
func (s *source) read(ctx context.Context, p []byte, off int64, prio core.Priority) (int, error) {
	return s.readWithDamageRecovery(ctx, p, off, prio)
}

// readPlan reads video bytes through the plan's layout, verifying volumes
// before serving from them.
func readPlan(ctx context.Context, e *Engine, rel *release, p *plan, buf []byte, off int64, prio core.Priority, fo fetchOpts) (int, error) {
	if p.encrypted != nil {
		return readEncryptedPlan(ctx, e, rel, p, buf, off, prio, fo)
	}
	v := verifier{e: e, prio: prio, fo: fo}
	for attempt := 0; ; attempt++ {
		lay := p.layout()
		pcs := lay.pieces(off, int64(len(buf)))
		var unverified []int
		for _, pc := range pcs {
			if !p.isVerified(pc.vol) {
				unverified = append(unverified, pc.vol)
			}
		}
		if len(unverified) > 0 {
			if attempt > 3 {
				return 0, fmt.Errorf("stream: %s layout keeps changing: %w", p.name, ErrCorrupt)
			}
			// Fetch the wanted segments speculatively while the volume
			// headers are checked; they are usually right. The read below
			// joins the same flights, so these waiters are canceled and
			// joined before returning, and never enter repair themselves.
			stop := speculate(ctx, e, rel, p, pcs, prio, fo)
			err := p.ensure(ctx, v, unverified)
			if err != nil || p.layout().version != lay.version {
				stop()
				if err != nil {
					return 0, err
				}
				continue
			}
			defer stop()
		}
		var n int
		for _, pc := range pcs {
			m, err := e.readFile(ctx, rel, p.vols[pc.vol], buf[pc.bufOff:pc.bufOff+pc.n], pc.off, prio, fo)
			n += m
			if err != nil {
				if err == io.EOF {
					err = fmt.Errorf("stream: volume %d of %s ended early: %w", pc.vol+1, p.name, ErrCorrupt)
				}
				return n, err
			}
		}
		return n, nil
	}
}

// speculate starts one fetch per slot of pcs and returns a function that
// cancels and joins them.
func speculate(ctx context.Context, e *Engine, rel *release, p *plan, pcs []piece, prio core.Priority, fo fetchOpts) func() {
	ctx, cancel := context.WithCancel(ctx)
	fo.noRepair = true
	var wg sync.WaitGroup
	for _, pc := range pcs {
		pf := p.vols[pc.vol]
		for _, i := range pf.slotsFor(pc.off, pc.n) {
			wg.Go(func() { _, _ = e.segment(ctx, rel, pf, i, prio, fo) })
		}
	}
	return func() { cancel(); wg.Wait() }
}

// readAhead updates the reader cursors and schedules background fetches
// ahead of a sequential reader.
func (s *source) readAhead(off, n int64) {
	now := time.Now()
	end := off + n
	opts := s.e.opts
	// A speculative window must coexist with the foreground request in the
	// decoded cache. Reserve half the cache for demand reads, and account for
	// whole articles straddling the read/window boundaries. Otherwise a small
	// cache can evict prefetched bytes before the player ever consumes them.
	partSize := s.rel.partSize.Load()
	if partSize <= 0 {
		partSize = 716800
	}
	windowCap := min(opts.ReadAheadMax, opts.HotBufferBytes/2,
		max(int64(0), opts.HotBufferBytes-n-2*partSize))

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	var c *cursor
	for _, cur := range s.cursors {
		if off >= cur.next-backSlack && off <= cur.next+cur.window {
			c = cur
			break
		}
	}
	if c == nil {
		initial := opts.ReadAheadMin
		if s.e.repairOptions().Enabled {
			initial = max(initial, 32<<20)
		}
		c = &cursor{next: end, window: min(initial, windowCap), frontier: end}
		if len(s.cursors) < maxCursors {
			s.cursors = append(s.cursors, c)
		} else {
			oldest := 0
			for i, cur := range s.cursors {
				if cur.used.Before(s.cursors[oldest].used) {
					oldest = i
				}
			}
			s.cursors[oldest] = c
		}
	} else {
		if off >= c.next-backSlack && end > c.next {
			// Sequential progress: grow the window with the bytes consumed.
			c.window = min(windowCap, c.window+2*(end-c.next))
		}
		c.next = max(c.next, end)
	}
	c.requestBegin = off
	c.window = min(c.window, windowCap)
	c.used = now
	c.blocked = false // a new demand read permits retrying a failed layout check
	s.mu.Unlock()
	s.kickReadAhead()
}

// kickReadAhead coalesces wakeups into one source-local scheduler. Workers add
// its reference before dropping their own, so Close/Wait cannot miss a refill.
func (s *source) kickReadAhead() {
	s.mu.Lock()
	if s.closed || s.ctx.Err() != nil {
		s.mu.Unlock()
		return
	}
	s.raPending = true
	if s.raPumping {
		s.mu.Unlock()
		return
	}
	s.raPumping = true
	s.raWG.Add(1)
	s.mu.Unlock()
	go s.pumpReadAhead()
}

func (s *source) pumpReadAhead() {
	defer s.raWG.Done()
	for {
		s.mu.Lock()
		if s.closed || s.ctx.Err() != nil || !s.raPending {
			s.raPumping = false
			s.mu.Unlock()
			return
		}
		s.raPending = false
		cursors := append([]*cursor(nil), s.cursors...)
		s.mu.Unlock()
		for _, c := range cursors {
			s.mu.Lock()
			present := false
			for _, current := range s.cursors {
				if current == c {
					present = true
					break
				}
			}
			if !present || s.closed || c.blocked || c.verifying {
				s.mu.Unlock()
				continue
			}
			from := max(c.frontier, c.next)
			to := min(c.next+c.window, s.plan.size)
			if from >= to {
				s.mu.Unlock()
				continue
			}
			c.frontier = to // this pump alone claims frontiers; completions only wake it
			s.mu.Unlock()
			scheduled := s.scheduleCursor(from, to, c)
			s.mu.Lock()
			if c.frontier == to && scheduled < to {
				c.frontier = scheduled
			}
			s.mu.Unlock()
		}
	}
}

// cursorWindow rejects work from evicted seek cursors and rechecks a window
// shrunk by a simultaneous foreground read. Already running jobs keep their
// original lifetime, but their completion cannot revive an obsolete cursor.
func (s *source) cursorWindow(c *cursor) (int64, int64, bool) {
	if c == nil {
		return 0, s.plan.size, true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || c.blocked {
		return 0, 0, false
	}
	for _, current := range s.cursors {
		if current == c {
			return c.next, min(c.next+c.window, s.plan.size), true
		}
	}
	return 0, 0, false
}

// repairNeeded includes the current demand read, not only the speculative
// window: advancing next before reading must not cancel the repair being joined.
func (s *source) repairNeeded(c *cursor, begin, end int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	if c == nil {
		return true
	}
	for _, current := range s.cursors {
		if current == c {
			return !c.blocked && end > c.requestBegin && begin < min(c.next+c.window, s.plan.size)
		}
	}
	return false
}

// schedule starts read-ahead fetches for [from, to) without blocking and
// returns how far it got (the start of the first slot it could not start).
func (s *source) schedule(from, to int64) int64 { return s.scheduleCursor(from, to, nil) }

func (s *source) scheduleCursor(from, to int64, c *cursor) int64 {
	lay := s.plan.layout()
	for _, pc := range lay.pieces(from, to-from) {
		pf := s.plan.vols[pc.vol]
		if _, limit, valid := s.cursorWindow(c); !valid || from+pc.bufOff >= limit {
			return from + pc.bufOff
		}
		if !s.plan.isVerified(pc.vol) {
			// A failed verification must not become a self-sustaining retry
			// loop. Only a later foreground read unblocks this cursor.
			vol := pc.vol
			if c != nil {
				s.mu.Lock()
				c.verifying = true
				s.mu.Unlock()
			}
			started := s.startRA(func() bool {
				err := s.plan.ensure(s.ctx, verifier{e: s.e, prio: core.PriorityReadahead, fo: s.fo}, []int{vol})
				if c != nil {
					s.mu.Lock()
					c.verifying = false
					c.blocked = err != nil
					s.mu.Unlock()
				}
				return err == nil
			})
			if !started && c != nil {
				s.mu.Lock()
				c.verifying = false
				s.mu.Unlock()
			}
			return from + pc.bufOff
		}
		for _, i := range pf.slotsFor(pc.off, pc.n) {
			begin, end, _ := pf.rangeOf(i)
			inner := from + pc.bufOff + max(0, begin-pc.off)
			innerEnd := from + pc.bufOff + min(pc.n, end-pc.off)
			floor, limit, valid := s.cursorWindow(c)
			if !valid || inner >= limit {
				return inner
			}
			if innerEnd <= floor {
				continue
			}
			key := segKey{s.rel.digest, pf.segBase + i}
			if s.e.hot.has(key) {
				continue
			}
			started := s.startRA(func() bool {
				fo := s.fo
				if s.e.repairOptions().Enabled {
					fo = s.bulk.options()
					fo.noRepair = false
				}
				fo.repairValid = func() bool {
					return s.repairNeeded(c, inner, innerEnd)
				}
				_, err := s.e.segment(s.ctx, s.rel, pf, i, core.PriorityReadahead, fo)
				if err != nil && s.ctx.Err() == nil && !isMissing(err) {
					s.e.log.Debug("read-ahead fetch failed", "name", s.plan.name, "part", i+1, "err", err)
				}
				return err == nil
			})
			if !started {
				b, _, _ := pf.rangeOf(i)
				inner := from + pc.bufOff + max(0, b-pc.off)
				return inner
			}
		}
	}
	return to
}

// startRA runs fn as a read-ahead worker when a slot is free and the
// source is still open. The closed check and raWG.Add happen under s.mu, the
// lock Close sets closed under before raWG.Wait, so no worker starts once
// Close is waiting for them (Add must not race Wait, and a worker must not
// outlive the release reference Close drops).
func (s *source) startRA(fn func() bool) bool {
	select {
	case s.raSem <- struct{}{}:
	default:
		return false
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		<-s.raSem
		return false
	}
	s.raWG.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.raWG.Done()
		progress := fn()
		<-s.raSem
		if progress {
			s.kickReadAhead()
		}
	}()
	return true
}

// prefetchRange warms a byte range of the video into the segment cache.
func (e *Engine) prefetchRange(ctx context.Context, rel *release, p *plan, releaseID string, offset, length int64) error {
	if offset < 0 {
		offset = 0
	}
	if offset >= p.size || length <= 0 {
		return nil
	}
	if length > p.size-offset {
		length = p.size - offset
	}
	fo := fetchOpts{cacheID: releaseID, noHot: e.cache != nil && releaseID != ""}
	if err := p.ensure(ctx, verifier{e: e, prio: core.PriorityPrefetch, fo: fo}, volsOf(p.fetchPieces(offset, length))); err != nil {
		return err
	}
	type job struct {
		pf *postedFile
		i  int
	}
	var jobs []job
	hc, _ := e.cache.(hasCache)
	for _, pc := range p.fetchPieces(offset, length) {
		pf := p.vols[pc.vol]
		for _, i := range pf.slotsFor(pc.off, pc.n) {
			if fo.noHot && hc != nil && hc.Has(releaseID, pf.segBase+i) {
				continue
			}
			if e.hot.has(segKey{rel.digest, pf.segBase + i}) && !fo.noHot {
				continue
			}
			jobs = append(jobs, job{pf, i})
		}
	}
	sem := make(chan struct{}, e.opts.PrefetchParallel)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var first error
	for _, j := range jobs {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func(j job) {
			defer wg.Done()
			defer func() { <-sem }()
			seg, err := e.segment(ctx, rel, j.pf, j.i, core.PriorityPrefetch, fo)
			if err != nil {
				mu.Lock()
				if first == nil {
					first = err
				}
				mu.Unlock()
				return
			}
			// A segment served from the hot buffer or a flight another
			// reader started may not be on disk yet; prefetch exists to put
			// it there, so finish that before reporting success.
			global := j.pf.segBase + j.i
			if fo.noHot && hc != nil && !hc.Has(releaseID, global) {
				if perr := e.cache.Put(releaseID, global, encodeSegment(seg)); perr != nil {
					e.log.Debug("prefetch cache put failed", "release", releaseID, "segment", global, "err", perr)
				}
			}
		}(j)
	}
	wg.Wait()
	if first != nil {
		return first
	}
	return ctx.Err()
}

func volsOf(pcs []piece) []int {
	var out []int
	seen := map[int]bool{}
	for _, pc := range pcs {
		if !seen[pc.vol] {
			seen[pc.vol] = true
			out = append(out, pc.vol)
		}
	}
	return out
}
