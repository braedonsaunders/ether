package viewerpolicy_test

import (
	"context"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/catalog"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/braedonsaunders/ether/internal/viewerpolicy"
)

// A metadata refresh that changes nothing a viewer is authorized to see
// (popularity, votes, the same genres and cast) leaves the policy epoch
// alone, so a home page being built is not failed by background refreshes;
// a rating change still advances it.
func TestCatalogRefreshLeavesPolicyEpochAlone(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c, err := catalog.New(db, nil, config.Catalog{}, nil, catalog.WithNow(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	title := core.Title{Key: core.MovieKey(1), Type: core.TypeMovie, IDs: core.IDs{TMDB: 1}, Title: "M", ContentRating: "PG", Released: now.AddDate(-1, 0, 0),
		Popularity: 10, VoteCount: 100, Genres: []string{"Drama"}, Countries: []string{"US"}, Cast: []core.Person{{TMDB: 7, Name: "Actor"}}}
	if err := c.UpsertTitle(ctx, title, true); err != nil {
		t.Fatal(err)
	}
	before, err := viewerpolicy.Read(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	refreshed := title
	refreshed.Popularity, refreshed.VoteCount, refreshed.Summary = 55.5, 180, "A new synopsis."
	if err := c.UpsertTitle(ctx, refreshed, true); err != nil {
		t.Fatal(err)
	}
	if after, _ := viewerpolicy.Read(ctx, db); after != before {
		t.Fatalf("a refresh with no policy change advanced the epoch %d -> %d", before, after)
	}
	rerated := refreshed
	rerated.ContentRating = "R"
	if err := c.UpsertTitle(ctx, rerated, true); err != nil {
		t.Fatal(err)
	}
	if after, _ := viewerpolicy.Read(ctx, db); after == before {
		t.Fatal("a rating change did not advance the epoch")
	}
}
