package catalog

import (
	"context"
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// TestSearchWalkMatchesSortedQuery checks that the popularity walk picks
// and orders exactly the titles the sorted search query does, and declines
// when popularity ties leave the order to the query.
func TestSearchWalkMatchesSortedQuery(t *testing.T) {
	ctx := context.Background()
	c := newChunkTestCatalog(t)
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	c.SetNow(func() time.Time { return now })
	rng := rand.New(rand.NewPCG(1, 2))
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	pops := rng.Perm(100000)
	for i := range 6000 {
		typ, key := "movie", int64(core.MovieKey(int64(i+1)))
		switch i % 5 {
		case 1:
			typ, key = "show", int64(core.ShowKey(int64(i+1)))
		case 2:
			typ, key = "episode", int64(core.EpisodeKey(int64(i+1), 1, 1))
		}
		title := fmt.Sprintf("Title %d", i)
		if i%2 == 0 {
			title = fmt.Sprintf("Star Thing %d", i)
		}
		pop := float64(pops[i]) / 7
		if typ == "episode" {
			pop = 0
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO catalog_titles(key,type,title,original_title,sort_title,released_ts,browsable,adult,popularity) VALUES(?,?,?,'',?,?,?,?,?)`,
			key, typ, title, title, now.Add(-time.Hour).Unix(), i%3 != 0, i%11 == 0, pop); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	sorted := func(query string, n int, unfiltered bool) []core.RatingKey {
		match, args := c.titleMatch("", query)
		adult := ""
		if unfiltered {
			adult = " AND adult=0"
		}
		rows, err := c.db.QueryContext(ctx, `SELECT key FROM catalog_titles WHERE type IN ('movie','show','season','episode')`+adult+` AND `+match+` ORDER BY CASE WHEN ?>0 AND year=? THEN 0 ELSE 1 END, popularity DESC LIMIT ?`, append(args, 0, 0, n)...)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []core.RatingKey
		for rows.Next() {
			var k int64
			if err := rows.Scan(&k); err != nil {
				t.Fatal(err)
			}
			out = append(out, core.RatingKey(k))
		}
		return out
	}
	for _, query := range []string{"star", "a", "st", "Thing"} {
		for _, unfiltered := range []bool{false, true} {
			for _, n := range []int{1, 20, 60} {
				got, ok, err := c.searchByPopularity(ctx, query, n, unfiltered)
				if err != nil {
					t.Fatal(err)
				}
				if !ok {
					t.Fatalf("%q n=%d unfiltered=%v: the walk declined a dense term", query, n, unfiltered)
				}
				if want := sorted(query, n, unfiltered); !slices.Equal(got, want) {
					t.Fatalf("%q n=%d unfiltered=%v:\n got %v\nwant %v", query, n, unfiltered, got, want)
				}
			}
		}
	}
	// A sparse term keeps the sorted query.
	if _, ok, _ := c.searchByPopularity(ctx, "Thing 4242", 20, false); ok {
		t.Fatal("walked a sparse term")
	}
	// Ties among the picks leave the order to the sorted query.
	if _, err := c.db.ExecContext(ctx, `UPDATE catalog_titles SET popularity=1000000 WHERE type IN ('movie','show') AND title LIKE 'Star%' AND key IN (SELECT key FROM catalog_titles WHERE type IN ('movie','show') AND title LIKE 'Star%' LIMIT 2)`); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := c.searchByPopularity(ctx, "star", 20, false); ok {
		t.Fatal("walked through a popularity tie")
	}
}
