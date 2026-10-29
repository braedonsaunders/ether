package app

import (
	"context"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

type surfaceViewerPolicy struct{ app *App }

func (s surfaceViewerPolicy) SetMaxRating(ctx context.Context, actor *frontend.Viewer, rating string) error {
	if actor == nil || actor.Delegated {
		return core.ErrUnauthorized
	}
	v, err := s.app.users.ActiveUser(ctx, actor.ID)
	if err != nil || v == nil || !v.IsOwner {
		return core.ErrUnauthorized
	}
	return s.app.users.SetViewerMaxRating(ctx, v.ID, rating)
}
