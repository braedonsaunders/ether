package silo

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

func TestSeasonEpisodesPageInsteadOfRefusing(t *testing.T) {
	viewer := &frontend.Viewer{ID: 7}
	show := frontend.RatingKey(core.ShowKey(91759))
	var gotLimit, gotOffset int
	exp := frontendtest.Experience{ChildrenFunc: func(_ context.Context, _ *frontend.Viewer, _ frontend.Device, key frontend.RatingKey, q frontend.ItemQuery) (core.Page, error) {
		if key != show || q.Season == nil || *q.Season != 1 || !q.Recursive {
			t.Fatalf("query %+v %s", q, key)
		}
		gotLimit, gotOffset = q.Limit, q.Offset
		start := q.Offset + 1
		return core.Page{Items: []core.Title{{Key: core.EpisodeKey(91759, 1, start), Type: core.TypeEpisode, Season: 1, Episode: start, Title: "Lo and Behold"}}, Total: 2868}, nil
	}}
	inst, err := (Frontend{MachineID: "test"}).Mount(context.Background(), frontend.Host{
		Identity:   testDeviceIdentity(frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }}, viewer),
		Experience: exp,
	})
	if err != nil {
		t.Fatal(err)
	}
	call := func(raw string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, raw, nil)
		r.Header.Set("Authorization", "Bearer grant")
		r.Header.Set("X-Silo-Device-Id", "tv")
		w := httptest.NewRecorder()
		inst.Handler().ServeHTTP(w, r)
		return w
	}
	id := ids.RatingKeyGUID(show)
	first := call("/api/v2/catalog/series/" + id + "/seasons/1/episodes")
	var body map[string]any
	if first.Code != 200 || json.Unmarshal(first.Body.Bytes(), &body) != nil || gotLimit != 50 || gotOffset != 0 {
		t.Fatalf("first %d limit %d offset %d %s", first.Code, gotLimit, gotOffset, first.Body.String())
	}
	page, _ := body["page"].(map[string]any)
	if page["has_more"] != true || page["next_cursor"] != "eyJvZmZzZXQiOjUwfQ" || !strings.Contains(first.Body.String(), `"title":"Lo and Behold"`) {
		t.Fatalf("page %#v %s", page, first.Body.String())
	}
	next := call("/api/v2/catalog/series/" + id + "/seasons/1/episodes?cursor=" + page["next_cursor"].(string) + "&limit=50")
	if next.Code != 200 || gotOffset != 50 || gotLimit != 50 || !strings.Contains(next.Body.String(), `"has_more":true`) {
		t.Fatalf("next %d offset %d %s", next.Code, gotOffset, next.Body.String())
	}
	decimal := call("/api/v2/catalog/series/" + id + "/seasons/1/episodes?cursor=100&limit=50")
	if decimal.Code != 200 || gotOffset != 100 {
		t.Fatalf("decimal %d offset %d", decimal.Code, gotOffset)
	}
	if w := call("/api/v2/catalog/series/" + id + "/seasons/1/episodes?limit=1000"); w.Code != 400 {
		t.Fatalf("limit %d", w.Code)
	}
	if w := call("/api/v2/catalog/series/" + id + "/seasons/1/episodes?cursor=nope"); w.Code != 400 {
		t.Fatalf("cursor %d", w.Code)
	}
}
