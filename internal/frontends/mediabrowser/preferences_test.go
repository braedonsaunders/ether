package mediabrowser

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/braedonsaunders/ether/internal/conform/openapi"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestDisplayPreferences(t *testing.T) {
	viewer := &frontend.Viewer{ID: 5}
	var saved json.RawMessage
	p := frontendtest.Preferences{GetFunc: func(_ context.Context, v *frontend.Viewer, client, key string) (json.RawMessage, error) {
		if v != viewer || client != "TV" || key != "home" {
			t.Fatal(v, client, key)
		}
		if saved == nil {
			return nil, frontend.ErrPreferencesNotFound
		}
		return saved, nil
	}, PutFunc: func(_ context.Context, v *frontend.Viewer, client, key string, b json.RawMessage) error {
		if v != viewer || client != "TV" || key != "home" {
			return errors.New("scope")
		}
		saved = append([]byte(nil), b...)
		return nil
	}}
	ident := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }}
	inst, err := (Jellyfin{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: ident, Preferences: p})
	if err != nil {
		t.Fatal(err)
	}
	path := "/DisplayPreferences/home?client=TV"
	call := func(method, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("X-Emby-Token", "grant")
		inst.Handler().ServeHTTP(w, r)
		return w
	}
	w := call("GET", "")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	spec, e := openapi.CachedSpec("../../conform/specs/manifest.json", "jellyfin", "")
	if e == nil {
		doc, e := openapi.Parse(spec)
		if e != nil {
			t.Fatal(e)
		}
		if e = doc.Validate("GET", path, w.Code, w.Header().Get("Content-Type"), w.Body.Bytes()); e != nil {
			t.Fatal(e)
		}
	} else if !os.IsNotExist(e) {
		t.Fatal(e)
	}
	if w = call("POST", `{"Id":"home","SortBy":"Name"}`); w.Code != 204 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w = call("GET", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"SortBy":"Name"`) {
		t.Fatal(w.Code, w.Body.String())
	}
}
