// Package flight collapses concurrent calls for the same key into one
// execution whose result every caller shares.
package flight

import (
	"context"
	"errors"
	"sync"
)

// ErrCapacity means a new key was refused before its function started.
var ErrCapacity = errors.New("flight: capacity reached")

// Group runs at most one call per key at a time. The zero Group is ready.
type Group[K comparable, V any] struct {
	mu    sync.Mutex
	calls map[K]*call[V]
}

type call[V any] struct {
	done chan struct{}
	val  V
	err  error
	// abandoned reports that a leader-owned call ended because its
	// leader's context did; that says nothing about the other callers.
	abandoned bool
}

// Do shares fn among concurrent callers of key. The first caller runs fn on
// its own goroutine and context, so no work outlives a caller that owns it.
// Others wait; if the leader's context ended the call, a waiter whose own
// context is live runs fn itself instead of inheriting that cancellation.
// Callers share the returned value and must not mutate it.
func (g *Group[K, V]) Do(ctx context.Context, key K, fn func(context.Context) (V, error)) (V, error) {
	for {
		var zero V
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		c, leader := g.join(key)
		if leader {
			c.val, c.err = fn(ctx)
			c.abandoned = c.err != nil && ctx.Err() != nil
			g.finish(key, c)
			return c.val, c.err
		}
		select {
		case <-ctx.Done():
			return zero, ctx.Err()
		case <-c.done:
		}
		if !c.abandoned {
			return c.val, c.err
		}
	}
}

// DoDetached shares fn among concurrent callers of key. fn runs to
// completion on its own goroutine with a context that keeps the first
// caller's values but none of its cancellation, so one caller giving up
// never fails the others; fn must bound its own work. Every caller,
// including the first, returns ctx.Err() as soon as its context ends.
// Callers share the returned value and must not mutate it.
func (g *Group[K, V]) DoDetached(ctx context.Context, key K, fn func(context.Context) (V, error)) (V, error) {
	return g.DoDetachedLimit(ctx, key, 0, fn)
}

// DoDetachedLimit is DoDetached with atomic admission of new keys. A positive
// max bounds the total keys in flight; zero or negative means unlimited.
// Existing-key waiters always join, even at capacity. Refused calls return
// ErrCapacity without running fn. Cancellation of a caller does not release
// capacity: the detached function owns its slot until it completes.
// Unrestricted Do/DoDetached calls do not enforce this per-call limit.
func (g *Group[K, V]) DoDetachedLimit(ctx context.Context, key K, max int, fn func(context.Context) (V, error)) (V, error) {
	var zero V
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	c, leader := g.joinLimit(key, max)
	if c == nil {
		return zero, ErrCapacity
	}
	if leader {
		fctx := context.WithoutCancel(ctx)
		go func() {
			c.val, c.err = fn(fctx)
			g.finish(key, c)
		}()
	}
	select {
	case <-ctx.Done():
		return zero, ctx.Err()
	case <-c.done:
		return c.val, c.err
	}
}

// Has reports whether key has a call in flight.
func (g *Group[K, V]) Has(key K) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	_, ok := g.calls[key]
	return ok
}

// Len reports how many keys have a call in flight.
func (g *Group[K, V]) Len() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.calls)
}

func (g *Group[K, V]) join(key K) (*call[V], bool) {
	return g.joinLimit(key, 0)
}

func (g *Group[K, V]) joinLimit(key K, max int) (*call[V], bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if c, ok := g.calls[key]; ok {
		return c, false
	}
	if max > 0 && len(g.calls) >= max {
		return nil, false
	}
	if g.calls == nil {
		g.calls = map[K]*call[V]{}
	}
	c := &call[V]{done: make(chan struct{})}
	g.calls[key] = c
	return c, true
}

// finish publishes c. Results are written before done closes, and waiters
// read them only after, so they need no lock.
func (g *Group[K, V]) finish(key K, c *call[V]) {
	g.mu.Lock()
	if g.calls[key] == c {
		delete(g.calls, key)
	}
	g.mu.Unlock()
	close(c.done)
}
