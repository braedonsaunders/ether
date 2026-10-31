package catalog

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/tmdb"
)

func TestMovieYearCorrectionUsesStoredDatesThenRefreshesTheRest(t *testing.T) {
	c := newChunkTestCatalog(t)
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	c.SetNow(func() time.Time { return now })
	c.yearGap = time.Hour
	ctx := context.Background()
	primary := time.Date(1942, 11, 26, 0, 0, 0, 0, time.UTC)
	theatrical := time.Date(1943, 1, 15, 0, 0, 0, 0, time.UTC)
	casablanca := core.MovieKey(289)
	summary := core.MovieKey(10)
	remembered := core.MovieKey(11)
	other := core.MovieKey(12)
	show := core.ShowKey(99)
	insert := func(key core.RatingKey, typ string, tmdbID int64, title string, year int, released, primaryTS int64, detail int, popularity int) {
		t.Helper()
		if _, err := c.db.ExecContext(ctx, `INSERT INTO catalog_titles(key,type,tmdb_id,title,year,released_ts,primary_released_ts,detail,detail_at,browsable,popularity) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
			int64(key), typ, tmdbID, title, year, released, primaryTS, detail, now.Unix(), 1, popularity); err != nil {
			t.Fatal(err)
		}
	}
	insert(summary, "movie", 10, "Summary", 1943, primary.Unix(), 0, 0, 1)
	insert(remembered, "movie", 11, "Remembered", 1955, time.Date(1956, 11, 19, 0, 0, 0, 0, time.UTC).Unix(), time.Date(1954, 4, 26, 0, 0, 0, 0, time.UTC).Unix(), 1, 2)
	insert(casablanca, "movie", 289, "Casablanca", 1943, theatrical.Unix(), 0, 1, 100)
	insert(other, "movie", 12, "Other", 2001, time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC).Unix(), 0, 1, 1)
	insert(show, "show", 99, "Show", 1960, time.Date(1960, 12, 9, 0, 0, 0, 0, time.UTC).Unix(), 0, 1, 50)
	if err := c.UpsertTitle(ctx, core.Title{Key: casablanca, Type: core.TypeMovie, IDs: core.IDs{TMDB: 289}, Title: "Casablanca", Year: 1942, Released: primary, Popularity: 100}, true); err != nil {
		t.Fatal(err)
	}
	var source string
	var year int
	if err := c.db.QueryRowContext(ctx, `SELECT year, year_source FROM catalog_titles WHERE key=?`, int64(casablanca)).Scan(&year, &source); err != nil {
		t.Fatal(err)
	}
	if year != 1943 || source != "" {
		t.Fatalf("summary closed a detailed movie: year %d source %q", year, source)
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	var calls atomicCalls
	c.SetTMDB(&yearTMDB{details: func(id int64) (*tmdb.MovieDetails, error) {
		calls.add()
		if id != 289 {
			t.Errorf("refreshed movie %d", id)
			return nil, tmdb.ErrUnavailable
		}
		once.Do(func() { close(entered) })
		<-release
		var d tmdb.MovieDetails
		if err := json.Unmarshal([]byte(`{"id":289,"title":"Casablanca","release_date":"1942-11-26","release_dates":{"results":[{"iso_3166_1":"SE","release_dates":[{"type":2,"release_date":"1943-01-15T00:00:00Z"}]}]}}`), &d); err != nil {
			return nil, err
		}
		return &d, nil
	}})
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.correctMovieYears(runCtx)
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("year refresh did not reach TMDB")
	}
	getCtx, getCancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	got, err := c.Get(getCtx, other)
	getCancel()
	if err != nil || got.Title != "Other" {
		t.Fatalf("browse waited on the year refresh: %v %+v", err, got)
	}
	var summaryYear, rememberedYear, showYear int
	var summarySource, rememberedSource string
	if err := c.db.QueryRowContext(ctx, `SELECT year, year_source FROM catalog_titles WHERE key=?`, int64(summary)).Scan(&summaryYear, &summarySource); err != nil {
		t.Fatal(err)
	}
	if summaryYear != 1942 || summarySource != yearSourcePrimary {
		t.Fatalf("stored summary year %d source %q", summaryYear, summarySource)
	}
	if err := c.db.QueryRowContext(ctx, `SELECT year, year_source FROM catalog_titles WHERE key=?`, int64(remembered)).Scan(&rememberedYear, &rememberedSource); err != nil {
		t.Fatal(err)
	}
	if rememberedYear != 1954 || rememberedSource != yearSourcePrimary {
		t.Fatalf("stored primary year %d source %q", rememberedYear, rememberedSource)
	}
	if err := c.db.QueryRowContext(ctx, `SELECT year FROM catalog_titles WHERE key=?`, int64(show)).Scan(&showYear); err != nil || showYear != 1960 {
		t.Fatalf("show year %d err %v", showYear, err)
	}
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var gotYear int
		var gotSource string
		var gotPrimary, gotReleased int64
		if err := c.db.QueryRowContext(ctx, `SELECT year, year_source, primary_released_ts, released_ts FROM catalog_titles WHERE key=?`, int64(casablanca)).Scan(&gotYear, &gotSource, &gotPrimary, &gotReleased); err != nil {
			t.Fatal(err)
		}
		if gotSource == yearSourcePrimary {
			if gotYear != 1942 || gotPrimary != primary.Unix() || gotReleased != theatrical.Unix() {
				t.Fatalf("refreshed year %d primary %d released %d", gotYear, gotPrimary, gotReleased)
			}
			stats, err := c.Stats(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if stats.YearCorrection.Pending == 0 || stats.YearCorrection.Stored < 2 || stats.YearCorrection.Refreshed < 1 {
				t.Fatalf("progress %+v", stats.YearCorrection)
			}
			if calls.n() != 1 {
				t.Fatalf("TMDB calls %d", calls.n())
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("Casablanca year was not refreshed")
}

func TestMovieYearRefreshLeavesTheQueueWhenTMDBIsDown(t *testing.T) {
	c := newChunkTestCatalog(t)
	ctx := context.Background()
	key := core.MovieKey(289)
	if _, err := c.db.ExecContext(ctx, `INSERT INTO catalog_titles(key,type,tmdb_id,title,year,released_ts,detail,browsable,popularity) VALUES(?,'movie',289,'Casablanca',1943,?,1,1,10)`, int64(key), time.Date(1943, 1, 15, 0, 0, 0, 0, time.UTC).Unix()); err != nil {
		t.Fatal(err)
	}
	c.SetTMDB(&yearTMDB{details: func(int64) (*tmdb.MovieDetails, error) { return nil, tmdb.ErrUnavailable }})
	ok, err := c.refreshOneMovieYear(ctx)
	if ok || err != tmdb.ErrUnavailable {
		t.Fatalf("refresh = %v, %v", ok, err)
	}
	var year int
	var source string
	if err := c.db.QueryRowContext(ctx, `SELECT year, year_source FROM catalog_titles WHERE key=?`, int64(key)).Scan(&year, &source); err != nil {
		t.Fatal(err)
	}
	if year != 1943 || source != "" {
		t.Fatalf("down TMDB stored year %d source %q", year, source)
	}
}

type yearTMDB struct {
	TMDB
	details func(int64) (*tmdb.MovieDetails, error)
}

func (y *yearTMDB) MovieDetails(_ context.Context, id int64) (*tmdb.MovieDetails, error) {
	return y.details(id)
}

type atomicCalls struct {
	mu sync.Mutex
	v  int
}

func (a *atomicCalls) add() {
	a.mu.Lock()
	a.v++
	a.mu.Unlock()
}

func (a *atomicCalls) n() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.v
}
