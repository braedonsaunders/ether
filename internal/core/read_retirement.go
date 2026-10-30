package core

import (
	"context"
	"sync"
)

// ReadRetirement tracks producer references belonging to one media request.
// Seal returns a channel closed only after all registered producers release
// their references. It does not wait or cancel those producers.
type ReadRetirement struct {
	mu      sync.Mutex
	pending int
	sealed  bool
	done    chan struct{}
}

type readRetirementKey struct{}

// WithReadRetirement attaches request-scoped retirement tracking.
func WithReadRetirement(ctx context.Context, tracker *ReadRetirement) context.Context {
	return context.WithValue(ctx, readRetirementKey{}, tracker)
}

// ReadRetirementFrom returns the tracker attached to ctx, if any.
func ReadRetirementFrom(ctx context.Context) *ReadRetirement {
	tracker, _ := ctx.Value(readRetirementKey{}).(*ReadRetirement)
	return tracker
}

// Begin registers one producer before it starts. Once sealed, registrations
// fail closed; a successful registration must call its returned function once.
func (t *ReadRetirement) Begin() (func(), bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.sealed {
		return nil, false
	}
	if t.done == nil {
		t.done = make(chan struct{})
	}
	t.pending++
	var once sync.Once
	return func() {
		once.Do(func() {
			t.mu.Lock()
			defer t.mu.Unlock()
			t.pending--
			if t.sealed && t.pending == 0 {
				close(t.done)
			}
		})
	}, true
}

// Seal rejects later producers and returns the completion signal.
func (t *ReadRetirement) Seal() <-chan struct{} {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.done == nil {
		t.done = make(chan struct{})
	}
	if !t.sealed {
		t.sealed = true
		if t.pending == 0 {
			close(t.done)
		}
	}
	return t.done
}
