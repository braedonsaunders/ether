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
