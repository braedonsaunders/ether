package silo

import (
	"context"
	"encoding/json"
	"errors"
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

func TestSeasonPageNeverSubstitutesAnotherSeason(t *testing.T) {
	viewer := &frontend.Viewer{ID: 7}
	show := frontend.RatingKey(core.ShowKey(91759))
	id := ids.RatingKeyGUID(show)
	mount := func(children frontendtest.Experience) http.Handler {
		t.Helper()
		inst, err := (Frontend{MachineID: "test"}).Mount(context.Background(), frontend.Host{
			Identity:   testDeviceIdentity(frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }}, viewer),
			Experience: children,
		})
		if err != nil {
			t.Fatal(err)
		}
		return inst.Handler()
	}
	call := func(h http.Handler, raw string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, raw, nil)
		r.Header.Set("Authorization", "Bearer grant")
		r.Header.Set("X-Silo-Device-Id", "tv")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	foreign := mount(frontendtest.Experience{ChildrenFunc: func(context.Context, *frontend.Viewer, frontend.Device, frontend.RatingKey, frontend.ItemQuery) (core.Page, error) {
		return core.Page{Items: []core.Title{{Key: core.EpisodeKey(91759, 11, 1), Type: core.TypeEpisode, Season: 11, Episode: 1, Title: "Later"}}, Total: 102}, nil
	}})
	wrong := call(foreign, "/api/v2/catalog/series/"+id+"/seasons/1/episodes")
	if wrong.Code != 200 || !strings.Contains(wrong.Body.String(), `"items":[]`) || strings.Contains(wrong.Body.String(), "Later") {
		t.Fatalf("foreign season %d %s", wrong.Code, wrong.Body.String())
	}
	failed := mount(frontendtest.Experience{ChildrenFunc: func(context.Context, *frontend.Viewer, frontend.Device, frontend.RatingKey, frontend.ItemQuery) (core.Page, error) {
		return core.Page{}, errors.New("season unavailable")
	}})
	down := call(failed, "/api/v2/catalog/series/"+id+"/seasons/1/episodes")
	if down.Code != 200 || !strings.Contains(down.Body.String(), `"items":[]`) {
		t.Fatalf("failed season %d %s", down.Code, down.Body.String())
	}
}

func TestSeasonEpisodeCountAndStills(t *testing.T) {
	viewer := &frontend.Viewer{ID: 7}
	show := frontend.RatingKey(core.ShowKey(91759))
	season := core.Title{Key: core.SeasonKey(91759, 1), Type: core.TypeSeason, Season: 1, Title: "Season 1", ChildCount: 7, LeafCount: 0}
	episode := core.Title{Key: core.EpisodeKey(91759, 1, 1), Type: core.TypeEpisode, Season: 1, Episode: 1, Title: "Street", StillPath: "/still.jpg"}
	exp := frontendtest.Experience{ChildrenFunc: func(_ context.Context, _ *frontend.Viewer, _ frontend.Device, _ frontend.RatingKey, q frontend.ItemQuery) (core.Page, error) {
		if q.Season != nil {
			return core.Page{Items: []core.Title{episode}, Total: 1}, nil
		}
		return core.Page{Items: []core.Title{season}, Total: 1}, nil
	}, UserDataFunc: func(context.Context, *frontend.Viewer, []frontend.RatingKey) (map[frontend.RatingKey]frontend.WatchStatus, error) {
		return map[frontend.RatingKey]frontend.WatchStatus{}, nil
	}}
	grants := pageImageSigner{sign: func(_ context.Context, req frontend.ImagePageRequest) ([]frontend.ImageBatchItem, error) {
		if len(req.Titles) != 1 || req.Titles[0].Key != episode.Key {
			return nil, nil
		}
		for _, kind := range req.Kinds {
			if kind == frontend.ImageStill {
				return []frontend.ImageBatchItem{{Item: episode.Key, Authoritative: true, Images: []frontend.GrantedImage{{Image: frontend.Image{Kind: frontend.ImageStill, Path: "/still.jpg"}, Secret: "still-token"}}}}, nil
			}
		}
		t.Fatal("episode card did not request a still")
		return nil, nil
	}}
	inst, err := (Frontend{MachineID: "test"}).Mount(context.Background(), frontend.Host{
		Identity:    testDeviceIdentity(frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }}, viewer),
		Experience:  exp,
		ImageGrants: grants,
	})
	if err != nil {
		t.Fatal(err)
	}
	id := ids.RatingKeyGUID(show)
	call := func(raw string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, raw, nil)
		r.Header.Set("Authorization", "Bearer grant")
		r.Header.Set("X-Silo-Device-Id", "tv")
		w := httptest.NewRecorder()
		inst.Handler().ServeHTTP(w, r)
		return w
	}
	seasons := call("/api/v2/catalog/series/" + id + "/seasons")
	if seasons.Code != 200 || !strings.Contains(seasons.Body.String(), `"episode_count":7`) {
		t.Fatalf("seasons %d %s", seasons.Code, seasons.Body.String())
	}
	episodes := call("/api/v2/catalog/series/" + id + "/seasons/1/episodes")
	if episodes.Code != 200 || !strings.Contains(episodes.Body.String(), `"still_url":"/api/v2/images/still-token"`) || !strings.Contains(episodes.Body.String(), `"poster_url":"/api/v2/images/still-token"`) || !strings.Contains(episodes.Body.String(), `"poster_is_episode_still":true`) {
		t.Fatalf("episodes %d %s", episodes.Code, episodes.Body.String())
	}
}
