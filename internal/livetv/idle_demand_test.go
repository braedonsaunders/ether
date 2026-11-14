package livetv_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/livetv"
	"github.com/braedonsaunders/ether/internal/markers"
	"github.com/braedonsaunders/ether/internal/onair"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/braedonsaunders/ether/internal/viewerpolicy"
)

type idleClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *idleClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *idleClock) Set(t time.Time) {
	c.mu.Lock()
	c.now = t
	c.mu.Unlock()
}

type idleSQLCounter struct {
	reads  atomic.Int64
	writes atomic.Int64
}

type idleCountConnector struct {
	dsn   string
	inner driver.Driver
	cnt   *idleSQLCounter
}

func (cc *idleCountConnector) Connect(_ context.Context) (driver.Conn, error) {
	conn, err := cc.inner.Open(cc.dsn)
	if err != nil {
		return nil, err
	}
	return &idleCountConn{Conn: conn, cnt: cc.cnt}, nil
}

func (cc *idleCountConnector) Driver() driver.Driver { return cc.inner }

type idleCountConn struct {
	driver.Conn
	cnt *idleSQLCounter
}

func (c *idleCountConn) Prepare(q string) (driver.Stmt, error) {
	st, err := c.Conn.Prepare(q)
	if err != nil {
		return nil, err
	}
	return &idleCountStmt{Stmt: st, cnt: c.cnt}, nil
}

func (c *idleCountConn) PrepareContext(ctx context.Context, q string) (driver.Stmt, error) {
	if p, ok := c.Conn.(driver.ConnPrepareContext); ok {
		st, err := p.PrepareContext(ctx, q)
		if err != nil {
			return nil, err
		}
		return &idleCountStmt{Stmt: st, cnt: c.cnt}, nil
	}
	return c.Prepare(q)
}

type idleCountStmt struct {
	driver.Stmt
	cnt *idleSQLCounter
}

func (s *idleCountStmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	s.cnt.writes.Add(1)
	if ex, ok := s.Stmt.(driver.StmtExecContext); ok {
		return ex.ExecContext(ctx, args)
	}
	vals := make([]driver.Value, len(args))
	for i := range args {
		vals[i] = args[i].Value
	}
	return s.Stmt.Exec(vals) //nolint:staticcheck
}

func (s *idleCountStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	s.cnt.reads.Add(1)
	if qr, ok := s.Stmt.(driver.StmtQueryContext); ok {
		return qr.QueryContext(ctx, args)
	}
	vals := make([]driver.Value, len(args))
	for i := range args {
		vals[i] = args[i].Value
	}
	return s.Stmt.Query(vals) //nolint:staticcheck
}

type idleLogHandler struct {
	snapshotReady atomic.Int64
}

func (h *idleLogHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *idleLogHandler) Handle(_ context.Context, r slog.Record) error {
	if r.Message == "virtual guide snapshots ready" {
		h.snapshotReady.Add(1)
	}
	return nil
}
func (h *idleLogHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *idleLogHandler) WithGroup(string) slog.Handler      { return h }

type idleShowcaseCatalog struct {
	shows    []core.Title
	movies   []core.Title
	seasons  map[core.RatingKey][]core.Title
	episodes map[core.RatingKey][]core.Title
	byKey    map[core.RatingKey]core.Title
}

func newIdleShowcaseCatalog() *idleShowcaseCatalog {
	c := &idleShowcaseCatalog{
		seasons:  map[core.RatingKey][]core.Title{},
		episodes: map[core.RatingKey][]core.Title{},
		byKey:    map[core.RatingKey]core.Title{},
	}
	var nextID int64 = 1000
	eras := [][2]int{{1982, 1988}, {1992, 1998}, {2002, 2008}, {2011, 2017}}
	showArchetypes := []struct {
		genres  []string
		rating  string
		runtime time.Duration
	}{
		{[]string{"Animation", "Kids"}, "TV-Y7", 22 * time.Minute},
		{[]string{"Animation", "Comedy"}, "TV-14", 22 * time.Minute},
		{[]string{"Comedy"}, "TV-PG", 22 * time.Minute},
		{[]string{"Drama", "Crime"}, "TV-14", 45 * time.Minute},
		{[]string{"Mystery", "Drama"}, "TV-14", 45 * time.Minute},
		{[]string{"Action & Adventure", "Sci-Fi & Fantasy"}, "TV-PG", 45 * time.Minute},
	}
	movieArchetypes := []struct {
		genres  []string
		rating  string
		runtime time.Duration
	}{
		{[]string{"Action", "Adventure", "Science Fiction"}, "PG-13", 115 * time.Minute},
		{[]string{"Adventure", "Family", "Comedy", "Fantasy"}, "PG", 98 * time.Minute},
		{[]string{"Drama", "Thriller", "Crime", "Romance"}, "R", 122 * time.Minute},
		{[]string{"Horror", "Thriller", "Mystery", "Science Fiction"}, "R", 104 * time.Minute},
	}
	for _, era := range eras {
		for aIdx, arch := range showArchetypes {
			for n := 0; n < 6; n++ {
				nextID++
				showID := nextID
				showKey := core.ShowKey(showID)
				year := era[0] + (n % (era[1] - era[0] + 1))
				sh := core.Title{
					Key:           showKey,
					Type:          core.TypeShow,
					Title:         fmt.Sprintf("Show %d-%d-%d", era[0], aIdx, n),
					Year:          year,
					Genres:        arch.genres,
					ContentRating: arch.rating,
					Runtime:       arch.runtime,
					VoteCount:     5000 + n*100,
					Popularity:    float64(100 - n),
					Countries:     []string{"US"},
					Language:      "en",
				}
				c.shows = append(c.shows, sh)
				c.byKey[showKey] = sh
				seasonKey := core.SeasonKey(showID, 1)
				sn := core.Title{Key: seasonKey, Type: core.TypeSeason, ShowKey: showKey, Season: 1, Title: "Season 1"}
				c.seasons[showKey] = []core.Title{sn}
				c.byKey[seasonKey] = sn
				for epNum := 1; epNum <= 10; epNum++ {
					epKey := core.EpisodeKey(showID, 1, epNum)
					ep := core.Title{
						Key:           epKey,
						Type:          core.TypeEpisode,
						ShowKey:       showKey,
						ShowTitle:     sh.Title,
						Season:        1,
						Episode:       epNum,
						Title:         fmt.Sprintf("Episode %d", epNum),
						Year:          year,
						ContentRating: arch.rating,
						Runtime:       arch.runtime,
					}
					c.episodes[seasonKey] = append(c.episodes[seasonKey], ep)
					c.byKey[epKey] = ep
				}
			}
		}
		for mIdx, arch := range movieArchetypes {
			for n := 0; n < 8; n++ {
				nextID++
				mKey := core.MovieKey(nextID)
				year := era[0] + (n % (era[1] - era[0] + 1))
				mv := core.Title{
					Key:           mKey,
					Type:          core.TypeMovie,
					Title:         fmt.Sprintf("Film %d-%d-%d", era[0], mIdx, n),
					Year:          year,
					Genres:        arch.genres,
					ContentRating: arch.rating,
					Runtime:       arch.runtime,
					VoteCount:     5000 + n*100,
					Popularity:    float64(200 - n),
					Countries:     []string{"US"},
					Language:      "en",
				}
				c.movies = append(c.movies, mv)
				c.byKey[mKey] = mv
			}
		}
	}
	return c
}

func idleMatchesFilter(t core.Title, f core.Filter) bool {
	if f.Type != "" && t.Type != f.Type {
		return false
	}
	if f.YearFrom > 0 && t.Year < f.YearFrom {
		return false
	}
	if f.YearTo > 0 && t.Year > f.YearTo {
		return false
	}
	if f.MinVotes > 0 && t.VoteCount < f.MinVotes {
		return false
	}
	if f.MinRuntime > 0 && t.Runtime < f.MinRuntime {
		return false
	}
	if f.MaxRuntime > 0 && t.Runtime > f.MaxRuntime {
		return false
	}
	for _, ex := range f.ExcludeGenres {
		for _, g := range t.Genres {
			if strings.EqualFold(ex, g) {
				return false
			}
		}
	}
	if len(f.Genres) > 0 {
		hit := false
		for _, want := range f.Genres {
			for _, g := range t.Genres {
				if strings.EqualFold(want, g) {
					hit = true
					break
				}
			}
		}
		if !hit {
			return false
		}
	}
	return true
}

func (c *idleShowcaseCatalog) Get(_ context.Context, key core.RatingKey) (*core.Title, error) {
	if t, ok := c.byKey[key]; ok {
		cp := t
		return &cp, nil
	}
	return nil, core.ErrNotFound
}

func (c *idleShowcaseCatalog) Children(_ context.Context, key core.RatingKey) ([]core.Title, error) {
	if s, ok := c.seasons[key]; ok {
		return append([]core.Title(nil), s...), nil
	}
	if e, ok := c.episodes[key]; ok {
		return append([]core.Title(nil), e...), nil
	}
	return nil, nil
}

func (c *idleShowcaseCatalog) Grandchildren(_ context.Context, key core.RatingKey) ([]core.Title, error) {
	var out []core.Title
	for _, sn := range c.seasons[key] {
		out = append(out, c.episodes[sn.Key]...)
	}
	return out, nil
}

func (c *idleShowcaseCatalog) Browse(_ context.Context, q core.BrowseQuery) (core.Page, error) {
	src := c.movies
	if q.Filter.Type == core.TypeShow {
		src = c.shows
	}
	var matched []core.Title
	for _, t := range src {
		if idleMatchesFilter(t, q.Filter) {
			matched = append(matched, t)
		}
	}
	if q.Offset >= len(matched) {
		return core.Page{Total: len(matched)}, nil
	}
	sliced := matched[q.Offset:]
	if q.Limit > 0 && q.Limit < len(sliced) {
		sliced = sliced[:q.Limit]
	}
	return core.Page{Items: sliced, Total: len(matched)}, nil
}

func (c *idleShowcaseCatalog) Search(context.Context, string, []core.MediaType, int) ([]core.Title, error) {
	return nil, nil
}
func (c *idleShowcaseCatalog) List(context.Context, string, core.MediaType, int) ([]core.Title, error) {
	return nil, nil
}
func (c *idleShowcaseCatalog) Similar(context.Context, core.RatingKey, int) ([]core.Title, error) {
	return nil, nil
}
func (c *idleShowcaseCatalog) Collection(context.Context, int64) (string, []core.Title, error) {
	return "", nil, nil
}
func (c *idleShowcaseCatalog) Facet(context.Context, core.MediaType, string) ([]core.FacetValue, error) {
	return nil, nil
}
func (c *idleShowcaseCatalog) NextEpisode(context.Context, core.RatingKey) (*core.Title, error) {
	return nil, core.ErrNotFound
}
func (c *idleShowcaseCatalog) ByExternalID(context.Context, string, string) (*core.Title, error) {
	return nil, core.ErrNotFound
}

type idleUsers struct{}

func (idleUsers) Users(context.Context) ([]core.User, error) {
	return []core.User{{ID: 1, Username: "owner", IsOwner: true}}, nil
}

type idlePicker struct{}

func (idlePicker) Picks(context.Context, *core.User, map[string]float64, int) ([]core.PersonalPick, error) {
	return nil, nil
}

type idleAvailability struct {
	calls atomic.Int64
}

func (a *idleAvailability) Available(context.Context, core.RatingKey) (bool, bool) {
	a.calls.Add(1)
	return true, true
}

func (a *idleAvailability) AvailabilityEpoch() uint64 { return 1 }

func seedFourShowcaseChannels(t *testing.T, dataDir string, cat core.Catalog, now time.Time) {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	quiet := slog.New(&idleLogHandler{})
	var ltv *livetv.Service
	oa, err := onair.New(onair.Options{
		Showcase: true,
		DB:       db,
		Dir:      filepath.Join(dataDir, "cache", "onair"),
		AssetDir: filepath.Join(dataDir, "onair-assets"),
		Logger:   quiet,
		Changed: func(ctx context.Context) error {
			if ltv != nil {
				return ltv.RefreshGuide(ctx)
			}
			return nil
		},
		StaticRequests: func(ctx context.Context, id string) []core.ClipRequest {
			if ltv == nil {
				return nil
			}
			return ltv.GraphicsRequests(ctx, id)
		},
		Upcoming: func(ctx context.Context) []core.ClipRequest {
			if ltv == nil {
				return nil
			}
			return ltv.GraphicsRequests(ctx, "")
		},
	})
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	ltv, err = livetv.New(livetv.Options{
		Showcase:     true,
		DB:           db,
		Catalog:      cat,
		Users:        idleUsers{},
		Availability: &idleAvailability{},
		Graphics:     oa,
		StyleScreens: oa.StyleScreens,
		Now:          func() time.Time { return now },
		Logger:       quiet,
	})
	if err != nil {
		oa.Close()
		db.Close()
		t.Fatal(err)
	}
	if err := ltv.AddShowcaseDefaults(ctx); err != nil {
		ltv.Close()
		oa.Close()
		db.Close()
		t.Fatal(err)
	}
	oa.Start()
	for deadline := time.Now().Add(15 * time.Second); ; {
		var live int
		_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM livetv_showcase WHERE state='live'`).Scan(&live)
		if live == 4 {
			break
		}
		if time.Now().After(deadline) {
			ltv.Close()
			oa.Close()
			db.Close()
			t.Fatalf("expected 4 live showcase channels, got %d", live)
		}
		time.Sleep(10 * time.Millisecond)
	}
	ltv.Close()
	oa.Close()
	_, _ = db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
	_ = db.Close()
}

func TestIdleDemandZeroSnapshotsAndPostIdleRecovery(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	cat := newIdleShowcaseCatalog()
	t0 := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	seedFourShowcaseChannels(t, dataDir, cat, t0)

	baseDB, err := store.Open(ctx, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer baseDB.Close()

	dbPath := filepath.Join(dataDir, "ether.db")
	dsn := "file:" + dbPath + "?_txlock=immediate&_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)"
	cnt := &idleSQLCounter{}
	measuredDB := sql.OpenDB(&idleCountConnector{dsn: dsn, inner: baseDB.Driver(), cnt: cnt})
	defer measuredDB.Close()

	clock := &idleClock{now: t0.Add(10 * time.Minute)}
	logH := &idleLogHandler{}
	logger := slog.New(logH)
	avail := &idleAvailability{}
	store.ResetWriteGateHolds()

	var ltv *livetv.Service
	oa, err := onair.New(onair.Options{
		Showcase: true,
		DB:       measuredDB,
		Dir:      filepath.Join(dataDir, "cache", "onair"),
		AssetDir: filepath.Join(dataDir, "onair-assets"),
		Logger:   logger,
		Changed: func(ctx context.Context) error {
			if ltv != nil {
				return ltv.RefreshGuide(ctx)
			}
			return nil
		},
		StaticRequests: func(ctx context.Context, id string) []core.ClipRequest {
			if ltv == nil {
				return nil
			}
			return ltv.GraphicsRequests(ctx, id)
		},
		Upcoming: func(ctx context.Context) []core.ClipRequest {
			if ltv == nil {
				return nil
			}
			return ltv.GraphicsRequests(ctx, "")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer oa.Close()

	ltv, err = livetv.New(livetv.Options{
		Showcase:      true,
		DB:            measuredDB,
		Catalog:       cat,
		Users:         idleUsers{},
		Picker:        idlePicker{},
		Availability:  avail,
		Graphics:      oa,
		StyleScreens:  oa.StyleScreens,
		BreakAnalyzer: &markers.BreakAnalyzer{DB: measuredDB},
		BreakInput: func(context.Context, core.RatingKey, int64) (markers.BreakInput, error) {
			return markers.BreakInput{}, core.ErrNotAvailable
		},
		Reels: func(context.Context, string) (livetv.ReelSnapshot, error) {
			return livetv.ReelSnapshot{}, core.ErrNotFound
		},
		Warm:   func(context.Context, core.RatingKey, time.Duration) error { return nil },
		Now:    clock.Now,
		Logger: logger,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer ltv.Close()
	oa.Start()

	channels, err := ltv.Channels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	enabled := map[string]bool{}
	for _, c := range channels {
		if c.Enabled {
			enabled[c.ID] = true
		}
	}
	// Channel IDs are "showcase-" + recipe id (see showcaseRecipes).
	for _, id := range []string{"showcase-chroma-80", "showcase-riot-90", "showcase-orbit-00", "showcase-shift-10"} {
		if !enabled[id] {
			t.Fatalf("shipped showcase channel %s missing or disabled", id)
		}
	}

	if got := avail.calls.Load(); got != 0 {
		t.Fatalf("expected 0 snapshot availability checks while idle, got %d", got)
	}
	if got := logH.snapshotReady.Load(); got != 0 {
		t.Fatalf("expected 0 background snapshot builds while idle, got %d", got)
	}
	if got := cnt.writes.Load(); got != 0 {
		t.Fatalf("expected 0 SQL writes while idle, got %d", got)
	}
	if holds := store.ReadWriteGateHolds().Holds; holds != 0 {
		t.Fatalf("expected 0 write gate holds while idle, got %d", holds)
	}

	// First cold guide request builds on demand and returns 200 OK with all 4 showcase channels.
	handler := ltv.Handler()
	fetchGuide := func() ([]livetv.Programme, int) {
		t.Helper()
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/guide", nil))
		if rec.Code != http.StatusOK {
			return nil, rec.Code
		}
		var ps []livetv.Programme
		if err := json.Unmarshal(rec.Body.Bytes(), &ps); err != nil {
			t.Fatalf("decode /guide: %v", err)
		}
		return ps, rec.Code
	}

	coldProgrammes, status := fetchGuide()
	if status != http.StatusOK || len(coldProgrammes) == 0 {
		t.Fatalf("first cold /guide returned status=%d programmes=%d", status, len(coldProgrammes))
	}
	seenChannels := map[string]bool{}
	for _, p := range coldProgrammes {
		seenChannels[p.ChannelID] = true
	}
	for _, id := range []string{"showcase-chroma-80", "showcase-riot-90", "showcase-orbit-00", "showcase-shift-10"} {
		if !seenChannels[id] {
			t.Fatalf("first cold /guide missing programmes for %s", id)
		}
	}
	if ltv.GuideSnapshotBuilds() == 0 {
		t.Fatal("expected cold /guide to build snapshots")
	}

	// Wait briefly if background virtualGuideLoop is finishing its demand wake, then verify warm read reuses cache.
	if err := ltv.RefreshGuide(ctx); err != nil {
		t.Fatal(err)
	}
	warmBaseline := ltv.GuideSnapshotBuilds()
	clock.Set(clock.Now().Add(5 * time.Minute))
	warmProgrammes, status := fetchGuide()
	if status != http.StatusOK || len(warmProgrammes) == 0 {
		t.Fatalf("warm /guide returned status=%d programmes=%d", status, len(warmProgrammes))
	}
	if got := ltv.GuideSnapshotBuilds(); got != warmBaseline {
		t.Fatalf("warm /guide within idle window rebuilt snapshots: builds %d -> %d", warmBaseline, got)
	}

	// Advance past the 15-minute idle window: snapshots evict and the next /guide rebuilds cleanly.
	clock.Set(clock.Now().Add(16 * time.Minute))
	postIdleProgrammes, status := fetchGuide()
	if status != http.StatusOK || len(postIdleProgrammes) == 0 {
		t.Fatalf("post-idle /guide returned status=%d programmes=%d", status, len(postIdleProgrammes))
	}
	if got := ltv.GuideSnapshotBuilds(); got <= warmBaseline {
		t.Fatalf("expected post-idle /guide to rebuild evicted snapshots: builds stayed %d", got)
	}
}

func TestShowcaseDailyRefreshUnchangedDoesNotBumpViewerPolicyEpoch(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	cat := newIdleShowcaseCatalog()
	t0 := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	seedFourShowcaseChannels(t, dataDir, cat, t0)

	db, err := store.Open(ctx, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Simulate learned natural break points on an existing showcase item plus an already-live channel row.
	var raw []byte
	if err := db.QueryRowContext(ctx, `SELECT data FROM livetv_channels WHERE id='showcase-chroma-80'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var ch livetv.Channel
	if err := json.Unmarshal(raw, &ch); err != nil {
		t.Fatal(err)
	}
	var injectedKey core.RatingKey
	var injectedDuration int64
	for bi := range ch.Blocks {
		if len(ch.Blocks[bi].Items) > 0 && ch.Blocks[bi].Items[0].DurationMS > 600000 {
			injectedKey = ch.Blocks[bi].Items[0].Key
			injectedDuration = ch.Blocks[bi].Items[0].DurationMS
			break
		}
	}
	if injectedKey == 0 {
		t.Fatal("expected at least one showcase-chroma-80 block item suitable for break point injection")
	}
	// publishBreaks writes learned points onto every occurrence of the title
	// (blocks, pins and materialized source snapshots), so inject the same way:
	// a daily refresh then has nothing new to write.
	inject := func(items []livetv.Item) {
		for i := range items {
			if items[i].Key == injectedKey && items[i].DurationMS == injectedDuration {
				items[i].BreakPoints = []livetv.BreakPoint{{AtMS: 300000, Confidence: 0.92, Source: "chapters"}}
				items[i].BreakPointsMS = []int64{300000}
			}
		}
	}
	var injectSources func([]livetv.ProgramSource)
	injectSources = func(sources []livetv.ProgramSource) {
		for i := range sources {
			inject(sources[i].Items)
			injectSources(sources[i].Sources)
		}
	}
	inject(ch.Items)
	inject(ch.Filler)
	inject(ch.Fallback)
	injectSources(ch.Sources)
	for bi := range ch.Blocks {
		inject(ch.Blocks[bi].Items)
		inject(ch.Blocks[bi].Filler)
		injectSources(ch.Blocks[bi].Sources)
	}
	for pi := range ch.Pins {
		inject(ch.Pins[pi].Items)
		injectSources(ch.Pins[pi].Sources)
	}
	updatedRaw, err := json.Marshal(ch)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE livetv_channels SET data=? WHERE id='showcase-chroma-80'`, updatedRaw); err != nil {
		t.Fatal(err)
	}

	vpBefore, err := viewerpolicy.Read(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	var chanEpochBefore int64
	if err := db.QueryRowContext(ctx, `SELECT generation FROM livetv_channel_epoch WHERE id=1`).Scan(&chanEpochBefore); err != nil {
		t.Fatal(err)
	}

	nextDay := t0.Add(25 * time.Hour)
	ltv, err := livetv.New(livetv.Options{
		Showcase:     true,
		DB:           db,
		Catalog:      cat,
		Users:        idleUsers{},
		Availability: &idleAvailability{},
		Now:          func() time.Time { return nextDay },
		Logger:       slog.New(&idleLogHandler{}),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer ltv.Close()

	for deadline := time.Now().Add(15 * time.Second); ; {
		var refreshed int
		_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM livetv_showcase WHERE state='live' AND refreshed_ms=?`, nextDay.UnixMilli()).Scan(&refreshed)
		if refreshed == 4 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected all 4 showcase channels to complete daily refresh, got %d", refreshed)
		}
		time.Sleep(10 * time.Millisecond)
	}

	vpAfter, err := viewerpolicy.Read(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if vpAfter != vpBefore {
		t.Fatalf("daily showcase refresh on unchanged catalogue bumped viewer_policy_epoch: %d -> %d", vpBefore, vpAfter)
	}
	var chanEpochAfter int64
	if err := db.QueryRowContext(ctx, `SELECT generation FROM livetv_channel_epoch WHERE id=1`).Scan(&chanEpochAfter); err != nil {
		t.Fatal(err)
	}
	if chanEpochAfter != chanEpochBefore {
		t.Fatalf("daily showcase refresh on unchanged catalogue rewrote livetv_channels: epoch %d -> %d", chanEpochBefore, chanEpochAfter)
	}

	afterCh, err := ltv.Channel(ctx, "showcase-chroma-80")
	if err != nil {
		t.Fatal(err)
	}
	preserved := false
	for _, b := range afterCh.Blocks {
		for _, it := range b.Items {
			if it.Key == injectedKey && len(it.BreakPoints) == 1 && it.BreakPoints[0].AtMS == 300000 {
				preserved = true
				break
			}
		}
	}
	if !preserved {
		t.Fatal("daily showcase refresh lost learned BreakPoints on unchanged item")
	}
}
