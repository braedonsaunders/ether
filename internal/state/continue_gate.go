package state

import (
	"context"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
	"time"
)

func init() {
	store.Register("state_history_playback_gate", 1, `CREATE INDEX state_continue_playback_gate ON state_history(user_id,view_type,completed,rating_key);`)
}

// PlaybackCompletedShows checks the complete history, independent of current
// watched flags and shelf recency. Each show is an indexed EXISTS range seek.
func (s *State) PlaybackCompletedShows(ctx context.Context, uid int64, shows []core.RatingKey) (map[core.RatingKey]bool, error) {
	out := make(map[core.RatingKey]bool, len(shows))
	for _, show := range shows {
		lo := core.EpisodeKey(show.TMDB(), 0, 0)
		hi := core.EpisodeKey(show.TMDB()+1, 0, 0)
		var yes bool
		if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM state_history WHERE user_id=? AND view_type=0 AND completed=1 AND rating_key>=? AND rating_key<?)`, uid, int64(lo), int64(hi)).Scan(&yes); err != nil {
			return nil, err
		}
		out[show] = yes
	}
	return out, nil
}

func init() {
	store.Register("state_watch_recent_settings", 1, `CREATE INDEX state_watch_recent_settings ON state_watch(user_id,updated_at DESC,rating_key) WHERE last_viewed_at>0;`)
}

// RecentlyViewedSettings reads a bounded viewer prefix in settings-write order.
func (s *State) RecentlyViewedSettings(ctx context.Context, uid int64, _ time.Time, limit int) ([]core.HistoryEntry, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT rating_key,last_viewed_at FROM state_watch WHERE user_id=? AND last_viewed_at>0 ORDER BY updated_at DESC,rating_key ASC LIMIT ?`, uid, min(max(limit, 1), 1000))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []core.HistoryEntry
	for rows.Next() {
		var h core.HistoryEntry
		var at int64
		if err := rows.Scan(&h.Key, &at); err != nil {
			return nil, err
		}
		h.ViewedAt = time.Unix(0, at)
		out = append(out, h)
	}
	return out, rows.Err()
}
