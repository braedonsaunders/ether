package clusterplayback

import (
	"context"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/nodewire"
	"time"
)

func (r *Runtime) sourceActivity(ctx context.Context, peer nodewire.Identity, m sourceRequest) (sourceResponse, error) {
	if m.Prepare == nil {
		return sourceResponse{}, ErrDenied
	}
	use, end, e := r.beginPreparationUse(peer, m.Prepare.Operation)
	if e != nil {
		return sourceResponse{}, e
	}
	defer end()
	use.ActivityMu.Lock()
	defer use.ActivityMu.Unlock()
	if err := ctx.Err(); err != nil {
		return sourceResponse{}, err
	}
	prepared, e := r.preparationCurrent(ctx, peer, m.Prepare.Operation)
	if e != nil {
		return sourceResponse{}, e
	}
	if err := ctx.Err(); err != nil {
		return sourceResponse{}, err
	}
	if (m.ActivityVersion == 0 && *use.ActivityVersion > 0) || (m.ActivityVersion > 0 && m.ActivityVersion <= *use.ActivityVersion) {
		return sourceResponse{Prepared: prepared}, nil
	}
	switch m.Op {
	case "prepare-open":
		if !r.o.Playback.MarkSurfaceOpened(prepared.PlayID) {
			return sourceResponse{}, ErrDenied
		}
	case "prepare-hidden":
		if !r.o.Playback.MarkSurfacePrepared(prepared.PlayID) {
			return sourceResponse{}, ErrDenied
		}
	case "prepare-activity":
		if m.Position < 0 || (m.State != "playing" && m.State != "paused") {
			return sourceResponse{}, ErrDenied
		}
		ps, ok := r.o.Playback.Session(prepared.PlayID)
		if !ok || ps == nil {
			return sourceResponse{}, ErrDenied
		}
		// Values selecting the source come only from the exact validated snapshot.
		if err := r.o.Playback.SourcePlaybackProgress(ctx, prepared.PlayID, prepared.Revision, ps.UserID, ps.ClientID, ps.Key, m.Position, m.State); err != nil {
			return sourceResponse{}, err
		}
	default:
		return sourceResponse{}, ErrDenied
	}
	*use.ActivityVersion = m.ActivityVersion
	return sourceResponse{Prepared: prepared}, nil
}

// OpenRemoteSource runs outside the broker mutex, before local visibility commit.
func (p *RemotePlayback) OpenRemoteSource(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p.mu.Lock()
	entry := p.entries[id]
	if entry == nil || entry.retiring || entry.retired || entry.snapshot == nil {
		p.mu.Unlock()
		return ErrDenied
	}
	entry.uses++
	entry.activity = p.o.Now()
	entry.mode++
	entry.activityVersion++
	version := entry.activityVersion
	owner, operation := entry.owner, entry.operation
	p.mu.Unlock()
	defer p.endUse(entry)
	_, err := p.r.sourceCall(ctx, Route{Source: owner}, sourceRequest{Op: "prepare-open", ActivityVersion: version, Prepare: &PrepareRequest{Operation: operation}})
	p.mu.Lock()
	defer p.mu.Unlock()
	if ctx.Err() != nil {
		// Cancellation cannot prove the remote open did not run. Seal this exact
		// unused operation and let receipt-aware retirement reconcile its effects.
		// An existing client or another admitted opener still owns its lease.
		if entry.snapshot.PreparedOnly && entry.pins == 0 && entry.uses == 1 {
			entry.retiring = true
		}
		return ctx.Err()
	}
	if entry.retiring || entry.retired {
		return ErrDenied
	}
	return err
}

// Touch and PlaybackProgress enqueue bounded state; Tick owns network work.
func (p *RemotePlayback) Touch(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e := p.entries[id]; e != nil && !e.retiring && !e.retired {
		e.activity = p.o.Now()
		e.mode++
		if e.state == "" {
			e.state = "playing"
		}
	}
}
func (p *RemotePlayback) PlaybackProgress(viewer int64, client string, key core.RatingKey, position time.Duration, state string) {
	if position < 0 || (state != "playing" && state != "paused") {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, e := range p.entries {
		if e.snapshot != nil && !e.retiring && !e.retired && e.snapshot.UserID == viewer && e.snapshot.ClientID == client && e.snapshot.Key == key {
			e.position = position
			e.state = state
			e.activity = p.o.Now()
			e.mode++
		}
	}
}
func (p *RemotePlayback) tickActivity(ctx context.Context) error {
	type pending struct {
		entry            *remotePreparation
		owner            nodewire.Identity
		operation, state string
		position         time.Duration
		activity         time.Time
		hide             bool
		mode             uint64
		version          uint64
	}
	p.mu.Lock()
	var list []pending
	for _, e := range p.entries {
		if e.retired || e.retiring || e.preparing || e.uses > 0 || e.snapshot == nil {
			continue
		}
		if e.mode > e.sentMode || e.activity.After(e.sentActivity) {
			e.activityVersion += 2
			e.uses++
			list = append(list, pending{version: e.activityVersion - 1, entry: e, owner: e.owner, operation: e.operation, state: e.state, position: e.position, activity: e.activity, hide: e.snapshot.PreparedOnly, mode: e.mode})
		}
	}
	p.mu.Unlock()
	var result error
	for _, item := range list {
		p.mu.Lock()
		sealed := item.entry.retiring || item.entry.retired
		p.mu.Unlock()
		if sealed {
			p.endUse(item.entry)
			continue
		}
		op := "prepare-open"
		if item.hide {
			op = "prepare-hidden"
		}
		if _, err := p.r.sourceCall(ctx, Route{Source: item.owner}, sourceRequest{Op: op, ActivityVersion: item.version, Prepare: &PrepareRequest{Operation: item.operation}}); err != nil {
			result = err
			p.endUse(item.entry)
			continue
		}
		if !item.activity.IsZero() {
			if item.state == "" {
				item.state = "playing"
			}
			if _, err := p.r.sourceCall(ctx, Route{Source: item.owner}, sourceRequest{Op: "prepare-activity", ActivityVersion: item.version + 1, Prepare: &PrepareRequest{Operation: item.operation}, Position: item.position, State: item.state}); err != nil {
				result = err
				p.endUse(item.entry)
				continue
			}
		}
		p.mu.Lock()
		item.entry.sentMode = max(item.entry.sentMode, item.mode)
		if item.activity.After(item.entry.sentActivity) {
			item.entry.sentActivity = item.activity
		}
		item.entry.uses--
		p.mu.Unlock()
	}
	return result
}
