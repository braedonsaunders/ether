package catalog

import (
	"context"
	"math/rand/v2"
	"slices"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// TestRecentShowsWalkMatchesSortedQuery checks the newest-first walk
// against the sorted query it replaces, over series with tied dates, no
// episodes, only future episodes, premieres before 1970 and a filter.
func TestRecentShowsWalkMatchesSortedQuery(t *testing.T) {
	ctx := context.Background()
	c := newChunkTestCatalog(t)
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	c.SetNow(func() time.Time { return now })
	rng := rand.New(rand.NewPCG(7, 11))
	day := int64(24 * 3600)
	// Few distinct days, so many series tie on their date added.
	date := func() int64 { return now.Unix() - day*int64(1+rng.IntN(400)) }
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	insert := func(key core.RatingKey, typ core.MediaType, show core.RatingKey, season, episode int, released int64, genre string) {
		t.Helper()
		if _, err := tx.ExecContext(ctx, `INSERT INTO catalog_titles(key,type,show_key,season,episode,title,sort_title,released_ts,browsable,adult) VALUES(?,?,?,?,?,?,?,?,?,0)`,
			int64(key), string(typ), int64(show), season, episode, "t", "t", released, typ == core.TypeShow); err != nil {
			t.Fatal(err)
		}
		if genre != "" {
			if _, err := tx.ExecContext(ctx, `INSERT INTO catalog_genres(key,genre) VALUES(?,?)`, int64(key), genre); err != nil {
				t.Fatal(err)
			}
		}
	}
	for i := range 1600 {
		id := int64(1000 + i)
		show := core.ShowKey(id)
		premiere := date() - 400*day
		if i%9 == 0 {
			// Dated by its premiere, on the same days as aired episodes.
			premiere = date()
		}
		switch {
		case i%37 == 0:
			premiere = -day * int64(1+rng.IntN(3000)) // before 1970
		case i%41 == 0:
			premiere = now.Unix() + day // not released yet: never browsed
		}
		genre := "Drama"
		if i%3 == 0 {
			genre = "Comedy"
		}
		insert(show, core.TypeShow, 0, 0, 0, premiere, genre)
		switch i % 9 {
		case 0: // no episodes
		case 1: // only future or undated episodes
			insert(core.EpisodeKey(id, 1, 1), core.TypeEpisode, show, 1, 1, now.Unix()+day, "")
			insert(core.EpisodeKey(id, 1, 2), core.TypeEpisode, show, 1, 2, 0, "")
		default:
			for e := range 1 + rng.IntN(6) {
				insert(core.EpisodeKey(id, 1, e+1), core.TypeEpisode, show, 1, e+1, date(), "")
			}
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	asOf := c.now()
	premiere := map[core.RatingKey]bool{}
	for i := 0; i < 1600; i += 9 {
		premiere[core.ShowKey(int64(1000+i))] = true
	}
	// A short step puts ties at the page's last date across steps. That case
	// is the unfiltered catalogue; the other filters repeat the same walk.
	defer func(n int) { recentWalkBatch = n }(recentWalkBatch)
	filters := []core.Filter{
		{Type: core.TypeShow},
		{Type: core.TypeShow, KnownAddedDate: true},
		{Type: core.TypeShow, KnownAddedDate: true, Genres: []string{"Comedy"}},
		{Type: core.TypeShow, ExcludeKeys: []core.RatingKey{core.ShowKey(1002), core.ShowKey(1003), core.ShowKey(1004)}},
	}
	for fi, f := range filters {
		where, args, err := c.browseWhere(f)
		if err != nil {
			t.Fatal(err)
		}
		var total int
		if err := c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM catalog_titles t WHERE `+where, args...).Scan(&total); err != nil {
			t.Fatal(err)
		}
		order := sortedAdded(t, c, where, args, asOf, 0, max(total, 1))
		if len(order) != total {
			t.Fatalf("%+v sorted %d, counted %d", f, len(order), total)
		}
		pages := [][2]int{{0, 1}, {0, 24}, {0, 50}, {13, 7}, {200, 50}, {total - 30, 50}, {total + 5, 10}}
		if fi == 0 {
			// Page ends across the newest series. A stride still lands on a
			// premiere-dated series; checking every offset repeats the walk.
			var sawPremiere bool
			for offset := 0; offset < min(total, 150); offset += 15 {
				pages = append(pages, [2]int{offset, 1})
				if premiere[order[offset].key] {
					sawPremiere = true
				}
			}
			if !sawPremiere {
				for i, row := range order[:min(len(order), 150)] {
					if premiere[row.key] {
						pages = append(pages, [2]int{i, 1})
						sawPremiere = true
						break
					}
				}
			}
			if !sawPremiere {
				t.Fatal("newest window has no premiere-dated series")
			}
		}
		batches := []int{256}
		if fi == 0 {
			batches = []int{256, 7}
		}
		for _, batch := range batches {
			recentWalkBatch = batch
			for _, page := range pages {
				offset, limit := max(page[0], 0), page[1]
				got, ok, err := c.recentShowsPage(ctx, where, args, asOf, offset, limit)
				if err != nil || !ok {
					t.Fatalf("batch %d %+v %v: walk ok=%v err=%v", batch, f, page, ok, err)
				}
				end := min(offset+limit, len(order))
				want := []addedKey{}
				if offset < len(order) {
					want = order[offset:end]
				}
				if !slices.Equal(got, want) {
					t.Fatalf("batch %d %+v page %v (total %d):\n got %v\nwant %v", batch, f, page, total, got, want)
				}
			}
		}
	}
}

func sortedAdded(t *testing.T, c *Catalog, where string, args []any, asOf time.Time, offset, limit int) []addedKey {
	t.Helper()
	rows, err := c.db.QueryContext(context.Background(), `SELECT t.key, `+airAddedSQL+` AS added FROM catalog_titles t WHERE `+where+` ORDER BY added DESC, t.key ASC LIMIT ? OFFSET ?`,
		append(append(airAddedArgs(asOf), args...), limit, offset)...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := []addedKey{}
	for rows.Next() {
		var key, added int64
		if err := rows.Scan(&key, &added); err != nil {
			t.Fatal(err)
		}
		out = append(out, addedKey{key: core.RatingKey(key), added: added})
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}
