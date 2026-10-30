package catalog_test

import (
	"context"
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
