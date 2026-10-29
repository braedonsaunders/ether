package stremio

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
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

func TestSetupCreatesSurfaceGrantFromVerifiedPeer(t *testing.T) {
	identity := frontendtest.Identity{SignInFunc: func(ctx context.Context, user, password string, device frontend.Device) (string, error) {
		ip, ok := frontend.ClientIP(ctx)
		if !ok || ip.String() != "192.0.2.1" || user != "viewer" || password != "secret" || device.ID == "" {
			t.Fatal("missing verified identity inputs")
		}
		return "grant-secret", nil
	}}
	host := frontend.Host{Identity: identity, Experience: frontendtest.Experience{}, Sessions: frontendtest.Sessions{},
		ClientIP:        func(*http.Request) (netip.Addr, bool) { return netip.MustParseAddr("192.0.2.1"), true },
		ViewerPortalURL: func(context.Context, *http.Request) (string, error) { return "https://ether.example/ether/me", nil },
	}
	inst, err := (Frontend{}).Mount(context.Background(), host)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	inst.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/stremio/setup", strings.NewReader(`{"username":"viewer","password":"secret"}`)))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "https://ether.example/stremio/grant-secret/manifest.json") {
		t.Fatalf("setup: %d %s", rec.Code, rec.Body.String())
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
