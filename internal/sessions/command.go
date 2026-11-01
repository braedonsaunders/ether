package sessions

import (
	"context"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

// Command targets one authenticated viewer's session. An active, non-delegated
// owner may target any viewer's session.
func (b *Broker) Command(ctx context.Context, actor *core.Viewer, id string, cmd frontend.Command) error {
	v, err := b.viewer(ctx, actor)
	if err != nil {
		return err
	}
	switch cmd.Kind {
	case frontend.CommandStop, frontend.CommandPause, frontend.CommandUnpause:
	case frontend.CommandSeek:
		if cmd.Position < 0 {
			return core.ErrNotAvailable
		}
	case frontend.CommandSetAudio:
		if cmd.StreamIndex < 0 {
			return core.ErrNotAvailable
		}
	case frontend.CommandSetSubtitle:
		if cmd.StreamIndex < -1 {
			return core.ErrNotAvailable
		}
	case frontend.CommandMessage:
		if len(cmd.Message) == 0 || len(cmd.Message) > 4096 {
			return core.ErrNotAvailable
		}
	default:
		return core.ErrNotAvailable
	}
	if len(cmd.ControllerSessionID) > 256 {
		return core.ErrNotAvailable
	}
	owner := v.IsOwner && !v.Delegated
	list, err := b.Sessions(ctx, frontend.SessionAccess{Viewer: v, Owner: owner})
	if err != nil {
		return err
	}
	if cmd.ControllerSessionID != "" {
		found := false
		for _, s := range list {
			if s.ID == cmd.ControllerSessionID && s.ViewerID == v.ID {
				found = true
				break
			}
		}
		if !found {
			return core.ErrUnauthorized
		}
	}
	for _, target := range list {
		if target.ID != id {
			continue
		}
		if target.ViewerID != v.ID && !owner {
			return core.ErrUnauthorized
		}
		event := frontend.Event{Type: "SessionCommand", ViewerID: target.ViewerID, ActorViewerID: v.ID, SessionID: id, TargetProtocol: target.Protocol, TargetDeviceID: target.Device.ID, Command: &cmd}
		publishErr := b.deps.Events.Publish(ctx, event)
		if cmd.Kind == frontend.CommandStop {
			stopErr := b.Stop(ctx, frontend.SessionAccess{Viewer: v, Owner: owner, Protocol: target.Protocol, Device: target.Device}, id)
			if stopErr != nil {
				return stopErr
			}
		}
		return publishErr
	}
	return core.ErrNotFound
}
