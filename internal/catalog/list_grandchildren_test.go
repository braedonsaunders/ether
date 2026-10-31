package catalog

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// StoredGrandchildren answers a show only when Grandchildren would not wait
// on TMDB, and then with exactly Grandchildren's episodes.
func TestStoredGrandchildrenMatchesGrandchildren(t *testing.T) {
	ctx := context.Background()
	c := newChunkTestCatalog(t)
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	c.SetNow(func() time.Time { return now })
	past, future := now.Add(-48*time.Hour).Unix(), now.Add(48*time.Hour).Unix()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := c.db.ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	show := func(id int64, detail int) {
		exec(`INSERT INTO catalog_titles(key,type,tmdb_id,title,sort_title,released_ts,browsable,adult,detail,detail_at,fetched_at) VALUES(?,'show',?,'s','s',?,1,0,?,?,?)`, int64(core.ShowKey(id)), id, past, detail, now.Unix(), now.Unix())
	}
	season := func(id int64, n, children, detail int, released int64) {
		exec(`INSERT INTO catalog_titles(key,type,show_key,show_tmdb,season,title,sort_title,released_ts,child_count,detail,detail_at,fetched_at) VALUES(?,'season',?,?,?,'x','x',?,?,?,?,?)`, int64(core.SeasonKey(id, n)), int64(core.ShowKey(id)), id, n, released, children, detail, now.Unix(), now.Unix())
	}
	episode := func(id int64, s, e int, released int64, adult int) {
		exec(`INSERT INTO catalog_titles(key,type,show_key,show_tmdb,season,episode,title,sort_title,released_ts,adult) VALUES(?,'episode',?,?,?,?,'e','e',?,?)`, int64(core.EpisodeKey(id, s, e)), int64(core.ShowKey(id)), id, s, e, released, adult)
	}
	// Answered: specials, an unaired and an adult episode, a season with no
	// episodes listed (child_count 0) and an unreleased season.
	show(1, 1)
	season(1, 0, 1, 1, past)
	episode(1, 0, 1, past, 0)
	season(1, 1, 3, 1, past)
	episode(1, 1, 1, past, 0)
	episode(1, 1, 2, past, 1)
	episode(1, 1, 3, future, 0)
	season(1, 2, 2, 1, past)
	episode(1, 2, 2, past, 0)
	episode(1, 2, 1, past, 0)
	season(1, 3, 0, 1, past)
	season(1, 4, 1, 1, future)
	episode(1, 4, 1, future, 0)
	// Not answered: a show without details, and a listed season without.
	show(2, 0)
	season(2, 1, 1, 1, past)
	episode(2, 1, 1, past, 0)
	show(3, 1)
	season(3, 1, 1, 0, past)
	episode(3, 1, 1, past, 0)
	// Answered, empty: no listable season.
	show(4, 1)

	got, err := c.StoredGrandchildren(ctx, []core.RatingKey{core.ShowKey(1), core.ShowKey(2), core.ShowKey(3), core.ShowKey(4), core.MovieKey(9)})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got[core.ShowKey(2)]; ok {
		t.Fatal("answered a show without details")
	}
	if _, ok := got[core.ShowKey(3)]; ok {
		t.Fatal("answered a show with an undetailed season")
	}
	for _, k := range []core.RatingKey{core.ShowKey(1), core.ShowKey(4)} {
		c.cacheMu.Lock()
		delete(c.grandCache, k)
		c.cacheMu.Unlock()
		want, err := c.Grandchildren(ctx, k)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(keysOfTitles(got[k]), keysOfTitles(want)) {
			t.Fatalf("show %v:\n got %v\nwant %v", k, keysOfTitles(got[k]), keysOfTitles(want))
		}
	}
	if len(got[core.ShowKey(1)]) == 0 {
		t.Fatal("no episodes for the answered show")
	}
}

func keysOfTitles(ts []core.Title) []core.RatingKey {
	out := []core.RatingKey{}
	for _, t := range ts {
		out = append(out, t.Key)
	}
	return out
}
