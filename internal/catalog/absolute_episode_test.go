package catalog

import (
	"context"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestHydrateAbsoluteEpisodes(t *testing.T) {
	ctx := context.Background()
	c := newChunkTestCatalog(t)
	aired := time.Date(2010, 1, 1, 0, 0, 0, 0, time.UTC)
	show := core.Title{Key: core.ShowKey(100), Type: core.TypeShow, Title: "Anime", Released: aired}
	gapped := core.Title{Key: core.ShowKey(200), Type: core.TypeShow, Title: "Gapped", Released: aired}
	season := func(sh core.Title, n, count int) core.Title {
		return core.Title{Key: core.SeasonKey(sh.Key.TMDB(), n), Type: core.TypeSeason, ShowKey: sh.Key, Season: n, ChildCount: count, Title: "Season", Released: aired}
	}
	titles := []core.Title{show, gapped,
		// Specials (season 0) never count towards absolute numbers.
		season(show, 0, 30), season(show, 1, 61), season(show, 2, 77), season(show, 3, 39),
		// Season 2 of the gapped show has no known size.
		season(gapped, 1, 12), season(gapped, 2, 0), season(gapped, 3, 12),
	}
	if err := c.upsertMany(ctx, titles, upsertOpts{browsable: true}); err != nil {
		t.Fatal(err)
	}
	ep := func(sh core.Title, s, e int) core.Title {
		return core.Title{Key: core.EpisodeKey(sh.Key.TMDB(), s, e), Type: core.TypeEpisode, ShowKey: sh.Key, Season: s, Episode: e}
	}
	items := []core.Title{ep(show, 1, 7), ep(show, 3, 5), ep(show, 0, 2), ep(gapped, 3, 1), ep(gapped, 1, 3), ep(show, 3, 5)}
	if err := c.hydrateSlice(ctx, items); err != nil {
		t.Fatal(err)
	}
	for i, want := range []int{7, 61 + 77 + 5, 0, 0, 3, 61 + 77 + 5} {
		if got := items[i].AbsoluteEpisode; got != want {
			t.Errorf("item %d S%dE%d: absolute %d, want %d", i, items[i].Season, items[i].Episode, got, want)
		}
	}
}
