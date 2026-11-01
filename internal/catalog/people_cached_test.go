package catalog_test

import (
	"context"
	"encoding/json"
	"github.com/braedonsaunders/ether/internal/config"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestCachedCreditItemsIncludesOnDemandDistinctAndBounds(t *testing.T) {
	var provider atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		provider.Add(1)
		http.Error(w, "unexpected provider request", 500)
	}))
	defer server.Close()
	c := newTestCatalog(t, server)
	ctx := context.Background()
	released := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	onDemand := core.Title{Key: core.MovieKey(601), Type: core.TypeMovie, Released: released, Cast: []core.Person{{TMDB: 30, Name: "On Demand"}}}
	listed := core.Title{Key: core.MovieKey(602), Type: core.TypeMovie, Released: released, Cast: []core.Person{{TMDB: 40, Name: "Listed"}}, Directors: []core.Person{{TMDB: 40, Name: "Listed"}}}
	empty := core.Title{Key: core.MovieKey(603), Type: core.TypeMovie, Released: released}
	for _, title := range []core.Title{onDemand, listed, empty} {
		if err := c.UpsertTitle(ctx, title, title.Key == listed.Key); err != nil {
			t.Fatal(err)
		}
	}
	first, err := c.CachedCreditItems(ctx, 0, 1)
	if err != nil || len(first) != 1 || first[0] != onDemand.Key {
		t.Fatalf("on-demand membership %+v %v", first, err)
	}
	next, err := c.CachedCreditItems(ctx, first[0], 1)
	if err != nil || len(next) != 1 || next[0] != listed.Key {
		t.Fatalf("distinct membership %+v %v", next, err)
	}
	tail, err := c.CachedCreditItems(ctx, next[0], 1)
	if err != nil || len(tail) != 0 {
		t.Fatalf("invented credit %+v %v", tail, err)
	}
	if _, err = c.CachedGet(ctx, onDemand.Key); err != nil {
		t.Fatal(err)
	}
	if provider.Load() != 0 {
		t.Fatal("cache-only membership reached provider")
	}
	for _, args := range [][2]int{{-1, 1}, {0, 0}, {0, 501}} {
		if _, err = c.CachedCreditItems(ctx, core.RatingKey(args[0]), args[1]); err == nil {
			t.Fatal("unbounded/invalid credit page accepted")
		}
	}
}

func TestCachedCreditTitlesUsesOwnedConfigAndTime(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("snapshot reached provider")
		http.Error(w, "unexpected", 500)
	}))
	defer server.Close()
	c := newTestCatalog(t, server)
	ctx := context.Background()
	key := core.MovieKey(604)
	title := core.Title{Key: key, Type: core.TypeMovie, Title: "Owned snapshot", Released: time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), Genres: []string{"Thriller"}, Cast: []core.Person{{TMDB: 30, Name: "Person"}}}
	if err := c.UpsertTitle(ctx, title, false); err != nil {
		t.Fatal(err)
	}
	// The old cached gate is allowed to omit extra entries. Install an open
	// catalog so this test specifically exercises the stricter owned snapshot.
	c.UpdateConfig(config.Catalog{})
	keys := []core.RatingKey{key}
	now := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	rows, err := c.CachedCreditTitles(ctx, keys, config.Catalog{}, now)
	if err != nil || len(rows) != 1 {
		t.Fatalf("open snapshot %+v %v", rows, err)
	}
	cfg := config.Catalog{Exclusions: []core.LibraryExclusion{{Name: "Blocked genre", MediaType: "movie", Match: "all", Rules: []core.CollectionRule{{Field: "genre", Op: "is", Value: json.RawMessage(`"Thriller"`)}}}}}
	rows, err = c.CachedCreditTitles(ctx, keys, cfg, now)
	if err != nil || len(rows) != 0 {
		t.Fatalf("leased exclusion ignored %+v %v", rows, err)
	}
	rows, err = c.CachedCreditTitles(ctx, keys, config.Catalog{GateMoviesByHomeRelease: true}, now)
	if err != nil || len(rows) != 0 {
		t.Fatalf("unknown home release exposed %+v %v", rows, err)
	}
	rows, err = c.CachedCreditTitles(ctx, keys, config.Catalog{}, now.AddDate(-2, 0, 0))
	if err != nil || len(rows) != 0 {
		t.Fatalf("live clock replaced captured release time %+v %v", rows, err)
	}
}
