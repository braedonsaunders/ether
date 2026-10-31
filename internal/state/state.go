// Package state implements per-user watch state and playlists over SQLite:
// core.State and core.Playlists.
//
// Semantics follow Plex:
//   - Progress at >= 90% of duration marks the item watched (view count
//     increments once per viewing, the resume offset clears).
//   - Progress below 5% is treated as "not really started": no resume point
//     is kept. Between 5% and 90% the item is in progress.
//   - Scrobble marks watched unconditionally; Unscrobble clears the view
//     count and resume point (the user's rating is kept).
package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func init() {
	store.Register("state", 1, `
CREATE TABLE state_watch (
  user_id INTEGER NOT NULL,
  rating_key INTEGER NOT NULL,
  view_offset_ns INTEGER NOT NULL DEFAULT 0,
  duration_ns INTEGER NOT NULL DEFAULT 0,
  view_count INTEGER NOT NULL DEFAULT 0,
  counted INTEGER NOT NULL DEFAULT 0,
  last_viewed_at INTEGER NOT NULL DEFAULT 0,
  user_rating REAL NOT NULL DEFAULT 0,
  updated_at INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (user_id, rating_key)
);
CREATE INDEX state_watch_inprogress ON state_watch(user_id, last_viewed_at DESC) WHERE view_offset_ns > 0;
CREATE TABLE state_history (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id INTEGER NOT NULL,
  rating_key INTEGER NOT NULL,
  viewed_at INTEGER NOT NULL,
  completed INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX state_history_user ON state_history(user_id, viewed_at DESC);
CREATE TABLE state_playlists (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id INTEGER NOT NULL,
  title TEXT NOT NULL,
  smart INTEGER NOT NULL DEFAULT 0,
  updated_at INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE state_playlist_items (
  playlist_id INTEGER NOT NULL REFERENCES state_playlists(id) ON DELETE CASCADE,
  position INTEGER NOT NULL,
  rating_key INTEGER NOT NULL,
  PRIMARY KEY (playlist_id, position)
);`)
	// version lets update() detect a concurrent writer on another node
	// (optimistic concurrency) instead of relying on an in-process lock.
	// Imported history rows are unique per (user, item, time) so two nodes
	// importing the same history cannot both insert it; live history keeps
	// every event. Rows written before this migration are all imported=0,
	// so no existing data needs deduplicating.
	store.Register("state", 2, `
ALTER TABLE state_watch ADD COLUMN version INTEGER NOT NULL DEFAULT 0;
ALTER TABLE state_history ADD COLUMN imported INTEGER NOT NULL DEFAULT 0;
CREATE UNIQUE INDEX state_history_imported ON state_history(user_id, rating_key, viewed_at) WHERE imported=1;`)
}

// WatchedThreshold and StartedThreshold are the fractions of duration at
// which an item counts as watched / meaningfully started.
const (
	WatchedThreshold = 0.90
	StartedThreshold = 0.05
)

// EventKind identifies a state change subscribers may react to.
type EventKind string

const (
	EventProgress   EventKind = "progress"   // a timeline update (incl. start/stop)
	EventCompleted  EventKind = "completed"  // crossed the watched threshold
	EventScrobble   EventKind = "scrobble"   // explicitly marked watched
	EventUnscrobble EventKind = "unscrobble" // explicitly marked unwatched
	EventRated      EventKind = "rated"
)

// Event is a watch-state change. Fraction is offset/duration (0 when the
// duration is unknown). Completed is true for EventCompleted/EventScrobble.
type Event struct {
	Kind      EventKind
	UserID    int64
	Key       core.RatingKey
	Offset    time.Duration
	Duration  time.Duration
	Fraction  float64
	Rating    float64 // EventRated only
	State     string  // raw timeline state for EventProgress: playing, paused, stopped
	Completed bool
	At        time.Time
	Imported  bool // reviewed history import; never an active playback timeline
	Origin    Origin
}

// Origin identifies the surface and client that made a watch-state change.
type Origin struct {
	Surface             core.Protocol
	SessionID, ClientID string
}
type originKey struct{}

// WithOrigin attaches the authenticated request origin to committed events.
func WithOrigin(ctx context.Context, origin Origin) context.Context {
	return context.WithValue(ctx, originKey{}, origin)
}
func originFrom(ctx context.Context) Origin {
	origin, _ := ctx.Value(originKey{}).(Origin)
	return origin
}

// Options tune a State. Now is injectable for tests.
type Options struct {
	Logger *slog.Logger
	Now    func() time.Time
	// Availability, when set (a clustered node on a shared PostgreSQL
	// database), makes timeline progress survive a database outage: a
	// Progress the database cannot take is held in memory (the latest per
	// user and title) and written when the database answers again, so
	// players are never blocked or failed by the outage. Nil (standalone)
	// writes progress synchronously as before.
	Availability *store.Availability
}

// State implements core.State and core.Playlists.
type State struct {
	db  *sql.DB
	log *slog.Logger
	now func() time.Time

	// wmu serialises this process's read-modify-write updates of watch rows
	// so they do not retry against each other. Correctness across processes
	// comes from the row version check in saveTx.
	// It is a one-slot channel so waiting for it honours the caller's
	// context (a write stuck on an unreachable database must not hold up
	// a bounded progress write beyond its timeout).
	wmu chan struct{}

	mu   sync.RWMutex
	subs map[int]func(Event)
	next int

	// progress holds timeline writes through a database outage (nil when
	// Options.Availability is nil).
	progress *store.Outbox[progressKey, progressWrite]
	gen      atomic.Uint64
}

// Generation changes when watch progress, played state, or a rating is saved.
func (s *State) Generation() uint64 {
	if s == nil {
		return 0
	}
	return s.gen.Load()
}

// State implements core.State; State.Playlists() returns the core.Playlists
// view (the two interfaces' Get methods cannot coexist on one Go type).
var _ core.State = (*State)(nil)

// New builds a State over an already-migrated database (see store.Open).
func New(db *sql.DB, opts Options) *State {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	s := &State{db: db, log: opts.Logger, now: opts.Now, subs: map[int]func(Event){}, wmu: make(chan struct{}, 1)}
	if opts.Availability != nil {
		s.progress = store.NewOutbox(store.OutboxOptions[progressKey, progressWrite]{
			Name: "watch progress", Availability: opts.Availability, Logger: opts.Logger,
			Merge: mergeProgress,
			Apply: func(ctx context.Context, k progressKey, w progressWrite) error {
				if w.before != nil {
					if err := s.progressAt(ctx, k.user, k.key, *w.before); err != nil {
						return err
					}
				}
				return s.progressAt(ctx, k.user, k.key, w)
			},
		})
	}
	return s
}

// Close writes progress still held from a database outage (bounded by
// ctx). It is a no-op on a standalone server.
func (s *State) Close(ctx context.Context) error {
	if s.progress == nil {
		return nil
	}
	return s.progress.Close(ctx)
}

// OutboxStatus reports timeline writes held through a database outage;
// ok is false when progress is written synchronously (standalone).
func (s *State) OutboxStatus() (st store.OutboxStatus, ok bool) {
	if s.progress == nil {
		return st, false
	}
	return s.progress.Status(), true
}

type progressKey struct {
	user int64
	key  core.RatingKey
}

// progressWrite is one held timeline update. before carries an earlier
// held update that crossed the watched threshold when a later one (a
// rewind, a restart from the top) replaced it, so the completion is not
// lost by keeping only the newest position.
type progressWrite struct {
	offset, duration time.Duration
	state            string
	at               time.Time
	before           *progressWrite
	origin           Origin
}

func (w progressWrite) completes() bool {
	return w.duration > 0 && float64(w.offset) >= WatchedThreshold*float64(w.duration)
}

func mergeProgress(old, w progressWrite) progressWrite {
	carry := old.before
	if old.completes() {
		c := old
		c.before = nil
		carry = &c
	}
	if w.completes() {
		carry = nil // the newer update completes the viewing itself
	}
	w.before = carry
	return w
}

// Subscribe registers fn for state events (scrobbles, progress, ratings) so
// Trakt sync, recommendations, prefetch etc. can react. It returns an
// unsubscribe function. Handlers run synchronously after the change is
// committed; a panicking handler is recovered and logged.
func (s *State) Subscribe(fn func(Event)) func() {
	s.mu.Lock()
	id := s.next
	s.next++
	s.subs[id] = fn
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		delete(s.subs, id)
		s.mu.Unlock()
	}
}

func (s *State) emit(ev Event) {
	s.mu.RLock()
	fns := make([]func(Event), 0, len(s.subs))
	for _, f := range s.subs {
		fns = append(fns, f)
	}
	s.mu.RUnlock()
	for _, f := range fns {
		func() {
			defer func() {
				if r := recover(); r != nil {
					s.log.Warn("state subscriber panicked", "recover", r, "kind", ev.Kind)
				}
			}()
			f(ev)
		}()
	}
}

// IsWatched reports whether a status counts as watched (watched at least once).
func IsWatched(ws core.WatchStatus) bool { return ws.ViewCount > 0 }

// IsInProgress reports whether a status has a resume point.
func IsInProgress(ws core.WatchStatus) bool { return ws.ViewOffset > 0 }

// Fraction returns offset/duration, or 0 when the duration is unknown.
func Fraction(ws core.WatchStatus) float64 {
	if ws.Duration <= 0 {
		return 0
	}
	return float64(ws.ViewOffset) / float64(ws.Duration)
}

type row struct {
	ws      core.WatchStatus
	counted bool
	exists  bool  // a stored row was loaded
	version int64 // stored version when loaded
}

// errConflict means another writer changed the row since it was loaded.
var errConflict = errors.New("state: concurrent watch update")

// maxUpdateAttempts bounds optimistic retries of one update.
const maxUpdateAttempts = 10

// saveTx writes r only if the stored row is still the one loaded (same
// version, or still absent); otherwise it returns errConflict.
func saveTx(ctx context.Context, tx *sql.Tx, userID int64, r row) error {
	var last int64
	if !r.ws.LastViewedAt.IsZero() {
		last = r.ws.LastViewedAt.UnixNano()
	}
	counted := 0
	if r.counted {
		counted = 1
	}
	var res sql.Result
	var err error
	if r.exists {
		res, err = tx.ExecContext(ctx, `UPDATE state_watch SET view_offset_ns=?, duration_ns=?, view_count=?, counted=?,
		  last_viewed_at=?, user_rating=?, updated_at=?, version=version+1
		  WHERE user_id=? AND rating_key=? AND version=?`,
			int64(r.ws.ViewOffset), int64(r.ws.Duration), r.ws.ViewCount, counted, last, r.ws.UserRating, r.ws.UpdatedAt.UnixNano(),
			userID, int64(r.ws.Key), r.version)
	} else {
		res, err = tx.ExecContext(ctx, `INSERT INTO state_watch(user_id, rating_key, view_offset_ns, duration_ns, view_count, counted, last_viewed_at, user_rating, updated_at)
		VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(user_id, rating_key) DO NOTHING`,
			userID, int64(r.ws.Key), int64(r.ws.ViewOffset), int64(r.ws.Duration), r.ws.ViewCount, counted, last, r.ws.UserRating, r.ws.UpdatedAt.UnixNano())
	}
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return errConflict
	}
	return nil
}

func addHistoryTx(ctx context.Context, tx *sql.Tx, userID int64, key core.RatingKey, at time.Time, completed bool) error {
	c := 0
	if completed {
		c = 1
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO state_history(user_id, rating_key, viewed_at, completed) VALUES(?,?,?,?)`,
		userID, int64(key), at.UnixNano(), c)
	return err
}

// Get returns the status for one key. A never-touched key returns a zero
// status (with Key set) and no error.
func (s *State) Get(ctx context.Context, userID int64, key core.RatingKey) (core.WatchStatus, error) {
	m, err := s.GetMany(ctx, userID, []core.RatingKey{key})
	if err != nil {
		return core.WatchStatus{Key: key}, err
	}
	return m[key], nil
}

// GetMany returns a status for every requested key (zero when untouched).
func (s *State) GetMany(ctx context.Context, userID int64, keys []core.RatingKey) (map[core.RatingKey]core.WatchStatus, error) {
	out := make(map[core.RatingKey]core.WatchStatus, len(keys))
	for _, k := range keys {
		out[k] = core.WatchStatus{Key: k}
	}
	if len(keys) == 0 {
		return out, nil
	}
	// Chunked IN queries keep the statement under SQLite's variable limit.
	for start := 0; start < len(keys); start += 400 {
		end := min(start+400, len(keys))
		chunk := keys[start:end]
		q := `SELECT rating_key, view_offset_ns, duration_ns, view_count, last_viewed_at, user_rating, updated_at
			FROM state_watch WHERE user_id=? AND rating_key IN (`
		args := []any{userID}
		for i, k := range chunk {
			if i > 0 {
				q += ","
			}
			q += "?"
			args = append(args, int64(k))
		}
		q += ")"
		rows, err := s.db.QueryContext(ctx, q, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var ws core.WatchStatus
			var key, off, dur, last, upd int64
			if err := rows.Scan(&key, &off, &dur, &ws.ViewCount, &last, &ws.UserRating, &upd); err != nil {
				rows.Close()
				return nil, err
			}
			ws.Key = core.RatingKey(key)
			ws.ViewOffset = time.Duration(off)
			ws.Duration = time.Duration(dur)
			if last > 0 {
				ws.LastViewedAt = time.Unix(0, last).UTC()
			}
			if upd > 0 {
				ws.UpdatedAt = time.Unix(0, upd).UTC()
			}
			out[ws.Key] = ws
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return out, nil
}

// PageUserData reads watch progress and the page's favorite, not-interested
// and watchlist flags in one query. Keys the caller has not authorized must
// not be passed in.
func (s *State) PageUserData(ctx context.Context, userID int64, keys []core.RatingKey) (map[core.RatingKey]core.WatchStatus, error) {
	out := make(map[core.RatingKey]core.WatchStatus, len(keys))
	for _, key := range keys {
		out[key] = core.WatchStatus{Key: key}
	}
	if s == nil || s.db == nil || userID <= 0 || len(keys) == 0 {
		return out, nil
	}
	for start := 0; start < len(keys); start += 200 {
		chunk := keys[start:min(start+200, len(keys))]
		var b strings.Builder
		args := make([]any, 0, 4+len(chunk)*4)
		writeIn := func(prefix string, asText bool, extra ...any) {
			b.WriteString(prefix)
			args = append(args, extra...)
			for i, key := range chunk {
				if i > 0 {
					b.WriteByte(',')
				}
				b.WriteByte('?')
				if asText {
					args = append(args, key.String())
				} else {
					args = append(args, int64(key))
				}
			}
			b.WriteByte(')')
		}
		writeIn(`SELECT 'w', rating_key, view_offset_ns, duration_ns, view_count, last_viewed_at, user_rating, updated_at FROM state_watch WHERE user_id=? AND rating_key IN (`, false, userID)
		// Rating keys are kind*10^15 plus a payload, so they do not fit in
		// PostgreSQL INTEGER (int4). SQLite INTEGER is already 64-bit;
		// BIGINT keeps both engines on that width.
		writeIn(` UNION ALL SELECT 'f', CAST(subject_id AS BIGINT), 0, 0, 0, 0, 0, 0 FROM favorites_items WHERE viewer_id=? AND kind=? AND subject_id IN (`, true, userID, "title")
		writeIn(` UNION ALL SELECT 'n', CAST(title_key AS BIGINT), 0, 0, 0, 0, 0, 0 FROM notinterested_titles WHERE viewer_id=? AND title_key IN (`, true, userID)
		writeIn(` UNION ALL SELECT 'l', item_key, 0, 0, 0, 0, 0, 0 FROM watchlist_items WHERE user_id=? AND item_key IN (`, false, userID)
		rows, err := s.db.QueryContext(ctx, b.String(), args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var kind string
			var key, off, dur, last, upd int64
			var views int
			var rating float64
			if err := rows.Scan(&kind, &key, &off, &dur, &views, &last, &rating, &upd); err != nil {
				rows.Close()
				return nil, err
			}
			status := out[core.RatingKey(key)]
			status.Key = core.RatingKey(key)
			switch kind {
			case "w":
				status.ViewOffset = time.Duration(off)
				status.Duration = time.Duration(dur)
				status.ViewCount = views
				status.UserRating = rating
				if last > 0 {
					status.LastViewedAt = time.Unix(0, last).UTC()
				}
				if upd > 0 {
					status.UpdatedAt = time.Unix(0, upd).UTC()
				}
			case "f":
				status.IsFavorite = true
			case "n":
				status.IsNotInterested = true
			case "l":
				status.InWatchlist = true
			}
			out[status.Key] = status
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return out, nil
}

// update runs one read-modify-write of watch rows: fn mutates the loaded
// row and reports whether to log history (and whether that history entry is
// a completion). The rows and history are written in one transaction. When
// another node changed a row in between, the transaction is discarded and
// re-run from fresh reads, so fn must be safe to call again. Events are
// returned so the caller emits them after the lock is released (subscribers
// may call back into State).
func (s *State) update(ctx context.Context, userID int64, keys []core.RatingKey, fn func(cur *row, now time.Time) (logHistory, completed bool, evs []Event)) error {
	return s.updateAt(ctx, userID, keys, time.Time{}, fn)
}

// updateAt is update at a given time (zero: now); held progress replays
// with the time it was reported.
func (s *State) updateAt(ctx context.Context, userID int64, keys []core.RatingKey, at time.Time, fn func(cur *row, now time.Time) (logHistory, completed bool, evs []Event)) error {
	if err := s.lockWrites(ctx); err != nil {
		return err
	}
	now := at.UTC()
	if at.IsZero() {
		now = s.now().UTC()
	}
	var events []Event
	var err error
	for attempt := 0; attempt < maxUpdateAttempts; attempt++ {
		events, err = s.updateTx(ctx, userID, keys, now, fn)
		if !errors.Is(err, errConflict) {
			break
		}
	}
	s.unlockWrites()
	if err != nil {
		return err
	}
	s.gen.Add(1)
	for _, ev := range events {
		ev.Origin = originFrom(ctx)
		s.emit(ev)
	}
	return nil
}

func (s *State) lockWrites(ctx context.Context) error {
	select {
	case s.wmu <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *State) unlockWrites() { <-s.wmu }

func (s *State) updateTx(ctx context.Context, userID int64, keys []core.RatingKey, now time.Time, fn func(cur *row, now time.Time) (bool, bool, []Event)) ([]Event, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if receipt, ok := ctx.Value(progressReceiptKey{}).(func(*sql.Tx) error); ok {
		if err := receipt(tx); err != nil {
			return nil, err
		}
	}
	var events []Event
	for _, key := range keys {
		cur, err := loadTx(ctx, tx, userID, key)
		if err != nil {
			return nil, err
		}
		logHistory, completed, evs := fn(&cur, now)
		if err := saveTx(ctx, tx, userID, cur); err != nil {
			return nil, err
		}
		if logHistory {
			if err := addHistoryTx(ctx, tx, userID, key, now, completed); err != nil {
				return nil, err
			}
		}
		events = append(events, evs...)
	}
	return events, tx.Commit()
}

func loadTx(ctx context.Context, tx *sql.Tx, userID int64, key core.RatingKey) (row, error) {
	r := row{ws: core.WatchStatus{Key: key}}
	var off, dur, last, upd int64
	var counted int
	err := tx.QueryRowContext(ctx, `SELECT view_offset_ns, duration_ns, view_count, counted, last_viewed_at, user_rating, updated_at, version
		FROM state_watch WHERE user_id=? AND rating_key=?`, userID, int64(key)).
		Scan(&off, &dur, &r.ws.ViewCount, &counted, &last, &r.ws.UserRating, &upd, &r.version)
	if errors.Is(err, sql.ErrNoRows) {
		return r, nil
	}
	if err != nil {
		return r, err
	}
	r.exists = true
	r.ws.ViewOffset = time.Duration(off)
	r.ws.Duration = time.Duration(dur)
	r.counted = counted != 0
	if last > 0 {
		r.ws.LastViewedAt = time.Unix(0, last).UTC()
	}
	if upd > 0 {
		r.ws.UpdatedAt = time.Unix(0, upd).UTC()
	}
	return r, nil
}

// Progress records a timeline update. state is Plex's timeline state
// ("playing", "paused", "stopped", "buffering"). See the package doc for the
// 5% start and 90% completion rules. A duration of 0 keeps the previously known duration; with no
// known duration the offset is kept as a resume point and nothing is ever
// marked watched by progress alone.
//
// On a clustered node a Progress the database cannot take within a short
// timeout is held and written later (see Options.Availability); it then
// returns nil.
func (s *State) Progress(ctx context.Context, userID int64, key core.RatingKey, offset, duration time.Duration, state string) error {
	if offset < 0 {
		offset = 0
	}
	w := progressWrite{offset: offset, duration: duration, state: state, origin: originFrom(ctx)}
	if s.progress == nil {
		return s.progressAt(ctx, userID, key, w)
	}
	w.at = s.now()
	_, err := s.progress.Write(ctx, progressKey{userID, key}, w)
	return err
}

// progressAt applies one timeline update (at zero: now).
func (s *State) progressAt(ctx context.Context, userID int64, key core.RatingKey, w progressWrite) error {
	ctx = WithOrigin(ctx, w.origin)
	reported, duration, state := w.offset, w.duration, w.state
	return s.updateAt(ctx, userID, []core.RatingKey{key}, w.at, func(cur *row, now time.Time) (bool, bool, []Event) {
		offset := reported // fn may be re-run after a conflict
		if duration > 0 {
			cur.ws.Duration = duration
		}
		if cur.ws.Duration > 0 && offset > cur.ws.Duration {
			offset = cur.ws.Duration
		}
		frac := 0.0
		if cur.ws.Duration > 0 {
			frac = float64(offset) / float64(cur.ws.Duration)
		}
		completed, started := false, false
		switch {
		case cur.ws.Duration > 0 && frac >= WatchedThreshold:
			if !cur.counted {
				cur.ws.ViewCount++
				completed = true
			}
			cur.counted = true
			cur.ws.ViewOffset = 0
		case cur.ws.Duration > 0 && frac < StartedThreshold:
			// Not really started (or rewound to the beginning): no resume
			// point. `counted` is left alone so scrubbing back and forth
			// over the end of one viewing cannot inflate the view count.
			cur.ws.ViewOffset = 0
		default:
			started = cur.ws.ViewOffset == 0 && !cur.counted && state != "stopped"
			cur.counted = false // a (re)viewing in progress
			cur.ws.ViewOffset = offset
		}
		cur.ws.LastViewedAt = now
		cur.ws.UpdatedAt = now

		evFrac := frac
		if completed {
			evFrac = 1
		}
		ev := Event{Kind: EventProgress, UserID: userID, Key: cur.ws.Key, Offset: offset, Duration: cur.ws.Duration,
			Fraction: evFrac, State: state, Completed: completed, At: now}
		evs := []Event{ev}
		if completed {
			ev.Kind = EventCompleted
			evs = append(evs, ev)
		}
		// History: a completion logs a completed entry; the first
		// meaningful progress of a viewing logs a "started" entry.
		return completed || started, completed, evs
	})
}

// Scrobble marks an item watched: view count increments, offset clears.
func (s *State) Scrobble(ctx context.Context, userID int64, key core.RatingKey) error {
	return s.ScrobbleMany(ctx, userID, []core.RatingKey{key})
}

// ScrobbleMany marks several items watched in one transaction — the Plex
// front end expands a show or season scrobble into its episode keys and
// calls this. Each key gets a history entry and an EventScrobble.
func (s *State) ScrobbleMany(ctx context.Context, userID int64, keys []core.RatingKey) error {
	return s.update(ctx, userID, keys, func(cur *row, now time.Time) (bool, bool, []Event) {
		cur.ws.ViewCount++
		cur.ws.ViewOffset = 0
		cur.counted = true
		cur.ws.LastViewedAt = now
		cur.ws.UpdatedAt = now
		return true, true, []Event{{Kind: EventScrobble, UserID: userID, Key: cur.ws.Key, Duration: cur.ws.Duration,
			Fraction: 1, Completed: true, At: now}}
	})
}

// Unscrobble marks an item unwatched: view count and offset clear. The
// user's rating and history log are kept (history is a log, not status).
func (s *State) Unscrobble(ctx context.Context, userID int64, key core.RatingKey) error {
	return s.UnscrobbleMany(ctx, userID, []core.RatingKey{key})
}

// UnscrobbleMany marks several items unwatched in one transaction (show or
// season "mark unwatched").
func (s *State) UnscrobbleMany(ctx context.Context, userID int64, keys []core.RatingKey) error {
	return s.update(ctx, userID, keys, func(cur *row, now time.Time) (bool, bool, []Event) {
		cur.ws.ViewCount = 0
		cur.ws.ViewOffset = 0
		cur.counted = false
		cur.ws.UpdatedAt = now
		return false, false, []Event{{Kind: EventUnscrobble, UserID: userID, Key: cur.ws.Key, At: now}}
	})
}

// Rate sets the user's rating (0-10; 0 clears it).
func (s *State) Rate(ctx context.Context, userID int64, key core.RatingKey, rating float64) error {
	if rating < 0 || rating > 10 || rating != rating {
		return fmt.Errorf("rating %v out of range 0-10", rating)
	}
	return s.update(ctx, userID, []core.RatingKey{key}, func(cur *row, now time.Time) (bool, bool, []Event) {
		cur.ws.UserRating = rating
		cur.ws.UpdatedAt = now
		return false, false, []Event{{Kind: EventRated, UserID: userID, Key: cur.ws.Key, Rating: rating, At: now}}
	})
}

// InProgress returns items with a resume point, most recently viewed first.
func (s *State) InProgress(ctx context.Context, userID int64, limit int) ([]core.WatchStatus, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `SELECT rating_key, view_offset_ns, duration_ns, view_count, last_viewed_at, user_rating, updated_at
		FROM state_watch WHERE user_id=? AND view_offset_ns>0 ORDER BY last_viewed_at DESC, updated_at DESC LIMIT ?`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []core.WatchStatus
	for rows.Next() {
		var ws core.WatchStatus
		var key, off, dur, last, upd int64
		if err := rows.Scan(&key, &off, &dur, &ws.ViewCount, &last, &ws.UserRating, &upd); err != nil {
			return nil, err
		}
		ws.Key = core.RatingKey(key)
		ws.ViewOffset = time.Duration(off)
		ws.Duration = time.Duration(dur)
		if last > 0 {
			ws.LastViewedAt = time.Unix(0, last).UTC()
		}
		if upd > 0 {
			ws.UpdatedAt = time.Unix(0, upd).UTC()
		}
		out = append(out, ws)
	}
	return out, rows.Err()
}

// History returns the viewing log, newest first. A zero since means all.
func (s *State) History(ctx context.Context, userID int64, since time.Time, limit int) ([]core.HistoryEntry, error) {
	if limit <= 0 {
		limit = 100
	}
	var sinceNs int64
	if !since.IsZero() {
		sinceNs = since.UnixNano()
	}
	rows, err := s.db.QueryContext(ctx, `SELECT rating_key, viewed_at, completed FROM state_history
		WHERE user_id=? AND viewed_at>=? ORDER BY viewed_at DESC, id DESC LIMIT ?`, userID, sinceNs, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []core.HistoryEntry
	for rows.Next() {
		var e core.HistoryEntry
		var key, at int64
		var completed int
		if err := rows.Scan(&key, &at, &completed); err != nil {
			return nil, err
		}
		e.UserID = userID
		e.Key = core.RatingKey(key)
		e.ViewedAt = time.Unix(0, at).UTC()
		e.Completed = completed != 0
		out = append(out, e)
	}
	return out, rows.Err()
}

// InProgressAny lists distinct keys in progress for any user.
func (s *State) InProgressAny(ctx context.Context) ([]core.RatingKey, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT rating_key FROM state_watch WHERE view_offset_ns>0 ORDER BY rating_key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []core.RatingKey
	for rows.Next() {
		var k int64
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out = append(out, core.RatingKey(k))
	}
	return out, rows.Err()
}

// WatchedEpisodes lists watched episode keys (view_count > 0) for userID.
func (s *State) WatchedEpisodes(ctx context.Context, userID int64) ([]core.RatingKey, error) {
	const (
		minEp = int64(core.KindEpisode) * 1_000_000_000_000_000
		maxEp = int64(core.KindEpisode+1)*1_000_000_000_000_000 - 1
	)
	rows, err := s.db.QueryContext(ctx, `SELECT rating_key FROM state_watch
		WHERE user_id=? AND rating_key BETWEEN ? AND ? AND view_count>0 ORDER BY rating_key`,
		userID, minEp, maxEp)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []core.RatingKey
	for rows.Next() {
		var k int64
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out = append(out, core.RatingKey(k))
	}
	return out, rows.Err()
}

// Rollup summarises the watched state of a set of children (the episodes of
// a season/show) for Plex's viewedLeafCount-style fields.
type Rollup struct {
	Total      int
	Watched    int  // viewCount > 0
	InProgress int  // has a resume point
	AllWatched bool // Total > 0 && Watched == Total
	AnyWatched bool
}

// Summarize rolls up children using an already-fetched status map. Missing
// entries count as untouched.
func Summarize(children []core.RatingKey, statuses map[core.RatingKey]core.WatchStatus) Rollup {
	r := Rollup{Total: len(children)}
	for _, k := range children {
		ws := statuses[k]
		if IsWatched(ws) {
			r.Watched++
		}
		if IsInProgress(ws) {
			r.InProgress++
		}
	}
	r.AnyWatched = r.Watched > 0
	r.AllWatched = r.Total > 0 && r.Watched == r.Total
	return r
}

// ChildrenStatus fetches statuses for children and rolls them up.
func (s *State) ChildrenStatus(ctx context.Context, userID int64, children []core.RatingKey) (Rollup, map[core.RatingKey]core.WatchStatus, error) {
	m, err := s.GetMany(ctx, userID, children)
	if err != nil {
		return Rollup{}, nil, err
	}
	return Summarize(children, m), m, nil
}
