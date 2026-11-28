package app

import (
	"context"
	"math"
	"net/http"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/identity"
)

// lanViewer shares the DLNA trust rule across credential-free LAN surfaces.
// Every surface requires its own explicit viewer selection; there is no owner fallback.
func (a *App) lanViewer(ctx context.Context, req *http.Request, settings frontend.Settings) (*core.Viewer, error) {
	_, lan := identity.ClientNet(req, identity.ProxyPrefixes(a.cfg.Read().Server.TrustedProxyCIDRs)...)
	if !lan || forwardedFromOutside(req) {
		return nil, core.ErrUnauthorized
	}
	value, ok := settings["viewerId"].(float64)
	if !ok || math.IsNaN(value) || math.IsInf(value, 0) || value < 1 || value > 9007199254740991 || math.Trunc(value) != value {
		return nil, core.ErrUnauthorized
	}
	viewer, err := a.users.ActiveUser(ctx, int64(value))
	if err != nil || viewer == nil || viewer.ID != int64(value) {
		return nil, core.ErrUnauthorized
	}
	return viewer, nil
}
