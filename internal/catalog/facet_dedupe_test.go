package catalog

import (
	"context"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/braedonsaunders/ether/internal/tmdb"
)

func TestKeywordNamesCollapseBothLists(t *testing.T) {
	got := keywordNames(&tmdb.Keywords{
		Keywords: []tmdb.Keyword{{Name: "Drama"}, {Name: "Mystery"}, {Name: "Drama"}},
		Results:  []tmdb.Keyword{{Name: "Mystery"}, {Name: "Soap"}, {Name: "Soap"}},
	})
	want := []string{"Drama", "Mystery", "Soap"}
	if len(got) != len(want) {
		t.Fatal(got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatal(got)
		}
	}
}

func TestHydrateAndVisibilityDoNotRepeatFacets(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	c, err := New(db, nil, config.Catalog{Exclusions: []core.LibraryExclusion{{
		Name: "hide missing genre", MediaType: "movie", Match: "all", Action: "hide",
		Rules: []core.CollectionRule{{Field: "genre", Op: "is", Value: []byte(`"Missing"`)}},
	}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	released := time.Date(2020, 1, 2, 0, 0, 0, 0, time.UTC)
	key := core.MovieKey(77)
	if err := c.UpsertTitle(ctx, core.Title{
		Key: key, Type: core.TypeMovie, IDs: core.IDs{TMDB: 77}, Title: "Soap",
		Released: released, Genres: []string{"Drama", "Mystery", "Drama", "Mystery"},
		Keywords: []string{"Soap", "Soap"},
	}, true); err != nil {
		t.Fatal(err)
	}
	stored, err := c.Get(ctx, key)
	if err != nil || len(stored.Genres) != 2 || stored.Genres[0] != "Drama" || stored.Genres[1] != "Mystery" || len(stored.Keywords) != 1 || stored.Keywords[0] != "Soap" {
		t.Fatalf("stored genres %v keywords %v err %v", stored.Genres, stored.Keywords, err)
	}
	again := *stored
	again.Genres = append(append([]string{}, stored.Genres...), stored.Genres...)
	again.Keywords = append(append([]string{}, stored.Keywords...), stored.Keywords...)
	items := []core.Title{again}
	if err := c.hydrateSlice(ctx, items); err != nil {
		t.Fatal(err)
	}
	if len(items[0].Genres) != 2 || items[0].Genres[0] != "Drama" || items[0].Genres[1] != "Mystery" || len(items[0].Keywords) != 1 {
		t.Fatalf("hydrated genres %v keywords %v", items[0].Genres, items[0].Keywords)
	}
	visible := c.FilterVisibility(ctx, []core.Title{{
		Key: key, Type: core.TypeMovie, Title: "Soap",
		Genres: []string{"Drama", "Mystery", "Drama", "Mystery"}, Keywords: []string{"Soap", "Soap"},
	}})
	if len(visible) != 1 || len(visible[0].Genres) != 2 || visible[0].Genres[0] != "Drama" || visible[0].Genres[1] != "Mystery" || len(visible[0].Keywords) != 1 || visible[0].Keywords[0] != "Soap" {
		t.Fatalf("visible %+v", visible)
	}
}
