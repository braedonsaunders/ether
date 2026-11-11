package mediabrowser

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/conform/openapi/spectest"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
)

var testStart = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// TestSessionListIdlePlayState pins the reference idle shape for both
// protocols: PlayState carries the player booleans, AdditionalUsers is empty,
// and the protocol-specific list fields are present even with nothing playing.
func TestSessionListIdlePlayState(t *testing.T) {
	viewer := &frontend.Viewer{ID: 1, Username: "viewer"}
	identity := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }}
	sessions := frontendtest.Sessions{SessionsFunc: func(context.Context, frontend.SessionAccess) ([]frontend.Session, error) {
		return []frontend.Session{{ID: "idle", ViewerID: 1, StartedAt: testStart, Device: frontend.Device{ID: "tv", Client: "TV", DeviceName: "TV"}}}, nil
	}}
	cases := []struct {
		name   string
		front  frontend.Frontend
		path   string
		assert func(t *testing.T, session map[string]any)
	}{
		{
			name: "jellyfin", front: Jellyfin{MachineID: "test"}, path: "/Sessions",
			assert: func(t *testing.T, session map[string]any) {
				if _, ok := session["NowPlayingQueue"].([]any); !ok {
					t.Fatal("NowPlayingQueue", session["NowPlayingQueue"])
				}
				caps, ok := session["Capabilities"].(map[string]any)
				if !ok || caps["SupportsMediaControl"] != false {
					t.Fatal("Capabilities", session["Capabilities"])
				}
				state := session["PlayState"].(map[string]any)
				if state["PlaybackOrder"] != "Default" {
					t.Fatal(state)
				}
				if _, ok := state["PlayMethod"]; ok {
					t.Fatal("idle session has PlayMethod", state)
				}
			},
		},
		{
			name: "emby", front: Emby{MachineID: "test"}, path: "/Sessions",
			assert: func(t *testing.T, session map[string]any) {
				if session["PlaylistIndex"] != float64(0) || session["PlaylistLength"] != float64(0) || session["Protocol"] != "HTTP/1.1" {
					t.Fatal(session)
				}
				if _, ok := session["Capabilities"]; ok {
					t.Fatal("emby session has Capabilities")
				}
				state := session["PlayState"].(map[string]any)
				if state["PlaybackRate"] != float64(1) || state["SleepTimerMode"] != "None" {
					t.Fatal(state)
				}
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			inst, err := c.front.Mount(context.Background(), frontend.Host{Identity: identity, Sessions: sessions})
			if err != nil {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			r := httptest.NewRequest("GET", c.path, nil)
			r.Header.Set("X-Emby-Token", "grant")
			inst.Handler().ServeHTTP(w, r)
			var result []map[string]any
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || len(result) != 1 {
				t.Fatal(w.Code, w.Body.String())
			}
			session := result[0]
			if _, ok := session["AdditionalUsers"].([]any); !ok || session["RemoteEndPoint"] != "" {
				t.Fatal(session)
			}
			state := session["PlayState"].(map[string]any)
			if state["CanSeek"] != false || state["IsPaused"] != false || state["IsMuted"] != false || state["RepeatMode"] != "RepeatNone" {
				t.Fatal(state)
			}
			c.assert(t, session)
			if doc := spectest.Document(t, c.name); doc != nil {
				if err := doc.Validate("GET", "/Sessions", w.Code, w.Header().Get("Content-Type"), w.Body.Bytes()); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

// TestSessionListPlayingPlayMethod reports the broker's delivery decision.
func TestSessionListPlayingPlayMethod(t *testing.T) {
	viewer := &frontend.Viewer{ID: 1, Username: "viewer"}
	identity := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }}
	sessions := frontendtest.Sessions{SessionsFunc: func(context.Context, frontend.SessionAccess) ([]frontend.Session, error) {
		return []frontend.Session{{ID: "playing", ViewerID: 1, Item: 42, PlayMethod: "DirectStream", StartedAt: testStart}}, nil
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
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || len(result) != 1 {
		t.Fatal(w.Code, w.Body.String())
	}
	state := result[0]["PlayState"].(map[string]any)
	if state["CanSeek"] != true || state["PlayMethod"] != "DirectStream" {
		t.Fatal(state)
	}
}
