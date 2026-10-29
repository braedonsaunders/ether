package mediabrowser

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

func TestEmbyDirectStreamCacheHeaderAfterRedirect(t *testing.T) {
	viewer := &frontend.Viewer{ID: 1}
	host := frontend.Host{
		Identity: frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }},
		Sessions: frontendtest.Sessions{
			SessionsFunc: func(context.Context, frontend.SessionAccess) ([]frontend.Session, error) {
				return []frontend.Session{{ID: "session", Item: 5, Protocol: "emby", Device: frontend.Device{ID: "tv"}}}, nil
			},
			StartStreamFunc: func(context.Context, frontend.SessionAccess, string, frontend.StreamRequest) (string, error) {
				return "/sessions/media/session/grant/file", nil
			},
			ServeMediaFunc: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusPartialContent) },
		},
	}
	inst, err := (Emby{MachineID: "test"}).Mount(context.Background(), host)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(inst.Handler())
	defer server.Close()
	r, _ := http.NewRequest("GET", server.URL+"/emby/Videos/"+ids.RatingKeyGUID(5)+"/stream", nil)
	r.Header.Set("X-Emby-Token", "grant")
	r.Header.Set("X-Emby-Device-Id", "tv")
	resp, err := server.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent || resp.Header.Get("Cache-Control") != "private, no-transform" {
		t.Fatal(resp.StatusCode, resp.Header)
	}
	other := httptest.NewRecorder()
	inst.Handler().ServeHTTP(other, httptest.NewRequest("GET", "/emby/sessions/media/session/grant/master.m3u8", nil))
	if other.Header().Get("Cache-Control") != "" {
		t.Fatal(other.Header())
	}
}
