package plex

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/personalize"
)

func hubRows(t *testing.T, body []byte) []struct {
	ID   string `json:"hubIdentifier"`
	Size int    `json:"size"`
} {
	t.Helper()
	var v struct {
		MediaContainer struct {
			Hub []struct {
				ID   string `json:"hubIdentifier"`
				Size int    `json:"size"`
			} `json:"Hub"`
		} `json:"MediaContainer"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatal(err)
	}
	return v.MediaContainer.Hub
}

func TestPromotedDropsEmptyRestrictedAndDuplicateHubs(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	blocked := fightClub
	blocked.Adult = true
	blocked.Title = "Not shown"
	f.srv.d.Recommender = &fakeRecommender{home: []core.Hub{
		{ID: "home.movies.recent", Title: "Recent movies", Promoted: true, Items: []core.Title{fightClub}},
		{ID: "home.movies.recent", Title: "Recent movies again", Promoted: true, Items: []core.Title{alien}},
		{ID: "home.blocked", Title: "Blocked", Promoted: true, Items: []core.Title{blocked}},
		{ID: "home.continue", Title: "Continue Watching", Promoted: true},
	}}

	for _, path := range []string{"/hubs", "/hubs/promoted"} {
		rows := hubRows(t, f.get(t, path, "Accept", "application/json", "X-Plex-Product", "Plex for Android", "X-Plex-Platform", "Android").Body.Bytes())
		seen := map[string]int{}
		for _, h := range rows {
			if h.Size == 0 {
				t.Errorf("%s: empty hub %s", path, h.ID)
			}
			seen[h.ID]++
		}
		if seen["home.movies.recent"] != 1 {
			t.Errorf("%s: home.movies.recent count = %d, rows %+v", path, seen["home.movies.recent"], rows)
		}
		if seen["home.blocked"] != 0 || seen["home.continue"] != 0 {
			t.Errorf("%s: empty rows survived: %+v", path, rows)
		}
	}
}

func TestSectionHubIdentifierOnce(t *testing.T) {
	rec := &contextualRecommender{fakeRecommender: fakeRecommender{
		section: map[core.MediaType][]core.Hub{
			core.TypeMovie: {{ID: "movie.recentlyadded.1", Title: "Recently Added again", Type: core.TypeMovie, Promoted: true, Items: []core.Title{alien}}},
		},
	}}
	f := newContextualFixture(t, rec)
	rows := hubRows(t, f.get(t, "/hubs/sections/1", "Accept", "application/json").Body.Bytes())
	n := 0
	for _, h := range rows {
		if h.ID == "movie.recentlyadded.1" {
			n++
		}
		if h.Size == 0 {
			t.Errorf("empty section hub %s", h.ID)
		}
	}
	if n != 1 {
		t.Fatalf("movie.recentlyadded.1 appears %d times: %+v", n, rows)
	}
}

func TestHomeRowOrderStableAcrossSessionAndCount(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	for i := int64(1); i <= 80; i++ {
		title := fightClub
		title.Key = core.MovieKey(10000 + i)
		title.Title = fmt.Sprintf("Comedy %d", i)
		title.Genres = []string{"Comedy"}
		title.Rating = 8.5
		title.VoteCount = 10000
		title.Popularity = 100
		f.catalog.titles[title.Key] = title
	}
	e, err := personalize.New(personalize.Options{DB: discoveryDB(t), Catalog: f.catalog, State: f.srv.d.State, RefreshEvery: -1, FlushEvery: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	f.srv.d.Recommender = e
	if err := e.Rebuild(context.Background(), &ownerUser); err != nil {
		t.Fatal(err)
	}
	headers := func(session string) []string {
		return []string{"Accept", "application/json", "X-Plex-Product", "Plex for Android (TV)", "X-Plex-Platform", "Android", "X-Plex-Device", "SHIELD Android TV", "X-Plex-Session-Identifier", session}
	}
	first := hubRows(t, f.get(t, "/hubs?count=12", headers("session-a")...).Body.Bytes())
	second := hubRows(t, f.get(t, "/hubs?count=4", headers("session-b")...).Body.Bytes())
	if len(first) == 0 || len(first) != len(second) {
		t.Fatalf("row counts %d and %d: %+v vs %+v", len(first), len(second), first, second)
	}
	for i := range first {
		if first[i].ID != second[i].ID || first[i].Size == 0 || second[i].Size == 0 {
			t.Fatalf("order changed or empty at %d: %+v vs %+v", i, first, second)
		}
	}
}
