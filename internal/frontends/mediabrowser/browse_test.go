package mediabrowser

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/conform/openapi"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

func TestBrowseUsesExperienceAndValidatesSpec(t *testing.T) {
	user := &frontend.Viewer{ID: 7, Username: "viewer"}
	title := frontend.Title{Key: 5, Type: "movie", Title: "A", Runtime: time.Minute}
	exp := frontendtest.Experience{ItemFunc: func(_ context.Context, req frontend.ItemRequest) (frontend.ItemDetail, error) {
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
