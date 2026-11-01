package sessions

import (
	"context"
	"sync"
)

// Lifecycle operations lock the owner first, then at most one encoder ID.
// Response streaming never holds these gates. ID gates also serialize reuse
// across titles or owners while the table's ownership claim is rechecked.
type playbackLifecycle struct {
	mu    sync.Mutex
	gates map[string]*playbackGate
}
type playbackGate struct {
	token chan struct{}
	refs  int
}

func newPlaybackLifecycle() *playbackLifecycle {
	return &playbackLifecycle{gates: make(map[string]*playbackGate)}
}
func (l *playbackLifecycle) acquire(ctx context.Context, key string) (func(), error) {
	l.mu.Lock()
	g := l.gates[key]
	if g == nil {
		g = &playbackGate{token: make(chan struct{}, 1)}
		g.token <- struct{}{}
		l.gates[key] = g
	}
	g.refs++
	l.mu.Unlock()
	drop := func() {
		l.mu.Lock()
		g.refs--
		if g.refs == 0 {
			delete(l.gates, key)
		}
		l.mu.Unlock()
	}
	select {
	case <-ctx.Done():
		drop()
		return nil, ctx.Err()
	case <-g.token:
	}
	if err := ctx.Err(); err != nil {
		g.token <- struct{}{}
		drop()
		return nil, err
	}
	var once sync.Once
	return func() { once.Do(func() { g.token <- struct{}{}; drop() }) }, nil
}

// Lifecycle owns the v39 owner and encoder gates.
type Lifecycle = playbackLifecycle

func NewLifecycle() *Lifecycle { return newPlaybackLifecycle() }
func (l *playbackLifecycle) Acquire(ctx context.Context, key string) (func(), error) {
	return l.acquire(ctx, key)
}

// References reports gate holders and waiters, for lifecycle diagnostics.
func (l *playbackLifecycle) References(key string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	if g := l.gates[key]; g != nil {
		return g.refs
	}
	return 0
}
