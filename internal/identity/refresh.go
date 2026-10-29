package identity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func init() {
	store.RegisterAfter("identity_refresh", "auth")
	store.Register("identity_refresh", 1, `CREATE TABLE identity_refresh_families (id TEXT PRIMARY KEY, viewer_id INTEGER NOT NULL REFERENCES auth_users(id) ON DELETE CASCADE, surface TEXT NOT NULL, device_id TEXT NOT NULL, current_hash TEXT NOT NULL, access_hash TEXT NOT NULL, absolute_expiry INTEGER NOT NULL, idle_expiry INTEGER NOT NULL, revoked INTEGER NOT NULL DEFAULT 0); CREATE TABLE identity_refresh_tokens (hash TEXT PRIMARY KEY, family_id TEXT NOT NULL REFERENCES identity_refresh_families(id) ON DELETE CASCADE, access_hash TEXT NOT NULL); CREATE INDEX identity_refresh_viewer ON identity_refresh_families(viewer_id);`)
}

const refreshPrefix = "er1_"
const refreshIdle = 30 * 24 * time.Hour
const refreshAbsolute = 90 * 24 * time.Hour

func refreshHash(token string) (string, bool) {
	if len(token) <= len(refreshPrefix) || token[:len(refreshPrefix)] != refreshPrefix {
		return "", false
	}
	raw, e := base64.RawURLEncoding.Strict().DecodeString(token[len(refreshPrefix):])
	if e != nil || len(raw) != 32 || base64.RawURLEncoding.EncodeToString(raw) != token[len(refreshPrefix):] {
		return "", false
	}
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:]), true
}
func randomCredential(prefix string) (string, error) {
	var b [32]byte
	if _, e := rand.Read(b[:]); e != nil {
		return "", e
	}
	return prefix + base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// IssueRefresh binds a refresh family to an existing active access grant.
func (g *Grants) IssueRefresh(ctx context.Context, surface, access string, device core.Device) (string, error) {
	ah, ok := grantHash(access)
	if !ok || surface == "" {
		return "", core.ErrUnauthorized
	}
	if _, e := g.Resolve(ctx, surface, access); e != nil {
		return "", e
	}
	token, e := randomCredential(refreshPrefix)
	if e != nil {
		return "", e
	}
	rh, _ := refreshHash(token)
	family, e := randomCredential("f_")
	if e != nil {
		return "", e
	}
	now := g.now().UnixMilli()
	tx, e := g.db.BeginTx(ctx, nil)
	if e != nil {
		return "", e
	}
	defer tx.Rollback()
	res, e := tx.ExecContext(ctx, `INSERT INTO identity_refresh_families(id,viewer_id,surface,device_id,current_hash,access_hash,absolute_expiry,idle_expiry) SELECT ?,g.viewer_id,g.surface,g.device_id,?,?,?,? FROM identity_grants g JOIN auth_users u ON u.id=g.viewer_id WHERE g.hash=? AND g.surface=? AND g.device_id=? AND g.revoked=0 AND g.expires>? AND u.active=1 AND (u.origin<>'dlna_guest' OR g.surface='dlna')`, family, rh, ah, now+refreshAbsolute.Milliseconds(), now+refreshIdle.Milliseconds(), ah, surface, device.ID, now)
	if e != nil {
		return "", e
	}
	n, e := res.RowsAffected()
	if e != nil {
		return "", e
	}
	if n != 1 {
		return "", core.ErrUnauthorized
	}
	if _, e = tx.ExecContext(ctx, `INSERT INTO identity_refresh_tokens(hash,family_id,access_hash) VALUES(?,?,?)`, rh, family, ah); e != nil {
		return "", e
	}
	if e = tx.Commit(); e != nil {
		return "", e
	}
	return token, nil
}

// Refresh rotates a family atomically. Replaying a consumed token revokes the
// family and its current access grant. Clients must serialize refreshes and
// must not retry an already successful rotation with the previous token.
func (g *Grants) Refresh(ctx context.Context, surface, token string, device core.Device) (string, string, error) {
	old, ok := refreshHash(token)
	if !ok || surface == "" {
		return "", "", core.ErrUnauthorized
	}
	var memberID int64
	e := g.db.QueryRowContext(ctx, `SELECT f.viewer_id FROM identity_refresh_tokens t JOIN identity_refresh_families f ON f.id=t.family_id JOIN auth_users u ON u.id=f.viewer_id WHERE t.hash=? AND f.surface=? AND f.device_id=? AND u.active=1 AND (u.origin<>'dlna_guest' OR f.surface='dlna')`, old, surface, device.ID).Scan(&memberID)
	if errors.Is(e, sql.ErrNoRows) {
		return "", "", core.ErrUnauthorized
	}
	if e != nil {
		return "", "", e
	}
	if _, e = g.viewers.ActiveUser(ctx, memberID); e != nil {
		return "", "", e
	}
	access, e := randomCredential(grantPrefix)
	if e != nil {
		return "", "", e
	}
	ah, _ := grantHash(access)
	fresh, e := randomCredential(refreshPrefix)
	if e != nil {
		return "", "", e
	}
	rh, _ := refreshHash(fresh)
	now := g.now().UnixMilli()
	tx, e := g.db.BeginTx(ctx, nil)
	if e != nil {
		return "", "", e
	}
	defer tx.Rollback()
	var family string
	var viewer int64
	var oldAccess string
	e = tx.QueryRowContext(ctx, `UPDATE identity_refresh_families SET current_hash=?,access_hash=?,idle_expiry=? WHERE current_hash=? AND surface=? AND device_id=? AND revoked=0 AND absolute_expiry>? AND idle_expiry>? AND EXISTS(SELECT 1 FROM auth_users WHERE id=identity_refresh_families.viewer_id AND active=1 AND (origin<>'dlna_guest' OR identity_refresh_families.surface='dlna')) AND EXISTS(SELECT 1 FROM identity_grants WHERE hash=identity_refresh_families.access_hash AND revoked=0) RETURNING id,viewer_id`, rh, ah, now+refreshIdle.Milliseconds(), old, surface, device.ID, now, now).Scan(&family, &viewer)
	if errors.Is(e, sql.ErrNoRows) {
		var currentAccess string
		err := tx.QueryRowContext(ctx, `UPDATE identity_refresh_families SET revoked=1 WHERE id=(SELECT family_id FROM identity_refresh_tokens WHERE hash=?) AND surface=? AND device_id=? AND current_hash<>? AND revoked=0 AND absolute_expiry>? AND idle_expiry>? AND EXISTS(SELECT 1 FROM auth_users WHERE id=identity_refresh_families.viewer_id AND active=1 AND (origin<>'dlna_guest' OR identity_refresh_families.surface='dlna')) RETURNING access_hash`, old, surface, device.ID, old, now, now).Scan(&currentAccess)
		if errors.Is(err, sql.ErrNoRows) {
			return "", "", core.ErrUnauthorized
		}
		if err != nil {
			return "", "", err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE identity_grants SET revoked=1 WHERE hash=?`, currentAccess); err != nil {
			return "", "", err
		}
		if err = tx.Commit(); err != nil {
			return "", "", err
		}
		return "", "", core.ErrUnauthorized
	}
	if e != nil {
		return "", "", e
	}
	if e = tx.QueryRowContext(ctx, `SELECT access_hash FROM identity_refresh_tokens WHERE hash=? AND family_id=?`, old, family).Scan(&oldAccess); e != nil {
		return "", "", e
	}
	if _, e = tx.ExecContext(ctx, `UPDATE identity_grants SET revoked=1 WHERE hash=?`, oldAccess); e != nil {
		return "", "", e
	}
	res, e := tx.ExecContext(ctx, `INSERT INTO identity_grants(hash,viewer_id,identity_id,surface,device_id,client,product,version,created,last_used,expires) SELECT ?,g.viewer_id,g.identity_id,g.surface,g.device_id,g.client,g.product,g.version,?,?,? FROM identity_grants g JOIN auth_users u ON u.id=g.viewer_id WHERE g.hash=? AND g.viewer_id=? AND u.active=1 AND (u.origin<>'dlna_guest' OR g.surface='dlna')`, ah, now, now, now+g.ttl.Milliseconds(), oldAccess, viewer)
	if e != nil {
		return "", "", e
	}
	n, e := res.RowsAffected()
	if e != nil {
		return "", "", e
	}
	if n != 1 {
		return "", "", core.ErrUnauthorized
	}
	if _, e = tx.ExecContext(ctx, `INSERT INTO identity_refresh_tokens(hash,family_id,access_hash) VALUES(?,?,?)`, rh, family, ah); e != nil {
		return "", "", e
	}
	if e = tx.Commit(); e != nil {
		return "", "", e
	}
	return access, fresh, nil
}
