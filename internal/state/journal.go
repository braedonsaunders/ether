package state

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// SyncAspect is the independent half of a title's account sync: watched
// state, or a rating. A newer rating must not erase a pending watch, and
// the reverse.
type SyncAspect string

const (
	SyncWatch  SyncAspect = "watch"
	SyncRating SyncAspect = "rating"
)

// SyncKind is what a journal row asks a subscriber to deliver. Watch kinds
// are the Plex viewState values "complete" and "unplayed". "rating" carries
// the raw local rating; the subscriber decides which values it can send.
const (
	SyncComplete   = "complete"
	SyncUnplayed   = "unplayed"
	SyncRatingKind = "rating"
)

// SyncRow is one durable intent. Seq orders intents. Bulk is set when the
// watch change was one of several keys in the same transaction (a season
// or show mark).
type SyncRow struct {
	Seq        int64
	UserID     int64
	Key        core.RatingKey
	Aspect     SyncAspect
	Kind       string
	Rating     float64
	At         time.Time
	Bulk       bool
	Dead       bool
	LastErr    string
	Attempts   int
	ConsentGen int64
}

// ErrLocalNewer means a remote import is older than the stored row.
var ErrLocalNewer = errors.New("state: local watch state is newer than the remote record")

// SetJournalGate installs the per-viewer decision for durable account-sync
// intents. The generation is the consent fence observed by that decision.
// The watch transaction still commits when the gate returns false.
func (s *State) SetJournalGate(fn func(ctx context.Context, tx *sql.Tx, userID int64) (generation int64, ok bool, err error)) {
	s.mu.Lock()
	s.journalGate = fn
	s.mu.Unlock()
}

// SetJournalFault installs a test hook that fails the journal write inside
// the watch transaction. Production leaves it unset.
func (s *State) SetJournalFault(fn func() error) {
	s.mu.Lock()
	s.journalFault = fn
	s.mu.Unlock()
}

func (s *State) journalEvents(ctx context.Context, tx *sql.Tx, events []Event, force bool) error {
	s.mu.RLock()
	fault, gate := s.journalFault, s.journalGate
	s.mu.RUnlock()
	if fault != nil {
		if err := fault(); err != nil {
			return err
		}
	}
	allowed := map[int64]int64{}
	admitted := map[int64]bool{}
	if !force && gate != nil {
		for _, ev := range events {
			if _, seen := admitted[ev.UserID]; seen {
				continue
			}
			gen, ok, err := gate(ctx, tx, ev.UserID)
			if err != nil {
				return err
			}
			admitted[ev.UserID] = ok
			allowed[ev.UserID] = gen
		}
	}
	watchN := 0
	for _, ev := range events {
		if ev.Kind == EventCompleted || ev.Kind == EventScrobble || ev.Kind == EventUnscrobble {
			watchN++
		}
	}
	bulk := watchN > 1
	for _, ev := range events {
		aspect, kind, ok := journalKind(ev)
		if !ok || ev.Account {
			continue
		}
		if !force && gate != nil && !admitted[ev.UserID] {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE state_sync_journal SET superseded=1, claimed_by='', claimed_at=0
			WHERE user_id=? AND rating_key=? AND aspect=? AND delivered_at=0 AND superseded=0 AND dead=0`,
			ev.UserID, int64(ev.Key), string(aspect)); err != nil {
			return err
		}
		at := ev.At.UTC()
		if at.IsZero() {
			at = time.Now().UTC()
		}
		b := 0
		if bulk && aspect == SyncWatch {
			b = 1
		}
		// Keep the generation the gate observed. Reading the fence again here
		// would stamp a disable that committed after that observation.
		gen := allowed[ev.UserID]
		if force {
			if err := tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT generation FROM plexwatch_fence WHERE user_id=?), 0)`, ev.UserID).Scan(&gen); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO state_sync_journal(user_id,rating_key,aspect,kind,rating,occurred_at,bulk,consent_generation)
			VALUES(?,?,?,?,?,?,?,?)`, ev.UserID, int64(ev.Key), string(aspect), kind, ev.Rating, at.UnixNano(), b, gen); err != nil {
			return err
		}
	}
	return nil
}

func journalKind(ev Event) (SyncAspect, string, bool) {
	switch ev.Kind {
	case EventCompleted, EventScrobble:
		return SyncWatch, SyncComplete, true
	case EventUnscrobble:
		return SyncWatch, SyncUnplayed, true
	case EventRated:
		return SyncRating, SyncRatingKind, true
	default:
		return "", "", false
	}
}

// NoteSyncIntents appends explicit intents (an owner-chosen export) in one
// transaction, superseding older undelivered rows of the same aspect.
func (s *State) NoteSyncIntents(ctx context.Context, rows []SyncRow) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	evs := make([]Event, 0, len(rows))
	for _, row := range rows {
		kind := EventScrobble
		switch row.Kind {
		case SyncUnplayed:
			kind = EventUnscrobble
		case SyncRatingKind:
			kind = EventRated
		}
		evs = append(evs, Event{Kind: kind, UserID: row.UserID, Key: row.Key, Rating: row.Rating, At: row.At})
	}
	// A multi-row export is not a season batch. Write each row as its own
	// non-bulk intent so isBulk is reserved for one user action.
	for _, ev := range evs {
		if err := s.journalEvents(ctx, tx, []Event{ev}, true); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ClaimSync takes the oldest live intent for holder. A row already claimed
// by a live holder is left alone until stale.
func (s *State) ClaimSync(ctx context.Context, holder string, now time.Time, stale time.Duration) (SyncRow, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return SyncRow{}, false, err
	}
	defer tx.Rollback()
	var row SyncRow
	var key, at, bulk int64
	var aspect, kind string
	err = tx.QueryRowContext(ctx, `SELECT seq, user_id, rating_key, aspect, kind, rating, occurred_at, bulk, attempts, consent_generation
		FROM state_sync_journal
		WHERE delivered_at=0 AND superseded=0 AND dead=0 AND next_attempt<=?
		  AND (claimed_by='' OR claimed_at<?)
		ORDER BY seq LIMIT 1`, now.UnixNano(), now.Add(-stale).UnixNano()).
		Scan(&row.Seq, &row.UserID, &key, &aspect, &kind, &row.Rating, &at, &bulk, &row.Attempts, &row.ConsentGen)
	if errors.Is(err, sql.ErrNoRows) {
		return SyncRow{}, false, nil
	}
	if err != nil {
		return SyncRow{}, false, err
	}
	res, err := tx.ExecContext(ctx, `UPDATE state_sync_journal SET claimed_by=?, claimed_at=?
		WHERE seq=? AND delivered_at=0 AND superseded=0 AND dead=0 AND (claimed_by='' OR claimed_at<?)`,
		holder, now.UnixNano(), row.Seq, now.Add(-stale).UnixNano())
	if err != nil {
		return SyncRow{}, false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return SyncRow{}, false, err
	}
	if n != 1 {
		return SyncRow{}, false, tx.Commit()
	}
	row.Key = core.RatingKey(key)
	row.Aspect = SyncAspect(aspect)
	row.Kind = kind
	row.At = time.Unix(0, at).UTC()
	row.Bulk = bulk != 0
	return row, true, tx.Commit()
}

// SyncCurrent reports whether seq is still the holder's live intent.
func (s *State) SyncCurrent(ctx context.Context, seq int64, holder string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM state_sync_journal
		WHERE seq=? AND claimed_by=? AND superseded=0 AND dead=0 AND delivered_at=0`, seq, holder).Scan(&n)
	return n == 1, err
}

// AckSync marks a send done only while the row is still the live intent.
// ok is false when a newer intent superseded it during the send.
func (s *State) AckSync(ctx context.Context, seq int64, holder string, at time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE state_sync_journal
		SET delivered_at=?, claimed_by='', claimed_at=0
		WHERE seq=? AND claimed_by=? AND superseded=0 AND dead=0 AND delivered_at=0`,
		at.UnixNano(), seq, holder)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// PauseSync puts a claimed row back without counting an attempt. Delivery
// uses it when another node still holds the title lock, so this node can
// move on to a different row instead of claiming the same one again.
func (s *State) PauseSync(ctx context.Context, seq int64, holder string, until time.Time, reason string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE state_sync_journal
		SET claimed_by='', claimed_at=0, next_attempt=?, last_error=?
		WHERE seq=? AND claimed_by=? AND delivered_at=0 AND dead=0`,
		until.UnixNano(), trimErr(reason), seq, holder)
	return err
}

// DeferSync puts a claimed row back, later, with an error note.
func (s *State) DeferSync(ctx context.Context, seq int64, holder string, until time.Time, reason string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE state_sync_journal
		SET claimed_by='', claimed_at=0, attempts=attempts+1, next_attempt=?, last_error=?
		WHERE seq=? AND claimed_by=? AND delivered_at=0 AND dead=0`,
		until.UnixNano(), trimErr(reason), seq, holder)
	return err
}

// DeadSync stops delivery of one row without changing local watch state.
func (s *State) DeadSync(ctx context.Context, seq int64, holder string, reason string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE state_sync_journal
		SET dead=1, claimed_by='', claimed_at=0, last_error=?
		WHERE seq=? AND (claimed_by=? OR claimed_by='') AND delivered_at=0`,
		trimErr(reason), seq, holder)
	return err
}

// ReleaseSync drops a claim so another worker can take the row.
func (s *State) ReleaseSync(ctx context.Context, seq int64, holder string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE state_sync_journal SET claimed_by='', claimed_at=0
		WHERE seq=? AND claimed_by=? AND delivered_at=0`, seq, holder)
	return err
}

// DropPendingSync cancels every undelivered intent for a viewer.
func (s *State) DropPendingSync(ctx context.Context, userID int64, reason string) error {
	return dropPending(ctx, s.db, userID, reason)
}

// DropPendingSyncTx is DropPendingSync on an open transaction, so a consent
// change and the cancellation commit together.
func (s *State) DropPendingSyncTx(ctx context.Context, tx *sql.Tx, userID int64, reason string) error {
	return dropPending(ctx, tx, userID, reason)
}

func dropPending(ctx context.Context, db interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, userID int64, reason string) error {
	_, err := db.ExecContext(ctx, `UPDATE state_sync_journal
		SET dead=1, claimed_by='', claimed_at=0, last_error=?
		WHERE user_id=? AND delivered_at=0 AND superseded=0 AND dead=0`,
		trimErr(reason), userID)
	return err
}

// RetryDeadSync makes missing-GUID rows claimable again. Rejected and
// unsupported rows stay dead.
func (s *State) RetryDeadSync(ctx context.Context, userID int64, reasons []string) error {
	if len(reasons) == 0 {
		return nil
	}
	q := `UPDATE state_sync_journal SET dead=0, attempts=0, next_attempt=0, claimed_by='', claimed_at=0
		WHERE user_id=? AND delivered_at=0 AND superseded=0 AND dead=1 AND last_error IN (`
	args := []any{userID}
	for i, reason := range reasons {
		if i > 0 {
			q += ","
		}
		q += "?"
		args = append(args, reason)
	}
	q += ")"
	_, err := s.db.ExecContext(ctx, q, args...)
	return err
}

// LockSync holds one user/title/aspect so two nodes cannot deliver it at once.
func (s *State) LockSync(ctx context.Context, userID int64, key core.RatingKey, aspect SyncAspect, holder string, now time.Time, ttl time.Duration) (bool, error) {
	until := now.Add(ttl).UnixNano()
	res, err := s.db.ExecContext(ctx, `UPDATE state_sync_lock SET holder=?, until_at=?
		WHERE user_id=? AND rating_key=? AND aspect=? AND (holder='' OR holder=? OR until_at<?)`,
		holder, until, userID, int64(key), string(aspect), holder, now.UnixNano())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if n == 1 {
		return true, nil
	}
	res, err = s.db.ExecContext(ctx, `INSERT INTO state_sync_lock(user_id, rating_key, aspect, holder, until_at)
		VALUES(?,?,?,?,?) ON CONFLICT(user_id, rating_key, aspect) DO NOTHING`,
		userID, int64(key), string(aspect), holder, until)
	if err != nil {
		return false, err
	}
	n, err = res.RowsAffected()
	return n == 1, err
}

// UnlockSync releases a lock this holder owns.
func (s *State) UnlockSync(ctx context.Context, userID int64, key core.RatingKey, aspect SyncAspect, holder string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE state_sync_lock SET holder='', until_at=0
		WHERE user_id=? AND rating_key=? AND aspect=? AND holder=?`,
		userID, int64(key), string(aspect), holder)
	return err
}

// PendingSyncCount is undelivered, not superseded, not dead.
func (s *State) PendingSyncCount(ctx context.Context, userID int64) (pending, dead int, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT
		COALESCE(SUM(CASE WHEN dead=0 AND superseded=0 AND delivered_at=0 THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN dead=1 THEN 1 ELSE 0 END),0)
		FROM state_sync_journal WHERE user_id=?`, userID).Scan(&pending, &dead)
	return pending, dead, err
}

// ApplyRemote writes a remote watched flag and/or rating without journaling
// an upload. Each aspect is compared with its own clock: a newer local
// rating does not block an older watch mark, and a newer watch mark does
// not block an older rating. ErrLocalNewer means every requested aspect was
// older than the local clock.
func (s *State) ApplyRemote(ctx context.Context, userID int64, key core.RatingKey, watched *bool, watchAt time.Time, rating *float64, ratingAt time.Time) error {
	if userID <= 0 || !key.Valid() || (watched == nil && rating == nil) {
		return errors.New("state: invalid remote watch update")
	}
	if rating != nil && (*rating < 0 || *rating > 10) {
		return errors.New("state: remote rating out of range")
	}
	if err := s.lockWrites(ctx); err != nil {
		return err
	}
	defer s.unlockWrites()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	cur, err := loadTx(ctx, tx, userID, key)
	if err != nil {
		return err
	}
	localWatch, localRating := observedClocks(cur)
	applyWatch := watched != nil && !remoteOlder(localWatch, watchAt)
	applyRating := rating != nil && !remoteOlder(localRating, ratingAt)
	if !applyWatch && !applyRating {
		return ErrLocalNewer
	}
	now := s.now().UTC()
	var watchEv *bool
	var rateEv *float64
	if applyWatch {
		watchEv = watched
		if *watched {
			if cur.ws.ViewCount == 0 {
				cur.ws.ViewCount = 1
			}
			cur.ws.ViewOffset = 0
			cur.counted = true
			if !watchAt.IsZero() {
				cur.ws.LastViewedAt = watchAt.UTC()
			}
		} else {
			cur.ws.ViewCount = 0
			cur.ws.ViewOffset = 0
			cur.counted = false
		}
		if watchAt.IsZero() {
			cur.watchAt = now
		} else {
			cur.watchAt = watchAt.UTC()
		}
	}
	if applyRating {
		rateEv = rating
		cur.ws.UserRating = *rating
		if ratingAt.IsZero() {
			cur.ratingAt = now
		} else {
			cur.ratingAt = ratingAt.UTC()
		}
	}
	cur.ws.UpdatedAt = now
	if err := saveTx(ctx, tx, userID, cur); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if watchEv != nil {
		for _, ev := range remoteEvents(userID, key, watchEv, nil, watchAt, now) {
			s.emit(ev)
		}
	}
	if rateEv != nil {
		for _, ev := range remoteEvents(userID, key, nil, rateEv, ratingAt, now) {
			s.emit(ev)
		}
	}
	return nil
}

// observedClocks is the time a remote import has to beat. A current writer
// stores watch_at or rating_at at the same moment as updated_at. A previous
// binary only updates last_viewed_at, updated_at, and version, leaving the
// new columns at the migration backfill, so those legacy columns are the
// local time when they are newer.
func observedClocks(r row) (watch, rating time.Time) {
	watch = r.watchAt
	if r.ws.LastViewedAt.After(watch) {
		watch = r.ws.LastViewedAt
	}
	rating = r.ratingAt
	if r.ws.UpdatedAt.After(r.watchAt) && r.ws.UpdatedAt.After(rating) {
		rating = r.ws.UpdatedAt
	}
	return watch, rating
}

// remoteOlder reports that remote must not replace local. A known local
// clock wins ties and wins against a remote record with no time.
func remoteOlder(local, remote time.Time) bool {
	if remote.IsZero() {
		return !local.IsZero()
	}
	if local.IsZero() {
		return false
	}
	return !local.Before(remote)
}

func remoteEvents(userID int64, key core.RatingKey, watched *bool, rating *float64, at, now time.Time) []Event {
	when := at.UTC()
	if when.IsZero() {
		when = now
	}
	var evs []Event
	if watched != nil {
		kind := EventUnscrobble
		if *watched {
			kind = EventScrobble
		}
		evs = append(evs, Event{
			Kind: kind, UserID: userID, Key: key, Completed: *watched, At: when,
			Imported: true, Account: true,
		})
	}
	if rating != nil {
		evs = append(evs, Event{
			Kind: EventRated, UserID: userID, Key: key, Rating: *rating, At: when,
			Imported: true, Account: true,
		})
	}
	return evs
}

const (
	journalTerminalAge = 14 * 24 * time.Hour
	journalPerUserCap  = 500
	journalGlobalCap   = 20000
)

// CompactSync deletes terminal journal rows and idle locks. A live intent,
// the newest undelivered row for a viewer and title, is kept.
func (s *State) CompactSync(ctx context.Context, now time.Time) error {
	cutoff := now.Add(-journalTerminalAge).UnixNano()
	if _, err := s.db.ExecContext(ctx, `DELETE FROM state_sync_journal
		WHERE (delivered_at<>0 OR superseded<>0 OR dead<>0) AND occurred_at<?`, cutoff); err != nil {
		return err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT user_id FROM state_sync_journal
		WHERE delivered_at<>0 OR superseded<>0 OR dead<>0
		GROUP BY user_id HAVING COUNT(*)>?`, journalPerUserCap)
	if err != nil {
		return err
	}
	var users []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		users = append(users, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, id := range users {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM state_sync_journal
			WHERE user_id=? AND (delivered_at<>0 OR superseded<>0 OR dead<>0)
			AND seq NOT IN (
				SELECT seq FROM state_sync_journal
				WHERE user_id=? AND (delivered_at<>0 OR superseded<>0 OR dead<>0)
				ORDER BY seq DESC LIMIT ?)`, id, id, journalPerUserCap); err != nil {
			return err
		}
	}
	var terminal int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM state_sync_journal
		WHERE delivered_at<>0 OR superseded<>0 OR dead<>0`).Scan(&terminal); err != nil {
		return err
	}
	if terminal > journalGlobalCap {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM state_sync_journal WHERE seq IN (
			SELECT seq FROM state_sync_journal
			WHERE delivered_at<>0 OR superseded<>0 OR dead<>0
			ORDER BY seq LIMIT ?)`, terminal-journalGlobalCap); err != nil {
			return err
		}
	}
	_, err = s.db.ExecContext(ctx, `DELETE FROM state_sync_lock WHERE holder='' OR (until_at>0 AND until_at<?)`, now.UnixNano())
	return err
}

func trimErr(s string) string {
	if len(s) > 200 {
		return s[:200]
	}
	return s
}
