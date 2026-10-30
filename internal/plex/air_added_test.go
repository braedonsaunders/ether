package plex

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/plex/model"
)

func TestRecentlyAddedUsesAirDerivedDatesAcrossSections(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	movie := core.Title{Key: core.MovieKey(9001), Type: core.TypeMovie, Title: "Movie", Released: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), UpdatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	show := core.Title{Key: core.ShowKey(9002), Type: core.TypeShow, Title: "Show", Released: time.Date(2004, 1, 1, 0, 0, 0, 0, time.UTC), AddedAt: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), UpdatedAt: time.Date(2022, 1, 1, 0, 0, 0, 0, time.UTC)}
	f.catalog.mu.Lock()
	f.catalog.titles = map[core.RatingKey]core.Title{movie.Key: movie, show.Key: show}
	f.catalog.mu.Unlock()
	rec := f.get(t, "/hubs/home/recentlyAdded", "Accept", "application/json")
	var body struct{ MediaContainer model.MediaContainer }
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	items := body.MediaContainer.Metadata
	if rec.Code != 200 || len(items) != 2 || items[0].RatingKey != show.Key.String() || items[0].AddedAt != show.AddedAt.Unix() || items[0].UpdatedAt != show.UpdatedAt.Unix() || items[1].AddedAt != movie.Released.Unix() || items[1].UpdatedAt != movie.UpdatedAt.Unix() {
		t.Fatalf("recent order or dates: %s", rec.Body.String())
	}
	if sort, _ := sortFor("updatedAt", "desc"); sort != core.SortUpdated {
		t.Fatalf("updatedAt sort changed to %q", sort)
	}
}
