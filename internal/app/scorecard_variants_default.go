//go:build !scorecard

package app

import (
	"context"
	"github.com/braedonsaunders/ether/internal/core"
)

func scorecardVariants(ctx context.Context, a *App, v *core.Viewer, t *core.Title, caps core.ClientCaps) ([]core.ReleaseVariant, error) {
	return a.resolver.Variants(ctx, core.ResolveRequest{Title: t, User: v, Client: caps}, 4)
}
