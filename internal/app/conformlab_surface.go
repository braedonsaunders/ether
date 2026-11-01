//go:build conformlab && plexparity && !scorecard

package app

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"

	"github.com/braedonsaunders/ether/internal/frontend"
)

// scorecardSurfaceURL has a test-only fallback for httptest listeners. The
// MediaBrowser corpus deliberately uses documentation-range synthetic peers
// to isolate client rate limits. Neither Host nor forwarded headers are used.
func scorecardSurfaceURL(ctx context.Context, r *frontendRuntime, id frontend.Protocol, req *http.Request) (string, error) {
	if base, err := r.surfaceURL(ctx, id, req); err == nil {
		return base, nil
	}
	return conformLabLoopbackURL(req)
}
func conformLabLoopbackURL(req *http.Request) (string, error) {
	peer, err := netip.ParseAddrPort(req.RemoteAddr)
	if err != nil {
		return "", fmt.Errorf("invalid fixture peer")
	}
	if !peer.Addr().IsLoopback() && !netip.MustParsePrefix("198.51.100.0/24").Contains(peer.Addr()) {
		return "", fmt.Errorf("peer is not a fixture client")
	}
	local, ok := req.Context().Value(http.LocalAddrContextKey).(*net.TCPAddr)
	if !ok || local.IP == nil || !local.IP.IsLoopback() || local.Port <= 0 {
		return "", fmt.Errorf("fixture listener is not verified loopback")
	}
	return (&url.URL{Scheme: "http", Host: net.JoinHostPort(local.IP.String(), strconv.Itoa(local.Port))}).String(), nil
}
