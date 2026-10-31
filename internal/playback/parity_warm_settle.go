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
	id := identity{userID: user.ID, clientID: caps.ClientID, key: key, surface: playbackSurface(string(caps.Protocol))}
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
