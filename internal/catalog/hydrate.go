package catalog

import (
	"context"
	"errors"
	"sync/atomic"
	"time"
)

// Reads never wait on TMDB for long. A title whose details are stored but
// stale is served at once and refreshed behind the read; a title with no
// details yet waits at most hydrateWait while the fetch carries on detached,
// so its result still lands in the mirror for the next read. Refreshes are
// deduplicated per key and bounded in number.
const (
	hydrateWait   = 2 * time.Second
	hydrateLimit  = 16
	hydrateBudget = 45 * time.Second
)

// errHydrating means a refresh is still running (or could not start because
// hydrateLimit refreshes already are); the caller serves what it has.
var errHydrating = errors.New("catalog: metadata refresh in progress")

type hydration struct {
	done chan struct{}
	val  any
	err  error
}

// hydrate runs fetch for refreshKey detached from ctx's cancellation (it keeps
// ctx's values) and waits for it at most wait. wait <= 0 starts the refresh
// if a slot is free and returns errHydrating at once. A caller that must wait
// also waits, within the same bound, for one of the hydrateLimit slots. A
// failed fetch is remembered with noteRefreshMiss, as a synchronous fetch was.
func (c *Catalog) hydrate(ctx context.Context, refreshKey string, wait time.Duration, fetch func(context.Context) error) error {
	_, err := c.hydrateValue(ctx, refreshKey, wait, func(ctx context.Context) (any, error) { return nil, fetch(ctx) })
	return err
}

// hydrateValue is hydrate for a fetch whose result every waiter shares.
func (c *Catalog) hydrateValue(ctx context.Context, refreshKey string, wait time.Duration, fetch func(context.Context) (any, error)) (any, error) {
	if budget, ok := ctx.Value(waitBudgetKey{}).(*waitBudget); ok && wait > 0 {
		wait = min(wait, time.Duration(budget.remaining.Load()))
		if wait > 0 {
			start := time.Now()
			defer func() { budget.remaining.Add(-int64(time.Since(start))) }()
		}
	}
	var deadline <-chan time.Time
	if wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		deadline = timer.C
	}
	h := c.startHydration(ctx, refreshKey, wait > 0, deadline, fetch)
	if h == nil || wait <= 0 {
		return nil, errHydrating
	}
	select {
	case <-h.done:
		return h.val, h.err
	case <-deadline:
		return nil, errHydrating
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// startHydration joins the running refresh of refreshKey or starts one in a
// free slot. It returns nil when no slot came free in time.
func (c *Catalog) startHydration(ctx context.Context, refreshKey string, block bool, deadline <-chan time.Time, fetch func(context.Context) (any, error)) *hydration {
	c.hydrateMu.Lock()
	if h := c.hydrating[refreshKey]; h != nil {
		c.hydrateMu.Unlock()
		return h
	}
	c.hydrateMu.Unlock()
	if block {
		select {
		case c.hydrateSlots <- struct{}{}:
		case <-deadline:
			return nil
		case <-ctx.Done():
			return nil
		}
	} else {
		select {
		case c.hydrateSlots <- struct{}{}:
		default:
			return nil
		}
	}
	c.hydrateMu.Lock()
	if h := c.hydrating[refreshKey]; h != nil {
		// Started by another reader while this one waited for a slot.
		c.hydrateMu.Unlock()
		<-c.hydrateSlots
		return h
	}
	if c.hydrating == nil {
		c.hydrating = make(map[string]*hydration)
	}
	h := &hydration{done: make(chan struct{})}
	c.hydrating[refreshKey] = h
	c.hydrateMu.Unlock()
	bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), hydrateBudget)
	go func() {
		defer cancel()
		h.val, h.err = fetch(bg)
		if h.err != nil && !errors.Is(bg.Err(), context.DeadlineExceeded) {
			c.noteRefreshMiss(refreshKey)
		}
		c.hydrateMu.Lock()
		delete(c.hydrating, refreshKey)
		c.hydrateMu.Unlock()
		<-c.hydrateSlots
		close(h.done)
	}()
	return h
}

// waitBudget is what is left of one request's time to wait on TMDB.
type waitBudget struct{ remaining atomic.Int64 }

type waitBudgetKey struct{}

// WithReadWaitBudget bounds the total time reads under ctx wait on TMDB,
// across every title they touch. A page that lists thirty titles with no
// details yet waits at most d in all, not 2 s for each; the rest are served
// as stored and refreshed in the background.
func WithReadWaitBudget(ctx context.Context, d time.Duration) context.Context {
	b := &waitBudget{}
	b.remaining.Store(int64(d))
	return context.WithValue(ctx, waitBudgetKey{}, b)
}

// refreshing reports whether a refresh of refreshKey is running.
func (c *Catalog) refreshing(refreshKey string) bool {
	c.hydrateMu.Lock()
	defer c.hydrateMu.Unlock()
	return c.hydrating[refreshKey] != nil
}

// WaitRefreshes blocks until no metadata refresh started by a read is still
// running.
func (c *Catalog) WaitRefreshes() { c.waitHydrations() }

func (c *Catalog) waitHydrations() {
	for {
		c.hydrateMu.Lock()
		var h *hydration
		for _, x := range c.hydrating {
			h = x
			break
		}
		c.hydrateMu.Unlock()
		if h == nil {
			return
		}
		<-h.done
	}
}
