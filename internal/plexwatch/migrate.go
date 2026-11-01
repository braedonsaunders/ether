package plexwatch

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/plexguid"
	"github.com/braedonsaunders/ether/internal/state"
)

// errUnmappedFull means this import cannot remember another unmatched title
// without dropping one. The cursor stays put so the page can be read again.
var errUnmappedFull = errors.New("plexwatch: unmatched title list is full")

// errUnmappedStored means the title was kept for a later retry. The import
// cursor may advance. A retry must not delete the row.
var errUnmappedStored = errors.New("plexwatch: unmatched title was kept")

// ExportCurrent enqueues the viewer's current watched titles and whole-star
// ratings. It does not run unless the viewer asks, and it does not upload
// in-progress offsets. Delivery still waits until sync is enabled.
func (s *Service) ExportCurrent(ctx context.Context, userID, plexID int64) (Status, error) {
	if _, err := s.requireMatch(ctx, userID, plexID); err != nil {
		return Status{}, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT rating_key, view_count, user_rating, last_viewed_at, watch_at, rating_at FROM state_watch WHERE user_id=?`, userID)
	if err != nil {
		return Status{}, err
	}
	defer rows.Close()
	now := s.now()
	var intents []state.SyncRow
	for rows.Next() {
		var key, viewed, watchAt, ratingAt int64
		var count int
		var rating float64
		if err := rows.Scan(&key, &count, &rating, &viewed, &watchAt, &ratingAt); err != nil {
			return Status{}, err
		}
		rk := core.RatingKey(key)
		if rk.Kind() != core.KindMovie && rk.Kind() != core.KindEpisode {
			continue
		}
		if count > 0 {
			at := now
			switch {
			case watchAt > 0:
				at = time.Unix(0, watchAt)
			case viewed > 0:
				at = time.Unix(0, viewed)
			}
			intents = append(intents, state.SyncRow{UserID: userID, Key: rk, Aspect: state.SyncWatch, Kind: state.SyncComplete, At: at})
		}
		if _, ok := starRating(rating); ok {
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
	if err := s.state.NoteSyncIntents(ctx, intents); err != nil {
		return Status{}, err
	}
	return s.Status(ctx, userID, plexID)
}

// ImportCurrent reads the account's current watch state and applies records
// that are newer than the local row. Episode GUIDs apply only when that exact
// GUID is already stored. Show GUIDs are not expanded into episodes.
func (s *Service) ImportCurrent(ctx context.Context, userID, plexID int64) (Status, error) {
	acct, err := s.requireMatch(ctx, userID, plexID)
	if err != nil {
		return Status{}, err
	}
	if acct.eligibility != "allowed" {
		return Status{}, ErrNotEligible
	}
	cursor := acct.cursor
	for page := 0; page < 20; page++ {
		remote, err := s.client.fetchState(ctx, acct.token, acct.plexID, cursor)
		if err != nil {
			return Status{}, err
		}
		for _, item := range remote.Items {
			if err := s.applyRemoteItem(ctx, userID, item); err != nil && !errors.Is(err, errUnmappedStored) {
				return Status{}, err
			}
		}
		if remote.EndCursor != "" {
			cursor = remote.EndCursor
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE plexwatch_account SET import_cursor=?, updated_at=? WHERE user_id=? AND plex_id=?`,
			cursor, s.now().UnixNano(), userID, plexID); err != nil {
			return Status{}, err
		}
		if !remote.HasNextPage {
			break
		}
	}
	_ = s.reconcileUser(ctx, userID, false)
	return s.Status(ctx, userID, plexID)
}

func (s *Service) applyRemoteItem(ctx context.Context, userID int64, item remoteItem) error {
	if strings.HasPrefix(item.GUID, "plex://show/") || strings.HasPrefix(item.GUID, "plex://season/") {
		return nil
	}
	if !strings.HasPrefix(item.GUID, "plex://movie/") && !strings.HasPrefix(item.GUID, "plex://episode/") {
		return nil
	}
	if s.guid == nil {
		return nil
	}
	key, err := s.guid.ExactKey(ctx, item.GUID)
	if errors.Is(err, core.ErrNotFound) || errors.Is(err, plexguid.ErrAmbiguous) {
		reason := "not_found"
		if errors.Is(err, plexguid.ErrAmbiguous) {
			reason = "ambiguous"
		}
		return s.storeUnmapped(ctx, userID, item, reason)
	}
	if err != nil {
		return err
	}
	if !guidKindMatches(item.GUID, key) {
		return s.storeUnmapped(ctx, userID, item, "kind")
	}
	var watched *bool
	var watchAt time.Time
	kind := ""
	if item.ViewState == state.SyncComplete || item.ViewState == state.SyncUnplayed {
		on := item.ViewState == state.SyncComplete
		watched = &on
		kind = item.ViewState
		if item.ViewedAt > 0 {
			watchAt = time.UnixMilli(item.ViewedAt)
		}
	}
	var rating *float64
	var ratingAt time.Time
	if item.Rating != nil {
		v := float64(*item.Rating)
		if _, ok := starRating(v); ok {
			rating = &v
			if item.RatedAt > 0 {
				ratingAt = time.UnixMilli(item.RatedAt)
			}
		}
	}
	if watched == nil && rating == nil {
		return nil
	}
	if kind != "" {
		var n int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM plexwatch_sent WHERE user_id=? AND guid=? AND kind=? AND at_ms=?`,
			userID, item.GUID, kind, item.ViewedAt).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			watched = nil
			kind = ""
		}
	}
	if watched == nil && rating == nil {
		return nil
	}
	err = s.state.ApplyRemote(ctx, userID, key, watched, watchAt, rating, ratingAt)
	if errors.Is(err, state.ErrLocalNewer) {
		return nil
	}
	return err
}

func guidKindMatches(guid string, key core.RatingKey) bool {
	switch {
	case strings.HasPrefix(guid, "plex://movie/"):
		return key.Kind() == core.KindMovie
	case strings.HasPrefix(guid, "plex://episode/"):
		return key.Kind() == core.KindEpisode
	default:
		return false
	}
}

func (s *Service) storeUnmapped(ctx context.Context, userID int64, item remoteItem, reason string) error {
	if err := s.rememberUnmapped(ctx, userID, item, reason); err != nil {
		return err
	}
	return errUnmappedStored
}

func (s *Service) rememberUnmapped(ctx context.Context, userID int64, item remoteItem, reason string) error {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM plexwatch_unmapped WHERE user_id=? AND guid<>?`, userID, item.GUID).Scan(&n); err != nil {
		return err
	}
	if n >= unmappedPerUserCap {
		return errUnmappedFull
	}
	rating := 0
	if item.Rating != nil {
		rating = *item.Rating
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO plexwatch_unmapped(user_id, guid, view_state, viewed_at, rating, rated_at, reason, seen_at)
		VALUES(?,?,?,?,?,?,?,?)
		ON CONFLICT(user_id, guid) DO UPDATE SET view_state=excluded.view_state, viewed_at=excluded.viewed_at,
		  rating=excluded.rating, rated_at=excluded.rated_at, reason=excluded.reason, seen_at=excluded.seen_at`,
		userID, item.GUID, item.ViewState, item.ViewedAt, rating, item.RatedAt, reason, s.now().UnixNano())
	return err
}

func (s *Service) retryUnmapped(ctx context.Context, userID int64) error {
	rows, err := s.db.QueryContext(ctx, `SELECT guid, view_state, viewed_at, rating, rated_at FROM plexwatch_unmapped WHERE user_id=?`, userID)
	if err != nil {
		return err
	}
	defer rows.Close()
	type saved struct {
		guid, view string
		viewed     int64
		rating     int
		rated      int64
	}
	var batch []saved
	for rows.Next() {
		var item saved
		if err := rows.Scan(&item.guid, &item.view, &item.viewed, &item.rating, &item.rated); err != nil {
			return err
		}
		batch = append(batch, item)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, item := range batch {
		if s.guid == nil {
			return nil
		}
		_, err := s.guid.ExactKey(ctx, item.guid)
		if errors.Is(err, core.ErrNotFound) || errors.Is(err, plexguid.ErrAmbiguous) {
			continue
		}
		if err != nil {
			return err
		}
		remote := remoteItem{GUID: item.guid, ViewState: item.view, ViewedAt: item.viewed, RatedAt: item.rated}
		if item.rating != 0 {
			n := item.rating
			remote.Rating = &n
		}
		if err := s.applyRemoteItem(ctx, userID, remote); err != nil {
			if errors.Is(err, errUnmappedStored) {
				continue
			}
			return err
		}
		if _, err := s.db.ExecContext(ctx, `DELETE FROM plexwatch_unmapped WHERE user_id=? AND guid=?`, userID, item.guid); err != nil {
			return err
		}
	}
	return nil
}
