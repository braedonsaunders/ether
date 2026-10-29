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

type lazyFixture struct{ issued bool }

func (f *lazyFixture) Issue(_ context.Context, credential string, request frontend.PlanRequest) (string, error) {
	if credential != "grant" || request.Protocol != "stremio" || request.Item.TMDB() != 42 || !request.Intent.Options.DirectPlay {
		return "", errors.New("wrong scope")
	}
	f.issued = true
	return "/sessions/lazy/scoped/file", nil
}
func (*lazyFixture) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusPartialContent)
}

func TestStreamUsesLazyCapabilityAndNeverOpensOnDiscovery(t *testing.T) {
	lazy := &lazyFixture{}
	host := frontend.Host{
		Identity: frontendtest.Identity{ResolveFunc: func(_ context.Context, token string) (*frontend.Viewer, error) {
			if token != "grant" {
				return nil, errors.New("invalid")
			}
			return &frontend.Viewer{ID: 7}, nil
		}},
		Experience: frontendtest.Experience{}, Sessions: frontendtest.Sessions{OpenFunc: func(context.Context, frontend.OpenRequest) (frontend.Session, error) {
			t.Fatal("stream discovery opened playback")
			return frontend.Session{}, nil
		}}, LazyStreams: lazy,
		Capabilities: frontendtest.Capabilities{}, ClientIP: func(*http.Request) (netip.Addr, bool) { return netip.MustParseAddr("192.168.1.7"), true },
		SecurePortalURL: func(context.Context, *http.Request) (string, error) { return "https://ether.example/ether/me", nil },
	}
	inst, err := (Frontend{}).Mount(context.Background(), host)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	inst.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/stremio/grant/stream/movie/tmdb:42.json", nil))
	if w.Code != 200 || !lazy.issued || !strings.Contains(w.Body.String(), "https://ether.example/stremio/sessions/lazy/scoped/file") || strings.Contains(w.Body.String(), "grant") {
		t.Fatalf("response: %d %s", w.Code, w.Body.String())
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

func TestSeriesMetaPaginatesEpisodes(t *testing.T) {
	identity := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return &frontend.Viewer{ID: 7}, nil }}
	calls := 0
	experience := frontendtest.Experience{
		ItemFunc: func(_ context.Context, req frontend.ItemRequest) (frontend.ItemDetail, error) {
			return frontend.ItemDetail{Title: frontend.Title{Key: req.Key, Title: "Long series", Type: "show"}}, nil
		},
		ChildrenFunc: func(_ context.Context, _ *frontend.Viewer, _ frontend.Device, _ frontend.RatingKey, q frontend.ItemQuery) (frontend.Page, error) {
			calls++
			if q.Limit != 500 || !q.Recursive || q.Offset != (calls-1)*500 {
				t.Fatalf("unexpected page query: %+v", q)
			}
			count := min(500, 501-q.Offset)
			items := make([]frontend.Title, count)
			for j := range items {
				items[j] = frontend.Title{Type: "episode", Title: "Episode", Season: 1, Episode: q.Offset + j + 1}
			}
			return frontend.Page{Items: items, Total: 501}, nil
		},
	}
	inst, err := (Frontend{}).Mount(context.Background(), frontend.Host{Identity: identity, Experience: experience, Sessions: frontendtest.Sessions{}})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	inst.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/stremio/valid/meta/series/tmdb:42.json", nil))
	if rec.Code != http.StatusOK || calls != 2 || !strings.Contains(rec.Body.String(), `"episode":501`) {
		t.Fatalf("missing episode: status=%d calls=%d", rec.Code, calls)
	}
}
