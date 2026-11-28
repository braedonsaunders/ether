package mediabrowser

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

func TestAnonymousStreamRejectsUntrustedForwarder(t *testing.T) {
	state := &anonState{live: []frontend.Session{{ID: "session-1", Item: 5, Protocol: "jellyfin", Device: frontend.Device{ID: "tv"}, MediaPath: "/sessions/media/session-1/scoped/file"}}}
	proxyIP := netip.MustParseAddr("192.168.1.50")
	viewer := &frontend.Viewer{ID: 1}
	ident := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }}
	host := frontend.Host{
		Identity:     ident,
		Sessions:     newAnonBroker(state, frontend.Device{ID: "tv"}),
		Capabilities: frontendtest.Capabilities{},
		ClientIP:     func(*http.Request) (netip.Addr, bool) { return proxyIP, true },
	}
	inst, err := (Jellyfin{MachineID: "test"}).Mount(context.Background(), host)
	if err != nil {
		t.Fatal(err)
	}
	// Request arrives from a proxy not in TrustedProxyCIDRs: ClientIP returns the proxy IP while X-Forwarded-For is set.
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/jellyfin/Items/"+ids.RatingKeyGUID(5)+"/PlaybackInfo", strings.NewReader(`{"IsPlayback":true}`))
	r.RemoteAddr = "192.168.1.50:44444"
	r.Header.Set("X-Forwarded-For", "198.51.100.10")
	r.Header.Set("X-Emby-Token", "grant")
	r.Header.Set("X-Emby-Device-Id", "tv")
	inst.Handler().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("PlaybackInfo = %d: %s", w.Code, w.Body.String())
	}

	w2 := httptest.NewRecorder()
	r2 := httptest.NewRequest("GET", "/jellyfin/Videos/"+ids.RatingKeyGUID(5)+"/stream?static=true", nil)
	r2.RemoteAddr = "192.168.1.50:55555"
	r2.Header.Set("X-Forwarded-For", "203.0.113.99")
	inst.Handler().ServeHTTP(w2, r2)
	if w2.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous stream behind untrusted proxy = %d, want 401", w2.Code)
	}
}
