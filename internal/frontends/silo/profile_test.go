package silo

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

type testTracks struct{ value frontend.TrackPreferences }

func (t *testTracks) Get(context.Context, *frontend.Viewer) (frontend.TrackPreferences, error) {
	return t.value, nil
}
func (t *testTracks) SetSubtitle(_ context.Context, _ *frontend.Viewer, languages []string, mode string) error {
	t.value.SubtitleLanguages = languages
	t.value.SubtitleMode = mode
	return nil
}
func TestProfilePreferencesRoundTrip(t *testing.T) {
	viewer := &frontend.Viewer{ID: 7, Username: "alice", CreatedAt: time.Unix(10000, 0), UpdatedAt: time.Unix(11000, 0)}
	var raw json.RawMessage
	prefs := frontendtest.Preferences{GetFunc: func(context.Context, *frontend.Viewer, string, string) (json.RawMessage, error) {
		if raw == nil {
			return nil, frontend.ErrPreferencesNotFound
		}
		return raw, nil
	}, PutFunc: func(_ context.Context, _ *frontend.Viewer, _, _ string, b json.RawMessage) error {
		raw = append([]byte(nil), b...)
		return nil
	}}
	tracks := &testTracks{value: frontend.TrackPreferences{SubtitleMode: "off"}}
	ident := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }}
	inst, err := (Frontend{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: ident, Preferences: prefs, TrackPreferences: tracks, Experience: frontendtest.Experience{}})
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer grant")
		w := httptest.NewRecorder()
		inst.Handler().ServeHTTP(w, r)
		return w
	}
	w := call("GET", "/api/v2/profiles", "")
	if w.Code != 200 {
		t.Fatalf("profiles %d %s", w.Code, w.Body.String())
	}
	spec, e := openapi.CachedSpec("../../conform/specs/manifest.json", "silo", "")
	if e == nil {
		doc, e := openapi.Parse(spec)
		if e != nil {
			t.Fatal(e)
		}
		if e = doc.Validate("GET", "/api/v2/profiles", w.Code, w.Header().Get("Content-Type"), w.Body.Bytes()); e != nil {
			t.Errorf("profile spec: %v", e)
		}
	} else if !os.IsNotExist(e) {
		t.Fatal(e)
	}
	w = call("PATCH", "/api/v2/profiles/7", `{"subtitle_language":"fr","subtitle_mode":"always","auto_skip_intro":true}`)
	if w.Code != 200 || tracks.value.SubtitleMode != "always" || len(tracks.value.SubtitleLanguages) != 1 || tracks.value.SubtitleLanguages[0] != "fr" || !strings.Contains(w.Body.String(), `"auto_skip_intro":true`) {
		t.Fatalf("patch %d %s %+v", w.Code, w.Body.String(), tracks.value)
	}
	w = call("GET", "/api/v2/profiles", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"subtitle_language":"fr"`) {
		t.Fatalf("readback %d %s", w.Code, w.Body.String())
	}
}
