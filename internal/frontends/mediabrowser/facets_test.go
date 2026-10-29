package mediabrowser

import (
	"context"
	"errors"
	"github.com/braedonsaunders/ether/internal/conform/openapi"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestFacetsAndPolicyScopedFilterResolution(t *testing.T) {
	v := &frontend.Viewer{ID: 1}
	facet := core.FacetValue{ID: "12345678901234567890123456789012", Key: "Drama", Title: "Drama"}
	filtered := false
	exp := frontendtest.Experience{FacetsFunc: func(_ context.Context, user *core.Viewer, _ core.Device, view, kind string) ([]core.FacetValue, error) {
		if user != v || view != "1" || kind != "genre" {
			t.Fatal(user, view, kind)
		}
		return []core.FacetValue{facet}, nil
	}, ResolveFacetFunc: func(_ context.Context, user *core.Viewer, _ core.Device, view, kind, id string) (core.FacetValue, error) {
		if user != v || view != "1" || kind != "genre" || id != facet.ID {
			return core.FacetValue{}, errors.New("denied")
		}
		return facet, nil
	}, LibraryItemsFunc: func(_ context.Context, _ *core.Viewer, _ core.Device, _ string, q frontend.ItemQuery) (core.Page, error) {
		filtered = len(q.Filter.Genres) == 1 && q.Filter.Genres[0] == "Drama"
		return core.Page{}, nil
	}}
	ident := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return v, nil }}
	inst, err := (Jellyfin{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: ident, Experience: exp})
	if err != nil {
		t.Fatal(err)
	}
	view, _ := ids.GUID(ids.View, 1)
	call := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", path, nil)
		r.Header.Set("X-Emby-Token", "grant")
		inst.Handler().ServeHTTP(w, r)
		return w
	}
	path := "/Genres?ParentId=" + view
	w := call(path)
	if w.Code != 200 || !strings.Contains(w.Body.String(), facet.ID) {
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
	w = call("/Items?ParentId=" + view + "&GenreIds=" + facet.ID)
	if w.Code != 200 || !filtered {
		t.Fatal(w.Code, w.Body.String())
	}
	w = call("/Items?ParentId=" + view + "&GenreIds=forbidden")
	if w.Code != 400 {
		t.Fatal(w.Code)
	}
}
