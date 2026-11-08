package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/contentrating"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

// A library page's total must be the number of titles its pages return, for
// every viewer restriction, availability hidden set, release setting and
// clock, through every kind of catalogue write and tally fold. The tally
// answers the common listings; this pins it to the direct count and to the
// pages themselves.
func TestBrowseTallyTotalsMatchPages(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	c, err := New(db, nil, config.Catalog{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	c.SetNow(func() time.Time { return now })
	rng := rand.New(rand.NewPCG(7, 11))
	ratings := []string{"", "G", "PG", "PG-13", "R", "NC-17", "TV-Y", "TV-14", "TV-MA", "XYZ"}
	genres := []string{"Drama", "Comedy", "Kids", "Horror", ""}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	released := func() int64 {
		switch rng.IntN(8) {
		case 0:
			return 0 // unknown
		case 1:
			return now.Add(time.Duration(rng.IntN(48)) * time.Hour).Unix() // coming days
		case 2:
			return now.Add(-time.Duration(rng.IntN(3600)) * time.Second).Unix() // the last hour
		case 3:
			return -int64(rng.IntN(1_000_000_000)) // before 1970
		default:
			return now.Add(-time.Duration(rng.IntN(20_000)) * time.Hour).Unix()
		}
	}
	setGenres := func(key core.RatingKey) {
		exec(`DELETE FROM catalog_genres WHERE key=?`, int64(key))
		for _, g := range genres {
			if rng.IntN(3) == 0 {
				exec(`INSERT INTO catalog_genres(key,genre) VALUES(?,?) ON CONFLICT DO NOTHING`, int64(key), g)
			}
		}
	}
	var keys []core.RatingKey
	insert := func(key core.RatingKey, typ core.MediaType) {
		r := ratings[rng.IntN(len(ratings))]
		exec(`INSERT INTO catalog_titles(key,type,title,sort_title,released_ts,browsable,adult,detail,content_rating,content_rating_rank,popularity,vote_count)
			VALUES(?,?,?,?,?,?,?,1,?,?,?,?)`, int64(key), string(typ), fmt.Sprint("t", int64(key)), fmt.Sprint("t", int64(key)), released(),
			flagInt(rng.IntN(5) > 0), flagInt(rng.IntN(10) == 0), r, int(contentrating.LevelOf(r)), float64(rng.IntN(20)), rng.IntN(50))
		setGenres(key)
		keys = append(keys, key)
	}
	for i := int64(1); i <= 140; i++ {
		insert(core.MovieKey(i), core.TypeMovie)
	}
	for i := int64(1); i <= 70; i++ {
		insert(core.ShowKey(i), core.TypeShow)
	}
	// An episode and a season never count toward a movie or series library.
	exec(`INSERT INTO catalog_titles(key,type,title,sort_title,released_ts,browsable,adult,detail) VALUES(?,?,?,?,?,1,0,1)`, int64(core.EpisodeKey(1, 1, 1)), "episode", "e", "e", now.Add(-time.Hour*30).Unix())
	// The availability component owns this table; its schema as there.
	exec(`CREATE TABLE IF NOT EXISTS availability_hidden(kind TEXT NOT NULL,generation TEXT NOT NULL,rating_key BIGINT NOT NULL,created_at BIGINT NOT NULL,PRIMARY KEY(kind,generation,rating_key))`)
	for _, k := range keys {
		if rng.IntN(4) == 0 {
			kind := "movie"
			if k.Kind() == core.KindShow {
				kind = "show"
			}
			exec(`INSERT INTO availability_hidden(kind,generation,rating_key,created_at) VALUES(?,?,?,0)`, kind, "g-"+kind, int64(k))
		}
	}
	excluded := []core.RatingKey{keys[3], keys[50], keys[150], core.MovieKey(9999)}
	filters := []core.Filter{
		{Type: core.TypeMovie},
		{Type: core.TypeShow},
		{},
		{Type: core.TypeMovie, Genres: []string{"Drama"}},
		{Type: core.TypeShow, Kids: true},
		{Genres: []string{"Kids"}, Kids: true},
		{Type: core.TypeMovie, HiddenMovieGeneration: "g-movie"},
		{Type: core.TypeShow, HiddenShowGeneration: "g-show", Genres: []string{"Comedy"}},
		{HiddenMovieGeneration: "g-movie", HiddenShowGeneration: "g-show"},
		{Type: core.TypeMovie, HiddenMovieGeneration: "g-movie", DigitalDiscovery: true, DigitalDateEvidence: true},
		{Type: core.TypeShow, HiddenShowGeneration: "g-show", DigitalDiscovery: true},
		// A restricted viewer: rating cap, key exclusions, empty block lists.
		{Type: core.TypeMovie, MaxContentRating: "PG-13", ExcludeKeys: excluded, ExcludeGenres: []string{}, ExcludeKeywords: []string{}, CatalogViewerID: 5, HiddenMovieGeneration: "g-movie"},
		{Type: core.TypeShow, MinContentRating: "PG", MaxContentRating: "TV-MA", Genres: []string{"Horror"}, ExcludeKeys: excluded},
		{MaxContentRating: "R", KnownAddedDate: true},
		// Counted directly: these must still agree with their pages.
		{Type: core.TypeMovie, Genres: []string{"Drama", "Comedy"}},
		{Type: core.TypeMovie, ExcludeGenres: []string{"horror"}},
		{Type: core.TypeMovie, MaxContentRating: "XYZ"},
		{Type: core.TypeMovie, YearFrom: 1990},
		{Type: core.TypeMovie, IncludeKeys: []core.RatingKey{}},
		{Type: core.TypeShow, IncludeKeys: []core.RatingKey{keys[150], keys[151]}},
		{Type: core.TypeMovie, DigitalDiscovery: true, DigitalDateEvidence: true},
	}
	configs := []config.Catalog{
		{},
		{ShowUpcomingMovies: true},
		{ShowUpcomingMovies: true, MinPopularity: 5, MinVotes: 10},
		{ReleaseMode: config.ModeDigital},
		{ReleaseMode: config.ModeDigital, ShowUpcomingMovies: true},
		{Exclusions: []core.LibraryExclusion{{MediaType: "mixed", Match: "all", Rules: []core.CollectionRule{{Field: "genre", Op: "in", Value: json.RawMessage(`["Horror"]`)}}}}},
	}
	tallied := 0
	check := func(stage string) {
		t.Helper()
		for ci, cfg := range configs {
			c.UpdateConfig(cfg)
			for fi, f := range filters {
				ff := c.viewerPaging(ctx, f)
				where, args, err := c.browseWhere(ff)
				if err != nil {
					t.Fatal(err)
				}
				var exact int
				if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM catalog_titles t WHERE `+where, args...).Scan(&exact); err != nil {
					t.Fatal(err)
				}
				got, ok, err := c.tallyTotal(ctx, ff, where, args)
				if err != nil {
					t.Fatalf("%s config %d filter %d: %v", stage, ci, fi, err)
				}
				if ok {
					tallied++
					if got != exact {
						t.Errorf("%s config %d filter %d: tally total %d, titles %d", stage, ci, fi, got, exact)
					}
				}
				page, err := c.Browse(ctx, core.BrowseQuery{Filter: f, Sort: core.SortTitle, Limit: 500})
				if err != nil {
					t.Fatal(err)
				}
				if page.Total != exact || len(page.Items) != exact {
					t.Errorf("%s config %d filter %d: total %d, page %d, titles %d", stage, ci, fi, page.Total, len(page.Items), exact)
				}
			}
		}
	}
	check("initial")
	if tallied == 0 {
		t.Fatal("no total came from the tally")
	}

	// Membership changes through every column and both tables.
	for i, k := range keys {
		switch i % 7 {
		case 0:
			exec(`UPDATE catalog_titles SET browsable=1-browsable WHERE key=?`, int64(k))
		case 1:
			exec(`UPDATE catalog_titles SET content_rating_rank=?, content_rating='R' WHERE key=?`, int(contentrating.LevelOf("R")), int64(k))
		case 2:
			setGenres(k)
		case 3:
			exec(`UPDATE catalog_titles SET released_ts=? WHERE key=?`, released(), int64(k))
		case 4:
			exec(`UPDATE catalog_titles SET adult=1-adult WHERE key=?`, int64(k))
		case 5:
			exec(`UPDATE catalog_genres SET genre='Drama' WHERE key=? AND genre='Comedy' AND NOT EXISTS (SELECT 1 FROM catalog_genres d WHERE d.key=catalog_genres.key AND d.genre='Drama')`, int64(k))
		}
	}
	exec(`DELETE FROM catalog_titles WHERE key IN (?,?)`, int64(keys[10]), int64(keys[160]))
	exec(`DELETE FROM catalog_genres WHERE key=?`, int64(keys[11]))
	// A title written through the catalogue's own upsert.
	if err := c.UpsertTitle(ctx, core.Title{Key: core.MovieKey(500), Type: core.TypeMovie, Title: "New", Released: now.Add(-time.Hour * 100), ContentRating: "PG", Genres: []string{"Drama", "Kids"}}, true); err != nil {
		t.Fatal(err)
	}
	check("after writes")

	if _, err := c.foldLibraryTallies(ctx); err != nil {
		t.Fatal(err)
	}
	var pending int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM catalog_browse_tally_delta`).Scan(&pending); err != nil || pending != 0 {
		t.Fatalf("pending deltas after a fold: %d %v", pending, err)
	}
	check("after a fold")

	// Titles due in the coming days are released as the clock moves.
	now = now.Add(30 * time.Hour)
	check("a day later")
	exec(`UPDATE catalog_titles SET browsable=1 WHERE key=?`, int64(keys[20]))
	if err := RebuildLibraryCounts(ctx, db); err != nil {
		t.Fatal(err)
	}
	check("after a rebuild")
}

// A restricted viewer's whole-library episode total (Plex allLeaves) must be
// the number of episodes its pages return: the series rating histograms
// stand in for a join over every episode, so they are pinned to it through
// episode and series writes, inherited ratings and the clock.
func TestRestrictedEpisodeTotalsMatchPages(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	c, err := New(db, nil, config.Catalog{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	c.SetNow(func() time.Time { return now })
	rng := rand.New(rand.NewPCG(3, 5))
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`CREATE TABLE IF NOT EXISTS availability_hidden(kind TEXT NOT NULL,generation TEXT NOT NULL,rating_key BIGINT NOT NULL,created_at BIGINT NOT NULL,PRIMARY KEY(kind,generation,rating_key))`)
	ratings := []string{"", "TV-Y", "TV-PG", "TV-14", "TV-MA", "NR", "PG-13"}
	collections := []string{"", "", "Hidden", " hidden ", "Other"}
	released := func() int64 {
		switch rng.IntN(6) {
		case 0:
			return 0
		case 1:
			return now.Add(time.Duration(rng.IntN(72)) * time.Hour).Unix()
		default:
			return now.Add(-time.Duration(rng.IntN(20_000)) * time.Hour).Unix()
		}
	}
	rate := func() (string, int) {
		r := ratings[rng.IntN(len(ratings))]
		return r, int(contentrating.LevelOf(r))
	}
	var episodes []core.RatingKey
	for s := int64(1); s <= 25; s++ {
		r, rank := rate()
		exec(`INSERT INTO catalog_titles(key,type,title,sort_title,released_ts,browsable,adult,detail,content_rating,content_rating_rank,collection_name) VALUES(?,'show','s','s',?,1,0,1,?,?,?)`,
			int64(core.ShowKey(s)), now.Add(-time.Hour*24*400).Unix(), r, rank, collections[rng.IntN(len(collections))])
		for e := 1; e <= 8; e++ {
			key := core.EpisodeKey(s, 1+e/4, e)
			r, rank := rate()
			exec(`INSERT INTO catalog_titles(key,type,title,sort_title,show_key,season,episode,released_ts,browsable,adult,detail,content_rating,content_rating_rank,collection_name) VALUES(?,'episode','e','e',?,?,?,?,0,?,1,?,?,?)`,
				int64(key), int64(core.ShowKey(s)), 1+e/4, e, released(), flagInt(rng.IntN(12) == 0), r, rank, collections[rng.IntN(len(collections))])
			episodes = append(episodes, key)
			if rng.IntN(6) == 0 {
				exec(`INSERT INTO availability_hidden(kind,generation,rating_key,created_at) VALUES('episode','g-ep',?,0)`, int64(key))
			}
		}
	}
	share := core.Filter{MaxContentRating: "TV-14", ExcludeContentRatings: []string{"TV-MA", "NR"}, ExcludeCollectionNames: []string{"hidden"}}
	filters := []core.Filter{
		{},
		{ChildVisibility: &core.Filter{MaxContentRating: "TV-14"}, MaxContentRating: "TV-14"},
		{ChildVisibility: &share, MaxContentRating: "TV-14", ExcludeCollectionNames: []string{"hidden"}, HiddenEpisodeGeneration: "g-ep", ExcludeKeys: []core.RatingKey{episodes[3], episodes[40]}},
		{ChildVisibility: &core.Filter{MinContentRating: "TV-PG", ContentRatings: []string{"TV-PG", "TV-14", "NR"}, IncludeCollectionNames: []string{"other"}}},
		{ChildVisibility: &core.Filter{MaxContentRating: "XYZ"}},
	}
	check := func(stage string) {
		t.Helper()
		c.writeGen.Add(1)
		for fi, f := range filters {
			ff := c.viewerPaging(ctx, f)
			if _, ok, err := c.countStoredEpisodesBySeries(ctx, ff); err != nil || !ok {
				t.Fatalf("%s filter %d: not counted by series (%v)", stage, fi, err)
			}
			total, err := c.CountStoredEpisodes(ctx, f)
			if err != nil {
				t.Fatal(err)
			}
			page, err := c.StoredEpisodesByKey(ctx, f, 0, 10_000)
			if err != nil {
				t.Fatal(err)
			}
			if total != len(page) {
				t.Errorf("%s filter %d: total %d, episodes %d", stage, fi, total, len(page))
			}
		}
	}
	check("initial")
	for i, k := range episodes {
		switch i % 6 {
		case 0:
			r, rank := rate()
			exec(`UPDATE catalog_titles SET content_rating=?, content_rating_rank=? WHERE key=?`, r, rank, int64(k))
		case 1:
			exec(`UPDATE catalog_titles SET collection_name=? WHERE key=?`, collections[rng.IntN(len(collections))], int64(k))
		case 2:
			exec(`UPDATE catalog_titles SET released_ts=? WHERE key=?`, released(), int64(k))
		case 3:
			exec(`UPDATE catalog_titles SET adult=1-adult WHERE key=?`, int64(k))
		case 4:
			exec(`UPDATE catalog_titles SET show_key=? WHERE key=?`, int64(core.ShowKey(int64(1+rng.IntN(25)))), int64(k))
		}
	}
	exec(`DELETE FROM catalog_titles WHERE key IN (?,?)`, int64(episodes[7]), int64(episodes[70]))
	r, rank := rate()
	exec(`UPDATE catalog_titles SET content_rating=?, content_rating_rank=? WHERE key=?`, r, rank, int64(core.ShowKey(2)))
	check("after writes")
	now = now.Add(48 * time.Hour)
	check("two days later")
	if err := RebuildLibraryCounts(ctx, db); err != nil {
		t.Fatal(err)
	}
	check("after a rebuild")
}
