package mediabrowser

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/conform/openapi"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

func TestItemDetailPassesVerifiedPeer(t *testing.T) {
	peer := netip.MustParseAddr("192.0.2.5")
	viewer := &frontend.Viewer{ID: 1}
	identity := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }}
	experience := frontendtest.Experience{ItemFunc: func(ctx context.Context, _ frontend.ItemRequest) (frontend.ItemDetail, error) {
		if got, ok := frontend.ClientIP(ctx); !ok || got != peer {
			return frontend.ItemDetail{}, errors.New("missing verified peer")
		}
		return frontend.ItemDetail{Title: frontend.Title{Key: 5, Type: "movie", Genres: []string{"Drama"}}}, nil
	}, NamedFacetIDsFunc: func(_ context.Context, v *frontend.Viewer, _ frontend.Device, key frontend.RatingKey, kind string, names []string) (map[string]string, error) {
		if v != viewer || key != 5 || kind != "genre" || len(names) != 1 || names[0] != "Drama" {
			return nil, errors.New("wrong facet scope")
		}
		return map[string]string{"Drama": "genre-id"}, nil
	}}
	inst, err := (Emby{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: identity, Experience: experience, ClientIP: func(*http.Request) (netip.Addr, bool) { return peer, true }})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/Items/"+ids.RatingKeyGUID(5), nil)
	r.Header.Set("X-Emby-Token", "grant")
	w := httptest.NewRecorder()
	inst.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"GenreItems":[{"Id":"genre-id","Name":"Drama"}]`) {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestItemDoesNotAdvertiseUnsupportedDeleteOrDownload(t *testing.T) {
	i := &instance{protocol: "jellyfin"}
	item := i.itemDTO(frontend.Title{Key: 5, Type: "movie"}, frontend.WatchStatus{})
	if item["CanDelete"] != false || item["CanDownload"] != false {
		t.Fatal(item)
	}
}

func TestItemDetailIncludesExperienceTrackMetadata(t *testing.T) {
	v := &frontend.Viewer{ID: 1}
	identity := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return v, nil }}
	experience := frontendtest.Experience{ItemFunc: func(context.Context, frontend.ItemRequest) (frontend.ItemDetail, error) {
		return frontend.ItemDetail{Title: frontend.Title{Key: 5, Type: "movie"}, Tracks: []frontend.StreamInfo{{Type: "video", Index: 2, Codec: "h264"}}, Versions: []frontend.Version{{ID: "version"}}, Markers: []frontend.Marker{{Type: "intro"}}}, nil
	}}
	inst, err := (Jellyfin{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: identity, Experience: experience})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/Items/"+ids.RatingKeyGUID(5), nil)
	r.Header.Set("X-Emby-Token", "grant")
	w := httptest.NewRecorder()
	inst.Handler().ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"MediaStreams":[`) || !strings.Contains(w.Body.String(), `"Codec":"h264"`) {
		t.Fatal(w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), `"HasSegments":true`) {
		t.Fatal("session markers were attributed to an unrelated version", w.Body.String())
	}
}

func TestBrowseUsesExperienceAndValidatesSpec(t *testing.T) {
	user := &frontend.Viewer{ID: 7, Username: "viewer"}
	title := frontend.Title{Key: 5, Type: "movie", Title: "A", Runtime: time.Minute}
	exp := frontendtest.Experience{ViewsFunc: func(context.Context, *frontend.Viewer) ([]frontend.View, error) {
		return []frontend.View{{ID: "1", Name: "Movies", Type: "movie"}}, nil
	}, ItemFunc: func(_ context.Context, req frontend.ItemRequest) (frontend.ItemDetail, error) {
		if req.Viewer != user || req.Key != 5 {
			return frontend.ItemDetail{}, errors.New("denied")
		}
		return frontend.ItemDetail{Title: title}, nil
	}, RowsFunc: func(_ context.Context, req frontend.RowRequest) ([]frontend.Row, error) {
		if req.Viewer != user {
			return nil, errors.New("denied")
		}
		return []frontend.Row{{Purpose: frontend.RowContinue, Items: []frontend.Title{title}}}, nil
	}}
	ident := frontendtest.Identity{ResolveFunc: func(_ context.Context, token string) (*frontend.Viewer, error) {
		if token != "valid" {
			return nil, errors.New("denied")
		}
		return user, nil
	}}
	inst, err := (Jellyfin{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: ident, Experience: exp})
	if err != nil {
		t.Fatal(err)
	}
	spec, err := openapi.CachedSpec("../../conform/specs/manifest.json", "jellyfin", "")
	var doc *openapi.Document
	if err == nil {
		doc, err = openapi.Parse(spec)
	}
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	for _, path := range []string{"/Items/" + ids.RatingKeyGUID(5), "/UserItems/Resume"} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", path, nil)
		r.Header.Set("X-Emby-Token", "valid")
		inst.Handler().ServeHTTP(w, r)
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"LocationType":"FileSystem"`) {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
		if doc != nil {
			if err := doc.Validate("GET", path, w.Code, w.Header().Get("Content-Type"), w.Body.Bytes()); err != nil {
				t.Errorf("%s: %v", path, err)
			}
		}
	}
}

func TestUserDataMutationIsViewerScoped(t *testing.T) {
	viewer := &frontend.Viewer{ID: 7, Username: "viewer"}
	var played bool
	exp := frontendtest.Experience{SetPlayedFunc: func(_ context.Context, v *frontend.Viewer, key frontend.RatingKey) error {
		if v != viewer || key != 5 {
			return errors.New("wrong scope")
		}
		played = true
		return nil
	}, UserDataFunc: func(_ context.Context, v *frontend.Viewer, keys []frontend.RatingKey) (map[frontend.RatingKey]frontend.WatchStatus, error) {
		if v != viewer || len(keys) != 1 || keys[0] != 5 {
			return nil, errors.New("wrong scope")
		}
		n := 0
		if played {
			n = 1
		}
		return map[frontend.RatingKey]frontend.WatchStatus{5: {ViewCount: n}}, nil
	}}
	ident := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }}
	inst, err := (Jellyfin{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: ident, Experience: exp})
	if err != nil {
		t.Fatal(err)
	}
	path := "/UserPlayedItems/" + ids.RatingKeyGUID(5)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", path, nil)
	r.Header.Set("X-Emby-Token", "valid")
	inst.Handler().ServeHTTP(w, r)
	if w.Code != 200 || !played || !strings.Contains(w.Body.String(), `"Played":true`) {
		t.Fatal(w.Code, w.Body.String())
	}
	spec, err := openapi.CachedSpec("../../conform/specs/manifest.json", "jellyfin", "")
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	doc, err := openapi.Parse(spec)
	if err != nil {
		t.Fatal(err)
	}
	if err = doc.Validate("POST", path, w.Code, w.Header().Get("Content-Type"), w.Body.Bytes()); err != nil {
		t.Fatal(err)
	}
	wrong, _ := ids.GUID(ids.User, 8)
	w = httptest.NewRecorder()
	r = httptest.NewRequest("POST", "/Users/"+wrong+"/PlayedItems/"+ids.RatingKeyGUID(5), nil)
	r.Header.Set("X-Emby-Token", "valid")
	inst.Handler().ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal(w.Code)
	}
}

func TestSearchHintsUsesExperience(t *testing.T) {
	viewer := &frontend.Viewer{ID: 1}
	exp := frontendtest.Experience{SearchFunc: func(_ context.Context, v *frontend.Viewer, _ frontend.Device, term string, q frontend.ItemQuery) (frontend.SearchPage, error) {
		if v != viewer || term != "search" || q.Offset != 1 || q.Limit != 2 {
			t.Fatal(v, term, q)
		}
		return frontend.SearchPage{Page: frontend.Page{Items: []frontend.Title{{Key: 5, Type: "movie", Title: "Found"}}, Total: 3}}, nil
	}}
	ident := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }}
	inst, err := (Jellyfin{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: ident, Experience: exp})
	if err != nil {
		t.Fatal(err)
	}
	path := "/Search/Hints?searchTerm=search&StartIndex=1&Limit=2"
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", path, nil)
	r.Header.Set("X-Emby-Token", "grant")
	inst.Handler().ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Found") {
		t.Fatal(w.Code, w.Body.String())
	}
	spec, err := openapi.CachedSpec("../../conform/specs/manifest.json", "jellyfin", "")
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	doc, err := openapi.Parse(spec)
	if err != nil {
		t.Fatal(err)
	}
	if err = doc.Validate("GET", path, w.Code, w.Header().Get("Content-Type"), w.Body.Bytes()); err != nil {
		t.Fatal(err)
	}
}

func TestLibraryQueriesUseNeutralFilters(t *testing.T) {
	viewer := &frontend.Viewer{ID: 2}
	called := false
	exp := frontendtest.Experience{LibraryItemsFunc: func(_ context.Context, v *frontend.Viewer, _ frontend.Device, id string, q frontend.ItemQuery) (frontend.Page, error) {
		called = true
		if v != viewer || id != "2" || q.Offset != 4 || q.Limit != 3 || !q.Recursive || len(q.Types) != 1 || q.Types[0] != "movie" || len(q.Filter.Genres) != 1 || q.Filter.Genres[0] != "Drama" || q.Filter.YearFrom != 2020 || q.Watched == nil || *q.Watched || q.Favorite == nil || !*q.Favorite {
			t.Fatal(id, q)
		}
		return frontend.Page{Items: []frontend.Title{{Key: 5, Type: "movie", Title: "Found"}}, Total: 9}, nil
	}}
	ident := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }}
	inst, err := (Jellyfin{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: ident, Experience: exp})
	if err != nil {
		t.Fatal(err)
	}
	id, _ := ids.GUID(ids.View, 2)
	path := "/Items?ParentId=" + id + "&Recursive=true&StartIndex=4&Limit=3&IncludeItemTypes=Movie&Genres=Drama&Years=2020&Filters=IsUnplayed,IsFavorite"
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", path, nil)
	r.Header.Set("X-Emby-Token", "grant")
	inst.Handler().ServeHTTP(w, r)
	if w.Code != 200 || !called || !strings.Contains(w.Body.String(), `"TotalRecordCount":9`) {
		t.Fatal(w.Code, w.Body.String())
	}
	spec, e := openapi.CachedSpec("../../conform/specs/manifest.json", "jellyfin", "")
	if os.IsNotExist(e) {
		return
	}
	if e != nil {
		t.Fatal(e)
	}
	doc, e := openapi.Parse(spec)
	if e != nil {
		t.Fatal(e)
	}
	if e = doc.Validate("GET", path, w.Code, w.Header().Get("Content-Type"), w.Body.Bytes()); e != nil {
		t.Fatal(e)
	}
}

func TestLikesUseAtomicExperienceMutation(t *testing.T) {
	viewer := &frontend.Viewer{ID: 1}
	called := false
	exp := frontendtest.Experience{SetLikesFunc: func(_ context.Context, v *frontend.Viewer, key frontend.RatingKey, like *bool) error {
		called = true
		if v != viewer || key != 5 || like == nil || *like {
			t.Fatal(v, key, like)
		}
		return nil
	}}
	ident := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }}
	inst, err := (Jellyfin{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: ident, Experience: exp})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/UserItems/"+ids.RatingKeyGUID(5)+"/Rating?likes=false", nil)
	r.Header.Set("X-Emby-Token", "grant")
	inst.Handler().ServeHTTP(w, r)
	if w.Code != 200 || !called {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestOnlyPresentedRowsReportImpressions(t *testing.T) {
	viewer := &frontend.Viewer{ID: 1}
	var handles []string
	exp := frontendtest.Experience{RowsFunc: func(context.Context, frontend.RowRequest) ([]frontend.Row, error) {
		return []frontend.Row{{Purpose: frontend.RowContinue, Handle: "first", Items: []frontend.Title{{Key: 1, Type: "movie"}}}, {Purpose: frontend.RowContinue, Handle: "second", Items: []frontend.Title{{Key: 2, Type: "movie"}}}}, nil
	}, ReportRowInteractionFunc: func(_ context.Context, r frontend.RowInteraction) error {
		if r.Viewer != viewer || r.Protocol != "jellyfin" || r.Kind != frontend.RowImpression {
			t.Fatal(r)
		}
		handles = append(handles, r.Handle)
		return nil
	}}
	ident := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }}
	inst, err := (Jellyfin{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: ident, Experience: exp})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/UserItems/Resume?StartIndex=1&Limit=1", nil)
	r.Header.Set("X-Emby-Token", "grant")
	inst.Handler().ServeHTTP(w, r)
	if w.Code != 200 || len(handles) != 1 || handles[0] != "second" {
		t.Fatal(w.Code, handles)
	}
}

func TestViewResponseMatchesSpec(t *testing.T) {
	ident := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return &frontend.Viewer{ID: 1}, nil }}
	exp := frontendtest.Experience{ViewsFunc: func(context.Context, *frontend.Viewer) ([]frontend.View, error) {
		return []frontend.View{{ID: "1", Name: "Movies", Type: "movie"}}, nil
	}}
	inst, err := (Jellyfin{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: ident, Experience: exp})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/UserViews", nil)
	r.Header.Set("X-Emby-Token", "grant")
	inst.Handler().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	spec, e := openapi.CachedSpec("../../conform/specs/manifest.json", "jellyfin", "")
	if os.IsNotExist(e) {
		return
	}
	if e != nil {
		t.Fatal(e)
	}
	doc, e := openapi.Parse(spec)
	if e != nil {
		t.Fatal(e)
	}
	if e = doc.Validate("GET", "/UserViews", w.Code, w.Header().Get("Content-Type"), w.Body.Bytes()); e != nil {
		t.Fatal(e)
	}
}
