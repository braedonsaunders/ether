package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontends/webdav"
)

func TestVirtualPathsReachWebDAVFilesAcrossShortPages(t *testing.T) {
	viewer := &core.Viewer{ID: 7}
	movies := []core.Title{
		{Key: core.MovieKey(1), Type: core.TypeMovie, Title: "Same", Year: 2024},
		{Key: core.MovieKey(2), Type: core.TypeMovie, Title: "Same", Year: 2024},
	}
	experience := frontendtest.Experience{
		ViewsFunc: func(context.Context, *core.Viewer) ([]frontend.View, error) {
			return []frontend.View{{ID: "movies", Type: core.TypeMovie}}, nil
		},
		LibraryItemsFunc: func(_ context.Context, _ *core.Viewer, _ core.Device, _ string, q frontend.ItemQuery) (core.Page, error) {
			return core.Page{Items: movies[q.Offset : q.Offset+1], Total: len(movies)}, nil
		},
	}
	paths, err := (surfaceVirtualPaths{experience: experience}).Paths(context.Background(), viewer, core.Device{}, []core.RatingKey{movies[0].Key, movies[1].Key})
	if err != nil {
		t.Fatal(err)
	}
	host := frontend.Host{
		Experience: experience,
		Identity: frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) {
			return viewer, nil
		}},
		Sessions: frontendtest.Sessions{},
	}
	inst, err := (webdav.Frontend{}).Mount(context.Background(), host)
	if err != nil {
		t.Fatal(err)
	}
	for _, movie := range movies {
		virtual := paths[movie.Key]
		if !strings.Contains(virtual, movie.Key.String()) {
			t.Fatalf("collision not disambiguated: %q", virtual)
		}
		req := httptest.NewRequest(http.MethodHead, (&url.URL{Path: "/webdav/" + virtual}).String(), nil)
		req.SetBasicAuth("7", "grant")
		w := httptest.NewRecorder()
		inst.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("path %q returned %d", virtual, w.Code)
		}
	}
}
