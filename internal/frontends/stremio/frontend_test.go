package stremio

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
)

func TestManifestRequiresSurfaceGrantAndProvidesCORS(t *testing.T) {
	identity := frontendtest.Identity{ResolveFunc: func(_ context.Context, token string) (*frontend.Viewer, error) {
		if token != "valid" {
			return nil, errors.New("invalid")
		}
		return &frontend.Viewer{ID: 7}, nil
	}}
	inst, err := (Frontend{}).Mount(context.Background(), frontend.Host{Identity: identity, Experience: frontendtest.Experience{}, Sessions: frontendtest.Sessions{}})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{http.MethodGet, "/stremio/valid/manifest.json", http.StatusOK},
		{http.MethodGet, "/stremio/invalid/manifest.json", http.StatusUnauthorized},
		{http.MethodOptions, "/stremio/valid/manifest.json", http.StatusNoContent},
	} {
		rec := httptest.NewRecorder()
		inst.Handler().ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		if rec.Code != tc.status || rec.Header().Get("Access-Control-Allow-Origin") != "*" || rec.Header().Get("Cache-Control") != "private, no-store" {
			t.Fatalf("%s %s: status=%d headers=%v", tc.method, tc.path, rec.Code, rec.Header())
		}
	}
}

func TestCatalogPassesViewerPaginationAndGenreToExperience(t *testing.T) {
	identity := frontendtest.Identity{ResolveFunc: func(_ context.Context, token string) (*frontend.Viewer, error) {
		if token != "valid" {
			return nil, errors.New("invalid")
		}
		return &frontend.Viewer{ID: 7}, nil
	}}
	experience := frontendtest.Experience{
		ViewsFunc: func(context.Context, *frontend.Viewer) ([]frontend.View, error) {
			return []frontend.View{{ID: "movies", Type: "movie"}}, nil
		},
		LibraryItemsFunc: func(_ context.Context, viewer *frontend.Viewer, device frontend.Device, view string, q frontend.ItemQuery) (frontend.Page, error) {
			if viewer.ID != 7 || device.ID == "" || view != "movies" || q.Offset != 20 || len(q.Filter.Genres) != 1 || q.Filter.Genres[0] != "Drama" {
				t.Fatalf("incorrect request: viewer=%+v device=%+v view=%s q=%+v", viewer, device, view, q)
			}
			item := frontend.Title{Title: "Fixture", Type: "movie"}
			item.IDs.TMDB = 42
			return frontend.Page{Items: []frontend.Title{item}}, nil
		},
	}
	inst, err := (Frontend{}).Mount(context.Background(), frontend.Host{Identity: identity, Experience: experience, Sessions: frontendtest.Sessions{}})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	inst.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/stremio/valid/catalog/movie/ether-movies/skip=20&genre=Drama.json", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"id":"tmdb:42"`) {
		t.Fatalf("catalog: %d %s", rec.Code, rec.Body.String())
	}
}

func TestMetaRejectsHiddenTitle(t *testing.T) {
	identity := frontendtest.Identity{ResolveFunc: func(_ context.Context, token string) (*frontend.Viewer, error) {
		return &frontend.Viewer{ID: 7}, nil
	}}
	experience := frontendtest.Experience{ItemFunc: func(_ context.Context, req frontend.ItemRequest) (frontend.ItemDetail, error) {
		if req.Viewer.ID != 7 || req.Key.TMDB() != 42 {
			t.Fatalf("wrong request: %+v", req)
		}
		return frontend.ItemDetail{}, errors.New("hidden")
	}}
	inst, err := (Frontend{}).Mount(context.Background(), frontend.Host{Identity: identity, Experience: experience, Sessions: frontendtest.Sessions{}})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	inst.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/stremio/valid/meta/movie/tmdb:42.json", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("hidden title returned: %d %s", rec.Code, rec.Body.String())
	}
}

func TestRowCatalogSkipsUnsupportedEntries(t *testing.T) {
	identity := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return &frontend.Viewer{ID: 7}, nil }}
	title := frontend.Title{Key: 42, Title: "Visible", Type: "movie"}
	title.IDs.TMDB = 42
	experience := frontendtest.Experience{RowsFunc: func(_ context.Context, req frontend.RowRequest) ([]frontend.Row, error) {
		if req.Viewer.ID != 7 || req.Placement != frontend.PlacementHome {
			t.Fatal("wrong row scope")
		}
		return []frontend.Row{{Purpose: frontend.RowContinue, Entries: []frontend.RowEntry{
			{Kind: frontend.RowEntryDirectory, Directory: &frontend.RowDirectory{Title: "Unsupported"}},
			{Kind: frontend.RowEntryTitle, Title: &title},
		}}}, nil
	}}
	inst, err := (Frontend{}).Mount(context.Background(), frontend.Host{Identity: identity, Experience: experience, Sessions: frontendtest.Sessions{}})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	inst.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/stremio/valid/catalog/movie/ether-continue.json", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "Visible") || strings.Contains(rec.Body.String(), "Unsupported") {
		t.Fatalf("row: %d %s", rec.Code, rec.Body.String())
	}
}
