package watchlist

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func init() {
	// Retention asks whether anyone still watchlists a show or season,
	// across viewers; the primary key starts with user_id.
	store.Register("watchlist", 3, `CREATE INDEX watchlist_items_item_key ON watchlist_items(item_key);`)
}

// alertRetention is how long a sent or exhausted episode alert is kept
// before it may be pruned: well past any lease or in-flight retry.
const alertRetention = 24 * time.Hour

// onceForgetter drops the dedupe keys TitleAvailableOnce recorded
// (notify.Service implements it).
type onceForgetter interface {
	ForgetTitleAvailableOnce(ctx context.Context, keys []string) (int64, error)
}

// forgetEpisodes drops a viewer's handled episodes of a show once no
// watchlist source holds the show. They are read only for a show on the
// watchlist that is already available, and a re-added show starts
// unavailable and records its newest episode without an alert, so nothing
// recorded before the removal is needed again.
func forgetEpisodes(ctx context.Context, tx *sql.Tx, userID int64, key core.RatingKey) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM watchlist_warm_intents WHERE user_id=? AND item_key=? AND NOT EXISTS(SELECT 1 FROM watchlist_items WHERE user_id=? AND item_key=?)`, userID, key, userID, key); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM watchlist_episodes WHERE user_id=? AND item_key=? AND NOT EXISTS(SELECT 1 FROM watchlist_items WHERE user_id=? AND item_key=?)`, userID, key, userID, key)
	return err
}

// Episode keys encode their show and season (core.EpisodeKey): payload =
// show*10^7 + season*10^4 + episode. deadAlerts derives both parents in SQL.
var (
	episodeKeyMin = int64(core.EpisodeKey(0, 0, 0))
	episodeKeyEnd = int64(core.KindEpisode+1) * 1_000_000_000_000_000 // the next kind
	deadAlerts    = func() string {
		p := fmt.Sprintf("(a.episode_key-%d)", episodeKeyMin)
		show := fmt.Sprintf("%d+%s/10000000", int64(core.ShowKey(0)), p)
		season := fmt.Sprintf("%d+(%s/10000000)*10000+(%s/10000)%%1000", int64(core.SeasonKey(0, 0)), p, p)
		return fmt.Sprintf(`SELECT a.episode_key FROM watchlist_episodes_alerts a WHERE a.episode_key>? AND a.episode_key<%d AND (a.sent=1 OR a.attempts>=%d) AND a.at<? AND NOT EXISTS(SELECT 1 FROM watchlist_items i WHERE i.item_key IN (%s,%s)) ORDER BY a.episode_key LIMIT ?`, episodeKeyEnd, alertAttempts, show, season)
	}()
)

// Prune removes watchlist state that can never be used again and reports
// the rows removed:
//
//   - handled episodes of shows no longer on the viewer's watchlist (left by
//     a check that raced a removal, or recorded before removals cleared
//     them);
//   - episode alerts that were sent or gave up, once no viewer watchlists
//     the episode's show or season. Only a watchlisted, already available
//     show alerts, and only for its newest released episode not yet handled;
//     a show added again records its newest episode without an alert, so the
//     episode cannot be announced again. The alert's notify_once key goes
//     with it.
//
// Alerts with keys that do not encode a show are kept.
func (s *Service) Prune(ctx context.Context) (int64, error) {
	if s == nil || s.db == nil {
		return 0, nil
	}
	n, err := store.PruneBatched(ctx, s.db, `DELETE FROM watchlist_episodes WHERE (user_id,item_key,episode_key) IN (SELECT e.user_id,e.item_key,e.episode_key FROM watchlist_episodes e WHERE NOT EXISTS(SELECT 1 FROM watchlist_items i WHERE i.user_id=e.user_id AND i.item_key=e.item_key) LIMIT ?)`)
	if err != nil {
		return n, err
	}
	m, err := s.pruneAlerts(ctx)
	return n + m, err
}

// pruneAlerts walks the alert keys in order, one batch at a time.
func (s *Service) pruneAlerts(ctx context.Context) (int64, error) {
	forget, _ := s.o.Notifier.(onceForgetter)
	cutoff := s.o.Now().Add(-alertRetention).Unix()
	after := episodeKeyMin - 1
	var total int64
	for {
		rows, err := s.db.QueryContext(ctx, deadAlerts, after, cutoff, store.PruneBatch)
		if err != nil {
			return total, err
		}
		var keys []int64
		for rows.Next() {
			var k int64
			if err := rows.Scan(&k); err != nil {
				rows.Close()
				return total, err
			}
			keys = append(keys, k)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return total, err
		}
		if len(keys) == 0 {
			return total, nil
		}
		after = keys[len(keys)-1]
		// The once key first: a crash in between leaves the alert row,
		// which the next prune finds again.
		if forget != nil {
			once := make([]string, len(keys))
			for i, k := range keys {
				once[i] = episodeOnceKey(core.RatingKey(k))
			}
			n, err := forget.ForgetTitleAvailableOnce(ctx, once)
			total += n
			if err != nil {
				return total, err
			}
		}
		args := make([]any, len(keys))
		for i, k := range keys {
			args[i] = k
		}
		res, err := s.db.ExecContext(ctx, fmt.Sprintf(`DELETE FROM watchlist_episodes_alerts WHERE (sent=1 OR attempts>=%d) AND episode_key IN (?%s)`, alertAttempts, strings.Repeat(",?", len(keys)-1)), args...)
		if err != nil {
			return total, err
		}
		n, err := res.RowsAffected()
		total += n
		if err != nil || len(keys) < store.PruneBatch {
			return total, err
		}
		if err := ctx.Err(); err != nil {
			return total, err
		}
	}
}

// episodeOnceKey is an episode alert's TitleAvailableOnce key.
func episodeOnceKey(episode core.RatingKey) string { return "watchlist.episode:" + episode.String() }
