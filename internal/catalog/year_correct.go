package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/braedonsaunders/ether/internal/tmdb"
)

const (
	yearRetryAfter = 6 * time.Hour
	// yearFetchRate is how many candidate movies the sweep starts per second.
	// TMDB allows about 40–50 requests a second per IP; this leaves most of
	// that budget for viewers. The shared client still applies its own limit
	// and breaker.
	yearFetchRate  = 10
	yearFetchSlots = 10
)

// MovieYearProgress is the upgrade correction shown on Catalog health.
// Stored counts movies whose year was taken from a date already in the
// catalogue. Refreshed counts movies whose detail was fetched because that
// date was not stored. Pending is how many movies are still unmarked.
type MovieYearProgress struct {
	Running   bool   `json:"running"`
	Finished  bool   `json:"finished"`
	Pending   int    `json:"pending"`
	Stored    int    `json:"stored"`
	Refreshed int    `json:"refreshed"`
	LastError string `json:"lastError,omitempty"`
}

// correctMovieYears rewrites movie years that were taken from a regional
// theatrical date. It runs beside catalogue sync and never on a request.
// A summary row already stores the primary release_date in released_ts, so
// that year is fixed with no TMDB call. A detailed row is fetched only when
// its stored date is in January–March: that is when a primary date or a
// type-1 premiere in the previous year can fall in a different calendar
// year. Everything else keeps the stored year. The sweep starts about ten
// fetches a second. A movie opened or drawn in a row starts at once.
func (c *Catalog) correctMovieYears(ctx context.Context) {
	if c == nil || !c.yearRunning.CompareAndSwap(false, true) {
		return
	}
	defer c.yearRunning.Store(false)
	c.setYearProgress(func(p *MovieYearProgress) {
		p.Running = true
		p.Finished = false
		p.LastError = ""
	})
	defer c.setYearProgress(func(p *MovieYearProgress) {
		p.Running = false
		p.Finished = true
	})
	n, err := c.rederiveStoredMovieYears(ctx)
	c.setYearProgress(func(p *MovieYearProgress) { p.Stored = n })
	if err != nil {
		c.noteYearError(ctx, err)
		if ctx.Err() != nil {
			return
		}
	}
	if err := c.settleUnchangedMovieYears(ctx); err != nil {
		c.noteYearError(ctx, err)
		if ctx.Err() != nil {
			return
		}
	}
	for ctx.Err() == nil {
		if until := c.yearPauseUntil.Load(); until > c.now().UnixNano() {
			if !waitGap(ctx, time.Duration(until-c.now().UnixNano())) {
				return
			}
			continue
		}
		c.drainMovieYearViews(ctx)
		if c.tmdb() == nil {
			c.noteYearError(ctx, errors.New("catalog: movie year refresh waiting for TMDB"))
			if !waitGap(ctx, 30*time.Second) {
				return
			}
			continue
		}
		key, ok, err := c.nextMovieYearKey(ctx)
		if err != nil {
			c.noteYearError(ctx, err)
			return
		}
		if ok {
			c.startYearFetch(ctx, key)
		} else if c.yearInflightN.Load() == 0 {
			pending, err := c.countPendingMovieYears(ctx)
			if err != nil {
				c.noteYearError(ctx, err)
				return
			}
			c.setYearProgress(func(p *MovieYearProgress) { p.Pending = pending })
			if pending == 0 {
				return
			}
		}
		gap := c.yearGap
		if gap <= 0 {
			gap = time.Second / yearFetchRate
		}
		if !waitGap(ctx, gap) {
			return
		}
	}
}

func (c *Catalog) setYearProgress(fn func(*MovieYearProgress)) {
	c.yearMu.Lock()
	fn(&c.yearState)
	c.yearMu.Unlock()
}

func (c *Catalog) noteYearError(ctx context.Context, err error) {
	if err == nil || ctx.Err() != nil {
		return
	}
	c.log.Error("catalog: movie year correction", "err", err)
	c.setYearProgress(func(p *MovieYearProgress) { p.LastError = err.Error() })
}

func (c *Catalog) movieYearProgress(ctx context.Context) MovieYearProgress {
	c.yearMu.RLock()
	p := c.yearState
	c.yearMu.RUnlock()
	n, err := c.countPendingMovieYears(ctx)
	if err == nil {
		p.Pending = n
	}
	return p
}

func (c *Catalog) countPendingMovieYears(ctx context.Context) (int, error) {
	var n int
	err := c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM catalog_titles WHERE type='movie' AND year_source=''`).Scan(&n)
	return n, err
}

// rederiveStoredMovieYears applies a primary date that is already stored.
// A movie that has never had a detail fetch keeps that date in released_ts.
// A row that already recorded primary_released_ts uses that, including a
// date before 1970. Detailed rows are left queued: their released_ts may
// be a later theatrical date, and the primary date was not kept.
func (c *Catalog) rederiveStoredMovieYears(ctx context.Context) (int, error) {
	yearOf := func(column string) string {
		if store.DialectOf(c.db) == store.Postgres {
			return `CAST(EXTRACT(YEAR FROM (to_timestamp(` + column + `) AT TIME ZONE 'UTC')) AS INTEGER)`
		}
		return `CAST(strftime('%Y', ` + column + `, 'unixepoch') AS INTEGER)`
	}
	total := 0
	res, err := c.db.ExecContext(ctx, `UPDATE catalog_titles SET primary_released_ts=released_ts, year=`+yearOf("released_ts")+`, year_source=? WHERE type='movie' AND year_source='' AND detail=0 AND released_ts<>0`, yearSourcePrimary)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	total += int(n)
	res, err = c.db.ExecContext(ctx, `UPDATE catalog_titles SET year=`+yearOf("primary_released_ts")+`, year_source=? WHERE type='movie' AND year_source='' AND primary_released_ts<>0`, yearSourcePrimary)
	if err != nil {
		return total, err
	}
	n, err = res.RowsAffected()
	if err != nil {
		return total, err
	}
	total += int(n)
	if total > 0 {
		c.writeGen.Add(1)
	}
	return total, nil
}

// settleUnchangedMovieYears marks movies whose stored year cannot differ
// from the primary year. A detailed January–March date stays queued: the
// primary release, or a type-1 premiere late the previous year, can fall in
// the other calendar year. No type-1 date is stored on its own.
func (c *Catalog) settleUnchangedMovieYears(ctx context.Context) error {
	month := movieReleaseMonthSQL(store.DialectOf(c.db))
	_, err := c.db.ExecContext(ctx, `UPDATE catalog_titles SET year_source=? WHERE type='movie' AND year_source='' AND released_ts=0`, yearSourceNone)
	if err != nil {
		return err
	}
	_, err = c.db.ExecContext(ctx, `UPDATE catalog_titles SET year_source=? WHERE type='movie' AND year_source='' AND NOT (detail=1 AND released_ts<>0 AND `+month+` BETWEEN 1 AND 3)`, yearSourceReleased)
	return err
}

func movieReleaseMonthSQL(d store.Dialect) string {
	if d == store.Postgres {
		return `CAST(EXTRACT(MONTH FROM (to_timestamp(released_ts) AT TIME ZONE 'UTC')) AS INTEGER)`
	}
	return `CAST(strftime('%m', released_ts, 'unixepoch') AS INTEGER)`
}

// movieYearMightDiffer reports whether a stored theatrical date can sit in
// a different calendar year from TMDB's primary release_date.
func movieYearMightDiffer(t core.Title) bool {
	if t.Type != core.TypeMovie || t.Released.IsZero() {
		return false
	}
	m := t.Released.UTC().Month()
	return m >= time.January && m <= time.March
}

// kickMovieYears starts a year refresh for movies on screen. It does not
// wait, and it does not fetch a movie whose stored month cannot change the year.
func (c *Catalog) kickMovieYears(items []core.Title) {
	if c == nil {
		return
	}
	for _, t := range items {
		if !movieYearMightDiffer(t) {
			continue
		}
		if c.yearAhead == nil || c.yearInflightN.Load() >= yearFetchSlots {
			c.enqueueMovieYear(t.Key)
			continue
		}
		if !c.startYearFetch(context.Background(), t.Key) {
			c.enqueueMovieYear(t.Key)
		}
	}
}

func (c *Catalog) enqueueMovieYear(key core.RatingKey) {
	if c.yearAhead == nil {
		return
	}
	select {
	case c.yearAhead <- key:
	default:
	}
}

func (c *Catalog) drainMovieYearViews(ctx context.Context) {
	if c.yearAhead == nil {
		return
	}
	for c.yearInflightN.Load() < yearFetchSlots {
		select {
		case <-ctx.Done():
			return
		case key := <-c.yearAhead:
			c.startYearFetch(ctx, key)
		default:
			return
		}
	}
}

func (c *Catalog) startYearFetch(ctx context.Context, key core.RatingKey) bool {
	if c.yearInflightN.Load() >= yearFetchSlots {
		return false
	}
	if _, loaded := c.yearInflight.LoadOrStore(key, struct{}{}); loaded {
		return false
	}
	if c.yearInflightN.Add(1) > yearFetchSlots {
		c.yearInflightN.Add(-1)
		c.yearInflight.Delete(key)
		return false
	}
	go func() {
		defer c.yearInflightN.Add(-1)
		defer c.yearInflight.Delete(key)
		advanced, err := c.refreshMovieYearKey(ctx, key)
		if advanced {
			c.setYearProgress(func(p *MovieYearProgress) { p.Refreshed++; p.LastError = "" })
		}
		if err != nil {
			c.noteYearError(ctx, err)
		}
		if errors.Is(err, tmdb.ErrUnavailable) || errors.Is(err, tmdb.ErrNoKey) {
			c.yearPauseUntil.Store(c.now().Add(30 * time.Second).UnixNano())
		}
	}()
	return true
}

func (c *Catalog) nextMovieYearKey(ctx context.Context) (core.RatingKey, bool, error) {
	var key int64
	err := c.db.QueryRowContext(ctx, `SELECT key FROM catalog_titles WHERE type='movie' AND year_source='' AND tmdb_id<>0 AND year_checked_at<=? ORDER BY browsable DESC, popularity DESC, key LIMIT 1`, c.now().Add(-yearRetryAfter).Unix()).Scan(&key)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return core.RatingKey(key), true, nil
}

// refreshMovieYearKey fetches one movie when its year is still unknown and
// its stored date can differ from the primary year. ErrUnavailable and
// ErrNoKey leave the row queued.
func (c *Catalog) refreshMovieYearKey(ctx context.Context, key core.RatingKey) (bool, error) {
	var source string
	var released int64
	var detail int
	err := c.db.QueryRowContext(ctx, `SELECT year_source, released_ts, detail FROM catalog_titles WHERE key=? AND type='movie'`, int64(key)).Scan(&source, &released, &detail)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if source != "" || detail != 1 || released == 0 || time.Unix(released, 0).UTC().Month() > time.March {
		return false, nil
	}
	return c.fetchMovieYear(ctx, key)
}

// refreshOneMovieYear fetches the next queued movie. Tests call it directly.
func (c *Catalog) refreshOneMovieYear(ctx context.Context) (bool, error) {
	key, ok, err := c.nextMovieYearKey(ctx)
	if !ok || err != nil {
		return false, err
	}
	return c.fetchMovieYear(ctx, key)
}

func (c *Catalog) fetchMovieYear(ctx context.Context, rating core.RatingKey) (bool, error) {
	if c.tmdb() == nil {
		return false, errors.New("catalog: movie year refresh waiting for TMDB")
	}
	d, err := c.tmdb().MovieDetails(ctx, rating.TMDB())
	if err != nil {
		if errors.Is(err, tmdb.ErrUnavailable) || errors.Is(err, tmdb.ErrNoKey) {
			return false, err
		}
		if errors.Is(err, core.ErrNotFound) {
			return true, c.markMovieYear(ctx, rating, yearSourceMissing)
		}
		if markErr := c.touchMovieYear(ctx, rating); markErr != nil {
			return false, markErr
		}
		return false, err
	}
	if d == nil || d.ID != rating.TMDB() {
		if markErr := c.touchMovieYear(ctx, rating); markErr != nil {
			return false, markErr
		}
		return false, fmt.Errorf("catalog: movie year refresh %d returned no details", rating.TMDB())
	}
	t := c.movieDetailsToTitle(ctx, d)
	if err := c.upsert(ctx, t, upsertOpts{browsable: c.meetsThreshold(t), detail: true, releaseDates: d.ReleaseDates, gateEnabled: c.config().GateMoviesByHomeRelease}); err != nil {
		return false, err
	}
	return true, nil
}

func (c *Catalog) markMovieYear(ctx context.Context, key core.RatingKey, source string) error {
	_, err := c.db.ExecContext(ctx, `UPDATE catalog_titles SET year_source=?, year_checked_at=? WHERE key=? AND type='movie' AND year_source=''`, source, c.now().Unix(), int64(key))
	return err
}

func (c *Catalog) touchMovieYear(ctx context.Context, key core.RatingKey) error {
	_, err := c.db.ExecContext(ctx, `UPDATE catalog_titles SET year_checked_at=? WHERE key=? AND type='movie' AND year_source=''`, c.now().Unix(), int64(key))
	return err
}

func waitGap(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// saveMovieYearSource records the date the year came from. A summary does
// not overwrite a detailed movie: that row's released_ts may be theatrical,
// and its year is corrected by the background refresh instead.
func saveMovieYearSource(ctx context.Context, tx *sql.Tx, t core.Title, detail int) error {
	if t.Type != core.TypeMovie || t.YearSource == "" {
		return nil
	}
	var primary int64
	if !t.PrimaryReleased.IsZero() {
		primary = t.PrimaryReleased.Unix()
	}
	_, err := tx.ExecContext(ctx, `UPDATE catalog_titles SET
 year_source=CASE WHEN ?=0 AND (year_source<>'' OR detail=1) THEN year_source ELSE ? END,
 primary_released_ts=CASE WHEN ?=0 AND (year_source<>'' OR detail=1) THEN primary_released_ts WHEN ?=1 THEN ? WHEN ?<>0 THEN ? ELSE primary_released_ts END
 WHERE key=? AND type='movie'`,
		detail, t.YearSource,
		detail, detail, primary, primary, primary,
		int64(t.Key))
	if err != nil {
		return fmt.Errorf("catalog: movie year source %d: %w", t.Key, err)
	}
	return nil
}
