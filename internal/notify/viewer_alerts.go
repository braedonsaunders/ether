package notify

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func init() {
	store.Register("viewer_alerts", 1, `CREATE TABLE viewer_alert_preferences (viewer_id INTEGER PRIMARY KEY, enabled INTEGER NOT NULL, enabled_at INTEGER NOT NULL);`)
}

// ViewerAlerts keeps viewer notification settings separate from owner channels.
// Active must consult current membership, not a cached viewer supplied by the caller.
type ViewerAlerts struct {
	DB     *sql.DB
	Active func(context.Context, int64) (*core.User, error)
	Now    func() time.Time
}

func (s *ViewerAlerts) authorized(ctx context.Context, viewer *core.User) error {
	if s == nil || s.DB == nil || s.Active == nil || viewer == nil || viewer.ID <= 0 {
		return core.ErrUnauthorized
	}
	active, err := s.Active(ctx, viewer.ID)
	if err != nil || active == nil || active.ID != viewer.ID {
		return core.ErrUnauthorized
	}
	return nil
}

// Enabled defaults to false for every viewer without an explicit preference.
func (s *ViewerAlerts) Enabled(ctx context.Context, viewer *core.User) (bool, error) {
	if err := s.authorized(ctx, viewer); err != nil {
		return false, err
	}
	var enabled int
	err := s.DB.QueryRowContext(ctx, `SELECT enabled FROM viewer_alert_preferences WHERE viewer_id=?`, viewer.ID).Scan(&enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return enabled != 0, err
}

// SetEnabled records the time of each off-to-on transition so detection can
// suppress episodes already aired before a viewer opted in.
func (s *ViewerAlerts) SetEnabled(ctx context.Context, viewer *core.User, enabled bool) error {
	if err := s.authorized(ctx, viewer); err != nil {
		return err
	}
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	value := 0
	if enabled {
		value = 1
	}
	_, err := s.DB.ExecContext(ctx, `INSERT INTO viewer_alert_preferences(viewer_id,enabled,enabled_at) VALUES(?,?,?) ON CONFLICT(viewer_id) DO UPDATE SET enabled=excluded.enabled, enabled_at=CASE WHEN viewer_alert_preferences.enabled=0 AND excluded.enabled=1 THEN CASE WHEN excluded.enabled_at > viewer_alert_preferences.enabled_at THEN excluded.enabled_at ELSE viewer_alert_preferences.enabled_at + 1 END ELSE viewer_alert_preferences.enabled_at END`, viewer.ID, value, now().UnixNano())
	return err
}
