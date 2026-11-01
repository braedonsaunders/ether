package m3u

import (
	"context"
	"errors"
	"fmt"
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

func (s *channelIssuer) IssueLineup(_ context.Context, credential string, req frontend.PlanRequest) (string, error) {
	if credential != "grant" || req.Viewer.ID != 7 || req.Protocol != "m3u" || req.Channel != "" || !strings.HasPrefix(req.Device.ID, "m3u-") {
		return "", errors.New("wrong scope")
	}
	s.issued++
	return s.path, nil
}
func (*channelIssuer) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusPartialContent)
}

func TestPlaylistIssuesOnlyScopedChannelMediaURLs(t *testing.T) {
	issuer := &channelIssuer{path: "/sessions/lineup/opaque/"}
	allowed := true
	channelCount := 1
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
				channels := []frontend.LiveChannel{{ID: "allowed", Name: "Channel"}}
				for n := 1; n < channelCount; n++ {
					channels = append(channels, frontend.LiveChannel{ID: fmt.Sprint(n), Name: "Channel"})
				}
				return channels, nil
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

	w := request(http.MethodGet, "/m3u/grant/playlist.m3u")
	if w.Code != 200 || issuer.issued != 1 || !strings.Contains(w.Body.String(), "https://ether.example/m3u/sessions/lineup/opaque/YWxsb3dlZA/live/index.m3u8") || strings.Contains(w.Body.String(), "grant") {
		t.Fatalf("playlist %d %q issued=%d", w.Code, w.Body, issuer.issued)
	}
	if w = request(http.MethodGet, "/m3u/grant/channel?id=allowed"); w.Code != 404 {
		t.Fatal("legacy login-grant media route remains")
	}
	if w = request(http.MethodGet, "/m3u/sessions/lineup/opaque/"); w.Code != http.StatusPartialContent {
		t.Fatal(w.Code)
	}
	channelCount = 300
	if w = request(http.MethodGet, "/m3u/grant/playlist.m3u"); w.Code != 200 || strings.Count(w.Body.String(), "#EXTINF:") != 300 || issuer.issued != 2 || strings.Contains(w.Body.String(), "grant") {
		t.Fatal("lineup must issue once for all channels", w.Code, issuer.issued)
	}
	allowed = false
	if w = request(http.MethodGet, "/m3u/grant/playlist.m3u"); w.Code != 200 || strings.Contains(w.Body.String(), "opaque") || issuer.issued != 3 {
		t.Fatal("hidden channel issued")
	}
	if w = request(http.MethodGet, "/m3u/invalid/playlist.m3u"); w.Code != 401 {
		t.Fatal(w.Code)
	}
	allowed = true
	issuer.path = "/sessions/lineup/../escape/"
	if w = request(http.MethodGet, "/m3u/grant/playlist.m3u"); w.Code != 503 {
		t.Fatal("unsafe capability accepted", w.Code)
	}
}

func (*channelIssuer) Issue(context.Context, string, frontend.PlanRequest) (string, error) {
	return "", errors.New("per-channel issuance forbidden")
}
