//go:build !scorecard

package app

import (
	"context"
	"github.com/braedonsaunders/ether/internal/frontend"
	"net/http"
)

func scorecardSurfaceURL(ctx context.Context, r *frontendRuntime, id frontend.Protocol, req *http.Request) (string, error) {
	return r.surfaceURL(ctx, id, req)
}
