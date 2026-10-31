package catalog

import (
	"context"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// A large browse total walks the whole filtered set, so after a catalogue
// write Browse serves the last total and recounts behind the read.
func TestLargeBrowseTotalRecountsBehindTheRead(t *testing.T) {
	ctx := context.Background()
	c := newChunkTestCatalog(t)
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	c.SetNow(func() time.Time { return now })
	if _, err := c.db.ExecContext(ctx, `WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i<?)
		INSERT INTO catalog_titles(key,type,title,sort_title,released_ts,browsable,adult,detail) SELECT i,'movie','m','m',?,1,0,1 FROM n`, staleCountMin, now.Add(-time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	q := core.BrowseQuery{Filter: core.Filter{Type: core.TypeMovie}, Sort: core.SortTitle, Limit: 1}
	page, err := c.Browse(ctx, q)
	if err != nil || page.Total != staleCountMin {
		t.Fatalf("first total %d %v", page.Total, err)
	}
	extra := core.Title{Key: core.MovieKey(int64(staleCountMin + 1)), Type: core.TypeMovie, Title: "New", Released: now.Add(-time.Hour)}
	if err := c.UpsertTitle(ctx, extra, true); err != nil {
		t.Fatal(err)
	}
	if page, err = c.Browse(ctx, q); err != nil || page.Total != staleCountMin {
		t.Fatalf("total after a write %d %v: want the last count while it recounts", page.Total, err)
	}
	c.WaitRefreshes()
	if page, err = c.Browse(ctx, q); err != nil || page.Total != staleCountMin+1 {
		t.Fatalf("recounted total %d %v", page.Total, err)
	}
}
