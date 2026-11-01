//go:build scorecard && !plexparity

package app

import (
	"context"
	"fmt"
	"github.com/braedonsaunders/ether/internal/frontend"
	"net"
	"net/http"
	"net/url"
	"strconv"
)

// scorecardSurfaceURL permits the actual loopback listener in the disposable
// fixture; it never derives an origin from a Host or forwarded header.
func scorecardSurfaceURL(ctx context.Context, r *frontendRuntime, id frontend.Protocol, req *http.Request) (string, error) {
	if u, e := r.surfaceURL(ctx, id, req); e == nil {
		return u, nil
	}
	return scorecardLoopbackURL(req)
}
func scorecardLoopbackURL(req *http.Request) (string, error) {
	peer, _, e := net.SplitHostPort(req.RemoteAddr)
	if e != nil || net.ParseIP(peer) == nil || !net.ParseIP(peer).IsLoopback() {
		return "", fmt.Errorf("fixture peer is not loopback")
	}
	local, ok := req.Context().Value(http.LocalAddrContextKey).(*net.TCPAddr)
	if !ok || local.IP == nil || !local.IP.IsLoopback() || local.Port <= 0 {
		return "", fmt.Errorf("fixture local listener is not verified loopback")
	}
	return (&url.URL{Scheme: "http", Host: net.JoinHostPort(local.IP.String(), strconv.Itoa(local.Port))}).String(), nil
}
