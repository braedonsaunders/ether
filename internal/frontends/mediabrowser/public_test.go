package mediabrowser

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/conform"
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
		if path == "/Jellyfin/System/Info/Public" && got["LocalAddress"] != "http://example.com/jellyfin" {
			t.Fatal(got)
		}
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/Users/Public", nil))
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != "[]" {
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

func TestPublicRecordedCorpus(t *testing.T) {
	fixtures := map[string]string{"/System/Info/Public": "findroid_jsonconnect_system info_GET__System_Info_Public.json", "/Users/Public": "findroid_jsonconnect_public users_GET__Users_Public.json"}
	r := &route.Router{}
	if err := PublicRoutes(r, PublicInfo{ID: "123", ServerName: "Ether", Version: "12.4.0", ProductName: "Jellyfin Server"}); err != nil {
		t.Fatal(err)
	}
	for path, file := range fixtures {
		reference, err := conform.LoadFixture("../../../testdata/conform/jellyfin/findroid/" + file)
		if err != nil {
			t.Fatal(err)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		report := conform.Compare(reference, conform.Response{Status: w.Code, Headers: w.Header(), Body: w.Body.Bytes()})
		if report.Blocking() != 0 {
			t.Errorf("%s: %+v", path, report.Differences)
		}
	}
}

func TestEmbyUnservedWebAssetsReturnNotFound(t *testing.T) {
	r := &route.Router{}
	if err := PublicRoutes(r, PublicInfo{ProductName: "Emby Server", Version: "4.10.2.0"}); err != nil {
		t.Fatal(err)
	}
	h := StripPrefix("/emby", r)
	for _, path := range []string{"/emby/web/manifest.json", "/emby/web/index.html", "/emby/web/strings/en-US.json", "/web/other.js"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", path, w.Code)
		}
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/emby/web", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Connect to Ether") {
		t.Fatalf("connection page: %d %q", w.Code, w.Body.String())
	}
}

func TestConnectionPageLocaleCoverage(t *testing.T) {
	for _, lang := range []string{"en", "fr", "es", "cs", "da", "de", "fi", "it", "ja", "ko", "nb", "nl", "pl", "pt-BR", "ru", "sv", "tr", "uk", "zh-CN", "zh-TW"} {
		t.Run(lang, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/web", nil)
			r.Header.Set("Accept-Language", lang)
			w := httptest.NewRecorder()
			webPage(w, r)
			if !strings.Contains(w.Body.String(), `lang="`+lang+`"`) {
				t.Fatalf("wrong locale: %s", w.Body.String())
			}
			if lang != "en" && strings.Contains(w.Body.String(), "Open a native") {
				t.Fatal("English placeholder")
			}
		})
	}
}

func TestSystemEndpointReportsCallerNetwork(t *testing.T) {
	r := &route.Router{}
	if err := PublicRoutes(r, PublicInfo{ID: "123", ServerName: "Ether", ProductName: "Emby Server"}); err != nil {
		t.Fatal(err)
	}
	h := StripPrefix("/emby", r)
	for _, tc := range []struct {
		path, remote string
		local, lan   bool
	}{
		{"/System/Endpoint", "192.168.68.70:51000", false, true},
		{"/emby/System/Endpoint", "127.0.0.1:51000", true, true},
		{"/emby/System/Endpoint", "[fe80::1]:51000", false, true},
		{"/System/Endpoint", "203.0.113.9:51000", false, false},
	} {
		req := httptest.NewRequest("GET", tc.path, nil)
		req.RemoteAddr = tc.remote
		req.Header.Set("X-Forwarded-For", "10.0.0.5")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		var got map[string]bool
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil || got["IsLocal"] != tc.local || got["IsInNetwork"] != tc.lan {
			t.Fatalf("%s from %s: %d %s", tc.path, tc.remote, w.Code, w.Body.String())
		}
	}
}
