package mediabrowser

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/conform/openapi"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
)

func TestSessionListUsesBrokerScopeWithoutGrants(t *testing.T) {
	viewer := &frontend.Viewer{ID: 1, Username: "viewer"}
	yes := true
	identity := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }}
	sessions := frontendtest.Sessions{SessionsFunc: func(_ context.Context, a frontend.SessionAccess) ([]frontend.Session, error) {
		if a.Viewer != viewer || a.Owner || a.Protocol != "jellyfin" {
			t.Fatal(a)
		}
		return []frontend.Session{{ID: "broker-session", ViewerID: 1, Protocol: "jellyfin", MediaPath: "/secret-grant", StartedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Capabilities: &frontend.SessionCapabilities{SupportsRemoteControl: &yes}}}, nil
	}}
	inst, err := (Jellyfin{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: identity, Sessions: sessions})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/Sessions", nil)
	r.Header.Set("X-Emby-Token", "grant")
	inst.Handler().ServeHTTP(w, r)
	var result []map[string]any
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || len(result) != 1 || result[0]["Id"] != "broker-session" || result[0]["SupportsRemoteControl"] != true || strings.Contains(w.Body.String(), "secret-grant") {
		t.Fatal(w.Code, w.Body.String())
	}
	spec, err := openapi.CachedSpec("../../conform/specs/manifest.json", "jellyfin", "")
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	doc, err := openapi.Parse(spec)
	if err != nil {
		t.Fatal(err)
	}
	if err = doc.Validate("GET", "/Sessions", w.Code, w.Header().Get("Content-Type"), w.Body.Bytes()); err != nil {
		t.Fatal(err)
	}
}
