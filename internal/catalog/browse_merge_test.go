package catalog

import (
	"context"
	"math/rand/v2"
	"slices"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// TestMixedBrowseMergeMatchesSortedQuery checks the per-type merge against
// the single sorted query over movies and series, ties included.
func TestMixedBrowseMergeMatchesSortedQuery(t *testing.T) {
	ctx := context.Background()
	c := newChunkTestCatalog(t)
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	c.SetNow(func() time.Time { return now })
	rng := rand.New(rand.NewPCG(3, 5))
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 600 {
		typ, key := core.TypeMovie, core.MovieKey(int64(i+1))
		if i%3 == 0 {
			typ, key = core.TypeShow, core.ShowKey(int64(i+1))
		}
		// Few distinct values, so the two types tie often.
		if _, err := tx.ExecContext(ctx, `INSERT INTO catalog_titles(key,type,title,sort_title,released_ts,browsable,adult,detail,popularity,rating,fetched_at) VALUES(?,?,?,?,?,1,0,1,?,?,?)`,
			int64(key), string(typ), "t", "t", now.Add(-time.Duration(1+rng.IntN(50))*24*time.Hour).Unix(), float64(rng.IntN(40))/4, float64(rng.IntN(20))/2, int64(rng.IntN(30))); err != nil {
			t.Fatal(err)
		}
		if i%4 == 0 {
			if _, err := tx.ExecContext(ctx, `INSERT INTO catalog_genres(key,genre) VALUES(?,?)`, int64(key), "Drama"); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	for _, sort := range []core.SortOrder{core.SortPopularity, core.SortRating, core.SortReleased, core.SortUpdated, ""} {
		for _, desc := range []bool{true, false} {
			for _, f := range []core.Filter{{}, {Genres: []string{"Drama"}}} {
				q := core.BrowseQuery{Filter: f, Sort: sort, Desc: desc}
				where, args, err := c.browseWhere(f)
				if err != nil {
					t.Fatal(err)
				}
				for _, page := range [][2]int{{0, 50}, {37, 13}, {550, 100}, {0, 1}} {
					got, err := c.mergedTypeKeys(ctx, where, args, q, page[0], page[1])
					if err != nil {
						t.Fatal(err)
					}
					rows, err := c.db.QueryContext(ctx, `SELECT t.key FROM catalog_titles t WHERE `+where+` ORDER BY `+browseOrder(q)+`, t.key ASC LIMIT ? OFFSET ?`, append(append([]any{}, args...), page[1], page[0])...)
					if err != nil {
						t.Fatal(err)
					}
					var want []core.RatingKey
					for rows.Next() {
						var k int64
						if err := rows.Scan(&k); err != nil {
							t.Fatal(err)
						}
						want = append(want, core.RatingKey(k))
					}
					rows.Close()
					if !slices.Equal(got, want) {
						t.Fatalf("%q desc=%v %+v page %v:\n got %v\nwant %v", sort, desc, f, page, got, want)
					}
				}
			}
		}
	}
}
