package plex

import (
	"context"
	"sync"
)

// bgQueue runs keyed background jobs on a fixed set of workers. A key that
// is already queued or running is not queued again, and jobs that don't fit
// the queue are dropped (the caller simply asks again on a later request),
// so a burst of listings can never pile up unbounded work or block a
// response.
type bgQueue struct {
	ctx  context.Context
	jobs chan bgJob

	mu      sync.Mutex
	idle    *sync.Cond
	pending map[string]bool // queued or running
}

type bgJob struct {
	key string
	run func(ctx context.Context)
}

// newBGQueue starts workers that stop when ctx is cancelled; wg tracks them.
func newBGQueue(ctx context.Context, wg *sync.WaitGroup, workers, depth int) *bgQueue {
	q := &bgQueue{ctx: ctx, jobs: make(chan bgJob, depth), pending: map[string]bool{}}
	q.idle = sync.NewCond(&q.mu)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go q.worker(wg)
	}
	// Wake waiters on shutdown so wait never hangs.
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-ctx.Done()
		q.mu.Lock()
		q.idle.Broadcast()
		q.mu.Unlock()
	}()
	return q
}

// submit queues run under key. It reports whether the job was queued (false
// when the key is already pending, the queue is full or the server is
// shutting down).
func (q *bgQueue) submit(key string, run func(ctx context.Context)) bool {
	if q.ctx.Err() != nil {
		return false
	}
	q.mu.Lock()
	if q.pending[key] {
		q.mu.Unlock()
		return false
	}
	q.pending[key] = true
	q.mu.Unlock()
	select {
	case q.jobs <- bgJob{key: key, run: run}:
		return true
	default:
		q.done(key)
		return false
	}
}

func (q *bgQueue) worker(wg *sync.WaitGroup) {
	defer wg.Done()
	for {
		select {
		case <-q.ctx.Done():
			return
		case j := <-q.jobs:
			func() {
				defer q.done(j.key)
				defer func() { _ = recover() }() // a bad job must not kill the worker
				j.run(q.ctx)
			}()
		}
	}
}

func (q *bgQueue) done(key string) {
	q.mu.Lock()
	delete(q.pending, key)
	if len(q.pending) == 0 {
		q.idle.Broadcast()
	}
	q.mu.Unlock()
}

// waitBackground blocks until the enrichment queues are idle. A finished
// title fetch can queue colour work, so it loops until both are quiet.
func (s *Server) waitBackground() {
	if s.titleJobs == nil {
		if s.d.PlexGUID != nil {
			s.d.PlexGUID.Wait()
		}
		if s.d.PlexMeta != nil {
			s.d.PlexMeta.Wait()
		}
		return
	}
	for i := 0; i < 4; i++ {
		s.titleJobs.wait()
		s.blurJobs.wait()
		if s.d.PlexGUID != nil {
			s.d.PlexGUID.Wait()
		}
		if s.d.PlexMeta != nil {
			s.d.PlexMeta.Wait()
		}
	}
}

// wait blocks until nothing is queued or running (or the queue is shut
// down). Tests use it to make background enrichment deterministic.
func (q *bgQueue) wait() {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.pending) > 0 && q.ctx.Err() == nil {
		q.idle.Wait()
	}
}
