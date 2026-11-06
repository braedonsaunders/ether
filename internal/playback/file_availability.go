package playback

import (
	"context"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// File checks are explicit source probes, not playback admissions. Bound their
// background work independently so metadata cannot consume viewer stream slots.
var fileCheckSlots = make(chan struct{}, 8)

func (m *Manager) CheckFileAvailability(ctx context.Context, user *core.User, key core.RatingKey, caps core.ClientCaps, release string) (bool, error) {
	if user == nil {
		return false, core.ErrUnauthorized
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	select {
	case fileCheckSlots <- struct{}{}:
	default:
		return false, core.ErrNotAvailable
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		<-fileCheckSlots
		return false, core.ErrNotAvailable
	}
	m.wg.Add(1)
	m.mu.Unlock()
	type result struct {
		live bool
		err  error
	}
	done := make(chan result, 1)
	u := *user
	go func() {
		defer m.wg.Done()
		defer func() { <-fileCheckSlots }()
		stop := context.AfterFunc(m.ctx, cancel)
		defer stop()
		title, err := m.deps.Catalog.Get(ctx, key)
		if err != nil || title == nil {
			if err == nil {
				err = core.ErrNotAvailable
			}
			done <- result{err: err}
			return
		}
		res, err := m.deps.Resolver.Resolve(core.WithIndexerPurpose(ctx, core.IndexerPurposeWarmup), core.ResolveRequest{Title: title, Client: caps, User: &u, Prefer: release})
		if err != nil {
			done <- result{err: err}
			return
		}
		if res == nil || release != "" && res.Release.ID != release {
			done <- result{err: core.ErrNotAvailable}
			return
		}
		lifetime := newSourceLifetime(u.ID, string(caps.Protocol), caps.ClientID)
		defer lifetime.seal()
		src, finish, err := m.openSource(ctx, lifetime, res)
		if err != nil {
			done <- result{err: err}
			return
		}
		live := src.Size() > 0
		closeErr := src.Close()
		finish(closeErr)
		done <- result{live: live, err: closeErr}
	}()
	select {
	case r := <-done:
		return r.live, r.err
	case <-ctx.Done():
		return false, ctx.Err()
	}
}
