package catalog

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

// The dashboard's catalogue counts must equal the direct counts of the
// titles, check rows and franchises they stand for, through every kind of
// write to each table, tally folds, clock moves and rebuilds.
func TestDashboardCountsMatchTitles(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	c, err := New(db, nil, config.Catalog{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	c.SetNow(func() time.Time { return now })
	rng := rand.New(rand.NewPCG(13, 17))
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	released := func() int64 {
		switch rng.IntN(6) {
		case 0:
			return 0 // unknown
		case 1:
			return now.Add(time.Duration(1+rng.IntN(48)) * time.Hour).Unix() // coming days
		case 2:
			return -int64(rng.IntN(1_000_000_000)) // before 1970
		default:
			return now.Add(-time.Duration(rng.IntN(20_000)) * time.Hour).Unix()
		}
	}
	yearSources := []string{"", "", yearSourcePrimary, yearSourceReleased}
	types := []string{"movie", "movie", "movie", "show", "season", "episode"}
	var keys []int64
	for i := int64(1); i <= 240; i++ {
		key := 1_000 + i
		exec(`INSERT INTO catalog_titles(key,type,title,sort_title,released_ts,browsable,adult,detail,year_source) VALUES(?,?,?,?,?,?,?,1,?)`,
			key, types[rng.IntN(len(types))], fmt.Sprint("t", key), fmt.Sprint("t", key), released(), flagInt(rng.IntN(3) > 0), flagInt(rng.IntN(8) == 0), yearSources[rng.IntN(len(yearSources))])
		keys = append(keys, key)
	}
	// Check rows: unchecked, checked, an unexpected state, and orphans whose
	// title is written later or never.
	for _, k := range keys {
		if rng.IntN(3) == 0 {
			exec(`INSERT INTO catalog_home_release(key,checked,earliest_ts,attempted_at) VALUES(?,?,0,?)`, k, []int{0, 1, 1, 2}[rng.IntN(4)], now.Unix())
		}
	}
	exec(`INSERT INTO catalog_home_release(key,checked) VALUES(?,1),(?,1)`, 90_001, 90_002)
	for i := int64(1); i <= 12; i++ {
		exec(`INSERT INTO catalog_collections(tmdb_id,name) VALUES(?,?)`, i, fmt.Sprint("c", i))
	}

	direct := func() (Stats, int) {
		t.Helper()
		var s Stats
		for q, dst := range map[string]*int{
			`SELECT COUNT(*) FROM catalog_titles WHERE type='movie'`:                 &s.Movies,
			`SELECT COUNT(*) FROM catalog_titles WHERE type='show'`:                  &s.Shows,
			`SELECT COUNT(*) FROM catalog_titles WHERE type='season'`:                &s.Seasons,
			`SELECT COUNT(*) FROM catalog_titles WHERE type='episode'`:               &s.Episodes,
			`SELECT COUNT(*) FROM catalog_titles WHERE type='movie' AND browsable=1`: &s.BrowsableMovies,
			`SELECT COUNT(*) FROM catalog_titles WHERE type='show' AND browsable=1`:  &s.BrowsableShows,
			`SELECT COUNT(*) FROM catalog_collections`:                               &s.Collections,
		} {
			if err := db.QueryRowContext(ctx, q).Scan(dst); err != nil {
				t.Fatal(err)
			}
		}
		if err := db.QueryRowContext(ctx, `
			SELECT COALESCE(SUM(CASE WHEN h.checked=1 THEN 1 ELSE 0 END), 0),
			       COALESCE(SUM(CASE WHEN h.checked IS NULL OR h.checked=0 THEN 1 ELSE 0 END), 0)
			FROM catalog_titles t LEFT JOIN catalog_home_release h ON h.key=t.key
			WHERE t.type='movie' AND t.adult=0 AND t.release_known=1 AND t.released_ts<=?`, now.Unix()).Scan(&s.HomeReleaseChecked, &s.HomeReleasePending); err != nil {
			t.Fatal(err)
		}
		var pending int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM catalog_titles WHERE type='movie' AND year_source=''`).Scan(&pending); err != nil {
			t.Fatal(err)
		}
		return s, pending
	}
	check := func(stage string) {
		t.Helper()
		want, wantPending := direct()
		got, err := c.Stats(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if got.Movies != want.Movies || got.Shows != want.Shows || got.Seasons != want.Seasons || got.Episodes != want.Episodes ||
			got.BrowsableMovies != want.BrowsableMovies || got.BrowsableShows != want.BrowsableShows || got.Collections != want.Collections ||
			got.HomeReleaseChecked != want.HomeReleaseChecked || got.HomeReleasePending != want.HomeReleasePending {
			t.Errorf("%s: dashboard counts\n got %+v\nwant %+v", stage, countsOf(got), countsOf(want))
		}
		if got.YearCorrection.Pending != wantPending {
			t.Errorf("%s: year correction pending %d, movies %d", stage, got.YearCorrection.Pending, wantPending)
		}
		if n, err := c.countPendingMovieYears(ctx); err != nil || n != wantPending {
			t.Errorf("%s: pending movie years %d (%v), movies %d", stage, n, err, wantPending)
		}
	}
	check("initial")

	// Every counted column of every table, and keys moving under rows.
	for i, k := range keys {
		switch i % 9 {
		case 0:
			exec(`UPDATE catalog_titles SET browsable=1-browsable WHERE key=?`, k)
		case 1:
			exec(`UPDATE catalog_titles SET released_ts=? WHERE key=?`, released(), k)
		case 2:
			exec(`UPDATE catalog_titles SET adult=1-adult WHERE key=?`, k)
		case 3:
			exec(`UPDATE catalog_titles SET year_source=? WHERE key=?`, yearSources[rng.IntN(len(yearSources))], k)
		case 4:
			exec(`UPDATE catalog_titles SET type=? WHERE key=?`, types[rng.IntN(len(types))], k)
		case 5:
			// The catalogue's own check writes: a stamp, then a result.
			exec(`INSERT INTO catalog_home_release(key,attempted_at) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET attempted_at=excluded.attempted_at`, k, now.Unix())
			if rng.IntN(2) == 0 {
				exec(`INSERT INTO catalog_home_release(key,checked,earliest_ts,attempted_at) VALUES(?,1,?,?) ON CONFLICT(key) DO UPDATE SET checked=1,earliest_ts=excluded.earliest_ts,attempted_at=excluded.attempted_at`, k, now.Unix(), now.Unix())
			}
		case 6:
			exec(`UPDATE catalog_home_release SET checked=? WHERE key=?`, rng.IntN(3), k)
		case 7:
			exec(`DELETE FROM catalog_home_release WHERE key=?`, k)
		case 8:
			exec(`UPDATE catalog_titles SET popularity=popularity+1, title='renamed' WHERE key=?`, k)
		}
	}
	exec(`UPDATE catalog_titles SET key=? WHERE key=?`, 90_001, keys[7]) // onto an orphan check row
	exec(`DELETE FROM catalog_home_release WHERE key=?`, keys[11])
	exec(`UPDATE catalog_home_release SET key=? WHERE key=?`, keys[11], 90_002)
	exec(`DELETE FROM catalog_titles WHERE key IN (?,?,?)`, keys[20], keys[21], keys[22])
	exec(`DELETE FROM catalog_collections WHERE tmdb_id IN (3,4)`)
	exec(`INSERT INTO catalog_collections(tmdb_id,name) VALUES(50,'new') ON CONFLICT(tmdb_id) DO UPDATE SET name=excluded.name`)
	exec(`INSERT INTO catalog_collections(tmdb_id,name) VALUES(5,'renamed') ON CONFLICT(tmdb_id) DO UPDATE SET name=excluded.name`)
	// Titles written through the catalogue's own upsert.
	if err := c.UpsertTitle(ctx, core.Title{Key: core.MovieKey(700), Type: core.TypeMovie, Title: "New", Released: now.Add(-time.Hour * 100)}, true); err != nil {
		t.Fatal(err)
	}
	if err := c.UpsertTitle(ctx, core.Title{Key: core.ShowKey(700), Type: core.TypeShow, Title: "New show", Released: now.Add(time.Hour)}, true); err != nil {
		t.Fatal(err)
	}
	check("after writes")

	if _, err := c.foldLibraryTallies(ctx); err != nil {
		t.Fatal(err)
	}
	var pending int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM catalog_stats_tally_delta`).Scan(&pending); err != nil || pending != 0 {
		t.Fatalf("pending deltas after a fold: %d %v", pending, err)
	}
	check("after a fold")

	// Movies due in the coming days are released as the clock moves.
	now = now.Add(30 * time.Hour)
	check("a day later")
	exec(`UPDATE catalog_titles SET browsable=1 WHERE key=?`, keys[30])
	exec(`DELETE FROM catalog_collections WHERE tmdb_id=6`)
	if err := RebuildLibraryCounts(ctx, db); err != nil {
		t.Fatal(err)
	}
	check("after a rebuild")
	exec(`DELETE FROM catalog_titles`)
	exec(`DELETE FROM catalog_collections`)
	if _, err := c.foldLibraryTallies(ctx); err != nil {
		t.Fatal(err)
	}
	check("emptied")
	var rows int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM catalog_stats_tally`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("tally rows left on an empty catalogue: %d %v", rows, err)
	}
}

func countsOf(s Stats) [9]int {
	return [9]int{s.Movies, s.Shows, s.Seasons, s.Episodes, s.BrowsableMovies, s.BrowsableShows, s.Collections, s.HomeReleaseChecked, s.HomeReleasePending}
}
