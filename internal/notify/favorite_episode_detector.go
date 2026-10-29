package notify

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func init() {
	store.Register("viewer_alerts", 2, `CREATE TABLE viewer_episode_alerts (viewer_id INTEGER NOT NULL, episode_key TEXT NOT NULL, show_title TEXT NOT NULL, episode_title TEXT NOT NULL, detected_at INTEGER NOT NULL, preference_generation INTEGER NOT NULL, PRIMARY KEY(viewer_id,episode_key)); CREATE TABLE viewer_episode_scan (viewer_id INTEGER PRIMARY KEY, enabled_at INTEGER NOT NULL, scanned_at INTEGER NOT NULL); CREATE TABLE viewer_episode_seen (viewer_id INTEGER NOT NULL, episode_key TEXT NOT NULL, PRIMARY KEY(viewer_id,episode_key));`)
}

// FavoriteEpisodeDetector scans only currently authorized favorites. The caller
// runs it under the cluster leader lease. The database uniqueness constraint
// also protects against overlap during a leader handoff.
type FavoriteEpisodeDetector struct {
	Preferences *ViewerAlerts
	// Episodes must return only visible, currently favorited shows and episodes.
	Episodes func(context.Context, *core.User) ([]core.Title, error)
	Now      func() time.Time
}

func (d *FavoriteEpisodeDetector) Scan(ctx context.Context) error {
	if d == nil || d.Preferences == nil || d.Preferences.DB == nil || d.Preferences.Active == nil || d.Episodes == nil {
		return core.ErrUnauthorized
	}
	now := time.Now()
	if d.Now != nil {
		now = d.Now()
	}
	rows, err := d.Preferences.DB.QueryContext(ctx, `SELECT viewer_id,enabled_at FROM viewer_alert_preferences WHERE enabled=1`)
	if err != nil {
		return err
	}
	type pref struct{ id, enabledAt int64 }
	var prefs []pref
	for rows.Next() {
		var p pref
		if err := rows.Scan(&p.id, &p.enabledAt); err != nil {
			rows.Close()
			return err
		}
		prefs = append(prefs, p)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, p := range prefs {
		viewer, err := d.Preferences.Active(ctx, p.id)
		if err != nil && !errors.Is(err, core.ErrUnauthorized) && !errors.Is(err, core.ErrNotFound) {
			return err
		}
		if err != nil || viewer == nil || viewer.ID != p.id {
			continue
		}
		episodes, err := d.Episodes(ctx, viewer)
		if err != nil {
			return err
		}
		tx, err := d.Preferences.DB.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if err = store.LockKey(ctx, store.DialectOf(d.Preferences.DB), tx, fmt.Sprintf("viewer-episode-scan:%d", p.id)); err != nil {
			tx.Rollback()
			return err
		}
		var previous, generation int64
		err = tx.QueryRowContext(ctx, `SELECT enabled_at,scanned_at FROM viewer_episode_scan WHERE viewer_id=?`, p.id).Scan(&generation, &previous)
		if err != nil && err != sql.ErrNoRows {
			tx.Rollback()
			return err
		}
		// An initial scan, a new opt-in, or a gap over two polling intervals
		// starts a new baseline instead of sending historical episodes.
		baseline := err == sql.ErrNoRows || generation != p.enabledAt || now.Unix()-previous > int64((30*time.Minute).Seconds()) || now.Unix() < previous
		for _, episode := range episodes {
			if episode.Type != core.TypeEpisode || episode.Key == 0 || episode.Released.IsZero() || episode.Released.After(now) {
				continue
			}
			res, err := tx.ExecContext(ctx, `INSERT INTO viewer_episode_seen(viewer_id,episode_key) VALUES(?,?) ON CONFLICT(viewer_id,episode_key) DO NOTHING`, p.id, episode.Key.String())
			if err != nil {
				tx.Rollback()
				return err
			}
			added, err := res.RowsAffected()
			if err != nil {
				tx.Rollback()
				return err
			}
			if baseline || added == 0 || episode.Released.Before(now.Add(-48*time.Hour)) {
				continue
			}
			_, err = tx.ExecContext(ctx, `INSERT INTO viewer_episode_alerts(viewer_id,episode_key,show_title,episode_title,detected_at,preference_generation) SELECT ?,?,?,?,?,? WHERE EXISTS (SELECT 1 FROM viewer_alert_preferences WHERE viewer_id=? AND enabled=1 AND enabled_at=?) ON CONFLICT(viewer_id,episode_key) DO NOTHING`, p.id, episode.Key.String(), episode.ShowTitle, episode.Title, now.Unix(), p.enabledAt, p.id, p.enabledAt)
			if err != nil {
				tx.Rollback()
				return err
			}
		}

		_, err = tx.ExecContext(ctx, `INSERT INTO viewer_episode_scan(viewer_id,enabled_at,scanned_at) VALUES(?,?,?) ON CONFLICT(viewer_id) DO UPDATE SET enabled_at=excluded.enabled_at,scanned_at=excluded.scanned_at`, p.id, p.enabledAt, now.Unix())
		if err != nil {
			tx.Rollback()
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}

	}
	return nil
}
