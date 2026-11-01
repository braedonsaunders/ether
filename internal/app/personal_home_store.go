package app

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/store"
)

func init() {
	store.Register("mediabrowser_personal_home", 1, `
CREATE TABLE mediabrowser_personal_home (
  protocol TEXT NOT NULL,
  viewer_id INTEGER NOT NULL,
  config TEXT NOT NULL,
  body TEXT NOT NULL,
  updated_ms INTEGER NOT NULL,
  PRIMARY KEY (protocol, viewer_id, config)
);`)
}

type personalHomeDisk struct{ app *App }

func (a *App) personalHomeDisk() personalHomeDisk { return personalHomeDisk{app: a} }

func (p personalHomeDisk) LoadPersonalHome(ctx context.Context, protocol string, viewerID int64, config string) ([]byte, error) {
	if p.app == nil || p.app.db == nil {
		return nil, nil
	}
	var body string
	err := p.app.db.QueryRowContext(ctx, `SELECT body FROM mediabrowser_personal_home WHERE protocol=? AND viewer_id=? AND config=?`, protocol, viewerID, config).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return []byte(body), nil
}

func (p personalHomeDisk) SavePersonalHome(ctx context.Context, protocol string, viewerID int64, config string, body []byte) error {
	if p.app == nil || p.app.db == nil {
		return nil
	}
	_, err := p.app.db.ExecContext(ctx, `INSERT INTO mediabrowser_personal_home (protocol, viewer_id, config, body, updated_ms)
VALUES (?,?,?,?,?)
ON CONFLICT(protocol, viewer_id, config) DO UPDATE SET body=excluded.body, updated_ms=excluded.updated_ms`,
		protocol, viewerID, config, string(body), time.Now().UnixMilli())
	return err
}

func (p personalHomeDisk) PersonalHomeViewers(ctx context.Context) ([]core.User, error) {
	if p.app == nil || p.app.users == nil {
		return nil, nil
	}
	return p.app.users.Users(ctx)
}

func (a *App) refreshPersonalHomes(userID int64) {
	if a == nil || a.frontends == nil || a.frontends.registry == nil || userID == 0 {
		return
	}
	for _, id := range []frontend.Protocol{"jellyfin", "emby"} {
		inst, ok := a.frontends.registry.Instance(id)
		if !ok || inst == nil {
			continue
		}
		warm, ok := inst.(interface{ RefreshPersonalHome(int64) })
		if ok {
			warm.RefreshPersonalHome(userID)
		}
	}
}
