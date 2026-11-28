package mediabrowser

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
)

// Episodes whose series cannot be resolved must not each trigger their own
// series lookup after the page was already filled in one batch.
func TestNextUpSeriesLookupIsBatched(t *testing.T) {
	show := core.ShowKey(5)
	var titles []frontend.Title
	for n := 1; n <= 6; n++ {
		titles = append(titles, frontend.Title{Key: core.EpisodeKey(5, 1, n), Type: "episode", Title: "Ep", Season: 1, Episode: n})
	}
	showLookups := 0
	exp := frontendtest.Experience{RowsFunc: func(context.Context, frontend.RowRequest) ([]frontend.Row, error) {
		return []frontend.Row{{Purpose: frontend.RowContinue, Items: titles}}, nil
	}, ItemFunc: func(_ context.Context, req frontend.ItemRequest) (frontend.ItemDetail, error) {
		if req.Key == show {
			showLookups++
			return frontend.ItemDetail{}, errors.New("series unavailable")
		}
		for _, title := range titles {
			if title.Key == req.Key {
				return frontend.ItemDetail{Title: title}, nil
			}
		}
		return frontend.ItemDetail{}, errors.New("unknown")
	}, UserDataFunc: func(context.Context, *frontend.Viewer, []frontend.RatingKey) (map[frontend.RatingKey]frontend.WatchStatus, error) {
		return map[frontend.RatingKey]frontend.WatchStatus{}, nil
	}}
	inst := mountJellyfin(t, exp)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/Shows/NextUp", nil)
	r.Header.Set("X-Emby-Token", "grant")
	inst.Handler().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if showLookups > 2 {
		t.Fatalf("series looked up %d times for one page, want at most 2 batched fills", showLookups)
	}
}
