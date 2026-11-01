package collections

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/catalog"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

// The catalogue index's first build reads the whole catalogue. A home does
// not wait it out: it skips the live row that needs the index and shows it
// once the index is ready. A collection listing waits only briefly.
func TestHomeDoesNotWaitForTheFirstIndexBuild(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	now := time.Now()
	cat, err := catalog.New(db, nil, config.Catalog{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := cat.UpsertTitle(ctx, core.Title{Key: core.MovieKey(1), Type: core.TypeMovie, Title: "Local", Year: 2000, Released: now.AddDate(-10, 0, 0), Genres: []string{"Thriller"}, Popularity: 10}, true); err != nil {
		t.Fatal(err)
	}
	s, err := New(db, cat, nil, WithNow(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	genre, _ := json.Marshal("Thriller")
	if _, err := s.CreateSpec(ctx, core.CollectionSpec{Name: "Live Thrills", MediaType: "movie", Sources: []core.CollectionSource{{Kind: "catalog"}}, Match: "all",
		Rules: []core.CollectionRule{{Field: "genre", Op: "is", Value: genre}}, Sort: "popularity", SortDesc: true, Refresh: RefreshLive, Enabled: true, LocalOnly: true,
		Targets: core.CollectionTargets{HomeRow: true, CollectionsTab: true, Users: core.AllUsers()}}); err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	s.idx = newIndex(db, s.now, time.Minute)
	s.idx.beforeLoad = func() { <-release }
	owner := &core.User{ID: 1, IsOwner: true}
	titles := func() map[string]bool {
		t.Helper()
		rows, err := s.HomeRowsFor(ctx, owner, core.HubContext{})
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]bool{}
		for _, r := range rows {
			out[r.Hub.Title] = true
		}
		return out
	}
	start := time.Now()
	if titles()["Live Thrills"] {
		t.Fatal("the live row was evaluated before the index was built")
	}
	if _, err := s.ListSpecs(ctx); err != nil {
		t.Fatal(err)
	}
	if waited := time.Since(start); waited > homeIndexWait+listIndexWait+time.Second {
		t.Fatalf("home and listing waited %s for the first index build", waited)
	}
	close(release)
	deadline := time.Now().Add(10 * time.Second)
	for s.idx.current() == nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !titles()["Live Thrills"] {
		t.Fatal("the live row is missing once the index is ready")
	}
}
