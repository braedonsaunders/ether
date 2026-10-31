package catalog

import (
	"context"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestAirAddedAndAiredEpisodeBrowse(t *testing.T) {
	ctx := context.Background()
	c := newChunkTestCatalog(t)
	now := time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }
	show, other := core.ShowKey(11), core.ShowKey(12)
	old := now.Add(-48 * time.Hour)
	future := now.Add(24 * time.Hour)
	items := []core.Title{
		{Key: show, Type: core.TypeShow, Title: "A", Released: now.Add(-100 * 24 * time.Hour)},
		{Key: other, Type: core.TypeShow, Title: "B", Released: now.Add(-72 * time.Hour)},
		{Key: core.EpisodeKey(11, 1, 1), Type: core.TypeEpisode, ShowKey: show, Season: 1, Episode: 1, Released: old},
		{Key: core.EpisodeKey(11, 1, 2), Type: core.TypeEpisode, ShowKey: show, Season: 1, Episode: 2, Released: future},
		{Key: core.EpisodeKey(11, 1, 3), Type: core.TypeEpisode, ShowKey: show, Season: 1, Episode: 3},
		{Key: core.EpisodeKey(12, 1, 1), Type: core.TypeEpisode, ShowKey: other, Season: 1, Episode: 1, Released: now.Add(-time.Hour), Adult: true},
		{Key: core.MovieKey(21), Type: core.TypeMovie, Title: "Movie", Released: now.Add(-96 * time.Hour)},
	}
	for _, item := range items {
		if err := c.UpsertTitle(ctx, item, item.Type == core.TypeShow || item.Type == core.TypeMovie); err != nil {
			t.Fatal(err)
		}
	}
	got, err := c.CachedAddedTimes(ctx, []core.RatingKey{show, other, items[2].Key, items[3].Key, items[4].Key})
	if err != nil || !got[show].Equal(items[0].Released) || !got[other].Equal(items[1].Released) || !got[items[2].Key].Equal(old) || !got[items[3].Key].IsZero() || !got[items[4].Key].IsZero() {
		t.Fatalf("release dates: %+v %v", got, err)
	}
	for _, tc := range []struct {
		key  core.RatingKey
		date time.Time
	}{{show, items[0].Released}, {items[6].Key, items[6].Released}} {
		cached, e := c.CachedGet(ctx, tc.key)
		if e != nil || !cached.AddedAt.Equal(tc.date) {
			t.Fatalf("cached root %v: %+v %v", tc.key, cached, e)
		}
	}
	page, err := c.CachedAiredEpisodes(ctx, 0, 0, 10)
	if err != nil || page.Total != 1 || len(page.Items) != 1 || page.Items[0].Key != items[2].Key || !page.Items[0].AddedAt.Equal(old) {
		t.Fatalf("aired episodes: %+v %v", page, err)
	}
	page, err = c.Browse(ctx, core.BrowseQuery{Filter: core.Filter{Type: core.TypeShow}, Sort: core.SortAdded, Desc: true, Limit: 10})
	if err != nil || len(page.Items) != 2 || page.Items[0].Key != other || !page.Items[0].AddedAt.Equal(items[1].Released) {
		t.Fatalf("show order: %+v %v", page, err)
	}
	if _, err = c.db.ExecContext(ctx, `UPDATE catalog_titles SET fetched_at=CASE WHEN key=? THEN CAST(? AS BIGINT) ELSE CAST(? AS BIGINT) END WHERE key IN (?,?)`, int64(show), now.Unix(), now.Add(-time.Hour).Unix(), int64(show), int64(other)); err != nil {
		t.Fatal(err)
	}
	page, err = c.Browse(ctx, core.BrowseQuery{Filter: core.Filter{Type: core.TypeShow}, Sort: core.SortUpdated, Desc: true, Limit: 10})
	if err != nil || len(page.Items) != 2 || page.Items[0].Key != show {
		t.Fatalf("update order must use actual metadata update: %+v %v", page, err)
	}
	now = future.Add(time.Minute)
	got, err = c.CachedAddedTimes(ctx, []core.RatingKey{show})
	if err != nil || !got[show].Equal(items[0].Released) {
		t.Fatalf("premiere stays date added: %+v %v", got, err)
	}
	page, err = c.CachedAiredEpisodes(ctx, show, 0, 1)
	if err != nil || page.Total != 2 || len(page.Items) != 1 || page.Items[0].Key != items[3].Key {
		t.Fatalf("per-show paging: %+v %v", page, err)
	}
	items[3].Released = now.Add(48 * time.Hour)
	if err := c.upsert(ctx, items[3], upsertOpts{airDates: true}); err != nil {
		t.Fatal(err)
	}
	got, err = c.CachedAddedTimes(ctx, []core.RatingKey{show})
	if err != nil || !got[show].Equal(items[0].Released) {
		t.Fatalf("corrected future episode: %+v %v", got, err)
	}
	fallback := core.Title{Key: core.ShowKey(13), Type: core.TypeShow, Title: "No episodes", Released: old}
	if err := c.UpsertTitle(ctx, fallback, true); err != nil {
		t.Fatal(err)
	}
	got, err = c.CachedAddedTimes(ctx, []core.RatingKey{fallback.Key})
	if err != nil || !got[fallback.Key].Equal(old) {
		t.Fatalf("show first-air fallback: %+v %v", got, err)
	}
}

func TestSeasonAirAddedFallbacks(t *testing.T) {
	ctx := context.Background()
	c := newChunkTestCatalog(t)
	now := time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }
	show, emptyShow, unknownShow := core.ShowKey(20), core.ShowKey(21), core.ShowKey(22)
	first := now.Add(-100 * 24 * time.Hour)
	seasonAir := now.Add(-20 * 24 * time.Hour)
	lastEpisode := now.Add(-48 * time.Hour)
	items := []core.Title{
		{Key: show, Type: core.TypeShow, Title: "Show", Released: first},
		{Key: emptyShow, Type: core.TypeShow, Title: "Empty", Released: first},
		{Key: unknownShow, Type: core.TypeShow, Title: "Unknown"},
		{Key: core.SeasonKey(20, 1), Type: core.TypeSeason, ShowKey: show, Season: 1, Released: seasonAir},
		{Key: core.SeasonKey(20, 2), Type: core.TypeSeason, ShowKey: show, Season: 2, Released: seasonAir},
		{Key: core.SeasonKey(20, 3), Type: core.TypeSeason, ShowKey: show, Season: 3},
		{Key: core.SeasonKey(21, 1), Type: core.TypeSeason, ShowKey: emptyShow, Season: 1},
		{Key: core.SeasonKey(22, 1), Type: core.TypeSeason, ShowKey: unknownShow, Season: 1},
		{Key: core.EpisodeKey(20, 1, 1), Type: core.TypeEpisode, ShowKey: show, Season: 1, Episode: 1, Released: lastEpisode},
		{Key: core.EpisodeKey(20, 1, 2), Type: core.TypeEpisode, ShowKey: show, Season: 1, Episode: 2, Released: now.Add(time.Hour)},
	}
	for _, item := range items {
		if err := c.UpsertTitle(ctx, item, item.Type == core.TypeShow); err != nil {
			t.Fatal(err)
		}
	}
	keys := []core.RatingKey{items[3].Key, items[4].Key, items[5].Key, items[6].Key, items[7].Key}
	got, err := c.CachedAddedTimes(ctx, keys)
	if err != nil || !got[keys[0]].Equal(seasonAir) || !got[keys[1]].Equal(seasonAir) || !got[keys[2]].IsZero() || !got[keys[3]].IsZero() || !got[keys[4]].IsZero() {
		t.Fatalf("season premiere dates: %+v %v", got, err)
	}
	now = now.Add(2 * time.Hour)
	got, err = c.CachedAddedTimes(ctx, []core.RatingKey{items[3].Key, items[5].Key})
	if err != nil || !got[items[3].Key].Equal(seasonAir) || !got[items[5].Key].IsZero() {
		t.Fatalf("airing boundary: %+v %v", got, err)
	}
}

func TestMovieAddedSortIgnoresCatalogueInsertion(t *testing.T) {
	ctx := context.Background()
	c := newChunkTestCatalog(t)
	c.now = func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }
	old := core.Title{Key: core.MovieKey(7001), Type: core.TypeMovie, Title: "Old", Released: time.Unix(1072915200, 0).UTC()}
	newer := core.Title{Key: core.MovieKey(7002), Type: core.TypeMovie, Title: "Newer", Released: time.Unix(1262304000, 0).UTC()}
	unknown := core.Title{Key: core.MovieKey(7003), Type: core.TypeMovie, Title: "Unknown"}
	future := core.Title{Key: core.MovieKey(7004), Type: core.TypeMovie, Title: "Future", Released: c.now().Add(time.Hour)}
	for _, title := range []core.Title{old, newer, unknown, future} {
		if err := c.UpsertTitle(ctx, title, true); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.db.ExecContext(ctx, `UPDATE catalog_titles SET added_at=? WHERE key=?`, int64(1704067200), int64(old.Key)); err != nil {
		t.Fatal(err)
	}
	page, err := c.Browse(ctx, core.BrowseQuery{Filter: core.Filter{Type: core.TypeMovie, KnownAddedDate: true}, Sort: core.SortAdded, Desc: true, Limit: 10})
	if err != nil || page.Total != 2 || len(page.Items) != 2 || page.Items[0].Key != newer.Key || page.Items[1].Key != old.Key || page.Items[1].AddedAt.Unix() != 1072915200 {
		t.Fatalf("page=%+v err=%v", page, err)
	}
}
