package catalog

import (
	"context"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func largeCatalogue(tb testing.TB, n int) *Catalog {
	tb.Helper()
	db, err := store.OpenMemory(context.Background())
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { db.Close() })
	c, err := New(db, nil, config.Catalog{}, slog.New(slog.DiscardHandler))
	if err != nil {
		tb.Fatal(err)
	}
	tx, err := db.Begin()
	if err != nil {
		tb.Fatal(err)
	}
	stmt, err := tx.Prepare(`INSERT INTO catalog_titles(key,type,tmdb_id,title,sort_title,year,released_ts,popularity,browsable,adult) VALUES(?,?,?,?,?,?,?,?,1,0)`)
	if err != nil {
		tb.Fatal(err)
	}
	released := time.Now().Add(-48 * time.Hour).Unix()
	for i := 1; i <= n; i++ {
		title := fmt.Sprintf("Title %05d", i)
		if _, err := stmt.Exec(int64(core.MovieKey(int64(i))), "movie", i, title, title, 2000, released, float64(n-i), i); err != nil {
			tb.Fatal(err)
		}
	}
	if _, err := tx.Exec(`INSERT INTO catalog_keywords(key,keyword) VALUES(?,?)`, int64(core.MovieKey(1)), "gore"); err != nil {
		tb.Fatal(err)
	}
	if err := stmt.Close(); err != nil {
		tb.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		tb.Fatal(err)
	}
	return c
}

func TestLargeCataloguePageUsesSQLOffset(t *testing.T) {
	c := largeCatalogue(t, 8000)
	page, err := c.Browse(context.Background(), core.BrowseQuery{Filter: core.Filter{Type: core.TypeMovie}, Sort: core.SortTitle, Offset: 2500, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 8000 || len(page.Items) != 1 || page.Items[0].Title != "Title 02501" || page.Items[0].IDs.TMDB != 2501 {
		t.Fatalf("page total=%d item=%+v", page.Total, page.Items)
	}
	blocked, err := c.Browse(context.Background(), core.BrowseQuery{Filter: core.Filter{Type: core.TypeMovie, ExcludeKeywords: []string{"Gore"}}, Sort: core.SortTitle, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if blocked.Total != 7999 || len(blocked.Items) != 1 || blocked.Items[0].Title == "Title 00001" {
		t.Fatalf("keyword exclusion total=%d first=%+v", blocked.Total, blocked.Items)
	}
}

func BenchmarkLargeCataloguePage(b *testing.B) {
	c := largeCatalogue(b, 8000)
	ctx := context.Background()
	q := core.BrowseQuery{Filter: core.Filter{Type: core.TypeMovie}, Sort: core.SortPopularity, Desc: true, Limit: 100}
	if _, err := c.Browse(ctx, q); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		page, err := c.Browse(ctx, q)
		if err != nil || len(page.Items) != 100 || page.Total != 8000 {
			b.Fatalf("page %d %v", len(page.Items), err)
		}
	}
}
