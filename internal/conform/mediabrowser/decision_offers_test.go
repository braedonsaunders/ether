//go:build conformlab

package mediabrowser

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDecisionOffersRequireAuthenticatedDetailAndMatchingDiagnostic(t *testing.T) {
	hash := strings.Repeat("a", 64)
	wrongItem := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Authorization"), `Token="secret"`) {
			http.Error(w, "unauthorized", 401)
			return
		}
		switch r.URL.Path {
		case "/Items/item":
			fmt.Fprint(w, `{"Id":"item","MediaSources":[{"Id":"handle"}]}`)
		case "/__scorecard/offer":
			item := "item"
			if wrongItem {
				item = "other"
			}
			if r.URL.Query().Get("handle") != "handle" {
				t.Error("unexpected handle")
			}
			fmt.Fprintf(w, `{"handle":"handle","item_id":%q,"release_id":"release","expected_sha256":%q}`, item, hash)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	s := decisionLabServer{base: server.URL, token: "secret", device: "device", client: server.Client()}
	got, err := s.offeredVersions("item")
	if err != nil || got["release"].id != "handle" || got["release"].hash != hash {
		t.Fatal(got, err)
	}
	wrongItem = true
	if _, err = s.offeredVersions("item"); err == nil {
		t.Fatal("cross-item offer accepted")
	}
}

func TestDecisionLabSettingsCanReadPrivateEnvironment(t *testing.T) {
	t.Setenv("JELLYFIN_ADULT_USER", "fixture-user")
	t.Setenv("JELLYFIN_ADULT_PASSWORD", "fixture-password")
	settings, err := decisionLabSettings("env")
	if err != nil || settings["JELLYFIN_ADULT_USER"] != "fixture-user" || settings["JELLYFIN_ADULT_PASSWORD"] != "fixture-password" {
		t.Fatal("environment credentials unavailable")
	}
}
