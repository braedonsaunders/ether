package catalog

import (
	"context"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestStoredEpisodesByKey(t *testing.T) {
	ctx := context.Background()
	c := newChunkTestCatalog(t)
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	c.SetNow(func() time.Time { return now })
	past, future := now.Add(-time.Hour).Unix(), now.Add(time.Hour).Unix()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := c.db.ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	for _, s := range []struct {
		id      int64
		adult   int
		rating  string
		rank    int
		release int64
	}{{1, 0, "TV-PG", 2, past}, {2, 1, "TV-PG", 2, past}, {3, 0, "TV-MA", 5, past}, {4, 0, "TV-PG", 2, future}} {
		exec(`INSERT INTO catalog_titles(key,type,title,sort_title,released_ts,browsable,adult,content_rating,content_rating_rank) VALUES(?,'show','s','s',?,1,?,?,?)`, int64(core.ShowKey(s.id)), s.release, s.adult, s.rating, s.rank)
		for e := 1; e <= 3; e++ {
			released := past
			if e == 3 {
				released = future
			}
			exec(`INSERT INTO catalog_titles(key,type,show_key,season,episode,title,sort_title,released_ts,adult) VALUES(?,'episode',?,1,?,'e','e',?,0)`, int64(core.EpisodeKey(s.id, 1, e)), int64(core.ShowKey(s.id)), e, released)
		}
	}
	got, err := c.StoredEpisodesByKey(ctx, core.Filter{}, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	want := []core.RatingKey{core.EpisodeKey(1, 1, 1), core.EpisodeKey(1, 1, 2), core.EpisodeKey(3, 1, 1), core.EpisodeKey(3, 1, 2)}
	if len(got) != len(want) {
		t.Fatalf("episodes %v", got)
	}
	for i := range want {
		if got[i].Key != want[i] {
			t.Fatalf("episode %d: %v want %v", i, got[i].Key, want[i])
		}
	}
	if page, _ := c.StoredEpisodesByKey(ctx, core.Filter{}, core.EpisodeKey(1, 1, 2), 1); len(page) != 1 || page[0].Key != core.EpisodeKey(3, 1, 1) {
		t.Fatalf("after a key: %v", page)
	}
	if page, _ := c.StoredEpisodesByKey(ctx, core.Filter{MaxContentRating: "TV-14"}, 0, 10); len(page) != 2 {
		t.Fatalf("rating cap: %v", page)
	}
	if n, err := c.CountStoredEpisodes(ctx, core.Filter{}); err != nil || n != 4 {
		t.Fatalf("count %d %v", n, err)
	}
	if ok, err := c.EpisodesUnranked(ctx); err != nil || !ok {
		t.Fatalf("unranked %v %v", ok, err)
	}
	exec(`UPDATE catalog_titles SET popularity=1.5 WHERE key=?`, int64(core.EpisodeKey(3, 1, 1)))
	if ok, err := c.EpisodesUnranked(ctx); err != nil || ok {
		t.Fatalf("a ranked episode: %v %v", ok, err)
	}
}

func TestBrowseStoredSeasonsAndEpisodesAndStableFetchedAt(t *testing.T) {
	ctx := context.Background()
	c := newChunkTestCatalog(t)
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	c.SetNow(func() time.Time { return now })

	t1 := time.Date(2025, 1, 10, 0, 0, 0, 0, time.UTC)
	t2 := time.Date(2025, 2, 10, 0, 0, 0, 0, time.UTC)
	show := core.Title{
		Key: core.ShowKey(77), Type: core.TypeShow, IDs: core.IDs{TMDB: 77},
		Title: "Signal", Released: t1, Genres: []string{"Drama"},
	}
	if err := c.UpsertDetailedTitle(ctx, show); err != nil {
		t.Fatal(err)
	}
	s1 := core.Title{
		Key: core.SeasonKey(77, 1), Type: core.TypeSeason, ShowKey: show.Key,
		IDs: core.IDs{TMDB: 77}, Season: 1, Title: "Season 1", Released: t1,
	}
	e1 := core.Title{
		Key: core.EpisodeKey(77, 1, 1), Type: core.TypeEpisode, ShowKey: show.Key,
		IDs: core.IDs{TMDB: 77, IMDB: "tt0000001"}, Season: 1, Episode: 1, Title: "Ep 1", Released: t1,
	}
	e2 := core.Title{
		Key: core.EpisodeKey(77, 1, 2), Type: core.TypeEpisode, ShowKey: show.Key,
		IDs: core.IDs{TMDB: 77, IMDB: "tt0000002"}, Season: 1, Episode: 2, Title: "Ep 2", Released: t2,
	}
	for _, it := range []core.Title{s1, e1, e2} {
		if err := c.UpsertTitle(ctx, it, false); err != nil {
			t.Fatal(err)
		}
	}

	// Advancing clock and re-upserting with only popularity/vote_count jitter
	// must NOT advance fetched_at.
	later := now.Add(2 * time.Hour)
	c.SetNow(func() time.Time { return later })
	jitter := show
	jitter.Popularity = 999.9
	jitter.VoteCount = 5000
	if err := c.UpsertTitle(ctx, jitter, true); err != nil {
		t.Fatal(err)
	}
	gotShow, err := c.Get(ctx, show.Key)
	if err != nil {
		t.Fatal(err)
	}
	if !gotShow.UpdatedAt.Equal(now) {
		t.Fatalf("UpdatedAt moved on popularity jitter: got %v want %v", gotShow.UpdatedAt, now)
	}

	// Changing material metadata advances fetched_at.
	changed := e2
	changed.Summary = "Updated synopsis"
	if err := c.UpsertTitle(ctx, changed, false); err != nil {
		t.Fatal(err)
	}

	// Browse episodes in key order and by updatedAt:desc.
	epPage, err := c.Browse(ctx, core.BrowseQuery{Filter: core.Filter{Type: core.TypeEpisode}, Limit: 10})
	if err != nil || epPage.Total != 2 || len(epPage.Items) != 2 || epPage.Items[0].Key != e1.Key || epPage.Items[1].Key != e2.Key {
		t.Fatalf("episode browse by key: %+v (%v)", epPage, err)
	}
	epUpdated, err := c.Browse(ctx, core.BrowseQuery{
		Filter: core.Filter{Type: core.TypeEpisode, UpdatedAfter: later.Unix()},
		Sort:   core.SortUpdated, Desc: true, Limit: 10,
	})
	if err != nil || epUpdated.Total != 1 || len(epUpdated.Items) != 1 || epUpdated.Items[0].Key != e2.Key {
		t.Fatalf("episode incremental browse: %+v (%v)", epUpdated, err)
	}

	// Browse seasons in key order: ChildCount reflects aired episodes (2), AddedAt reflects latest aired episode (t2).
	sePage, err := c.Browse(ctx, core.BrowseQuery{Filter: core.Filter{Type: core.TypeSeason}, Limit: 10})
	if err != nil || sePage.Total != 1 || len(sePage.Items) != 1 {
		t.Fatalf("season browse: %+v (%v)", sePage, err)
	}
	if sePage.Items[0].ChildCount != 2 || !sePage.Items[0].AddedAt.Equal(t2) {
		t.Fatalf("season counts/dates: %+v", sePage.Items[0])
	}
}

func TestRecentAirSkipsExcludedTitles(t *testing.T) {
	ctx := context.Background()
	c := newChunkTestCatalog(t)
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	c.SetNow(func() time.Time { return now })
	for i := 1; i <= 5; i++ {
		if _, err := c.db.ExecContext(ctx, `INSERT INTO catalog_titles(key,type,title,sort_title,released_ts,browsable,adult,detail) VALUES(?,'movie','m','m',?,1,0,1)`, int64(core.MovieKey(int64(i))), now.Add(-time.Duration(i)*time.Hour).Unix()); err != nil {
			t.Fatal(err)
		}
	}
	got, _, err := c.RecentAirExcluding(ctx, core.TypeMovie, []core.RatingKey{core.MovieKey(1), core.MovieKey(2)}, 0, 2)
	if err != nil || len(got) != 2 || got[0].Key != core.MovieKey(3) || got[1].Key != core.MovieKey(4) {
		t.Fatalf("window %v %v", got, err)
	}
}
