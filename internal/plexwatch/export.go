package plexwatch

import (
	"context"
	"errors"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/state"
)

// ErrSendingOff means the viewer's direction does not send to Plex, and
// ErrReceivingOff that it does not read from Plex.
var (
	ErrSendingOff   = errors.New("plexwatch: sending to Plex is off for this viewer")
	ErrReceivingOff = errors.New("plexwatch: reading from Plex is off for this viewer")
)

// ExportCurrent queues the viewer's watched movies and episodes, their
// ratings and their resume positions for sending. It runs only when the
// viewer asks; turning sending on does not upload history by itself. The
// scan reads only this viewer's own watch rows.
func (s *Service) ExportCurrent(ctx context.Context, userID, plexID int64) (Status, error) {
	acct, err := s.requireMatch(ctx, userID, plexID)
	if err != nil {
		return Status{}, err
	}
	if acct.direction&1 == 0 {
		return Status{}, ErrSendingOff
	}
	rows, err := s.db.QueryContext(ctx, `SELECT rating_key, view_count, view_offset_ns, duration_ns, user_rating, last_viewed_at, watch_at, rating_at FROM state_watch
		WHERE user_id=? AND (view_count>0 OR user_rating>0 OR view_offset_ns>0)`, userID)
	if err != nil {
		return Status{}, err
	}
	defer rows.Close()
	now := s.now()
	var intents []state.SyncRow
	for rows.Next() {
		var key, offset, duration, viewed, watchAt, ratingAt int64
		var count int
		var rating float64
		if err := rows.Scan(&key, &count, &offset, &duration, &rating, &viewed, &watchAt, &ratingAt); err != nil {
			return Status{}, err
		}
		rk := core.RatingKey(key)
		if rk.Kind() != core.KindMovie && rk.Kind() != core.KindEpisode {
			continue
		}
		at := now
		switch {
		case watchAt > 0:
			at = time.Unix(0, watchAt)
		case viewed > 0:
			at = time.Unix(0, viewed)
		}
		if count > 0 {
			intents = append(intents, state.SyncRow{UserID: userID, Key: rk, Aspect: state.SyncWatch, Kind: state.SyncComplete, At: at})
		}
		// A position after the watch mark is a rewatch in progress; it is
		// queued after the mark so the mark does not supersede it.
		if offset > 0 && duration > 0 {
			intents = append(intents, state.SyncRow{UserID: userID, Key: rk, Aspect: state.SyncOffset, Kind: state.SyncOffsetKind,
				Offset: time.Duration(offset), Duration: time.Duration(duration), At: at})
		}
		if plexRating(rating) > 0 {
			at := now
			if ratingAt > 0 {
				at = time.Unix(0, ratingAt)
			}
			intents = append(intents, state.SyncRow{UserID: userID, Key: rk, Aspect: state.SyncRating, Kind: state.SyncRatingKind, Rating: rating, At: at})
		}
	}
	if err := rows.Err(); err != nil {
		return Status{}, err
	}
	rows.Close()
	if err := s.state.NoteSyncIntents(ctx, intents); err != nil {
		return Status{}, err
	}
	return s.Status(ctx, userID, plexID)
}
