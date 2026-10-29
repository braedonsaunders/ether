package sessions

import (
	"context"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

// Caller holds b.mu. Keeping a bounded tombstone makes late progress and
// duplicate stops harmless without retaining grants or upstream credentials.
func (b *Broker) rememberRetiredLocked(s *brokerSession) {
	now := b.deps.Now()
	for id, r := range b.retired {
		if !r.until.After(now) {
			delete(b.retired, id)
		}
	}
	if len(b.retired) >= 4096 {
		for id := range b.retired {
			delete(b.retired, id)
			break
		}
	}
	b.retired[s.public.ID] = retiredSession{viewerID: s.public.ViewerID, protocol: s.public.Protocol, deviceID: s.public.Device.ID, until: now.Add(2 * time.Hour)}
}

// retiredAccess keeps late reports harmless while preserving viewer/device
// authorization for the short-lived supersession history.
func (b *Broker) retiredAccess(ctx context.Context, a frontend.SessionAccess, id string) (bool, error) {
	b.mu.Lock()
	r, ok := b.retired[id]
	if ok && !r.until.After(b.deps.Now()) {
		delete(b.retired, id)
		ok = false
	}
	b.mu.Unlock()
	if !ok {
		return false, nil
	}
	v, err := b.viewer(ctx, a.Viewer)
	if err != nil {
		return false, err
	}
	owner := a.Owner && v.IsOwner && !v.Delegated
	if a.Owner && !owner {
		return false, core.ErrUnauthorized
	}
	if !owner && (v.ID != r.viewerID || a.Protocol != r.protocol || a.Device.ID == "" || a.Device.ID != r.deviceID) {
		return false, core.ErrUnauthorized
	}
	return true, nil
}
