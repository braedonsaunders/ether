package clusterplayback

import (
	"context"
	"errors"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/nodewire"
)

func (p *RemotePlayback) CheckFileAvailability(ctx context.Context, u *core.User, k core.RatingKey, caps core.ClientCaps, release string) (bool, error) {
	if u == nil {
		return false, core.ErrUnauthorized
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	owner, err := p.o.SelectSource(ctx)
	if err != nil {
		return false, err
	}
	reply, err := p.r.sourceCall(ctx, Route{Source: owner}, sourceRequest{Op: "file-check", Prepare: &PrepareRequest{Viewer: u.ID, Key: k, Caps: caps, ReleaseID: release}})
	if err != nil {
		return false, err
	}
	if reply.FileAvailable == nil {
		return false, core.ErrPrepareSlow
	}
	return *reply.FileAvailable, nil
}

func (r *Runtime) checkSourceFile(ctx context.Context, peer nodewire.Identity, q *PrepareRequest) (sourceResponse, error) {
	if q == nil || q.Viewer <= 0 || !q.Key.Valid() || r.o.Eligible(ctx, peer, config.RoleServer) != nil || r.o.Eligible(ctx, r.o.Self, config.RoleSource) != nil {
		return sourceResponse{}, ErrDenied
	}
	checker := r.o.Playback
	if checker == nil {
		return sourceResponse{}, nil
	}
	viewer, err := r.o.ActiveViewer(ctx, q.Viewer)
	if err != nil || viewer == nil || viewer.ID != q.Viewer {
		return sourceResponse{}, ErrDenied
	}
	ps := &core.PlaySession{UserID: viewer.ID, Key: q.Key, Client: q.Caps}
	if r.o.SourcePolicy(ctx, viewer, ps) != nil {
		return sourceResponse{}, ErrDenied
	}
	live, err := checker.CheckFileAvailability(ctx, viewer, q.Key, q.Caps, q.ReleaseID)
	if err != nil && !errors.Is(err, core.ErrNotAvailable) && !errors.Is(err, core.ErrNotFound) {
		return sourceResponse{}, nil
	}
	return sourceResponse{FileAvailable: &live}, nil
}
