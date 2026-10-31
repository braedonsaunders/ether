package sessions

import (
	"context"
	"slices"
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
	b.retired[s.public.ID] = retiredSession{viewerID: s.public.ViewerID, protocol: s.public.Protocol, deviceID: s.public.Device.ID, item: s.public.Item, until: now.Add(2 * time.Hour)}
}

// collectSupersededLocked keeps one active playback for a viewer, surface and
// device, regardless of which item or channel that device opens next.
func (b *Broker) collectSupersededLocked(replacement *brokerSession) []*brokerSession {
	var old []*brokerSession
	for id, s := range b.sessions {
		if s.public.ViewerID == replacement.public.ViewerID && s.clientID == replacement.clientID {
			delete(b.sessions, id)
			b.rememberRetiredLocked(s)
			old = append(old, s)
		}
	}
	return old
}

func (b *Broker) finishSuperseded(ctx context.Context, entries []*brokerSession, newSource string) {
	for _, old := range entries {
		b.mu.Lock()
		reads := slices.Clone(old.servingReads)
		b.mu.Unlock()
		waitReadRetirement(reads)
		if old.trickplay != nil {
			old.trickplay.Close(true)
		}
		b.mu.Lock()
		transfers := b.lazyTransfers[old.public.ID]
		delete(b.lazyTransfers, old.public.ID)
		b.mu.Unlock()
		for _, cancel := range transfers {
			cancel()
		}
		b.bindings.Remove(old.public.ID)
		if b.cluster != nil {
			if err := b.cluster.store.remove(ctx, old.public.ID); err != nil && b.deps.OnEventError != nil {
				b.deps.OnEventError(err)
			}
			b.cluster.announceRetired(ctx, old)
		}
		b.cleanupSampleReceipt(ctx, old)
		if old.live {
			retired := true
			if old.playID != newSource && b.deps.Live != nil {
				if err := b.deps.Live.Stop(ctx, &core.Viewer{ID: old.public.ViewerID}, old.playID); err != nil {
					retired = false
					if b.deps.OnEventError != nil {
						b.deps.OnEventError(err)
					}
				}
			}
			b.settleRetirement(context.WithoutCancel(ctx), old, retired)
		} else {
			if old.sourcePinned {
				if pins, ok := b.deps.Playback.(core.PlaybackRevisionPin); ok {
					pins.UnpinSourceRevision(old.playID)
				}
			}
			b.mu.Lock()
			stopSource := b.releaseSessionSourceLocked(old.playID)
			b.mu.Unlock()
			if old.playID == newSource {
				stopSource = false
			}
			if err := b.retireBackends(ctx, old, stopSource); err != nil && b.deps.OnEventError != nil {
				b.deps.OnEventError(err)
			}
		}
		b.publish(ctx, sessionEvent("SessionStopped", old.public, nil))
	}
}

// retiredAccess keeps late reports harmless while preserving viewer/device
// authorization for the short-lived supersession history.
func (b *Broker) retiredAccess(ctx context.Context, a frontend.SessionAccess, id string) (core.RatingKey, bool, error) {
	b.mu.Lock()
	r, ok := b.retired[id]
	if ok && !r.until.After(b.deps.Now()) {
		delete(b.retired, id)
		ok = false
	}
	b.mu.Unlock()
	if !ok {
		return 0, false, nil
	}
	v, err := b.viewer(ctx, a.Viewer)
	if err != nil {
		return 0, false, err
	}
	owner := a.Owner && v.IsOwner && !v.Delegated
	if a.Owner && !owner {
		return 0, false, core.ErrUnauthorized
	}
	if !owner && (v.ID != r.viewerID || a.Protocol != r.protocol || a.Device.ID == "" || a.Device.ID != r.deviceID) {
		return 0, false, core.ErrUnauthorized
	}
	return r.item, true, nil
}
