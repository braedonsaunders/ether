package mediabrowser

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
)

func TestRefusedCredentialsNeverReachFrontendLog(t *testing.T) {
	var logs bytes.Buffer
	identity := frontendtest.Identity{ResolveFunc: func(_ context.Context, token string) (*frontend.Viewer, error) {
		return nil, errors.New("invalid token: " + token)
	}}
	inst, err := (Jellyfin{MachineID: "security"}).Mount(context.Background(), frontend.Host{Identity: identity, Log: slog.New(slog.NewTextHandler(&logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"X-Emby-Token", "X-MediaBrowser-Token"} {
		req := httptest.NewRequest("GET", "/Users/Me", nil)
		req.Header.Set(h, "secret-refused-credential")
		rec := httptest.NewRecorder()
		inst.Handler().ServeHTTP(rec, req)
		if rec.Code != 401 || strings.Contains(rec.Body.String(), "secret-refused-credential") {
			t.Fatalf("%s: status %d body %q", h, rec.Code, rec.Body.String())
		}
	}
	if strings.Contains(logs.String(), "secret-refused-credential") {
		t.Fatal("refused credential leaked to logs")
	}
}

func TestForeignPlaybackIDCannotStopAnotherSession(t *testing.T) {
	viewer := &frontend.Viewer{ID: 2}
	stopped := false
	identity := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }}
	broker := frontendtest.Sessions{ReportFunc: func(_ context.Context, access frontend.SessionAccess, id string, _ frontend.Timeline) error {
		if access.Viewer != viewer || access.Protocol != "jellyfin" || id != "other-viewer-plex-session" {
			t.Fatalf("access=%+v id=%q", access, id)
		}
		return errors.New("not authorized")
	}, StopFunc: func(context.Context, frontend.SessionAccess, string) error { stopped = true; return nil }}
	inst, err := (Jellyfin{MachineID: "security"}).Mount(context.Background(), frontend.Host{Identity: identity, Sessions: broker})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/Sessions/Playing/Stopped", strings.NewReader(`{"PlaySessionId":"other-viewer-plex-session"}`))
	req.Header.Set("X-Emby-Token", "grant")
	rec := httptest.NewRecorder()
	inst.Handler().ServeHTTP(rec, req)
	if rec.Code == 204 || stopped {
		t.Fatalf("foreign session accepted: status %d stopped=%v", rec.Code, stopped)
	}
}
