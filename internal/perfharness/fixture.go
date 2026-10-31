package perfharness

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// Scale sizes a synthetic library. Every title is browsable, detailed and
// fresh, so a request never needs TMDB: the fixture measures Ether, not a
// provider.
type Scale struct {
	Movies            int
	Shows             int
	SeasonsPerShow    int
	EpisodesPerSeason int
	// Viewers beyond the owner, each with HistoryPerViewer watched titles
	// and InProgressPerViewer partly watched ones.
	Viewers             int
	HistoryPerViewer    int
	InProgressPerViewer int
}

// Rows is the number of catalogue rows the scale produces.
func (s Scale) Rows() int {
	return s.Movies + s.Shows*(1+s.SeasonsPerShow*(1+s.EpisodesPerSeason))
}

// Divide shrinks the library by n, keeping the household.
func (s Scale) Divide(n int) Scale {
	s.Movies, s.Shows, s.HistoryPerViewer = max(s.Movies/n, 1), max(s.Shows/n, 1), max(s.HistoryPerViewer/n, 1)
	return s
}

// LargeScale is the CI gate's library: 500k+ catalogue rows and a
// household with deep watch history.
var LargeScale = Scale{Movies: 250_000, Shows: 10_000, SeasonsPerShow: 4, EpisodesPerSeason: 6, Viewers: 12, HistoryPerViewer: 20_000, InProgressPerViewer: 40}

// SmallScale is a tenth of LargeScale; the gate compares the two.
var SmallScale = Scale{Movies: 25_000, Shows: 1_000, SeasonsPerShow: 4, EpisodesPerSeason: 6, Viewers: 12, HistoryPerViewer: 2_000, InProgressPerViewer: 40}

var fixtureGenres = []string{"Action", "Adventure", "Animation", "Comedy", "Crime", "Documentary", "Drama", "Family", "Fantasy", "History", "Horror", "Music", "Mystery", "Romance", "Science Fiction", "Thriller", "War", "Western", "TV Movie"}

var fixtureWords = []string{"Star", "Night", "River", "Empire", "Shadow", "Garden", "Signal", "Harbor", "Winter", "Glass", "Echo", "Stone", "Summer", "Silver", "Iron", "Paper", "Ghost", "Ocean", "Crown", "Wild"}

// Generate fills an empty, migrated database with a synthetic library and
// household at scale s. The output is deterministic for a given scale and
// now.
func Generate(ctx context.Context, db *sql.DB, s Scale, now time.Time) error {
	rng := rand.New(rand.NewPCG(42, uint64(s.Rows())))
	g := &generator{ctx: ctx, db: db, rng: rng, now: now}
	// Per-row triggers (search index, policy generation) would dominate the
	// load; they are set aside and their effect rebuilt once at the end.
	restore, err := suspendTriggers(ctx, db, "catalog_titles", "catalog_genres", "catalog_countries", "state_watch", "state_history")
	if err != nil {
		return err
	}
	if err := g.titles(s); err != nil {
		return fmt.Errorf("titles: %w", err)
	}
	if err := g.viewers(s); err != nil {
		return fmt.Errorf("viewers: %w", err)
	}
	if err := restore(); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO catalog_titles_fts(catalog_titles_fts) VALUES('rebuild')`); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `ANALYZE`); err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
	return err
}

// suspendTriggers drops the triggers on tables and returns a function that
// recreates them.
func suspendTriggers(ctx context.Context, db *sql.DB, tables ...string) (func() error, error) {
	in := strings.TrimSuffix(strings.Repeat("?,", len(tables)), ",")
	args := make([]any, len(tables))
	for i, t := range tables {
		args[i] = t
	}
	rows, err := db.QueryContext(ctx, `SELECT name, sql FROM sqlite_master WHERE type='trigger' AND tbl_name IN (`+in+`)`, args...)
	if err != nil {
		return nil, err
	}
	var names, defs []string
	for rows.Next() {
		var name, def string
		if err := rows.Scan(&name, &def); err != nil {
			rows.Close()
			return nil, err
		}
		names, defs = append(names, name), append(defs, def)
	}
	rows.Close()
	for _, name := range names {
		if _, err := db.ExecContext(ctx, `DROP TRIGGER "`+name+`"`); err != nil {
			return nil, err
		}
	}
	return func() error {
		for _, def := range defs {
			if _, err := db.ExecContext(ctx, def); err != nil {
				return err
			}
		}
		return nil
	}, nil
}

type generator struct {
	ctx context.Context
	db  *sql.DB
	rng *rand.Rand
	now time.Time

	tx      *sql.Tx
	pending int
	flushes int
	batches map[string]*pendingInsert
	movies  []core.RatingKey
	eps     []core.RatingKey
}

// Rows are written as multi-row INSERTs of fixtureRows rows, committed
// every fixtureBatch rows.
const (
	fixtureBatch = 20000
	fixtureRows  = 100
)

type pendingInsert struct {
	table, cols string
	width, rows int
	args        []any
}

// insert queues one row for table(cols); args must match cols.
func (g *generator) insert(table, cols string, args ...any) error {
	key := table + "(" + cols + ")"
	b := g.batches[key]
	if b == nil {
		if g.batches == nil {
			g.batches = map[string]*pendingInsert{}
		}
		b = &pendingInsert{table: table, cols: cols, width: len(args)}
		g.batches[key] = b
	}
	b.args = append(b.args, args...)
	b.rows++
	g.pending++
	if b.rows >= fixtureRows {
		if err := g.write(b); err != nil {
			return err
		}
	}
	if g.pending >= fixtureBatch {
		return g.flush()
	}
	return nil
}

func (g *generator) write(b *pendingInsert) error {
	if b.rows == 0 {
		return nil
	}
	if g.tx == nil {
		tx, err := g.db.BeginTx(g.ctx, nil)
		if err != nil {
			return err
		}
		g.tx = tx
	}
	tuple := "(" + strings.TrimSuffix(strings.Repeat("?,", b.width), ",") + ")"
	q := "INSERT INTO " + b.table + "(" + b.cols + ") VALUES " + strings.TrimSuffix(strings.Repeat(tuple+",", b.rows), ",")
	if _, err := g.tx.ExecContext(g.ctx, q, b.args...); err != nil {
		g.tx.Rollback()
		g.tx = nil
		return err
	}
	b.args, b.rows = b.args[:0], 0
	return nil
}

func (g *generator) flush() error {
	g.pending = 0
	for _, b := range g.batches {
		if err := g.write(b); err != nil {
			return err
		}
	}
	if g.tx == nil {
		return nil
	}
	err := g.tx.Commit()
	g.tx = nil
	if err != nil {
		return err
	}
	// store opens SQLite with automatic checkpoints off (the server runs a
	// background checkpointer); without one the WAL grows by gigabytes and
	// every later statement slows down.
	g.flushes++
	if g.flushes%20 == 0 {
		_, err = g.db.ExecContext(g.ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
	}
	return err
}

const titleCols = `key,type,tmdb_id,imdb,show_key,show_tmdb,show_title,season_title,season,episode,title,original_title,sort_title,summary,year,released_ts,runtime_sec,status,language,content_rating,rating,vote_count,popularity,poster,backdrop,child_count,leaf_count,browsable,added_at,fetched_at,detail,detail_at`

func (g *generator) name(n int) string {
	return fixtureWords[g.rng.IntN(len(fixtureWords))] + " " + fixtureWords[g.rng.IntN(len(fixtureWords))] + fmt.Sprintf(" %d", n)
}

func (g *generator) released() time.Time {
	// Spread over 1950 to last month.
	span := g.now.AddDate(0, -1, 0).Sub(time.Date(1950, 1, 1, 0, 0, 0, 0, time.UTC))
	return time.Date(1950, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(g.rng.Int64N(int64(span))))
}

func (g *generator) facets(key core.RatingKey) error {
	first := g.rng.IntN(len(fixtureGenres))
	for i, genre := range []string{fixtureGenres[first], fixtureGenres[(first+1+g.rng.IntN(len(fixtureGenres)-1))%len(fixtureGenres)]} {
		if i == 1 && g.rng.IntN(2) == 0 {
			break
		}
		if err := g.insert("catalog_genres", "key,genre", int64(key), genre); err != nil {
			return err
		}
	}
	country := []string{"US", "GB", "CA", "FR", "JP", "KR", "DE"}[g.rng.IntN(7)]
	return g.insert("catalog_countries", "key,country", int64(key), country)
}

func (g *generator) titles(s Scale) error {
	fresh := g.now.Unix()
	for i := 1; i <= s.Movies; i++ {
		key := core.MovieKey(int64(i))
		title := g.name(i)
		rel := g.released()
		if err := g.insert("catalog_titles", titleCols, int64(key), "movie", i, fmt.Sprintf("tt%07d", i), 0, 0, "", "", 0, 0, title, title, strings.ToLower(title), "A synthetic film.", rel.Year(), rel.Unix(), 5400+g.rng.IntN(3600), "Released", "en",
			[]string{"G", "PG", "PG-13", "R"}[g.rng.IntN(4)], float64(g.rng.IntN(90))/10, g.rng.IntN(20000), g.rng.Float64()*100, "/p.jpg", "/b.jpg", 0, 0, 1, rel.Unix(), fresh, 1, fresh); err != nil {
			return err
		}
		if err := g.facets(key); err != nil {
			return err
		}
		g.movies = append(g.movies, key)
	}
	for i := 1; i <= s.Shows; i++ {
		id := int64(i)
		key := core.ShowKey(id)
		title := g.name(i)
		rel := g.released()
		leaves := s.SeasonsPerShow * s.EpisodesPerSeason
		if err := g.insert("catalog_titles", titleCols, int64(key), "show", i, fmt.Sprintf("tt9%06d", i), 0, 0, "", "", 0, 0, title, title, strings.ToLower(title), "A synthetic series.", rel.Year(), rel.Unix(), 2700, "Ended", "en",
			[]string{"TV-G", "TV-PG", "TV-14", "TV-MA"}[g.rng.IntN(4)], float64(g.rng.IntN(90))/10, g.rng.IntN(20000), g.rng.Float64()*100, "/p.jpg", "/b.jpg", s.SeasonsPerShow, leaves, 1, rel.Unix(), fresh, 1, fresh); err != nil {
			return err
		}
		if err := g.facets(key); err != nil {
			return err
		}
		day := rel
		for season := 1; season <= s.SeasonsPerShow; season++ {
			skey := core.SeasonKey(id, season)
			stitle := fmt.Sprintf("Season %d", season)
			if err := g.insert("catalog_titles", titleCols, int64(skey), "season", i, "", int64(key), id, title, stitle, season, 0, stitle, stitle, strings.ToLower(stitle), "", day.Year(), day.Unix(), 0, "", "en", "", 0, 0, 0, "/s.jpg", "", s.EpisodesPerSeason, s.EpisodesPerSeason, 0, day.Unix(), fresh, 1, fresh); err != nil {
				return err
			}
			for ep := 1; ep <= s.EpisodesPerSeason; ep++ {
				ekey := core.EpisodeKey(id, season, ep)
				etitle := fmt.Sprintf("Episode %d", ep)
				if err := g.insert("catalog_titles", titleCols, int64(ekey), "episode", i, fmt.Sprintf("tt8%03d%02d%02d", i%1000, season, ep), int64(key), id, title, stitle, season, ep, etitle, etitle, strings.ToLower(etitle), "", day.Year(), day.Unix(), 2700, "", "en", "", 0, 0, 0, "", "", 0, 0, 0, day.Unix(), fresh, 1, fresh); err != nil {
					return err
				}
				g.eps = append(g.eps, ekey)
				day = day.AddDate(0, 0, 7)
			}
		}
	}
	return g.flush()
}

func (g *generator) viewers(s Scale) error {
	now := g.now.Unix()
	for v := 0; v <= s.Viewers; v++ {
		owner := 0
		name := fmt.Sprintf("viewer%02d", v)
		if v == 0 {
			owner, name = 1, "owner"
		}
		if err := g.flush(); err != nil {
			return err
		}
		var id int64
		if err := g.db.QueryRowContext(g.ctx, `INSERT INTO auth_users(username,title,is_owner,origin,active,created_at,updated_at) VALUES(?,?,?,?,1,?,?) RETURNING id`, name, name, owner, "local", now, now).Scan(&id); err != nil {
			return err
		}
		if err := g.history(id, s); err != nil {
			return err
		}
	}
	return g.flush()
}

func (g *generator) history(user int64, s Scale) error {
	seen := map[core.RatingKey]bool{}
	pick := func() core.RatingKey {
		if len(g.eps) > 0 && g.rng.IntN(2) == 0 {
			return g.eps[g.rng.IntN(len(g.eps))]
		}
		return g.movies[g.rng.IntN(len(g.movies))]
	}
	span := int64(2 * 365 * 24 * time.Hour / time.Second)
	for i := 0; i < s.HistoryPerViewer; i++ {
		key := pick()
		at := g.now.Unix() - g.rng.Int64N(span)
		if err := g.insert("state_history", "user_id,rating_key,viewed_at,completed", user, int64(key), at, 1); err != nil {
			return err
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		if err := g.insert("state_watch", "user_id,rating_key,duration_ns,view_count,counted,last_viewed_at,updated_at,watch_at", user, int64(key), int64(45*time.Minute), 1, 1, at, at, at); err != nil {
			return err
		}
	}
	for i := 0; i < s.InProgressPerViewer; i++ {
		key := pick()
		if seen[key] {
			continue
		}
		seen[key] = true
		at := g.now.Unix() - g.rng.Int64N(30*24*3600)
		if err := g.insert("state_watch", "user_id,rating_key,view_offset_ns,duration_ns,view_count,last_viewed_at,updated_at,watch_at", user, int64(key), int64(20*time.Minute), int64(45*time.Minute), 0, at, at, at); err != nil {
			return err
		}
	}
	return nil
}
