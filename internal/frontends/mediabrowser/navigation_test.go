package mediabrowser

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

func TestNavigationQueryUsesPolicyFilteredAnchors(t *testing.T) {
	anchor := ids.RatingKeyGUID(5)
	for _, tc := range []struct {
		query           string
		allowStart      bool
		adjacent, start frontend.RatingKey
		fail            bool
	}{
		{"adjacentTo=" + anchor, false, 5, 0, false},
		{"startItemId=" + anchor, true, 0, 5, false},
		{"startItemId=" + anchor, false, 0, 0, true},
		{"adjacentTo=" + anchor + "&adjacentTo=" + anchor, true, 0, 0, true},
		{"adjacentTo=" + anchor + "&startItemId=" + anchor, true, 0, 0, true},
		{"adjacentTo=invalid", true, 0, 0, true},
	} {
		q := frontend.ItemQuery{}
		err := navigationQuery(httptest.NewRequest("GET", "/Items?"+tc.query, nil), &q, tc.allowStart)
		if (err != nil) != tc.fail || q.AdjacentTo != tc.adjacent || q.StartAt != tc.start {
			t.Fatalf("%s: q=%+v err=%v", tc.query, q, err)
		}
	}
}

func TestNavigationReturnsNotFoundForInaccessibleAnchor(t *testing.T) {
	viewer := &frontend.Viewer{ID: 1}
	missing := fmt.Errorf("anchor: %w", frontend.ErrNotFound)
	exp := frontendtest.Experience{
		LibraryItemsFunc: func(context.Context, *frontend.Viewer, frontend.Device, string, frontend.ItemQuery) (frontend.Page, error) {
			return frontend.Page{}, missing
		},
		ChildrenFunc: func(context.Context, *frontend.Viewer, frontend.Device, frontend.RatingKey, frontend.ItemQuery) (frontend.Page, error) {
			return frontend.Page{}, missing
		},
	}
	identity := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }}
	inst, err := (Jellyfin{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: identity, Experience: exp})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/Items?AdjacentTo=" + ids.RatingKeyGUID(5), "/Shows/" + ids.RatingKeyGUID(7) + "/Episodes?StartItemId=" + ids.RatingKeyGUID(5)} {
		r := httptest.NewRequest("GET", path, nil)
		r.Header.Set("X-Emby-Token", "grant")
		w := httptest.NewRecorder()
		inst.Handler().ServeHTTP(w, r)
		if w.Code != http.StatusNotFound {
			t.Fatal(path, w.Code, w.Body.String())
		}
	}
}

func TestNavigationRoutesDelegateToExperience(t *testing.T) {
	viewer := &frontend.Viewer{ID: 1}
	var query frontend.ItemQuery
	exp := frontendtest.Experience{
		LibraryItemsFunc: func(_ context.Context, v *frontend.Viewer, _ frontend.Device, _ string, q frontend.ItemQuery) (frontend.Page, error) {
			if v != viewer {
				t.Fatal("wrong viewer")
			}
			query = q
			return frontend.Page{}, nil
		},
		ChildrenFunc: func(_ context.Context, v *frontend.Viewer, _ frontend.Device, key frontend.RatingKey, q frontend.ItemQuery) (frontend.Page, error) {
			if v != viewer || key != 7 {
				t.Fatal("wrong scope", key)
			}
			query = q
			return frontend.Page{}, nil
		},
	}
	identity := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }}
	inst, err := (Jellyfin{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: identity, Experience: exp})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		path            string
		adjacent, start frontend.RatingKey
		recursive       bool
		status          int
	}{
		{"/Items?AdjacentTo=" + ids.RatingKeyGUID(5), 5, 0, false, http.StatusOK},
		{"/Shows/" + ids.RatingKeyGUID(7) + "/Episodes?AdjacentTo=" + ids.RatingKeyGUID(5), 5, 0, true, http.StatusOK},
		{"/Shows/" + ids.RatingKeyGUID(7) + "/Episodes?StartItemId=" + ids.RatingKeyGUID(5), 0, 5, true, http.StatusOK},
		{"/Items?AdjacentTo=invalid", 0, 0, false, http.StatusBadRequest},
	} {
		query = frontend.ItemQuery{}
		r := httptest.NewRequest("GET", tc.path, nil)
		r.Header.Set("X-Emby-Token", "grant")
		w := httptest.NewRecorder()
		inst.Handler().ServeHTTP(w, r)
		if w.Code != tc.status || query.AdjacentTo != tc.adjacent || query.StartAt != tc.start || query.Recursive != tc.recursive {
			t.Fatalf("%s: %d %+v", tc.path, w.Code, query)
		}
	}
}
