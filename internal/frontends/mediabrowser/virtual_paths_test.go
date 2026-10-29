package mediabrowser

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

func TestVirtualPathsBatchUsesAuthorizedHostResults(t *testing.T) {
	viewer := &frontend.Viewer{ID: 7}
	calls := 0
	host := frontendtest.VirtualPaths{PathsFunc: func(_ context.Context, v *frontend.Viewer, _ frontend.Device, keys []frontend.RatingKey) (map[frontend.RatingKey]string, error) {
		calls++
		if v != viewer || len(keys) > 50 {
			t.Fatal(v, len(keys))
		}
		out := map[frontend.RatingKey]string{}
		for _, key := range keys {
			if key != 2 {
				out[key] = fmt.Sprintf("Movies/Movie [%d]/Movie.mkv", key)
			}
		}
		out[1] = "../unauthorized"
		return out, nil
	}}
	i := &instance{host: frontend.Host{VirtualPaths: host}}
	keys := make([]frontend.RatingKey, 51)
	for n := range keys {
		keys[n] = frontend.RatingKey(n + 1)
	}
	got := i.virtualPaths(httptest.NewRequest("GET", "/Items", nil), viewer, keys)
	if calls != 2 || got[1] != "" || got[2] != "" || got[51] != "/Movies/Movie [51]/Movie.mkv" {
		t.Fatal(calls, got)
	}
}

func TestVirtualPathsAppearOnlyForAuthorizedItems(t *testing.T) {
	viewer := &frontend.Viewer{ID: 7}
	title := frontend.Title{Key: 5, Type: "movie", Title: "Movie"}
	exp := frontendtest.Experience{ItemFunc: func(context.Context, frontend.ItemRequest) (frontend.ItemDetail, error) {
		return frontend.ItemDetail{Title: title}, nil
	}, RowsFunc: func(context.Context, frontend.RowRequest) ([]frontend.Row, error) {
		return []frontend.Row{{Purpose: frontend.RowContinue, Items: []frontend.Title{title}}}, nil
	}}
	paths := frontendtest.VirtualPaths{PathsFunc: func(_ context.Context, v *frontend.Viewer, _ frontend.Device, keys []frontend.RatingKey) (map[frontend.RatingKey]string, error) {
		if v != viewer || len(keys) != 1 || keys[0] != 5 {
			t.Fatal(v, keys)
		}
		return map[frontend.RatingKey]string{5: "Movies/Movie/Movie.mkv"}, nil
	}}
	identity := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }}
	inst, err := (Jellyfin{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: identity, Experience: exp, VirtualPaths: paths})
	if err != nil {
		t.Fatal(err)
	}
	for _, route := range []string{"/Items/" + ids.RatingKeyGUID(5), "/UserItems/Resume"} {
		r := httptest.NewRequest("GET", route, nil)
		r.Header.Set("X-Emby-Token", "grant")
		w := httptest.NewRecorder()
		inst.Handler().ServeHTTP(w, r)
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"Path":"/Movies/Movie/Movie.mkv"`) {
			t.Fatal(route, w.Code, w.Body.String())
		}
	}
}
