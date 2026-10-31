package state

import (
	"context"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// ShowActivity is a viewer's most recent watch activity on one show.
type ShowActivity struct {
	UserID     int64
	Show       core.RatingKey
	LastViewed time.Time
}

// FollowedShows lists, per viewer, the shows with an episode watched or in
// progress since the given time, most recently viewed first.
func (s *State) FollowedShows(ctx context.Context, since time.Time) ([]ShowActivity, error) {
	const (
		minEp = int64(core.KindEpisode) * 1_000_000_000_000_000
		maxEp = int64(core.KindEpisode+1)*1_000_000_000_000_000 - 1
	)
	// An episode key is 4e15 + show*1e7 + season*1e4 + episode.
	rows, err := s.db.QueryContext(ctx, `SELECT user_id, (rating_key-4000000000000000)/10000000 AS show_id, MAX(last_viewed_at) AS last_at FROM state_watch
		WHERE rating_key BETWEEN ? AND ? AND (view_count>0 OR view_offset_ns>0) AND last_viewed_at>=?
		GROUP BY user_id, show_id ORDER BY last_at DESC`,
		minEp, maxEp, since.UnixNano())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ShowActivity
	for rows.Next() {
		var a ShowActivity
		var show, last int64
		if err := rows.Scan(&a.UserID, &show, &last); err != nil {
			return nil, err
		}
		if show <= 0 {
			continue
		}
		a.Show = core.ShowKey(show)
		a.LastViewed = time.Unix(0, last).UTC()
		out = append(out, a)
	}
	return out, rows.Err()
}
