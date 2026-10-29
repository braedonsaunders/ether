package identity

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
	"strings"
	"time"
)

func init() {
	store.RegisterAfter("identity_invites", "auth")
	store.Register("identity_invites", 1, `CREATE TABLE identity_invites (hash TEXT PRIMARY KEY, viewer_id INTEGER NOT NULL REFERENCES auth_users(id) ON DELETE CASCADE, expires INTEGER NOT NULL, used INTEGER NOT NULL DEFAULT 0);`)
}

// Invite issues one-use links to an existing active viewer.
type Invite struct {
	db            *sql.DB
	now           func() time.Time
	ttl, grantTTL time.Duration
}

func NewInvite(db *sql.DB, ttl, grantTTL time.Duration, now func() time.Time) (*Invite, error) {
	if db == nil || ttl < time.Second || grantTTL < time.Second {
		return nil, errors.New("identity: invalid invite configuration")
	}
	if now == nil {
		now = time.Now
	}
	return &Invite{db, now, ttl, grantTTL}, nil
}
func (i *Invite) Create(ctx context.Context, viewerID int64) (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	secret := base64.RawURLEncoding.EncodeToString(raw[:])
	hash, _ := secretHash(secret)
	res, err := i.db.ExecContext(ctx, `INSERT INTO identity_invites(hash,viewer_id,expires) SELECT ?,?,? WHERE EXISTS(SELECT 1 FROM auth_users WHERE id=? AND active=1 AND is_owner=0)`, hash, viewerID, i.now().Add(i.ttl).UnixMilli(), viewerID)
	if err != nil {
		return "", err
	}
	n, err := res.RowsAffected()
	if err != nil || n != 1 {
		return "", core.ErrUnauthorized
	}
	return secret, nil
}
func (i *Invite) Redeem(ctx context.Context, secret string) (string, error) {
	hash, ok := secretHash(secret)
	if !ok {
		return "", core.ErrUnauthorized
	}
	tx, err := i.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	now := i.now().UnixMilli()
	var viewer int64
	err = tx.QueryRowContext(ctx, `UPDATE identity_invites SET used=1 WHERE hash=? AND used=0 AND expires>? AND EXISTS(SELECT 1 FROM auth_users WHERE id=identity_invites.viewer_id AND active=1 AND is_owner=0) RETURNING viewer_id`, hash, now).Scan(&viewer)
	if errors.Is(err, sql.ErrNoRows) {
		return "", core.ErrUnauthorized
	}
	if err != nil {
		return "", err
	}
	var id int64
	err = tx.QueryRowContext(ctx, `INSERT INTO identity_links(viewer_id,provider,subject,display,verified_at) VALUES(?,'invite',?,'Invite',?) RETURNING id`, viewer, hash, now).Scan(&id)
	if err != nil {
		return "", err
	}
	var raw [32]byte
	if _, err = rand.Read(raw[:]); err != nil {
		return "", err
	}
	token := grantPrefix + base64.RawURLEncoding.EncodeToString(raw[:])
	gh, _ := grantHash(token)
	_, err = tx.ExecContext(ctx, `INSERT INTO identity_grants(hash,viewer_id,identity_id,surface,device_id,client,product,version,created,last_used,expires) VALUES(?,?,?,'viewer-portal','','Ether browser','','',?,?,?)`, gh, viewer, id, now, now, now+i.grantTTL.Milliseconds())
	if err != nil {
		return "", err
	}
	if err = tx.Commit(); err != nil {
		return "", err
	}
	return token, nil
}

// CreateViewer creates an owner-managed passwordless viewer and its first
// invite atomically, so a failed invite cannot leave an unreachable account.
func (i *Invite) CreateViewer(ctx context.Context, display string) (int64, string, error) {
	if len(display) == 0 || len(display) > 128 || strings.TrimSpace(display) != display {
		return 0, "", errors.New("identity: invalid display")
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return 0, "", err
	}
	secret := base64.RawURLEncoding.EncodeToString(raw[:])
	hash, _ := secretHash(secret)
	tx, err := i.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, "", err
	}
	defer tx.Rollback()
	now := i.now().Unix()
	var id int64
	err = tx.QueryRowContext(ctx, `INSERT INTO auth_users(plex_id,username,title,is_owner,active,origin,created_at,updated_at) VALUES(NULL,'',?,0,1,'local',?,?) RETURNING id`, display, now, now).Scan(&id)
	if err != nil {
		return 0, "", err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO identity_invites(hash,viewer_id,expires) VALUES(?,?,?)`, hash, id, i.now().Add(i.ttl).UnixMilli())
	if err != nil {
		return 0, "", err
	}
	if err = tx.Commit(); err != nil {
		return 0, "", err
	}
	return id, secret, nil
}
