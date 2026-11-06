package state

import (
	"context"
	"github.com/braedonsaunders/ether/internal/core"
)

// fillParentViewed recovers a parent's display timestamp when imported leaf
// state did not cascade. Each query is restricted by the viewer/key index.
func (s *State) fillParentViewed(ctx context.Context, user int64, status map[core.RatingKey]core.WatchStatus) error {
	for k, st := range status {
		if !st.LastViewedAt.IsZero() || (k.Kind() != core.KindShow && k.Kind() != core.KindSeason) {
			continue
		}
		lo, hi := core.EpisodeKey(k.TMDB(), 0, 0), core.EpisodeKey(k.TMDB()+1, 0, 0)
		if k.Kind() == core.KindSeason {
			lo = core.EpisodeKey(k.TMDB(), k.Season(), 0)
			hi = core.EpisodeKey(k.TMDB(), k.Season()+1, 0)
		}
		var stamp int64
		if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(last_viewed_at),0) FROM state_watch WHERE user_id=? AND rating_key>=? AND rating_key<?`, user, int64(lo), int64(hi)).Scan(&stamp); err != nil {
			return err
		}
		st.LastViewedAt = unixTime(stamp)
		status[k] = st
	}
	return nil
}
