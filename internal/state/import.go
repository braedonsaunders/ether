package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// ImportRecord uses native time units; source-specific parsing belongs to the
// importer. RatingsOnly preserves existing progress and completion counts.
type ImportRecord struct {
	UserID           int64
	Key              core.RatingKey
	Views            int
	Offset, Duration time.Duration
	Rating           float64
	ViewedAt         time.Time
	RatingsOnly      bool
}
type ImportBatch struct{ Records, History []ImportRecord }

// ImportTransaction commits a reviewed import with the caller's own bookkeeping
// and list writes. State owns every watch/history write and emits ordinary state
// events only after the complete transaction succeeds. Repeated history is inert.
func (s *State) ImportTransaction(ctx context.Context, prepare func(*sql.Tx) (ImportBatch, error)) error {
	var events []Event
	err := func() error {
		if err := s.lockWrites(ctx); err != nil {
			return err
		}
		defer s.unlockWrites()
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		batch, err := prepare(tx)
		if err != nil {
			return err
		}
		events, err = s.importTx(ctx, tx, batch)
		if err != nil {
			return err
		}
		return tx.Commit()
	}()
	if err == nil {
		s.gen.Add(1)
		for _, ev := range events {
			ev.Imported = true
			s.emit(ev)
		}
	}
	return err
}

func (s *State) importTx(ctx context.Context, tx *sql.Tx, b ImportBatch) ([]Event, error) {
	now := s.now().UnixNano()
	var events []Event
	// Track source timestamps within the batch, not the wall-clock
	// updated_at written by this import. Older rows still add history, but
	// must not replace the progress or rating from a newer imported row.
	latest := map[[2]int64]int64{}
	addHistory := func(r ImportRecord) error {
		at := int64(0)
		if !r.ViewedAt.IsZero() {
			at = r.ViewedAt.UnixNano()
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO state_history(user_id,rating_key,viewed_at,completed,imported) SELECT CAST(? AS BIGINT),CAST(? AS BIGINT),CAST(? AS BIGINT),1,1 WHERE NOT EXISTS(SELECT 1 FROM state_history WHERE user_id=? AND rating_key=? AND viewed_at=?) ON CONFLICT DO NOTHING`, r.UserID, r.Key, at, r.UserID, r.Key, at)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n > 0 {
			events = append(events, Event{Kind: EventScrobble, UserID: r.UserID, Key: r.Key, Completed: true, At: r.ViewedAt})
		}
		return nil
	}
	for _, r := range b.Records {
		if !r.Key.Valid() || r.UserID <= 0 || r.Views < 0 || r.Offset < 0 || r.Duration < 0 || r.Rating < 0 || r.Rating > 10 {
			return nil, errors.New("invalid imported watch state")
		}
		var oldRating float64
		if err := tx.QueryRowContext(ctx, `SELECT user_rating FROM state_watch WHERE user_id=? AND rating_key=?`, r.UserID, r.Key).Scan(&oldRating); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		if r.RatingsOnly {
			if _, err := tx.ExecContext(ctx, `INSERT INTO state_watch(user_id,rating_key,user_rating,updated_at,rating_at) VALUES(?,?,?,?,?) ON CONFLICT(user_id,rating_key) DO UPDATE SET user_rating=excluded.user_rating,updated_at=excluded.updated_at,rating_at=CASE WHEN excluded.rating_at>state_watch.rating_at THEN excluded.rating_at ELSE state_watch.rating_at END,version=state_watch.version+1`, r.UserID, r.Key, r.Rating, now, now); err != nil {
				return nil, err
			}
		} else {
			if r.Views > 0 {
				if err := addHistory(r); err != nil {
					return nil, err
				}
			}
			ident := [2]int64{r.UserID, int64(r.Key)}
			old, seen := latest[ident]
			if !seen {
				if err := tx.QueryRowContext(ctx, `SELECT updated_at FROM state_watch WHERE user_id=? AND rating_key=?`, r.UserID, r.Key).Scan(&old); err != nil && !errors.Is(err, sql.ErrNoRows) {
					return nil, err
				}
				latest[ident] = old
			}
			viewed := int64(0)
			if !r.ViewedAt.IsZero() {
				viewed = r.ViewedAt.UnixNano()
			}
			if old > viewed && old > 0 && viewed > 0 {
				continue
			}
			watchAt := viewed
			if watchAt == 0 {
				watchAt = now
			}
			ratingAt := int64(0)
			if r.Rating > 0 {
				ratingAt = now
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO state_watch(user_id,rating_key,view_offset_ns,duration_ns,view_count,counted,last_viewed_at,user_rating,updated_at,watch_at,rating_at) VALUES(?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(user_id,rating_key) DO UPDATE SET view_offset_ns=excluded.view_offset_ns,duration_ns=excluded.duration_ns,counted=excluded.counted,view_count=CASE WHEN excluded.view_count>state_watch.view_count THEN excluded.view_count ELSE state_watch.view_count END,last_viewed_at=CASE WHEN excluded.last_viewed_at>state_watch.last_viewed_at THEN excluded.last_viewed_at ELSE state_watch.last_viewed_at END,user_rating=CASE WHEN excluded.user_rating>0 THEN excluded.user_rating ELSE state_watch.user_rating END,updated_at=excluded.updated_at,watch_at=CASE WHEN excluded.watch_at>state_watch.watch_at THEN excluded.watch_at ELSE state_watch.watch_at END,rating_at=CASE WHEN excluded.rating_at>state_watch.rating_at THEN excluded.rating_at ELSE state_watch.rating_at END,version=state_watch.version+1`, r.UserID, r.Key, int64(r.Offset), int64(r.Duration), r.Views, b2i(r.Views > 0 && r.Offset == 0), viewed, r.Rating, now, watchAt, ratingAt); err != nil {
				return nil, err
			}
			latest[ident] = max(old, viewed)
			events = append(events, Event{Kind: EventProgress, UserID: r.UserID, Key: r.Key, Offset: r.Offset, Duration: r.Duration, State: "stopped", At: s.now()})
		}
		if r.Rating != oldRating && (r.RatingsOnly || r.Rating > 0) {
			events = append(events, Event{Kind: EventRated, UserID: r.UserID, Key: r.Key, Rating: r.Rating, At: s.now()})
		}
	}
	type histAgg struct {
		userID int64
		key    core.RatingKey
		views  int
		latest int64
	}
	var histOrder [][2]int64
	histByIdent := map[[2]int64]*histAgg{}
	for _, r := range b.History {
		if !r.Key.Valid() || r.UserID <= 0 || r.ViewedAt.IsZero() {
			return nil, errors.New("invalid imported history event")
		}
		if err := addHistory(r); err != nil {
			return nil, err
		}
		ident := [2]int64{r.UserID, int64(r.Key)}
		agg := histByIdent[ident]
		if agg == nil {
			agg = &histAgg{userID: r.UserID, key: r.Key}
			histByIdent[ident] = agg
			histOrder = append(histOrder, ident)
		}
		agg.views++
		if v := r.ViewedAt.UnixNano(); v > agg.latest {
			agg.latest = v
		}
	}
	for _, ident := range histOrder {
		agg := histByIdent[ident]
		if _, err := tx.ExecContext(ctx, `INSERT INTO state_watch(user_id,rating_key,view_offset_ns,duration_ns,view_count,counted,last_viewed_at,user_rating,updated_at) VALUES(?,?,0,0,?,1,?,0,?) ON CONFLICT(user_id,rating_key) DO UPDATE SET counted=CASE WHEN state_watch.view_offset_ns=0 THEN 1 ELSE state_watch.counted END,view_count=CASE WHEN excluded.view_count>state_watch.view_count THEN excluded.view_count ELSE state_watch.view_count END,last_viewed_at=CASE WHEN excluded.last_viewed_at>state_watch.last_viewed_at THEN excluded.last_viewed_at ELSE state_watch.last_viewed_at END,updated_at=excluded.updated_at,version=state_watch.version+1`, agg.userID, agg.key, agg.views, agg.latest, now); err != nil {
			return nil, err
		}
	}
	return events, nil
}

// ImportPlaylistTx preserves imported playlist IDs while enforcing ownership.
func ImportPlaylistTx(ctx context.Context, tx *sql.Tx, id, userID int64, title string, keys []core.RatingKey, now int64) (int64, error) {
	if userID <= 0 || title == "" {
		return 0, errors.New("invalid imported playlist")
	}
	if id == 0 {
		if err := tx.QueryRowContext(ctx, `INSERT INTO state_playlists(user_id,title,updated_at) VALUES(?,?,?) RETURNING id`, userID, title, now).Scan(&id); err != nil {
			return 0, err
		}
	} else {
		res, err := tx.ExecContext(ctx, `UPDATE state_playlists SET title=?,updated_at=? WHERE id=? AND user_id=?`, title, now, id, userID)
		if err != nil {
			return 0, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return 0, err
		}
		if n != 1 {
			return 0, core.ErrNotFound
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM state_playlist_items WHERE playlist_id=?`, id); err != nil {
		return 0, err
	}
	for i, k := range keys {
		if !k.Valid() {
			return 0, fmt.Errorf("invalid playlist item %d", k)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO state_playlist_items(playlist_id,position,rating_key) VALUES(?,?,?)`, id, i, k); err != nil {
			return 0, err
		}
	}
	return id, nil
}

// ExportEntry is the state-owned, source-neutral view used by CSV exports.
type ExportEntry struct {
	Key    core.RatingKey
	Rating float64
	At     time.Time
}

func (s *State) ExportEntries(ctx context.Context, userID int64, ratings bool) ([]ExportEntry, error) {
	query := `SELECT h.rating_key,COALESCE(w.user_rating,0),h.viewed_at FROM state_history h LEFT JOIN state_watch w ON w.user_id=h.user_id AND w.rating_key=h.rating_key WHERE h.user_id=? AND h.completed=1 ORDER BY h.viewed_at`
	if ratings {
		query = `SELECT rating_key,user_rating,last_viewed_at FROM state_watch WHERE user_id=? AND user_rating>0`
	}
	rows, err := s.db.QueryContext(ctx, query, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ExportEntry{}
	for rows.Next() {
		var e ExportEntry
		var at int64
		if err := rows.Scan(&e.Key, &e.Rating, &at); err != nil {
			return nil, err
		}
		if at > 0 {
			e.At = time.Unix(0, at)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
