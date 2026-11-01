package catalog

import (
	"context"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// The series-sum total of a whole-library episode listing matches a direct
// count of the episodes as series and episodes are added, changed and
// removed, for every viewer and with episodes not yet aired.
func TestStoredEpisodeTotalBySeriesMatchesDirectCount(t *testing.T) {
	ctx := context.Background()
	c := newChunkTestCatalog(t)
	now := c.now()
	put := func(ti core.Title, opts upsertOpts) {
		t.Helper()
		if err := c.upsert(ctx, ti, opts); err != nil {
			t.Fatal(err)
		}
	}
	for id := int64(1); id <= 6; id++ {
		rating := []string{"TV-PG", "TV-14", "TV-MA"}[id%3]
		show := core.Title{Key: core.ShowKey(id), Type: core.TypeShow, Title: "Show", ContentRating: rating, Released: now.AddDate(-1, 0, 0)}
		put(show, upsertOpts{browsable: true})
		for ep := 1; ep <= 5; ep++ {
			aired := now.AddDate(0, 0, -30+ep*7) // the last ones air in the future
			put(core.Title{Key: core.EpisodeKey(id, 1, ep), Type: core.TypeEpisode, ShowKey: show.Key, Season: 1, Episode: ep, Title: "Ep", Released: aired, ContentRating: rating}, upsertOpts{})
		}
	}
	direct := func(f core.Filter) int {
		t.Helper()
		where, args, err := c.storedEpisodesWhere(f)
		if err != nil {
			t.Fatal(err)
		}
		var n int
		if err := c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM catalog_titles e JOIN catalog_titles t ON t.key=e.show_key WHERE e.key>? AND e.key<? AND `+where,
			append([]any{episodeKeyStart, episodeKeyEnd}, args...)...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	check := func(when string) {
		t.Helper()
		for _, f := range []core.Filter{
			{Type: core.TypeShow},
			{Type: core.TypeShow, MaxContentRating: "TV-14"},
			{Type: core.TypeShow, ExcludeContentRatings: []string{"TV-PG"}},
		} {
			got, ok, err := c.countStoredEpisodesBySeries(ctx, f)
			if err != nil || !ok {
				t.Fatalf("%s: by series %v %v", when, ok, err)
			}
			if want := direct(f); got != want || want == 0 {
				t.Fatalf("%s: filter %+v: by series %d, direct %d", when, f, got, want)
			}
		}
	}
	check("stored")
	// An episode turned adult, one moved to an unknown release, one removed.
	if _, err := c.db.ExecContext(ctx, `UPDATE catalog_titles SET adult=1 WHERE key=?`, int64(core.EpisodeKey(1, 1, 1))); err != nil {
		t.Fatal(err)
	}
	if _, err := c.db.ExecContext(ctx, `UPDATE catalog_titles SET released_ts=0 WHERE key=?`, int64(core.EpisodeKey(2, 1, 2))); err != nil {
		t.Fatal(err)
	}
	if _, err := c.db.ExecContext(ctx, `DELETE FROM catalog_titles WHERE key=?`, int64(core.EpisodeKey(3, 1, 1))); err != nil {
		t.Fatal(err)
	}
	check("changed")
	// Time passes: an episode that had not aired now counts.
	c.clock.Store(func() time.Time { return now.AddDate(0, 0, 30) })
	check("later")
	// A filter on the episodes themselves takes the direct count.
	if _, ok, _ := c.countStoredEpisodesBySeries(ctx, core.Filter{Type: core.TypeShow, ExcludeKeys: []core.RatingKey{core.EpisodeKey(4, 1, 1)}}); ok {
		t.Fatal("an episode exclusion used the series sum")
	}
}
