package identity

import (
	"context"
	"errors"
	"time"
)

// CreateViewer atomically creates an owner-managed local viewer.
func (l *Local) CreateViewer(ctx context.Context, username, password string) (int64, error) {
	if !validUsername(username) {
		return 0, errors.New("identity: invalid username")
	}
	hash, err := hashPassword(password)
	if err != nil {
		return 0, err
	}
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	var id int64
	err = tx.QueryRowContext(ctx, `INSERT INTO auth_users(plex_id,username,title,is_owner,active,origin,created_at,updated_at) VALUES(NULL,?,?,0,1,'local',?,?) RETURNING id`, username, username, now, now).Scan(&id)
	if err != nil {
		return 0, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO identity_links(viewer_id,provider,subject,display,verified_at,password_hash) VALUES(?,'local',?,?,?,?)`, id, username, username, now, hash)
	if err != nil {
		return 0, err
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	return id, nil
}
