package m3u

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
)

type channelIssuer struct {
	issued int
	path   string
}

func (s *channelIssuer) Issue(_ context.Context, credential string, req frontend.PlanRequest) (string, error) {
	if credential != "grant" || req.Viewer.ID != 7 || req.Protocol != "m3u" || req.Channel != "allowed" || !strings.HasPrefix(req.Device.ID, "m3u-") {
		return "", errors.New("wrong scope")
	}
	s.issued++
	return s.path, nil
}
func (*channelIssuer) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusPartialContent)
}

func TestCachedChannelURLReauthorizesAndRedirectsToScopedStream(t *testing.T) {
	issuer := &channelIssuer{path: "/sessions/lazy/opaque/live/index.m3u8"}
	allowed := true
	host := frontend.Host{
		Identity: frontendtest.Identity{ResolveFunc: func(_ context.Context, token string) (*frontend.Viewer, error) {
			if token != "grant" {
				return nil, errors.New("bad grant")
			}
			return &frontend.Viewer{ID: 7}, nil
		}},
		Sessions: frontendtest.Sessions{}, LazyStreams: issuer, Capabilities: frontendtest.Capabilities{},
		ClientIP:   func(*http.Request) (netip.Addr, bool) { return netip.MustParseAddr("192.168.1.7"), true },
		SurfaceURL: func(context.Context, *http.Request) (string, error) { return "https://ether.example/m3u", nil },
		LiveTV: frontendtest.LiveTV{ChannelsFunc: func(context.Context, *frontend.Viewer) ([]frontend.LiveChannel, error) {
			if allowed {
				return []frontend.LiveChannel{{ID: "allowed", Name: "Channel"}}, nil
			}
			return nil, nil
		}},
	}
	inst, err := (Frontend{}).Mount(context.Background(), host)
	if err != nil {
		t.Fatal(err)
	}
	request := func(method, target string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		inst.Handler().ServeHTTP(w, httptest.NewRequest(method, target, nil))
		return w
	}
	if w := request(http.MethodHead, "/m3u/grant/channel?id=allowed"); w.Code != 200 || issuer.issued != 0 {
		t.Fatalf("HEAD %d issued=%d", w.Code, issuer.issued)
	}
	w := request(http.MethodGet, "/m3u/grant/channel?id=allowed")
	if w.Code != 302 || w.Header().Get("Location") != "https://ether.example/m3u/sessions/lazy/opaque/live/index.m3u8" || strings.Contains(w.Header().Get("Location"), "grant") {
		t.Fatalf("redirect %d %q", w.Code, w.Header().Get("Location"))
	}
	if w = request(http.MethodGet, "/m3u/sessions/lazy/opaque/live/index.m3u8"); w.Code != http.StatusPartialContent {
		t.Fatalf("lazy forwarding %d", w.Code)
	}
	allowed = false
	if w = request(http.MethodGet, "/m3u/grant/channel?id=allowed"); w.Code != 404 || issuer.issued != 1 {
		t.Fatalf("restricted channel %d issued=%d", w.Code, issuer.issued)
	}
	if w = request(http.MethodGet, "/m3u/invalid/channel?id=allowed"); w.Code != 401 {
		t.Fatalf("invalid grant %d", w.Code)
	}
	allowed = true
	issuer.path = "/sessions/lazy/../escape/live/index.m3u8"
	if w = request(http.MethodGet, "/m3u/grant/channel?id=allowed"); w.Code != 503 {
		t.Fatalf("unsafe capability path accepted: %d", w.Code)
	}
}
