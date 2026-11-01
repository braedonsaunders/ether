// Package catalog implements core.Catalog: a SQLite mirror of a filtered
// TMDB slice plus on-demand fetches for anything else.
package catalog

import (
	"context"
	"database/sql"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/flight"
	"github.com/braedonsaunders/ether/internal/libraryfilter"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/braedonsaunders/ether/internal/tmdb"
)

func init() {
	store.Register("catalog", 1, `
CREATE TABLE catalog_titles (
  key INTEGER PRIMARY KEY,
  type TEXT NOT NULL,
  tmdb_id INTEGER NOT NULL DEFAULT 0,
  imdb TEXT NOT NULL DEFAULT '',
  tvdb INTEGER NOT NULL DEFAULT 0,
  show_key INTEGER NOT NULL DEFAULT 0,
  show_tmdb INTEGER NOT NULL DEFAULT 0,
  show_title TEXT NOT NULL DEFAULT '',
  season_title TEXT NOT NULL DEFAULT '',
  season INTEGER NOT NULL DEFAULT 0,
  episode INTEGER NOT NULL DEFAULT 0,
  title TEXT NOT NULL DEFAULT '',
  original_title TEXT NOT NULL DEFAULT '',
  sort_title TEXT NOT NULL DEFAULT '',
  tagline TEXT NOT NULL DEFAULT '',
  summary TEXT NOT NULL DEFAULT '',
  year INTEGER NOT NULL DEFAULT 0,
  released_ts INTEGER NOT NULL DEFAULT 0,
  runtime_sec INTEGER NOT NULL DEFAULT 0,
  status TEXT NOT NULL DEFAULT '',
  language TEXT NOT NULL DEFAULT '',
  content_rating TEXT NOT NULL DEFAULT '',
  content_rating_rank INTEGER NOT NULL DEFAULT 0,
  rating REAL NOT NULL DEFAULT 0,
  vote_count INTEGER NOT NULL DEFAULT 0,
  popularity REAL NOT NULL DEFAULT 0,
  poster TEXT NOT NULL DEFAULT '',
  backdrop TEXT NOT NULL DEFAULT '',
  still TEXT NOT NULL DEFAULT '',
  logo TEXT NOT NULL DEFAULT '',
  collection_id INTEGER NOT NULL DEFAULT 0,
  collection_name TEXT NOT NULL DEFAULT '',
  child_count INTEGER NOT NULL DEFAULT 0,
  leaf_count INTEGER NOT NULL DEFAULT 0,
  browsable INTEGER NOT NULL DEFAULT 0,
  added_at INTEGER NOT NULL DEFAULT 0,
  fetched_at INTEGER NOT NULL DEFAULT 0,
  detail INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX catalog_titles_type_browse_pop ON catalog_titles(type, browsable, popularity DESC);
CREATE INDEX catalog_titles_type_browse_rating ON catalog_titles(type, browsable, rating DESC);
CREATE INDEX catalog_titles_type_browse_released ON catalog_titles(type, browsable, released_ts DESC);
CREATE INDEX catalog_titles_type_browse_added ON catalog_titles(type, browsable, added_at DESC);
CREATE INDEX catalog_titles_sort ON catalog_titles(sort_title);
CREATE INDEX catalog_titles_tmdb ON catalog_titles(tmdb_id, type);
CREATE INDEX catalog_titles_imdb ON catalog_titles(imdb);
CREATE INDEX catalog_titles_tvdb ON catalog_titles(tvdb);
CREATE INDEX catalog_titles_show ON catalog_titles(show_key, season, episode);
CREATE INDEX catalog_titles_collection ON catalog_titles(collection_id);
CREATE INDEX catalog_titles_year ON catalog_titles(year);
CREATE TABLE catalog_genres (key INTEGER NOT NULL, genre TEXT NOT NULL, PRIMARY KEY(key, genre));
CREATE INDEX catalog_genres_genre ON catalog_genres(genre, key);
CREATE TABLE catalog_studios (key INTEGER NOT NULL, studio TEXT NOT NULL, PRIMARY KEY(key, studio));
CREATE INDEX catalog_studios_studio ON catalog_studios(studio, key);
CREATE TABLE catalog_countries (key INTEGER NOT NULL, country TEXT NOT NULL, PRIMARY KEY(key, country));
CREATE INDEX catalog_countries_country ON catalog_countries(country, key);
CREATE TABLE catalog_credits (
  key INTEGER NOT NULL, person_tmdb INTEGER NOT NULL, name TEXT NOT NULL DEFAULT '',
  role TEXT NOT NULL DEFAULT '', kind TEXT NOT NULL DEFAULT 'cast', ord INTEGER NOT NULL DEFAULT 0,
  thumb TEXT NOT NULL DEFAULT '', PRIMARY KEY(key, person_tmdb, kind, role));
CREATE INDEX catalog_credits_person ON catalog_credits(person_tmdb, key);
CREATE TABLE catalog_collections (
  tmdb_id INTEGER PRIMARY KEY, name TEXT NOT NULL DEFAULT '', summary TEXT NOT NULL DEFAULT '',
  poster TEXT NOT NULL DEFAULT '', backdrop TEXT NOT NULL DEFAULT '', fetched_at INTEGER NOT NULL DEFAULT 0);
CREATE TABLE catalog_meta (k TEXT PRIMARY KEY, v TEXT NOT NULL DEFAULT '');
`)
	store.Register("catalog", 2, `
CREATE TABLE catalog_keywords (key INTEGER NOT NULL, keyword TEXT NOT NULL, PRIMARY KEY(key, keyword));
CREATE INDEX catalog_keywords_keyword ON catalog_keywords(keyword, key);
`)
	store.Register("catalog", 3, `ALTER TABLE catalog_titles ADD COLUMN adult INTEGER NOT NULL DEFAULT 0;`)
	// detail_at is when the full detail payload was last fetched. fetched_at
	// also moves on every summary upsert (sync, lists and search), so it cannot
	// decide when details are stale.
	store.Register("catalog", 5, `ALTER TABLE catalog_titles ADD COLUMN detail_at INTEGER NOT NULL DEFAULT 0;`)
	store.Register("catalog", 6, dropUnkeyableEpisodes)
	store.Register("catalog", 9, `
CREATE TABLE catalog_home_release (key INTEGER PRIMARY KEY, checked INTEGER NOT NULL DEFAULT 0, earliest_ts INTEGER NOT NULL DEFAULT 0, fallback_admitted INTEGER NOT NULL DEFAULT 0, attempted_at INTEGER NOT NULL DEFAULT 0);
CREATE INDEX catalog_home_release_pending ON catalog_home_release(checked,earliest_ts,fallback_admitted);
CREATE TABLE catalog_home_release_dates (key INTEGER NOT NULL, country TEXT NOT NULL, kind INTEGER NOT NULL, released_ts INTEGER NOT NULL, PRIMARY KEY(key,country,kind,released_ts));
`)
	store.Register("catalog", 10, `CREATE INDEX catalog_titles_episode_aired ON catalog_titles(show_key, released_ts DESC) WHERE type='episode' AND released_ts>0;
CREATE INDEX catalog_titles_season_episode_aired ON catalog_titles(show_key, season, released_ts DESC) WHERE type='episode' AND released_ts>0;
CREATE INDEX catalog_titles_episode_global_aired ON catalog_titles(released_ts DESC, key ASC) WHERE type='episode' AND released_ts>0 AND adult=0;`)
	store.Register("catalog", 11, `ALTER TABLE catalog_titles ADD COLUMN last_air_ts INTEGER NOT NULL DEFAULT 0;`)
	store.Register("catalog", 12, `CREATE TABLE catalog_people (person_tmdb INTEGER PRIMARY KEY, imdb TEXT NOT NULL DEFAULT '');`)
	store.Register("catalog", 13, `ALTER TABLE catalog_credits ADD COLUMN provider_id TEXT NOT NULL DEFAULT '';`)
	store.Register("catalog", 14, `CREATE TABLE catalog_remote_trailers (key INTEGER PRIMARY KEY, name TEXT NOT NULL DEFAULT '', url TEXT NOT NULL);`)
	// Alternative titles (altTitles) for release-name matching, from the
	// detail fetch; at most maxAltTitles per title, keyed for hydration.
	store.Register("catalog", 15, `CREATE TABLE catalog_alt_titles (key INTEGER NOT NULL, title TEXT NOT NULL, PRIMARY KEY(key, title));`)
	// content_rating_source records where content_rating came from; see
	// rating.go. content_rating_rank now holds contentrating.Level, re-ranked
	// once at startup (rankRatings).
	store.Register("catalog", 19, `ALTER TABLE catalog_titles ADD COLUMN content_rating_source TEXT NOT NULL DEFAULT '';`)
	// release_known is 1 when released_ts is a real Unix instant and 0 when
	// the date is unknown. Zero stays the unknown sentinel: a date before
	// 1970 is negative, so "released_ts>0" is not "has a date". The column
	// is generated so every writer of released_ts keeps the flag, including
	// rows already stored. SQLite cannot ALTER in a STORED generated column,
	// so the flag is VIRTUAL there. PostgreSQL stores it. The aired indexes
	// use the flag and therefore include those historical instants.
	const releaseKnownSQLite = `
ALTER TABLE catalog_titles ADD COLUMN release_known INTEGER GENERATED ALWAYS AS (CASE WHEN released_ts<>0 THEN 1 ELSE 0 END) VIRTUAL;
DROP INDEX IF EXISTS catalog_titles_episode_aired;
DROP INDEX IF EXISTS catalog_titles_season_episode_aired;
DROP INDEX IF EXISTS catalog_titles_episode_global_aired;
CREATE INDEX catalog_titles_episode_aired ON catalog_titles(show_key, released_ts DESC) WHERE type='episode' AND release_known=1;
CREATE INDEX catalog_titles_season_episode_aired ON catalog_titles(show_key, season, released_ts DESC) WHERE type='episode' AND release_known=1;
CREATE INDEX catalog_titles_episode_global_aired ON catalog_titles(released_ts DESC, key ASC) WHERE type='episode' AND release_known=1 AND adult=0;
`
	store.Register("catalog", 20, releaseKnownSQLite)
	// Catalog migration 21 is the recent-episodes index, owned elsewhere.
	// primary_released_ts is TMDB's primary release_date. year_source says
	// which date produced year (empty until this correction has seen the
	// row). year_checked_at cools down a failed refresh. The pending index
	// is the background queue: detailed movies whose primary date was never
	// stored, browsable and most popular first.
	store.Register("catalog", 22, `
ALTER TABLE catalog_titles ADD COLUMN primary_released_ts INTEGER NOT NULL DEFAULT 0;
ALTER TABLE catalog_titles ADD COLUMN year_source TEXT NOT NULL DEFAULT '';
ALTER TABLE catalog_titles ADD COLUMN year_checked_at INTEGER NOT NULL DEFAULT 0;
CREATE INDEX catalog_titles_movie_year_pending ON catalog_titles(browsable DESC, popularity DESC, key) WHERE type='movie' AND year_source='';
`)
	store.RegisterDialect("catalog", 20, store.Postgres, `
ALTER TABLE catalog_titles ADD COLUMN release_known BIGINT GENERATED ALWAYS AS (CASE WHEN released_ts<>0 THEN 1 ELSE 0 END) STORED;
DROP INDEX IF EXISTS catalog_titles_episode_aired;
DROP INDEX IF EXISTS catalog_titles_season_episode_aired;
DROP INDEX IF EXISTS catalog_titles_episode_global_aired;
CREATE INDEX catalog_titles_episode_aired ON catalog_titles(show_key, released_ts DESC) WHERE type='episode' AND release_known=1;
CREATE INDEX catalog_titles_season_episode_aired ON catalog_titles(show_key, season, released_ts DESC) WHERE type='episode' AND release_known=1;
CREATE INDEX catalog_titles_episode_global_aired ON catalog_titles(released_ts DESC, key ASC) WHERE type='episode' AND release_known=1 AND adult=0;
`)
	store.Register("catalog", 23, `
CREATE INDEX catalog_titles_type_browse_updated ON catalog_titles(type, browsable, adult, fetched_at DESC, key ASC);
CREATE INDEX catalog_titles_episode_updated ON catalog_titles(fetched_at DESC, key ASC) WHERE type='episode' AND release_known=1 AND adult=0;
CREATE INDEX catalog_titles_season_key ON catalog_titles(key ASC) WHERE type='season' AND adult=0;
CREATE INDEX catalog_titles_season_updated ON catalog_titles(fetched_at DESC, key ASC) WHERE type='season' AND adult=0;
CREATE INDEX catalog_titles_season_released ON catalog_titles(released_ts DESC, key ASC) WHERE type='season' AND release_known=1 AND adult=0;
`)
	store.Register("catalog", 7, `
CREATE INDEX catalog_titles_type_browse_sort ON catalog_titles(type, browsable, adult, sort_title ASC, key ASC);
CREATE INDEX catalog_titles_browse_warm ON catalog_titles(browsable, detail, adult, popularity DESC, vote_count DESC);
CREATE INDEX catalog_titles_type_browse_year ON catalog_titles(type, browsable, adult, year DESC, popularity DESC);
CREATE INDEX catalog_titles_type_browse_lang ON catalog_titles(type, browsable, adult, language, popularity DESC);
CREATE INDEX catalog_titles_type_browse_runtime ON catalog_titles(type, browsable, adult, runtime_sec, popularity DESC);
CREATE INDEX catalog_credits_person_kind ON catalog_credits(person_tmdb, kind, key);
`)
}

// dropUnkeyableEpisodes removes mirrored seasons and episodes whose numbers
// do not fit a rating key (episodeKeyFits). Before that check, a season
// numbered by year (2023) was stored under another show's episode key,
// overwriting that show's row; both are mirror data and are fetched again
// on demand.
const dropUnkeyableEpisodes = `DELETE FROM catalog_titles WHERE
 (type='episode' AND (season < 0 OR season >= 1000 OR episode < 0 OR episode >= 10000))
 OR (type='season' AND (season < 0 OR season >= 1000));`

// TMDB is the subset of the TMDB client the catalogue needs.
// *tmdb.Client satisfies it.
type TMDB interface {
	MovieDetails(ctx context.Context, id int64) (*tmdb.MovieDetails, error)
	TVDetails(ctx context.Context, id int64) (*tmdb.TVDetails, error)
	SeasonDetails(ctx context.Context, tvID int64, season int) (*tmdb.SeasonDetails, error)
	EpisodeExternalIDs(ctx context.Context, tvID int64, season, episode int) (*tmdb.ExternalIDs, error)
	CollectionDetails(ctx context.Context, id int64) (*tmdb.CollectionDetails, error)
	DiscoverMovies(ctx context.Context, p tmdb.DiscoverParams) (*tmdb.Paged[tmdb.Movie], error)
	DiscoverTV(ctx context.Context, p tmdb.DiscoverParams) (*tmdb.Paged[tmdb.TVShow], error)
	SearchMovies(ctx context.Context, q string, page int) (*tmdb.Paged[tmdb.Movie], error)
	SearchTV(ctx context.Context, q string, page int) (*tmdb.Paged[tmdb.TVShow], error)
	SearchMulti(ctx context.Context, q string, page int) (*tmdb.Paged[tmdb.MultiResult], error)
	TrendingMovies(ctx context.Context, window string, page int) (*tmdb.Paged[tmdb.Movie], error)
	TrendingTV(ctx context.Context, window string, page int) (*tmdb.Paged[tmdb.TVShow], error)
	TrendingAll(ctx context.Context, window string, page int) (*tmdb.Paged[tmdb.MultiResult], error)
	MovieList(ctx context.Context, list string, page int) (*tmdb.Paged[tmdb.Movie], error)
	TVList(ctx context.Context, list string, page int) (*tmdb.Paged[tmdb.TVShow], error)
	MovieSimilar(ctx context.Context, id int64, page int) (*tmdb.Paged[tmdb.Movie], error)
	MovieRecommendations(ctx context.Context, id int64, page int) (*tmdb.Paged[tmdb.Movie], error)
	TVSimilar(ctx context.Context, id int64, page int) (*tmdb.Paged[tmdb.TVShow], error)
	TVRecommendations(ctx context.Context, id int64, page int) (*tmdb.Paged[tmdb.TVShow], error)
	FindByExternalID(ctx context.Context, source, id string) (*tmdb.FindResult, error)
	Genres(ctx context.Context, mediaType string) ([]tmdb.Genre, error)
}

var _ TMDB = (*tmdb.Client)(nil)
var _ core.Catalog = (*Catalog)(nil)

// tmdbRef is the swappable metadata client. The pointer is what concurrent
// readers and SetTMDB publish.
type tmdbRef struct{ client TMDB }

// Catalog implements core.Catalog.
type Catalog struct {
	visibility     *libraryfilter.Filter
	db             *sql.DB
	remote         atomic.Pointer[tmdbRef]
	configUpdateMu sync.Mutex // serializes transitions without blocking config readers
	// Retry a failed preservation on the next config transition. The saved
	// settings still take effect immediately, even when the database fails.
	admissionsPending bool
	cfgMu             sync.RWMutex
	cfg               config.Catalog
	cfgVersion        *configVersion
	log               *slog.Logger
	clock             atomic.Value // func() time.Time

	staleAfter time.Duration
	listTTL    time.Duration
	// resumeSchedule: Run waits out the interval since the recorded last
	// sync (WithResumeSchedule).
	resumeSchedule bool
	crowdTTL       time.Duration // co-engagement lists (crowd.go)

	genreMu sync.Mutex
	genres  map[string]map[int]string // mediaType -> id -> name

	// refreshMiss remembers a failed metadata read so one library list does
	// not repeat it for every row. A later read retries after refreshMissTTL.
	refreshMissMu sync.Mutex
	refreshMiss   map[string]time.Time

	// hydrating holds the TMDB refreshes running behind reads (hydrate.go).
	hydrateMu    sync.Mutex
	hydrating    map[string]*hydration
	hydrateSlots chan struct{}

	trendFlights flight.Group[string, trendSnapshot]
	statsFlights flight.Group[string, Stats]
	listMu       sync.Mutex
	listCache    map[string]listEntry

	syncMu     sync.Mutex
	syncing    atomic.Bool
	progress   SyncProgress
	progressMu sync.RWMutex

	yearMu         sync.RWMutex
	yearRunning    atomic.Bool
	yearState      MovieYearProgress
	yearGap        time.Duration
	yearAhead      chan core.RatingKey
	yearInflight   sync.Map
	yearInflightN  atomic.Int32
	yearPauseUntil atomic.Int64
	syncYears      [2]int // 0,0 = 1900..now+2

	// High-speed in-memory read caches, invalidated atomically on writes.
	writeGen    atomic.Uint64
	cacheMu     sync.RWMutex
	titleCache  map[core.RatingKey]cachedStoredTitle
	countCache  map[string]cachedCount
	browseCache map[string]cachedBrowse
	grandCache  map[core.RatingKey]cachedBrowse
	facetCache  map[string]cachedFacet
	statsCache  cachedStats

	admitMu     sync.Mutex
	admitGen    uint64
	admitMinute int64
	admitReady  bool

	personMu       sync.Mutex
	personHydrated map[int64]time.Time
}

type cachedStoredTitle struct {
	st     storedTitle
	gen    uint64
	minute int64
}

type cachedCount struct {
	total   int
	gen     uint64
	minute  int64
	counted time.Time // when the count ran; zero for a synchronous count
}

type cachedBrowse struct {
	page   core.Page
	gen    uint64
	minute int64
}

type cachedFacet struct {
	vals    []core.FacetValue
	gen     uint64
	minute  int64
	counted time.Time
}

type cachedStats struct {
	stats     Stats
	gen       uint64
	minute    int64
	fetchedAt time.Time
	ready     bool
}

type listEntry struct {
	items   []core.Title
	expires time.Time
}

// Option customises a Catalog.
type Option func(*Catalog)

func WithNow(fn func() time.Time) Option {
	return func(c *Catalog) { c.setNow(fn) }
}

// SetNow replaces the clock used for release checks and the minute snapshot.
// The swap is atomic so a background browse can keep reading it.
func (c *Catalog) SetNow(now func() time.Time) {
	if c == nil {
		return
	}
	c.setNow(now)
}

func (c *Catalog) setNow(fn func() time.Time) {
	if fn == nil {
		fn = time.Now
	}
	c.clock.Store(fn)
}

func (c *Catalog) now() time.Time {
	fn, _ := c.clock.Load().(func() time.Time)
	if fn == nil {
		return time.Now()
	}
	return fn()
}
func WithStaleAfter(d time.Duration) Option {
	return func(c *Catalog) { c.staleAfter = d }
}
func WithListTTL(d time.Duration) Option { return func(c *Catalog) { c.listTTL = d } }

// WithResumeSchedule makes Run continue the refresh schedule recorded in
// the database instead of syncing as soon as it starts: the first sync
// waits until RefreshInterval has passed since the last successful sync
// (by any node). A cluster uses it because Run restarts on every leader
// handoff; with no recorded sync Run still syncs at once.
func WithResumeSchedule() Option { return func(c *Catalog) { c.resumeSchedule = true } }

// WithSyncYears bounds the year slices used when discover exceeds 500 pages.
func WithSyncYears(from, to int) Option { return func(c *Catalog) { c.syncYears = [2]int{from, to} } }

// New creates a catalogue. client may be nil (mirror-only / offline mode).
func New(db *sql.DB, client TMDB, cfg config.Catalog, log *slog.Logger, opts ...Option) (*Catalog, error) {
	if db == nil {
		return nil, errNilDB
	}
	if log == nil {
		log = slog.Default()
	}
	cfg = cloneCatalogConfig(cfg)
	visibility, err := libraryfilter.Compile(cfg.Exclusions)
	if err != nil {
		return nil, err
	}
	stale := cfg.RefreshInterval.D()
	if stale <= 0 {
		stale = 24 * time.Hour
	}
	c := &Catalog{visibility: visibility, db: db, cfg: cfg, cfgVersion: &configVersion{}, log: log, hydrateSlots: make(chan struct{}, hydrateLimit),
		staleAfter: stale, listTTL: 15 * time.Minute, yearGap: 100 * time.Millisecond, yearAhead: make(chan core.RatingKey, 256),
		genres: map[string]map[int]string{}, listCache: map[string]listEntry{},
		titleCache: map[core.RatingKey]cachedStoredTitle{}, countCache: map[string]cachedCount{},
		browseCache: map[string]cachedBrowse{}, grandCache: map[core.RatingKey]cachedBrowse{}, facetCache: map[string]cachedFacet{},
		personHydrated: map[int64]time.Time{}}
	c.setNow(time.Now)
	c.SetTMDB(client)
	for _, fn := range opts {
		fn(c)
	}
	c.ensureSortTitlesFolded(context.Background())
	return c, nil
}

var errNilDB = &simpleError{"catalog: nil *sql.DB"}

type simpleError struct{ s string }

func (e *simpleError) Error() string { return e.s }

// UpdateConfig swaps the catalogue config (wire to config.Store.Subscribe).
func (c *Catalog) UpdateConfig(cfg config.Catalog) {
	cfg = cloneCatalogConfig(cfg)
	f, err := libraryfilter.Compile(cfg.Exclusions)
	if err != nil {
		c.log.Error("catalog: invalid visibility rules")
		return
	}
	c.configUpdateMu.Lock()
	defer c.configUpdateMu.Unlock()
	previous := c.config()
	if c.admissionsPending || (previous.GateMoviesByHomeRelease && !cfg.GateMoviesByHomeRelease) {
		if _, err := c.db.Exec(`UPDATE catalog_home_release SET fallback_admitted=1 WHERE checked=1 AND earliest_ts=0 AND EXISTS (SELECT 1 FROM catalog_titles t WHERE t.key=catalog_home_release.key AND t.release_known=1 AND t.released_ts<=?)`, c.now().Unix()-int64(homeReleaseGrace.Seconds())); err != nil {
			c.log.Error("catalog: preserve fallback admissions", "err", err)
			c.admissionsPending = true
		} else {
			c.admissionsPending = false
		}
	}
	c.cfgMu.Lock()
	c.cfg = cfg
	c.visibility = f
	c.cfgVersion = &configVersion{}
	c.writeGen.Add(1)
	c.cfgMu.Unlock()
	c.listMu.Lock()
	c.listCache = map[string]listEntry{}
	c.listMu.Unlock()
}

func (c *Catalog) config() config.Catalog {
	c.cfgMu.RLock()
	defer c.cfgMu.RUnlock()
	return c.cfg
}

// MeetsThreshold reports whether a title qualifies for the browsable set under
// cfg: popularity >= MinPopularity OR votes >= MinVotes, and released.
func MeetsThreshold(cfg config.Catalog, t core.Title, now time.Time) bool {
	if t.Type != core.TypeMovie && t.Type != core.TypeShow {
		return false
	}
	if !t.IsReleased(now) {
		// A title with no date is treated as unreleased.
		return false
	}
	if cfg.MinPopularity <= 0 && cfg.MinVotes <= 0 {
		return true
	}
	popOK := cfg.MinPopularity > 0 && t.Popularity >= cfg.MinPopularity
	voteOK := cfg.MinVotes > 0 && t.VoteCount >= cfg.MinVotes
	if cfg.MinPopularity > 0 && cfg.MinVotes > 0 {
		return popOK || voteOK
	}
	if cfg.MinPopularity > 0 {
		return popOK
	}
	return voteOK
}

func (c *Catalog) meetsThreshold(t core.Title) bool {
	return MeetsThreshold(c.config(), t, c.now())
}

// SetTMDB replaces the remote metadata client. Nil answers from the local catalogue only.
func (c *Catalog) SetTMDB(client TMDB) {
	if c != nil {
		c.remote.Store(&tmdbRef{client: client})
	}
}

func (c *Catalog) tmdb() TMDB {
	if c == nil {
		return nil
	}
	ref := c.remote.Load()
	if ref == nil {
		return nil
	}
	return ref.client
}

// WriteGeneration changes when a catalogue write drops the read caches.
func (c *Catalog) WriteGeneration() uint64 {
	if c == nil {
		return 0
	}
	return c.writeGen.Load()
}
