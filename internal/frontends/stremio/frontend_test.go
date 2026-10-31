package stremio

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
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

func TestRecentCatalogPagesByReleaseDate(t *testing.T) {
	identity := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return &frontend.Viewer{ID: 7}, nil }}
	experience := frontendtest.Experience{
		ViewsFunc: func(context.Context, *frontend.Viewer) ([]frontend.View, error) {
			return []frontend.View{{ID: "movies", Type: "movie"}}, nil
		},
		LibraryItemsFunc: func(_ context.Context, _ *frontend.Viewer, _ frontend.Device, view string, q frontend.ItemQuery) (frontend.Page, error) {
			if view != "movies" || q.Sort != "released" || !q.Desc || q.Offset != 40 || q.Limit != 100 {
				t.Fatalf("recent query view=%s %+v", view, q)
			}
			item := frontend.Title{Title: "New", Type: "movie"}
			item.IDs.TMDB = 9
			return frontend.Page{Items: []frontend.Title{item}, Total: 41}, nil
		},
		RowsFunc: func(context.Context, frontend.RowRequest) ([]frontend.Row, error) {
			t.Fatal("recent catalog built the home")
			return nil, nil
		},
	}
	inst, err := (Frontend{}).Mount(context.Background(), frontend.Host{Identity: identity, Experience: experience, Sessions: frontendtest.Sessions{}})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	inst.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/stremio/valid/catalog/movie/ether-recent/skip=40.json", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"id":"tmdb:9"`) {
		t.Fatalf("recent: %d %s", rec.Code, rec.Body.String())
	}
}

func TestSeriesRowUsesShowIdentityWithoutAnItemRead(t *testing.T) {
	identity := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return &frontend.Viewer{ID: 7}, nil }}
	episode := frontend.Title{Key: 99, Type: "episode", Title: "Pilot", ShowKey: 42, ShowTitle: "Visible", ShowYear: 2020}
	episode.ShowIDs.TMDB = 42
	experience := frontendtest.Experience{
		RowsFunc: func(_ context.Context, req frontend.RowRequest) ([]frontend.Row, error) {
			if req.Purpose != frontend.RowContinue {
				t.Fatalf("purpose %q", req.Purpose)
			}
			return []frontend.Row{{Purpose: frontend.RowContinue, Entries: []frontend.RowEntry{{Kind: frontend.RowEntryTitle, Title: &episode}}}}, nil
		},
		ItemFunc: func(context.Context, frontend.ItemRequest) (frontend.ItemDetail, error) {
			t.Fatal("episode row read the show")
			return frontend.ItemDetail{}, nil
		},
	}
	inst, err := (Frontend{}).Mount(context.Background(), frontend.Host{Identity: identity, Experience: experience, Sessions: frontendtest.Sessions{}})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	inst.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/stremio/valid/catalog/series/ether-continue.json", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"id":"tmdb:42"`) || !strings.Contains(rec.Body.String(), "Visible") {
		t.Fatalf("series row: %d %s", rec.Code, rec.Body.String())
	}
}

func TestCatalogBodyIsCachedForTheSameFreshness(t *testing.T) {
	calls := 0
	fresh := frontend.PageFreshness{Catalog: 3}
	host := frontend.Host{
		Identity: frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return &frontend.Viewer{ID: 7}, nil }},
		Experience: frontendtest.Experience{
			ViewsFunc: func(context.Context, *frontend.Viewer) ([]frontend.View, error) {
				return []frontend.View{{ID: "movies", Type: "movie"}}, nil
			},
			LibraryItemsFunc: func(context.Context, *frontend.Viewer, frontend.Device, string, frontend.ItemQuery) (frontend.Page, error) {
				calls++
				item := frontend.Title{Title: "Cached", Type: "movie"}
				item.IDs.TMDB = 5
				return frontend.Page{Items: []frontend.Title{item}, Total: 1}, nil
			},
		},
		Sessions:      frontendtest.Sessions{},
		PageFreshness: func() frontend.PageFreshness { return fresh },
	}
	inst, err := (Frontend{}).Mount(context.Background(), host)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		inst.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/stremio/valid/catalog/movie/ether-movies.json", nil))
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), "Cached") {
			t.Fatalf("cached catalog: %d %s", rec.Code, rec.Body.String())
		}
	}
	if calls != 1 {
		t.Fatalf("library reads %d", calls)
	}
	fresh.Catalog++
	rec := httptest.NewRecorder()
	inst.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/stremio/valid/catalog/movie/ether-movies.json", nil))
	if rec.Code != 200 || calls != 2 {
		t.Fatalf("stale cache served: code=%d calls=%d", rec.Code, calls)
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
				key, err := ids.StremioTMDBKey("series", ids.StremioID{Source: "tmdb", ID: "42", HasEpisode: true, Season: 1, Episode: q.Offset + j + 1})
				if err != nil {
					t.Fatal(err)
				}
				items[j] = frontend.Title{Key: key, Type: "episode", Title: "Episode", Season: 1, Episode: q.Offset + j + 1}
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

func TestCatalogPaginatesAcrossLibraries(t *testing.T) {
	identity := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return &frontend.Viewer{ID: 7}, nil }}
	experience := frontendtest.Experience{
		ViewsFunc: func(context.Context, *frontend.Viewer) ([]frontend.View, error) {
			return []frontend.View{{ID: "first", Type: "movie"}, {ID: "second", Type: "movie"}}, nil
		},
		LibraryItemsFunc: func(_ context.Context, _ *frontend.Viewer, _ frontend.Device, view string, q frontend.ItemQuery) (frontend.Page, error) {
			if view == "first" {
				if q.Offset != 2 {
					t.Fatalf("first offset %d", q.Offset)
				}
				return frontend.Page{Total: 2}, nil
			}
			if view != "second" || q.Offset != 0 {
				t.Fatalf("second: %s %+v", view, q)
			}
			title := frontend.Title{Title: "Second library", Type: "movie"}
			title.IDs.TMDB = 42
			return frontend.Page{Items: []frontend.Title{title}, Total: 1}, nil
		},
	}
	inst, err := (Frontend{}).Mount(context.Background(), frontend.Host{Identity: identity, Experience: experience, Sessions: frontendtest.Sessions{}})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	inst.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/stremio/valid/catalog/movie/ether-movies/skip=2.json", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Second library") {
		t.Fatalf("catalog: %d %s", rec.Code, rec.Body.String())
	}
}

func TestIMDbEpisodeUsesPolicyCheckedExternalResolution(t *testing.T) {
	lazy := &lazyFixture{}
	experience := frontendtest.Experience{ResolveExternalFunc: func(_ context.Context, viewer *frontend.Viewer, device frontend.Device, req frontend.ExternalRequest) (frontend.RatingKey, error) {
		if viewer.ID != 7 || device.ID == "" || req.Source != "imdb" || req.ID != "tt1234567" || req.Type != "show" || req.Season == nil || *req.Season != 2 || req.Episode == nil || *req.Episode != 3 {
			t.Fatalf("unexpected resolution: %+v", req)
		}
		return ids.StremioTMDBKey("series", ids.StremioID{Source: "tmdb", ID: "42", HasEpisode: true, Season: 2, Episode: 3})
	}}
	host := frontend.Host{Identity: frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return &frontend.Viewer{ID: 7}, nil }}, Experience: experience, Sessions: frontendtest.Sessions{}, LazyStreams: lazy, Capabilities: frontendtest.Capabilities{}, ClientIP: func(*http.Request) (netip.Addr, bool) { return netip.MustParseAddr("192.168.1.7"), true }, SecurePortalURL: func(context.Context, *http.Request) (string, error) { return "https://ether.example/ether/me", nil }}
	inst, err := (Frontend{}).Mount(context.Background(), host)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	inst.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/stremio/grant/stream/series/tt1234567:2:3.json", nil))
	if rec.Code != http.StatusOK || !lazy.issued {
		t.Fatalf("stream: %d %s", rec.Code, rec.Body.String())
	}
}

func TestCatalogArtworkUsesScopedImageGrant(t *testing.T) {
	title := frontend.Title{Key: 42, Title: "Visible", Type: "movie"}
	title.IDs.TMDB = 42
	host := frontend.Host{
		Identity: frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return &frontend.Viewer{ID: 7}, nil }},
		Experience: frontendtest.Experience{ViewsFunc: func(context.Context, *frontend.Viewer) ([]frontend.View, error) {
			return []frontend.View{{ID: "movies", Type: "movie"}}, nil
		}, LibraryItemsFunc: func(context.Context, *frontend.Viewer, frontend.Device, string, frontend.ItemQuery) (frontend.Page, error) {
			return frontend.Page{Items: []frontend.Title{title}, Total: 1}, nil
		}},
		Sessions: frontendtest.Sessions{}, SecurePortalURL: func(context.Context, *http.Request) (string, error) { return "https://ether.example/ether/me", nil },
		ImageGrants: frontendtest.ImageGrants{IssueBatchFunc: func(_ context.Context, req frontend.ImageBatchRequest) ([]frontend.ImageBatchItem, error) {
			if req.Credential != "grant" || len(req.Items) != 1 || req.Items[0] != 42 || req.Device.ID == "" {
				t.Fatalf("bad image scope: %+v", req)
			}
			return []frontend.ImageBatchItem{{Item: 42, Authoritative: true, Images: []frontend.GrantedImage{{Secret: "opaque-image"}}}}, nil
		}, ServeFunc: func(w http.ResponseWriter, _ *http.Request, secret string) {
			if secret != "opaque-image" {
				t.Fatal(secret)
			}
			_, _ = w.Write([]byte("poster"))
		}},
	}
	inst, err := (Frontend{}).Mount(context.Background(), host)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	inst.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/stremio/grant/catalog/movie/ether-movies.json", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "https://ether.example/stremio/image/opaque-image") || strings.Contains(rec.Body.String(), "/grant/") {
		t.Fatalf("catalog: %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	inst.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/stremio/image/opaque-image", nil))
	if rec.Code != 200 || rec.Body.String() != "poster" {
		t.Fatalf("poster: %d %s", rec.Code, rec.Body.String())
	}
}

func TestSeriesMetadataUsesCanonicalKeysForVirtualOrder(t *testing.T) {
	key, err := ids.StremioTMDBKey("series", ids.StremioID{Source: "tmdb", ID: "42", HasEpisode: true, Season: 3, Episode: 8})
	if err != nil {
		t.Fatal(err)
	}
	host := frontend.Host{Identity: frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return &frontend.Viewer{ID: 7}, nil }}, Sessions: frontendtest.Sessions{}, Experience: frontendtest.Experience{
		ItemFunc: func(_ context.Context, req frontend.ItemRequest) (frontend.ItemDetail, error) {
			return frontend.ItemDetail{Title: frontend.Title{Key: req.Key, Title: "Series", Type: "show"}}, nil
		},
		ChildrenFunc: func(context.Context, *frontend.Viewer, frontend.Device, frontend.RatingKey, frontend.ItemQuery) (frontend.Page, error) {
			return frontend.Page{Items: []frontend.Title{{Key: key, Type: "episode", Title: "Episode", Season: 1, Episode: 2}}, Total: 1}, nil
		},
	}}
	inst, err := (Frontend{}).Mount(context.Background(), host)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	inst.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/stremio/grant/meta/series/tmdb:42.json", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"id":"tmdb:42:3:8"`) || !strings.Contains(rec.Body.String(), `"season":1`) || !strings.Contains(rec.Body.String(), `"episode":2`) {
		t.Fatalf("metadata: %d %s", rec.Code, rec.Body.String())

	}
}

func TestRegisteredRoutesMatchMountedFrontend(t *testing.T) {
	got, err := RegisteredRoutes()
	if err != nil {
		t.Fatal(err)
	}
	inst, err := (Frontend{}).Mount(context.Background(), frontend.Host{Identity: frontendtest.Identity{}, Experience: frontendtest.Experience{}, Sessions: frontendtest.Sessions{}})
	if err != nil {
		t.Fatal(err)
	}
	actual := inst.(frontend.RouteSource).Routes()
	if !slices.Equal(got, actual) {
		t.Fatalf("inventory differs: %+v vs %+v", got, actual)

	}
}

func TestOnlyAuthenticatedClientsAreCounted(t *testing.T) {
	var seen []string
	host := frontend.Host{Identity: frontendtest.Identity{ResolveFunc: func(_ context.Context, token string) (*frontend.Viewer, error) {
		if token != "valid" {
			return nil, errors.New("invalid")
		}
		return &frontend.Viewer{ID: 7}, nil
	}}, Experience: frontendtest.Experience{}, Sessions: frontendtest.Sessions{}, ClientSeen: func(id string) { seen = append(seen, id) }}
	inst, err := (Frontend{}).Mount(context.Background(), host)
	if err != nil {
		t.Fatal(err)
	}
	for _, credential := range []string{"invalid", "valid", "valid"} {
		inst.Handler().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/stremio/"+credential+"/manifest.json", nil))
	}
	if len(seen) != 2 || seen[0] == "" || seen[0] != seen[1] || strings.Contains(seen[0], "valid") {
		t.Fatalf("unexpected clients: %+v", seen)
	}
}
