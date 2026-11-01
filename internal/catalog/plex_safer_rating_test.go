package catalog

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/contentrating"
	"github.com/braedonsaunders/ether/internal/core"
	_ "github.com/braedonsaunders/ether/internal/plexguid"
	"github.com/braedonsaunders/ether/internal/plexmeta"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestBrowseSaferPlexRatingSQLAndGUIDMatch(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	c, err := New(db, nil, config.Catalog{}, nil, WithSaferRatingSQL(plexmeta.RatingSQL{}))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	c.SetNow(func() time.Time { return now })
	pg := core.Title{
		Key: core.MovieKey(551), Type: core.TypeMovie, IDs: core.IDs{TMDB: 551},
		Title: "Family Fun", SortTitle: "family fun", Year: 2020,
		Released: time.Date(2020, 6, 1, 0, 0, 0, 0, time.UTC), ContentRating: "PG", Popularity: 40,
	}
	remap := core.Title{
		Key: core.MovieKey(552), Type: core.TypeMovie, IDs: core.IDs{TMDB: 552},
		Title: "Remapped", SortTitle: "remapped", Year: 2021,
		Released: time.Date(2021, 6, 1, 0, 0, 0, 0, time.UTC), ContentRating: "PG", Popularity: 30,
	}
	for _, ti := range []core.Title{pg, remap} {
		if err := c.UpsertTitle(ctx, ti, true); err != nil {
			t.Fatal(err)
		}
	}
	payload, _ := json.Marshal(plexmeta.Overlay{ContentRating: "R"})
	level := int(contentrating.LevelOf("R"))
	if _, err := db.Exec(`INSERT INTO plexguid_mappings(rating_key,guid,checked_at) VALUES(?,?,?)`, int64(pg.Key), "plex://movie/aaaaaaaaaaaaaaaaaaaaaaaa", now.Unix()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO plexguid_mappings(rating_key,guid,checked_at) VALUES(?,?,?)`, int64(remap.Key), "plex://movie/bbbbbbbbbbbbbbbbbbbbbbbb", now.Unix()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO plexmeta_titles(rating_key,guid,payload,fetched_at,rating_level) VALUES(?,?,?,?,?)`, int64(pg.Key), "plex://movie/aaaaaaaaaaaaaaaaaaaaaaaa", string(payload), now.Unix(), level); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO plexmeta_titles(rating_key,guid,payload,fetched_at,rating_level) VALUES(?,?,?,?,?)`, int64(remap.Key), "plex://movie/000000000000000000000000", string(payload), now.Unix(), level); err != nil {
		t.Fatal(err)
	}

	plain, err := c.Browse(ctx, core.BrowseQuery{Filter: core.Filter{Type: core.TypeMovie, MaxContentRating: "PG"}, Limit: 50})
	if err != nil || len(plain.Items) != 2 {
		t.Fatalf("TMDB-only browse: %d %v", len(plain.Items), err)
	}
	safer, err := c.Browse(ctx, core.BrowseQuery{Filter: core.Filter{Type: core.TypeMovie, MaxContentRating: "PG", SaferPlexRating: true}, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, it := range safer.Items {
		got[it.Title] = true
	}
	if got["Family Fun"] {
		t.Fatal("matching Plex R overlay still visible to PG")
	}
	if !got["Remapped"] {
		t.Fatal("guid-mismatched overlay hid a remapped title")
	}
}
