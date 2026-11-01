package playback

import (
	"context"

	"github.com/braedonsaunders/ether/internal/core"
)

// PrepareVariant implements core.VariantPlayback: like Prepare, but the
// viewer picked releaseID ("Play Version"), so the resolver is asked to
// try it first (ResolveRequest.Prefer). An existing session for the same
// user, client and title on a different release is replaced, and a warm
// entry is only reused when it holds the picked release. When the picked
// release is unavailable, resolution falls back to the normal choice.
func (m *Manager) PrepareVariant(ctx context.Context, user *core.User, key core.RatingKey, caps core.ClientCaps, releaseID string) (*core.PlaySession, error) {
	return m.prepareEntry(ctx, user, key, caps, releaseID, false, false)
}

// PrepareExactVariant fails closed when the requested release cannot be
// opened, and never changes a session to a different release on failover.
func (m *Manager) PrepareExactVariant(ctx context.Context, user *core.User, key core.RatingKey, caps core.ClientCaps, releaseID string) (*core.PlaySession, error) {
	if releaseID == "" {
		return nil, core.ErrNotAvailable
	}
	return m.prepareEntry(ctx, user, key, caps, releaseID, true, false)
}
