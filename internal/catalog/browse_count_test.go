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
	defer func(d time.Duration) { recountEvery = d }(recountEvery)
	recountEvery = 0
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

// NoTotal returns the same page without counting the filter's matches.
func TestBrowseWithoutTotal(t *testing.T) {
	ctx := context.Background()
	c := newChunkTestCatalog(t)
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	c.SetNow(func() time.Time { return now })
	if _, err := c.db.ExecContext(ctx, `WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i<30)
		INSERT INTO catalog_titles(key,type,title,sort_title,released_ts,browsable,adult,detail,popularity) SELECT i,'movie','m','m',?,1,0,1,i FROM n`, now.Add(-time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	q := core.BrowseQuery{Filter: core.Filter{Type: core.TypeMovie}, Sort: core.SortPopularity, Desc: true, Offset: 5, Limit: 10}
	counted, err := c.Browse(ctx, q)
	if err != nil || counted.Total != 30 {
		t.Fatalf("counted %d %v", counted.Total, err)
	}
	q.NoTotal = true
	page, err := c.Browse(ctx, q)
	if err != nil || page.Total != 15 || len(page.Items) != len(counted.Items) || page.Items[0].Key != counted.Items[0].Key {
		t.Fatalf("without a total: %d items, total %d, %v", len(page.Items), page.Total, err)
	}
	q.NoTotal = false
	if again, _ := c.Browse(ctx, q); again.Total != 30 {
		t.Fatalf("a page without a total was cached for a counted read: %d", again.Total)
	}
}

// A large catalogue's dashboard counts are served from the last count while
// they recount behind the read.
func TestLargeCatalogueStatsRecountBehindTheRead(t *testing.T) {
	defer func(d time.Duration) { recountEvery = d }(recountEvery)
	recountEvery = 0
	ctx := context.Background()
	c := newChunkTestCatalog(t)
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	c.SetNow(func() time.Time { return now })
	if _, err := c.db.ExecContext(ctx, `WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i<?)
		INSERT INTO catalog_titles(key,type,title,sort_title,released_ts,browsable,adult,detail) SELECT i,'movie','m','m',?,1,0,1 FROM n`, staleCountMin, now.Add(-time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	if s, err := c.Stats(ctx); err != nil || s.Movies != staleCountMin {
		t.Fatalf("first %d %v", s.Movies, err)
	}
	if err := c.UpsertTitle(ctx, core.Title{Key: core.MovieKey(int64(staleCountMin + 1)), Type: core.TypeMovie, Title: "New", Released: now.Add(-time.Hour)}, true); err != nil {
		t.Fatal(err)
	}
	if s, _ := c.Stats(ctx); s.Movies != staleCountMin {
		t.Fatalf("after a write %d: want the last count while it recounts", s.Movies)
	}
	c.WaitRefreshes()
	if s, _ := c.Stats(ctx); s.Movies != staleCountMin+1 {
		t.Fatalf("recounted %d", s.Movies)
	}
}
