// Package reqmemo memoizes point reads for the length of one read request.
//
// A list page builds each item with the same helpers, and several of them
// read the same row (a viewer's track rules, a server setting, a title's
// resume record) once per item, or several times per item. The app attaches
// a memo to every GET and HEAD request; a helper that reads through Do asks
// the database once per key per request. Without a memo (a background job,
// a write request) Do just loads.
package reqmemo

import (
	"context"
	"strings"
	"sync"
	"time"
)

// ttl bounds how long a value serves a long-lived request (an event
// stream), which must not read stale rows for its whole life.
const ttl = 2 * time.Second

type entry struct {
	v  any
	at time.Time
}

type memoKey struct{}

type memo struct {
	mu sync.Mutex
	m  map[string]entry
}

// With attaches an empty memo to ctx.
func With(ctx context.Context) context.Context {
	return context.WithValue(ctx, memoKey{}, &memo{m: map[string]entry{}})
}

// Do returns the memoized value for key, or loads, remembers and returns it.
// A failed load is not remembered. Callers must not change a returned value
// that other callers share (copy slices and maps before changing them).
func Do[T any](ctx context.Context, key string, load func() (T, error)) (T, error) {
	m, _ := ctx.Value(memoKey{}).(*memo)
	if m == nil {
		return load()
	}
	m.mu.Lock()
	if e, ok := m.m[key]; ok && time.Since(e.at) < ttl {
		m.mu.Unlock()
		return e.v.(T), nil
	}
	m.mu.Unlock()
	v, err := load()
	if err != nil {
		return v, err
	}
	m.mu.Lock()
	if len(m.m) < 4096 {
		m.m[key] = entry{v: v, at: time.Now()}
	}
	m.mu.Unlock()
	return v, nil
}

// Forget drops key, for a request that writes what it read.
func Forget(ctx context.Context, key string) {
	if m, _ := ctx.Value(memoKey{}).(*memo); m != nil {
		m.mu.Lock()
		delete(m.m, key)
		m.mu.Unlock()
	}
}

// ForgetPrefix drops every key starting with prefix.
func ForgetPrefix(ctx context.Context, prefix string) {
	if m, _ := ctx.Value(memoKey{}).(*memo); m != nil {
		m.mu.Lock()
		for k := range m.m {
			if strings.HasPrefix(k, prefix) {
				delete(m.m, k)
			}
		}
		m.mu.Unlock()
	}
}
