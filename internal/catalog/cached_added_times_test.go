package catalog

import (
	"context"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestMinuteSnapshotKeepsFirstCreditRead(t *testing.T) {
	ctx := context.Background()
	c := newChunkTestCatalog(t)
	c.SetNow(func() time.Time { return time.Date(2026, 1, 1, 12, 0, 30, 0, time.UTC) })
	key := core.MovieKey(781)
	title := core.Title{Key: key, Type: core.TypeMovie, Released: time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), Cast: []core.Person{{TMDB: 30, Name: "Person", ThumbPath: "/old.jpg"}}}
	if err := c.UpsertTitle(ctx, title, false); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CachedGet(ctx, key); err != nil {
		t.Fatal(err)
	}
	if _, err := c.db.ExecContext(ctx, `UPDATE catalog_credits SET thumb='/new.jpg' WHERE key=? AND person_tmdb=30`, int64(key)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.titlesByKeys(ctx, []core.RatingKey{key}); err != nil {
		t.Fatal(err)
	}
	title.Cast[0].ThumbPath = "/new.jpg"
	c.rememberTitles(c.writeGen.Load(), c.minuteBucket(), []core.Title{title})
	got, err := c.StoredTitle(ctx, key)
	if err != nil || len(got.Cast) != 1 || got.Cast[0].ThumbPath != "/old.jpg" {
		t.Fatalf("in-flight credit read replaced the minute snapshot: %+v %v", got.Cast, err)
	}
}

func TestCachedAddedTimesBatch(t *testing.T) {
	ctx := context.Background()
	c := newChunkTestCatalog(t)
	known, zero, missing := core.MovieKey(1), core.MovieKey(2), core.MovieKey(3)
	for _, key := range []core.RatingKey{known, zero} {
		if err := c.UpsertTitle(ctx, core.Title{Key: key, Type: core.TypeMovie, Title: "Title"}, true); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.db.ExecContext(ctx, `UPDATE catalog_titles SET released_ts=CASE WHEN key=? THEN ? ELSE 0 END WHERE key IN (?,?)`, int64(known), int64(1700000000), int64(known), int64(zero)); err != nil {
		t.Fatal(err)
	}
	keys := []core.RatingKey{known, zero, missing, known}
	for i := 0; i < 500; i++ {
		keys = append(keys, core.MovieKey(int64(1000+i)))
	}
	keys = append(keys, known)
	got, err := c.CachedAddedTimes(ctx, keys)
	if err != nil || len(got) != 1 || !got[known].Equal(time.Unix(1700000000, 0)) || got[known].Location() != time.UTC {
		t.Fatalf("added times: %+v %v", got, err)
	}
	empty, err := c.CachedAddedTimes(ctx, nil)
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty: %+v %v", empty, err)
	}
}
