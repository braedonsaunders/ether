package mediabrowser

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

func TestShowListsUseSeasonIDAndDoNotCapAtFifty(t *testing.T) {
	viewer := &frontend.Viewer{ID: 1}
	show := core.ShowKey(42)
	seasonKey := core.SeasonKey(42, 2)
	var got frontend.ItemQuery
	exp := frontendtest.Experience{ChildrenFunc: func(_ context.Context, v *frontend.Viewer, _ frontend.Device, key frontend.RatingKey, q frontend.ItemQuery) (core.Page, error) {
		if v != viewer {
			t.Fatal(v)
		}
		// Unplayed counts on a season row ask for that season's children.
		// The list query itself is always the show.
		if key != show {
			if key != seasonKey || q.Limit != 0 {
				t.Fatal(key, q.Limit)
			}
			return core.Page{}, nil
		}
		got = q
		item := frontend.Title{Key: core.EpisodeKey(42, 2, 1), Type: "episode", Title: "Second season", ShowTitle: "Corrie", Season: 2, Episode: 1}
		if len(q.Types) == 0 {
			item = frontend.Title{Key: seasonKey, Type: "season", Title: "Season 2", ShowTitle: "Corrie", Season: 2}
		}
		return core.Page{Items: []frontend.Title{item}, Total: 102}, nil
	}}
	ident := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }}
	inst, err := (Jellyfin{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: ident, Experience: exp})
	if err != nil {
		t.Fatal(err)
	}
	call := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set("X-Emby-Token", "grant")
		inst.Handler().ServeHTTP(w, r)
		return w
	}
	showID := ids.RatingKeyGUID(show)
	seasonID := ids.RatingKeyGUID(seasonKey)
	eps := call("/Shows/" + showID + "/Episodes?seasonId=" + seasonID)
	if eps.Code != 200 || got.Season == nil || *got.Season != 2 || got.Limit != showChildPageMax || !strings.Contains(eps.Body.String(), "Second season") || !strings.Contains(eps.Body.String(), `"TotalRecordCount":102`) {
		t.Fatal(eps.Code, got.Season, got.Limit, eps.Body.String())
	}
	other := call("/Shows/" + ids.RatingKeyGUID(core.ShowKey(99)) + "/Episodes?seasonId=" + seasonID)
	if other.Code != 200 || !strings.Contains(other.Body.String(), `"TotalRecordCount":0`) || strings.Contains(other.Body.String(), "Second season") {
		t.Fatal(other.Code, other.Body.String())
	}
	limited := call("/Shows/" + showID + "/Episodes?season=2&Limit=50")
	if limited.Code != 200 || got.Season == nil || *got.Season != 2 || got.Limit != 50 {
		t.Fatal(got.Season, got.Limit, limited.Body.String())
	}
	seasons := call("/Shows/" + showID + "/Seasons")
	if seasons.Code != 200 || got.Limit != showChildPageMax || !strings.Contains(seasons.Body.String(), "Season 2") || !strings.Contains(seasons.Body.String(), `"TotalRecordCount":102`) {
		t.Fatal(got.Limit, seasons.Code, seasons.Body.String())
	}
}
