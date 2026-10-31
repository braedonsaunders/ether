//go:build plexparity

package playback

import (
	"context"

	"github.com/braedonsaunders/ether/internal/core"
)

// ParitySettleWarm starts this title's detail warm-up when none is recorded,
// then waits until that warm-up has finished or failed. Disposable parity
// builds call it before a metadata read so both servers render probed tracks
// instead of racing the 100ms detail bound. Production builds do not include it.
func (m *Manager) ParitySettleWarm(ctx context.Context, user *core.User, key core.RatingKey, caps core.ClientCaps) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if m == nil || user == nil || caps.ClientID == "" {
		return nil
	}
	m.Prewarm(ctx, user, key, caps, core.PrewarmOptions{Reason: core.PrewarmDetail})
	// Plex warm-ups use the zero surface. Leaving newer identity fields at
	// zero keeps this seam compiling against released main.
	id := identity{userID: user.ID, clientID: caps.ClientID, key: key}
	m.mu.Lock()
	e := m.warm[id]
	live := e != nil && m.now().Before(e.expires)
	m.mu.Unlock()
	if !live {
		return nil
	}
	select {
	case <-e.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ParitySettlePending waits until every recorded warm-up has finished or
// failed. It does not start a probe. Disposable parity builds call it after
// seeding and after each corpus mutation so the next read renders probed
// facts instead of racing the 100ms detail bound. Production builds do not
// include it.
func (m *Manager) ParitySettlePending(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if m == nil {
		return nil
	}
	m.mu.Lock()
	now := m.now()
	pending := make([]<-chan struct{}, 0, len(m.warm))
	for _, e := range m.warm {
		if e == nil || !now.Before(e.expires) {
			continue
		}
		select {
		case <-e.done:
		default:
			pending = append(pending, e.done)
		}
	}
	m.mu.Unlock()
	for _, done := range pending {
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}
