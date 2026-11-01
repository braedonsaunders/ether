package watchlist

import (
	"context"
	"fmt"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

// New episodes of watchlisted shows. watchlist_episodes records, per viewer
// and show, the episodes already handled (found or present when the show
// became available), so each newer episode is processed once.
// watchlist_episodes_alerts holds each episode's owner alert once for every
// viewer, node and restart: it is created in the same transaction that
// records the episode and stays pending (sent=0) until the alert is queued,
// so a failure or crash in between is retried rather than lost. Its own
// migration component keeps the numbering independent of the watchlist
// component's.
func init() {
	store.Register("watchlist_episodes", 1, `
CREATE TABLE watchlist_episodes(user_id INTEGER NOT NULL,item_key INTEGER NOT NULL,episode_key INTEGER NOT NULL,at INTEGER NOT NULL,PRIMARY KEY(user_id,item_key,episode_key));
CREATE TABLE watchlist_episodes_alerts(episode_key INTEGER PRIMARY KEY,label TEXT NOT NULL,at INTEGER NOT NULL,sent INTEGER NOT NULL DEFAULT 0,attempts INTEGER NOT NULL DEFAULT 0);
`)
}

const (
	// freshAiring is how long after an episode airs its release is looked
	// for more often, at freshAiringCheck instead of the check interval.
	freshAiring      = 48 * time.Hour
	freshAiringCheck = time.Hour
	// alertLease is how long a pass that took a pending alert may take to
	// queue it before another pass retries it.
	alertLease = 2 * time.Minute
	// alertAttempts bounds how often one alert is retried. With a plain
	// Notifier a retry after "queued but not marked sent" repeats the alert,
	// so this also bounds those repeats.
	alertAttempts = 5
)

// OnceNotifier queues an alert at most once per key, however often it is
// called (notify.Service implements it). With it, an episode alert is queued
// exactly once even when a crash or error interrupts marking it sent. A plain
// Notifier is still supported but then at least once: such a retry repeats
// the alert, at most alertAttempts more times.
type OnceNotifier interface {
	TitleAvailableOnce(ctx context.Context, key, title string) error
}

// episodeRange returns the first and the newest released regular episodes
// of a show or season that u may see. Specials (season 0) air out of order
// and are often missing from indexers, so they are never "newest" and are
// "first" only for a show with nothing else released.
func (s *Service) episodeRange(ctx context.Context, u *core.User, t *core.Title) (first, newest *core.Title, err error) {
	var episodes []core.Title
	if t.Type == core.TypeSeason {
		episodes, err = s.o.Catalog.Children(ctx, t.Key)
	} else {
		episodes, err = s.o.Catalog.Grandchildren(ctx, t.Key)
	}
	if err != nil {
		return nil, nil, err
	}
	now := s.o.Now()
	var special *core.Title
	for i := range episodes {
		e := &episodes[i]
		if e.ContentRating == "" {
			e.ContentRating = t.ContentRating
		}
		if t.Adult {
			e.Adult = true
		}
		if !e.IsReleased(now) || !s.allowed(u, e) {
			continue
		}
		if e.Season <= 0 {
			if special == nil || e.Episode < special.Episode {
				special = e
			}
			continue
		}
		if first == nil || e.Season < first.Season || (e.Season == first.Season && e.Episode < first.Episode) {
			first = e
		}
		if e.Episode > 0 && (newest == nil || e.Season > newest.Season || (e.Season == newest.Season && e.Episode > newest.Episode)) {
			newest = e
		}
	}
	if first == nil {
		first = special
	}
	return first, newest, nil
}

func (s *Service) episodeKnown(ctx context.Context, userID int64, show, episode core.RatingKey) (bool, error) {
	var known bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM watchlist_episodes WHERE user_id=? AND item_key=? AND episode_key=?)`, userID, show, episode).Scan(&known)
	return known, err
}

// recordEpisode marks an episode handled for a viewer.
func (s *Service) recordEpisode(ctx context.Context, userID int64, show, episode core.RatingKey) error {
	_, err := s.claimEpisode(ctx, userID, show, episode)
	return err
}

// claimEpisode records an episode for a viewer and reports whether this call
// recorded it (false: already recorded, here or on another node).
func (s *Service) claimEpisode(ctx context.Context, userID int64, show, episode core.RatingKey) (bool, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO watchlist_episodes(user_id,item_key,episode_key,at) VALUES(?,?,?,?) ON CONFLICT(user_id,item_key,episode_key) DO NOTHING`, userID, show, episode, s.o.Now().Unix())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// newEpisode looks for a release of an available show's newest episode.
// Once one is found, one transaction records the episode for the viewer and
// (with alerts on, the first time any viewer finds it) creates its pending
// alert; the alert is then queued, and the episode prewarmed separately.
// Until a release exists the show is checked again on the next due pass.
func (s *Service) newEpisode(ctx context.Context, cfg Settings, u *core.User, show, episode *core.Title) error {
	res, e := s.resolve(ctx, u, episode)
	if _, err := s.db.ExecContext(ctx, `UPDATE watchlist_items SET checked=? WHERE user_id=? AND item_key=?`, s.o.Now().Unix(), u.ID, show.Key); err != nil {
		return err
	}
	if e != nil || res == nil {
		return nil
	}
	now := s.o.Now().Unix()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rec, err := tx.ExecContext(ctx, `INSERT INTO watchlist_episodes(user_id,item_key,episode_key,at) VALUES(?,?,?,?) ON CONFLICT(user_id,item_key,episode_key) DO NOTHING`, u.ID, show.Key, episode.Key, now)
	if err != nil {
		return err
	}
	if n, err := rec.RowsAffected(); err != nil || n == 0 {
		return err // recorded already, here or on another node
	}
	alert := false
	if cfg.Notify && s.o.Notifier != nil {
		res, err := tx.ExecContext(ctx, `INSERT INTO watchlist_episodes_alerts(episode_key,label,at) VALUES(?,?,?) ON CONFLICT(episode_key) DO NOTHING`, episode.Key, episodeLabel(show, episode), now)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		alert = n == 1
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if s.o.OnAvailable != nil {
		s.o.OnAvailable(ctx, u.ID, episode.Key)
	}
	if alert {
		if err := s.sendAlert(ctx, episode.Key, episodeLabel(show, episode), now); err != nil {
			return err
		}
	}
	if cfg.Prewarm && s.o.Engine != nil {
		if e := s.prewarm(ctx, res); e != nil {
			s.o.Logger.Debug("watchlist: prewarm new episode", "episode", episode.Key, "err", e)
		}
	}
	return nil
}

// sendAlert queues a pending episode alert whose lease (its at) the caller
// holds, and marks it sent. A failed alert stays pending for retryAlerts.
func (s *Service) sendAlert(ctx context.Context, episode core.RatingKey, label string, at int64) error {
	var e error
	if once, ok := s.o.Notifier.(OnceNotifier); ok {
		e = once.TitleAvailableOnce(ctx, "watchlist.episode:"+episode.String(), label)
	} else {
		e = s.o.Notifier.TitleAvailable(ctx, label)
	}
	if e != nil {
		s.o.Logger.Warn("watchlist: new episode alert failed; retrying later", "episode", episode, "err", e)
		return nil
	}
	_, err := s.db.ExecContext(ctx, `UPDATE watchlist_episodes_alerts SET sent=1 WHERE episode_key=? AND at=?`, episode, at)
	return err
}

// retryAlerts queues episode alerts left pending by a failed or interrupted
// pass, once their lease has expired, up to alertAttempts times. Taking the
// lease is a conditional update, so two passes never retry an alert at the
// same time; whether a retry can repeat an alert that was queued but not
// marked sent depends on the notifier (see OnceNotifier).
func (s *Service) retryAlerts(ctx context.Context) error {
	if s.o.Notifier == nil {
		return nil
	}
	now := s.o.Now().Unix()
	rows, err := s.db.QueryContext(ctx, `SELECT episode_key,label,at FROM watchlist_episodes_alerts WHERE sent=0 AND attempts<? AND at<=? ORDER BY at LIMIT 20`, alertAttempts, now-int64(alertLease/time.Second))
	if err != nil {
		return err
	}
	type pending struct {
		key   core.RatingKey
		label string
		at    int64
	}
	var ps []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.key, &p.label, &p.at); err != nil {
			rows.Close()
			return err
		}
		ps = append(ps, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, p := range ps {
		lease := max(now, p.at+1)
		res, err := s.db.ExecContext(ctx, `UPDATE watchlist_episodes_alerts SET at=?,attempts=attempts+1 WHERE episode_key=? AND sent=0 AND at=?`, lease, p.key, p.at)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n == 1 {
			if err := s.sendAlert(ctx, p.key, p.label, lease); err != nil {
				return err
			}
		}
	}
	return nil
}

// episodeLabel names an episode in an alert: "Show S02E05 · Title".
func episodeLabel(show, episode *core.Title) string {
	name := show.Title
	if episode.ShowTitle != "" {
		name = episode.ShowTitle
	}
	label := fmt.Sprintf("%s S%02dE%02d", name, episode.Season, episode.Episode)
	if episode.Title != "" {
		label += " · " + episode.Title
	}
	return label
}
