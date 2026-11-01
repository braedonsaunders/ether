package app

import (
	"context"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/identity"
	"github.com/braedonsaunders/ether/internal/safety"
)

func (a *App) createDLNAGuest(ctx context.Context) (*core.Viewer, error) {
	local, err := identity.NewLocal(a.db)
	if err != nil {
		return nil, err
	}
	id, err := local.CreateDLNAGuest(ctx, "Living-room guest")
	if err != nil {
		return nil, err
	}
	if err = a.safety.Put(ctx, id, safety.KidsPreset("children")); err != nil {
		return nil, err
	}
	if err = local.ActivateDLNAGuest(ctx, id); err != nil {
		return nil, err
	}
	if err = a.users.RefreshViewers(ctx); err != nil {
		return nil, err
	}
	return a.users.ActiveUser(ctx, id)
}
