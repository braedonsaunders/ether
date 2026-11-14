package catalog

import (
	"context"
	"database/sql"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Run explicitly: ETHER_CATALOG_PERF=1 go test ./internal/catalog -run TestConsole600K -count=1 -v.
// Cold page caches, warm database pages. Counts are deliberately excluded from
// interactive pages and refreshed independently. A 150ms p95 is the target;
// this test reports violations instead of hiding them behind warmed result caches.
func TestConsole600K(t *testing.T) {
	if os.Getenv("ETHER_CATALOG_PERF") != "1" {
		t.Skip("set ETHER_CATALOG_PERF=1 for the 600k-title performance fixture")
	}
	// Exercise the production file-backed database. Deserialized in-memory
	// fixtures have a SQLite memory-file limit below this catalogue size.
	var db *sql.DB
	var err error
	if os.Getenv(store.TestPostgresEnv) != "" {
		db, err = store.OpenMemory(context.Background())
	} else {
		db, err = store.Open(context.Background(), t.TempDir())
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	c, err := New(db, nil, config.Catalog{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	sqlite := store.DialectOf(c.db) != store.Postgres
	// SQLite bulk import defers FTS row triggers and rebuilds afterward;
	// PostgreSQL maintains its production GIN index during this bulk insert.
	if sqlite {
		if _, err := c.db.Exec(`DROP TRIGGER catalog_titles_fts_insert`); err != nil {
			t.Fatal(err)
		}
	}
	t.Log("loading 600k fixture")
	insert := `WITH RECURSIVE seq(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM seq WHERE i<600000)
      INSERT INTO catalog_titles(key,type,tmdb_id,title,sort_title,year,released_ts,popularity,rating,vote_count,runtime_sec,language,browsable,adult)
      SELECT CAST(? AS BIGINT)+i,'movie',i,printf('Title %06d',i),printf('Title %06d',i),1970+(i%55),1600000000,600000-i,4+(i%60)/10.0,i%10000,3600+(i%180)*60,CASE WHEN i%3=0 THEN 'fr' ELSE 'en' END,1,0 FROM seq`
	if !sqlite {
		insert = strings.ReplaceAll(insert, "printf('Title %06d',i)", "('Title ' || lpad(CAST(i AS TEXT),6,'0'))")
	}
	if _, err := c.db.ExecContext(context.Background(), insert, int64(core.MovieKey(0))); err != nil {
		t.Fatal(err)
	}
	if sqlite {
		t.Log("rebuilding FTS")
		if _, err := c.db.Exec(`INSERT INTO catalog_titles_fts(catalog_titles_fts) VALUES('rebuild'); CREATE TRIGGER catalog_titles_fts_insert AFTER INSERT ON catalog_titles BEGIN INSERT INTO catalog_titles_fts(rowid,title,original_title) VALUES(new.key,new.title,new.original_title); END;`); err != nil {
			t.Fatal(err)
		}
	}
	genreSeed := `INSERT INTO catalog_genres(key,genre) SELECT key,CASE WHEN tmdb_id%2=0 THEN 'Thriller' ELSE 'Comedy' END FROM catalog_titles`
	if sqlite {
		if _, err := c.db.Exec(genreSeed); err != nil {
			t.Fatal(err)
		}
	} else {
		// Production metadata transactions are bounded per batch. Keep the same
		// trigger/advisory-lock behavior instead of enlarging PG's lock table.
		for start := 0; start < 600000; start += 2000 {
			if _, err := c.db.ExecContext(context.Background(), genreSeed+` WHERE key>? AND key<=?`, int64(core.MovieKey(int64(start))), int64(core.MovieKey(int64(min(start+2000, 600000))))); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := c.db.Exec(`ANALYZE`); err != nil {
		t.Fatal(err)
	}
	c.SetConsoleAvailabilityPolicy(func() string { return "perf-policy" })
	until := time.Now().Add(time.Hour).Unix()
	for _, seed := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO catalog_release_evidence(rating_key,release_id,source_id,policy,verified_until,cache_until,cached,height,hdr,dv,atmos) SELECT key,'fixture-release','fixture-source','perf-policy',?,?,1,2160,1,0,1 FROM catalog_titles WHERE tmdb_id%20=0`, []any{until, until}},
		{`INSERT INTO state_watch(user_id,rating_key,view_count) SELECT 7,key,1 FROM catalog_titles WHERE tmdb_id%40=0`, nil},
		{`CREATE TABLE IF NOT EXISTS watchlist_items(user_id BIGINT NOT NULL,item_key BIGINT NOT NULL,source TEXT NOT NULL,title TEXT NOT NULL,added BIGINT NOT NULL,checked INTEGER NOT NULL DEFAULT 0,available INTEGER NOT NULL DEFAULT 0,notified INTEGER NOT NULL DEFAULT 0,warmed INTEGER NOT NULL DEFAULT 0,PRIMARY KEY(user_id,item_key,source)); CREATE INDEX IF NOT EXISTS watchlist_viewer ON watchlist_items(user_id,available,added)`, nil},
		{`INSERT INTO watchlist_items(user_id,item_key,source,title,added) SELECT 7,key,'local',title,1600000000 FROM catalog_titles WHERE tmdb_id%20=0`, nil},
		{`ANALYZE`, nil},
	} {
		if _, err := c.db.ExecContext(context.Background(), seed.sql, seed.args...); err != nil {
			t.Fatal(err)
		}
	}
	t.Log("fixture ready")
	cases := []struct {
		name string
		q    core.BrowseQuery
	}{
		{"popular", core.BrowseQuery{Filter: core.Filter{Type: core.TypeMovie}, Sort: core.SortPopularity, Desc: true, Limit: 49, NoTotal: true}},
		{"90s-thriller-under2h-rating7", core.BrowseQuery{Filter: core.Filter{Type: core.TypeMovie, YearFrom: 1990, YearTo: 1999, Genres: []string{"Thriller"}, MinRating: 7, MaxRuntime: 2 * time.Hour}, Sort: core.SortPopularity, Desc: true, Limit: 49, NoTotal: true}},
		{"rare-language-votes", core.BrowseQuery{Filter: core.Filter{Type: core.TypeMovie, Languages: []string{"fr"}, MinVotes: 9900}, Sort: core.SortRating, Desc: true, Limit: 49, NoTotal: true}},
		{"or-secondary", core.BrowseQuery{Filter: core.Filter{Type: core.TypeMovie, Group: &core.FilterGroup{Operator: "or", Children: []core.FilterGroup{{Filter: &core.Filter{YearTo: 1971}}, {Filter: &core.Filter{MinRating: 9.8}}}}}, Sort: core.SortYear, Desc: true, SecondarySorts: []core.SortSpec{{Sort: core.SortTitle}}, Limit: 49, NoTotal: true}},
		{"zero-match", core.BrowseQuery{Filter: core.Filter{Type: core.TypeMovie, YearFrom: 2199, Genres: []string{"Thriller"}, MinRating: 9.9}, Sort: core.SortPopularity, Desc: true, Limit: 49, NoTotal: true}},
	}
	wireTypical := core.Filter{Group: &core.FilterGroup{Operator: "and", Children: []core.FilterGroup{{Operator: "and", Children: []core.FilterGroup{
		{Filter: &core.Filter{YearFrom: 1990, YearTo: 1999}}, {Filter: &core.Filter{Genres: []string{"Thriller"}}}, {Filter: &core.Filter{MinRating: 7}}, {Filter: &core.Filter{MaxRuntime: 2 * time.Hour}},
	}}}}}
	cases = append(cases, struct {
		name string
		q    core.BrowseQuery
	}{"wire-all-titles-typical", core.BrowseQuery{Filter: wireTypical, Sort: core.SortPopularity, Desc: true, Limit: 49, NoTotal: true}})
	wireOR := cases[3].q
	wireOR.Filter.Type = ""
	cases = append(cases, struct {
		name string
		q    core.BrowseQuery
	}{"wire-all-titles-or-secondary", wireOR})
	deep := cases[0].q
	deep.AfterKey = core.MovieKey(540000)
	cases = append(cases, struct {
		name string
		q    core.BrowseQuery
	}{"deep-keyset-popular-540k", deep})
	deepOR := wireOR
	deepOR.AfterKey = core.MovieKey(550000)
	cases = append(cases, struct {
		name string
		q    core.BrowseQuery
	}{"deep-keyset-or-secondary", deepOR})
	deepWire := core.BrowseQuery{Filter: wireTypical, Sort: core.SortPopularity, Desc: true, Limit: 49, NoTotal: true, AfterKey: core.MovieKey(540000)}
	cases = append(cases, struct {
		name string
		q    core.BrowseQuery
	}{"deep-keyset-wire-typical-540k", deepWire})
	deepReleased := cases[0].q
	deepReleased.Sort = core.SortReleased
	deepReleased.AfterKey = core.MovieKey(540000)
	cases = append(cases, struct {
		name string
		q    core.BrowseQuery
	}{"deep-keyset-identical-release-540k", deepReleased})
	deepRating := cases[0].q
	deepRating.Sort = core.SortRating
	deepRating.AfterKey = core.MovieKey(540000)
	cases = append(cases, struct {
		name string
		q    core.BrowseQuery
	}{"deep-keyset-rating-ties-540k", deepRating})
	unwatched, onWatchlist := true, true
	persona := wireTypical
	persona.Group = &core.FilterGroup{Operator: "and", Children: []core.FilterGroup{
		*wireTypical.Group, {Filter: &core.Filter{Playable: "yes", Qualities: []string{"4k"}}}, {Unwatched: &unwatched, UserID: 7},
	}}
	cases = append(cases, struct {
		name string
		q    core.BrowseQuery
	}{"wire-persona-unwatched-90s-thriller-7-under2h-playable4k", core.BrowseQuery{Filter: persona, Sort: core.SortPopularity, Desc: true, Limit: 49, NoTotal: true}})
	cases = append(cases, struct {
		name string
		q    core.BrowseQuery
	}{"viewer-watchlist-cached4k", core.BrowseQuery{Filter: core.Filter{Playable: "yes", DebridCached: "yes", Qualities: []string{"4k"}, Group: &core.FilterGroup{OnWatchlist: &onWatchlist, UserID: 7}}, Sort: core.SortPopularity, Desc: true, Limit: 49, NoTotal: true}})
	cases = append(cases, struct {
		name string
		q    core.BrowseQuery
	}{"unknown-genre-valid-years", core.BrowseQuery{Filter: core.Filter{Type: core.TypeMovie, YearFrom: 1990, YearTo: 1999, Genres: []string{"Nonexistent genre"}}, Sort: core.SortPopularity, Desc: true, Limit: 49, NoTotal: true}})
	cases = append(cases, struct {
		name string
		q    core.BrowseQuery
	}{"disjoint-genre-and-valid-years", core.BrowseQuery{Filter: core.Filter{Type: core.TypeMovie, YearFrom: 1990, YearTo: 1999, Genres: []string{"Comedy"}, Group: &core.FilterGroup{Filter: &core.Filter{Genres: []string{"Thriller"}}}}, Sort: core.SortPopularity, Desc: true, Limit: 49, NoTotal: true}})
	converted := cases[1].q
	converted.Filter = core.Filter{Type: core.TypeMovie, ExtraRules: []core.LibraryExclusion{{Action: "show", MediaType: "movie", Match: "all", Rules: core.LibraryPredicateConditions(cases[1].q.Filter)}}, ExtraFallback: "hide"}
	cases = append(cases, struct {
		name string
		q    core.BrowseQuery
	}{"library-visible-rules", converted})
	samples := 31
	if raw := os.Getenv("ETHER_CATALOG_SAMPLES"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 3 || n > 101 {
			t.Fatal("ETHER_CATALOG_SAMPLES must be 3..101")
		}
		samples = n
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.q.Filter.LocalOnly = true
			tc.q.CursorPage = true
			if os.Getenv("ETHER_CATALOG_PLAN") == "1" && len(normalizeConsoleFilter(tc.q.Filter).Qualities) > 0 {
				consoleEvidencePlan(t, c, tc.q)
			}
			times := make([]time.Duration, samples)
			for i := range times {
				c.cacheMu.Lock()
				c.browseCache = map[string]cachedBrowse{}
				c.cacheMu.Unlock()
				start := time.Now()
				_, err := c.Browse(context.Background(), tc.q)
				times[i] = time.Since(start)
				if err != nil {
					t.Fatal(err)
				}
			}
			sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
			p50, p95 := times[len(times)/2], times[int(math.Ceil(float64(len(times))*.95))-1]
			t.Logf("600k cold-result-cache n=%d p50=%s p95=%s target=150ms", samples, p50, p95)
			if p95 > 150*time.Millisecond {
				t.Errorf("p95 exceeds interactive budget: %s", p95)
			}
		})
	}
}
