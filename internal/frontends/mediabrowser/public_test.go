package mediabrowser

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/braedonsaunders/ether/internal/conform/openapi"
	"github.com/braedonsaunders/ether/internal/frontend/kit/route"
)

func TestPublicRoutes(t *testing.T) {
	r := &route.Router{}
	if err := PublicRoutes(r, PublicInfo{ID: "123", ServerName: "Ether", Version: "12.4.0", ProductName: "Jellyfin Server"}); err != nil {
		t.Fatal(err)
	}
	h := StripPrefix("/jellyfin", r)
	for _, path := range []string{"/System/Info/Public", "/Jellyfin/System/Info/Public"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		var got map[string]any
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil || got["Id"] != "123" || got["StartupWizardCompleted"] != true {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/Users/Public", nil))
	if w.Code != 200 || w.Body.String() != "[]" {
		t.Fatal(w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/jellyfinish/Users/Public", nil))
	if w.Code != 404 {
		t.Fatal(w.Code)
	}
}

func TestJellyfinPublicSpec(t *testing.T) {
	spec, err := openapi.CachedSpec("../../conform/specs/manifest.json", "jellyfin", "")
	if os.IsNotExist(err) {
		t.Skip("spec cache empty")
	}
	if err != nil {
		t.Fatal(err)
	}
	doc, err := openapi.Parse(spec)
	if err != nil {
		t.Fatal(err)
	}
	r := &route.Router{}
	if err := PublicRoutes(r, PublicInfo{ID: "00000000000000000000000000000001", ServerName: "Ether", Version: "12.4.0", ProductName: "Jellyfin Server"}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/System/Info/Public", "/Users/Public", "/System/Ping", "/Branding/Configuration"} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if err := doc.Validate("GET", path, w.Code, w.Header().Get("Content-Type"), w.Body.Bytes()); err != nil {
			t.Errorf("%s: %v", path, err)
		}
	}
}
