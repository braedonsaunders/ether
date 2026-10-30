package mediabrowser

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/conform"
	conformmb "github.com/braedonsaunders/ether/internal/conform/mediabrowser"
	"github.com/braedonsaunders/ether/internal/conform/openapi"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

func TestGroupingOptionsInfuseRecordedStatusAndPolicy(t *testing.T) {
	viewer := &frontend.Viewer{ID: 1}
	identity := frontendtest.Identity{ResolveFunc: func(_ context.Context, token string) (*frontend.Viewer, error) {
		if token != "valid" {
			return nil, errors.New("denied")
		}
		return viewer, nil
	}}
	experience := frontendtest.Experience{ViewsFunc: func(_ context.Context, v *frontend.Viewer) ([]frontend.View, error) {
		if v != viewer {
			t.Fatal("wrong viewer")
		}
		return []frontend.View{{ID: "1", Name: "Movies", Type: "movie"}, {ID: "2", Name: "TV", Type: "show"}, {ID: "3", Name: "Music", Type: "music"}}, nil
	}}
	protocol, err := conformmb.Load("jellyfin", "../../conform/profiles/jellyfin")
	if err != nil {
		t.Fatal(err)
	}
	var request conform.Request
	for _, item := range protocol.Corpus("infuse-jellyfin") {
		if strings.HasPrefix(item.Path, "/UserViews/GroupingOptions?") {
			request = item
			break
		}
	}
	if request.Path == "" {
		t.Fatal("captured Infuse probe missing")
	}
	fixture, err := conform.LoadFixture(filepath.Join("..", "..", "..", "testdata", "conform", "jellyfin", "infuse-jellyfin", protocol.FixtureName("infuse-jellyfin", request, true)+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if fixture.Status != 200 {
		t.Fatal("reference status", fixture.Status)
	}
	// Verify the actual comparison pipeline cannot silently accept a 404.
	bad := protocol.Compare(fixture, conform.Response{Status: 404, Headers: http.Header{"Content-Type": []string{"application/json"}}, Body: []byte(`{}`)})
	if bad.Blocking() == 0 {
		t.Fatal("captured 200 versus 404 was not blocking")
	}
	for _, tc := range []struct {
		name  string
		front frontend.Frontend
	}{{"jellyfin", Jellyfin{MachineID: "test"}}, {"emby", Emby{MachineID: "test"}}} {
		inst, err := tc.front.Mount(context.Background(), frontend.Host{Identity: identity, Experience: experience})
		if err != nil {
			t.Fatal(err)
		}
		uid, _ := ids.GUID(ids.User, 1)
		r := httptest.NewRequest("GET", "/UserViews/GroupingOptions?userId="+uid, nil)
		r.Header.Set("X-Emby-Token", "valid")
		w := httptest.NewRecorder()
		inst.Handler().ServeHTTP(w, r)
		movie, _ := ids.GUID(ids.View, 1)
		show, _ := ids.GUID(ids.View, 2)
		if w.Code != 200 || !strings.Contains(w.Body.String(), movie) || !strings.Contains(w.Body.String(), show) || strings.Contains(w.Body.String(), "Music") {
			t.Fatal(tc.name, w.Code, w.Body.String())
		}
		if tc.name == "jellyfin" {
			spec, err := openapi.CachedSpec("../../conform/specs/manifest.json", "jellyfin", "")
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			doc, err := openapi.Parse(spec)
			if err != nil {
				t.Fatal(err)
			}
			if err := doc.Validate("GET", r.URL.Path, w.Code, w.Header().Get("Content-Type"), w.Body.Bytes()); err != nil {
				t.Fatal(err)
			}
		}
	}
}
