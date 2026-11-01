package mediabrowser

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

func TestShowsNextUpHonoursSeriesId(t *testing.T) {
	series := core.ShowKey(10)
	other := core.ShowKey(99)
	first := frontend.Title{Key: core.EpisodeKey(10, 1, 1), Type: "episode", Title: "Kick Off", ShowKey: series, Season: 1, Episode: 1, ShowTitle: "Match of the Day"}
	second := frontend.Title{Key: core.EpisodeKey(10, 1, 2), Type: "episode", Title: "Second Half", ShowKey: series, Season: 1, Episode: 2, ShowTitle: "Match of the Day"}
	special := frontend.Title{Key: core.EpisodeKey(10, 0, 1), Type: "episode", Title: "Preview", ShowKey: series, Season: 0, Episode: 1, ShowTitle: "Match of the Day"}
	lupin := frontend.Title{Key: core.EpisodeKey(99, 1, 2), Type: "episode", Title: "Chapter 2", ShowKey: other, Season: 1, Episode: 2, ShowTitle: "Lupin"}
	episodes := []frontend.Title{special, first, second}

	for _, dialect := range []string{"jellyfin", "emby"} {
		for _, tc := range []struct {
			name  string
			query string
			state map[frontend.RatingKey]frontend.WatchStatus
			want  string
		}{
			{name: "no history", query: "seriesId=" + ids.RatingKeyGUID(series) + "&limit=1", want: first.Title},
			{name: "SeriesId", query: "SeriesId=" + ids.RatingKeyGUID(series) + "&Limit=1", want: first.Title},
			{name: "parentId", query: "parentId=" + ids.RatingKeyGUID(series) + "&limit=1", want: first.Title},
			{name: "partial history", query: "seriesId=" + ids.RatingKeyGUID(series) + "&limit=1", state: map[frontend.RatingKey]frontend.WatchStatus{first.Key: {ViewCount: 1}, lupin.Key: {ViewCount: 1}}, want: second.Title},
			{name: "EnableResumable false", query: "seriesId=" + ids.RatingKeyGUID(series) + "&limit=1&EnableResumable=false", state: map[frontend.RatingKey]frontend.WatchStatus{first.Key: {ViewCount: 1}, lupin.Key: {ViewCount: 1, ViewOffset: 1}}, want: second.Title},
		} {
			t.Run(dialect+"/"+tc.name, func(t *testing.T) {
				var rowsCalled bool
				var childKey frontend.RatingKey
				var childQuery frontend.ItemQuery
				exp := frontendtest.Experience{
					RowsFunc: func(context.Context, frontend.RowRequest) ([]frontend.Row, error) {
						rowsCalled = true
						return []frontend.Row{{Purpose: frontend.RowNextUp, Items: []frontend.Title{lupin}}}, nil
					},
					ChildrenFunc: func(_ context.Context, _ *frontend.Viewer, _ frontend.Device, key frontend.RatingKey, q frontend.ItemQuery) (core.Page, error) {
						childKey, childQuery = key, q
						if key != series {
							t.Fatalf("children key %v", key)
						}
						return core.Page{Items: append([]frontend.Title{lupin}, episodes...), Total: 4}, nil
					},
					UserDataFunc: func(_ context.Context, _ *frontend.Viewer, keys []frontend.RatingKey) (map[frontend.RatingKey]frontend.WatchStatus, error) {
						st := map[frontend.RatingKey]frontend.WatchStatus{}
						for k, v := range tc.state {
							st[k] = v
						}
						for _, key := range keys {
							if _, ok := st[key]; !ok {
								st[key] = frontend.WatchStatus{}
							}
						}
						return st, nil
					},
					ItemFunc: func(_ context.Context, req frontend.ItemRequest) (frontend.ItemDetail, error) {
						for _, title := range episodes {
							if title.Key == req.Key {
								return frontend.ItemDetail{Title: title}, nil
							}
						}
						if req.Key == series {
							return frontend.ItemDetail{Title: frontend.Title{Key: series, Type: "show", Title: "Match of the Day"}}, nil
						}
						return frontend.ItemDetail{}, core.ErrNotFound
					},
				}
				inst := mountDialect(t, dialect, exp)
				w := httptest.NewRecorder()
				r := httptest.NewRequest(http.MethodGet, "/Shows/NextUp?"+tc.query, nil)
				r.Header.Set("X-Emby-Token", "grant")
				inst.Handler().ServeHTTP(w, r)
				var body struct {
					Items []map[string]any
				}
				if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &body) != nil || len(body.Items) != 1 || body.Items[0]["Name"] != tc.want {
					t.Fatalf("%s: %d %s", tc.name, w.Code, w.Body.String())
				}
				if body.Items[0]["SeriesName"] != "Match of the Day" {
					t.Fatalf("series name %v", body.Items[0]["SeriesName"])
				}
				if rowsCalled {
					t.Fatal("global next-up row used for a series-scoped request")
				}
				if childKey != series || !childQuery.Recursive || childQuery.Limit != experience.SeriesNextUpLimit || len(childQuery.Types) != 1 || childQuery.Types[0] != core.TypeEpisode {
					t.Fatalf("children not series-indexed: key=%v q=%+v", childKey, childQuery)
				}
			})
		}
	}
}

func mountDialect(t *testing.T, protocol string, exp frontendtest.Experience) frontend.Instance {
	t.Helper()
	ident := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) {
		return &frontend.Viewer{ID: 1}, nil
	}}
	var front frontend.Frontend = Jellyfin{MachineID: "test"}
	if protocol == "emby" {
		front = Emby{MachineID: "test"}
	}
	inst, err := front.Mount(context.Background(), frontend.Host{Identity: ident, Experience: exp})
	if err != nil {
		t.Fatal(err)
	}
	return inst
}
