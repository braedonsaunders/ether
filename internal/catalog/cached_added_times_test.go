package catalog

import (
	"context"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

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
