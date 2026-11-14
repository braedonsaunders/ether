package state

import (
	"context"
	"database/sql"
	"errors"

	"github.com/braedonsaunders/ether/internal/core"
)

// ShowPosition returns the furthest watched or in-progress episode of one show.
// The viewer and episode-key range use the watch primary key; no catalogue or
// other viewer's history is read. A show without activity returns zero status.
func (s *State) ShowPosition(ctx context.Context, uid int64, show core.RatingKey) (core.WatchStatus, error) {
	var key core.RatingKey
	err := s.db.QueryRowContext(ctx, `SELECT rating_key FROM state_watch WHERE user_id=? AND rating_key>=? AND rating_key<? AND (view_count>0 OR view_offset_ns>0) ORDER BY rating_key DESC LIMIT 1`, uid, core.EpisodeKey(show.TMDB(), 0, 0), core.EpisodeKey(show.TMDB()+1, 0, 0)).Scan(&key)
	if errors.Is(err, sql.ErrNoRows) {
		return core.WatchStatus{}, nil
	}
	if err != nil {
		return core.WatchStatus{}, err
	}
	return s.Get(ctx, uid, key)
}
