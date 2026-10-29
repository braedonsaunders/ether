package sessions

import (
	"sync"
	"time"
)

// Tracker holds timeline entries without prescribing a wire-specific client
// snapshot. newEntry and updatedAt are adapter functions, used under the lock.
// Returned values are snapshots; callers must not mutate referenced fields.
type Tracker[T any] struct {
	mu        sync.Mutex
	next      int
	entries   map[string]*T
	newEntry  func(string, int) T
	updatedAt func(*T) time.Time
}

func NewTracker[T any](newEntry func(string, int) T, updatedAt func(*T) time.Time) *Tracker[T] {
	return &Tracker[T]{entries: make(map[string]*T), newEntry: newEntry, updatedAt: updatedAt}
}
func (t *Tracker[T]) Has(id string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, ok := t.entries[id]
	return ok
}
func (t *Tracker[T]) Ensure(id string, key int) {
	if key <= 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.entries[id]; !ok {
		e := t.newEntry(id, key)
		t.entries[id] = &e
	}
}
func (t *Tracker[T]) Update(id string, fn func(*T)) T {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.entries[id]
	if !ok {
		t.next++
		entry := t.newEntry(id, t.next)
		e = &entry
		t.entries[id] = e
	}
	fn(e)
	return *e
}
func (t *Tracker[T]) Remove(id string) (T, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.entries[id]
	if !ok {
		var zero T
		return zero, false
	}
	delete(t.entries, id)
	return *e, true
}
func (t *Tracker[T]) List() []T {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]T, 0, len(t.entries))
	for _, e := range t.entries {
		out = append(out, *e)
	}
	return out
}
func (t *Tracker[T]) Expire(now time.Time, ttl time.Duration) []T {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []T
	for id, e := range t.entries {
		if now.Sub(t.updatedAt(e)) > ttl {
			out = append(out, *e)
			delete(t.entries, id)
		}
	}
	return out
}
