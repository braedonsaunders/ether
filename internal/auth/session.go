package auth

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

// Owner console sessions. Signing in to the admin console used to hand the
// browser the owner's plex.tv account token, which then sat in localStorage
// where any script on the page could read it and take over the Plex account.
// The console now exchanges that credential for a server-local session: it
// only opens the admin API, is never accepted as a Plex or playback
// credential, and can be revoked. Only hashes persist, and every check reads
// the shared database, so a session issued on one cluster node works on all
// of them and dies everywhere the moment it is revoked or ownership changes.
const (
	ownerSessionPrefix = "ether-admin-"
	// OwnerSessionTTL is how long an unused session stays valid. Use
	// extends it, at most once a day, up to OwnerSessionMaxAge.
	OwnerSessionTTL = 30 * 24 * time.Hour
	// OwnerSessionMaxAge bounds a session regardless of use.
	OwnerSessionMaxAge   = 90 * 24 * time.Hour
	ownerSessionRenewGap = 24 * time.Hour
	maxOwnerSessions     = 32
)

// The table is its own migration component so its numbering can never
// collide with the auth component's, and sorts after it (auth_users exists).
func init() {
	// Explicit, not only lexical: RegisterAfter can pull another component
	// ahead of "auth", and auth_sessions references auth_users.
	store.RegisterAfter("auth_sessions", "auth")
	store.Register("auth_sessions", 1, `
CREATE TABLE auth_sessions (
 token_hash TEXT PRIMARY KEY,
 user_id INTEGER NOT NULL REFERENCES auth_users(id) ON DELETE CASCADE,
 created_at INTEGER NOT NULL,
 expires_at INTEGER NOT NULL
);
CREATE INDEX auth_sessions_user ON auth_sessions(user_id);
`)
}

// IsOwnerSession reports whether token has the shape of an owner console
// session. It says nothing about validity.
func IsOwnerSession(token string) bool {
	return strings.HasPrefix(strings.TrimSpace(token), ownerSessionPrefix)
}

// IssueOwnerSession exchanges an owner credential (a plex.tv token Ether
// accepts as the owner's) for a console session. Sessions and delegated
// credentials cannot be exchanged, so a stolen session cannot extend itself
// past its absolute lifetime.
func (s *Service) IssueOwnerSession(ctx context.Context, credential string) (string, time.Time, error) {
	credential = strings.TrimSpace(credential)
	if IsOwnerSession(credential) || strings.HasPrefix(credential, delegationPrefix) {
		return "", time.Time{}, core.ErrUnauthorized
	}
	u, err := s.Authenticate(ctx, credential)
	if err != nil {
		return "", time.Time{}, err
	}
	if !u.IsOwner || u.Delegated {
		return "", time.Time{}, core.ErrUnauthorized
	}
	now := s.o.Now()
	expires := now.Add(OwnerSessionTTL)
	token := ownerSessionPrefix + rand.Text()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", time.Time{}, err
	}
	defer tx.Rollback()
	// Lock the owner's row first. On PostgreSQL (READ COMMITTED) two
	// sign-ins would otherwise both see room under the cap and both insert;
	// the second now waits here and its cap check sees the first's session.
	// SQLite already serialises write transactions.
	locked, err := tx.ExecContext(ctx, `UPDATE auth_users SET updated_at=updated_at WHERE id=? AND is_owner=1 AND active=1`, u.ID)
	if err != nil {
		return "", time.Time{}, err
	}
	if n, err := locked.RowsAffected(); err != nil {
		return "", time.Time{}, err
	} else if n != 1 {
		return "", time.Time{}, core.ErrUnauthorized
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM auth_sessions WHERE expires_at<=? OR created_at<=?`,
		now.Unix(), now.Add(-OwnerSessionMaxAge).Unix()); err != nil {
		return "", time.Time{}, err
	}
	// Keep the newest sessions; a browser that signs in over and over must
	// not grow the table without bound.
	if _, err := tx.ExecContext(ctx, `DELETE FROM auth_sessions WHERE user_id=? AND token_hash NOT IN (
 SELECT token_hash FROM auth_sessions WHERE user_id=? ORDER BY created_at DESC, token_hash LIMIT ?)`,
		u.ID, u.ID, maxOwnerSessions-1); err != nil {
		return "", time.Time{}, err
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO auth_sessions(token_hash,user_id,created_at,expires_at)
 SELECT CAST(? AS TEXT),id,CAST(? AS BIGINT),CAST(? AS BIGINT) FROM auth_users WHERE id=? AND is_owner=1 AND active=1`,
		HashToken(token), now.Unix(), expires.Unix(), u.ID)
	if err != nil {
		return "", time.Time{}, err
	}
	if n, err := result.RowsAffected(); err != nil {
		return "", time.Time{}, err
	} else if n != 1 {
		return "", time.Time{}, core.ErrUnauthorized
	}
	if err := tx.Commit(); err != nil {
		return "", time.Time{}, err
	}
	return token, expires, nil
}

// OwnerSession returns the owner a console session belongs to. It fails with
// core.ErrUnauthorized once the session expired, was revoked, or its user is
// no longer the active owner.
func (s *Service) OwnerSession(ctx context.Context, token string) (*core.User, error) {
	token = strings.TrimSpace(token)
	if !IsOwnerSession(token) {
		return nil, core.ErrUnauthorized
	}
	now := s.o.Now()
	h := HashToken(token)
	var id, created, expires int64
	err := s.db.QueryRowContext(ctx, `SELECT s.user_id, s.created_at, s.expires_at FROM auth_sessions s
 JOIN auth_users u ON u.id=s.user_id
 WHERE s.token_hash=? AND s.expires_at>? AND s.created_at>? AND u.is_owner=1 AND u.active=1`,
		h, now.Unix(), now.Add(-OwnerSessionMaxAge).Unix()).Scan(&id, &created, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, core.ErrUnauthorized
	}
	if err != nil {
		return nil, err
	}
	renewed := now.Add(OwnerSessionTTL)
	if limit := time.Unix(created, 0).Add(OwnerSessionMaxAge); renewed.After(limit) {
		renewed = limit
	}
	if renewed.Unix()-expires >= int64(ownerSessionRenewGap/time.Second) {
		// Best effort: a failed renewal only shortens the session.
		if _, err := s.db.ExecContext(ctx, `UPDATE auth_sessions SET expires_at=? WHERE token_hash=? AND expires_at<?`,
			renewed.Unix(), h, renewed.Unix()); err != nil {
			s.log.Warn("owner session renewal", "err", err)
		}
	}
	r, err := scanRow(s.db.QueryRowContext(ctx, `SELECT `+userCols+` FROM auth_users WHERE id=? AND is_owner=1 AND active=1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, core.ErrUnauthorized
	}
	if err != nil {
		return nil, err
	}
	u := r.effective()
	s.touch(u.ID, now)
	return &u, nil
}

// RevokeOwnerSession ends a console session. Unknown tokens are not an
// error: signing out twice is harmless.
func (s *Service) RevokeOwnerSession(ctx context.Context, token string) error {
	if !IsOwnerSession(token) {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM auth_sessions WHERE token_hash=?`, HashToken(strings.TrimSpace(token)))
	return err
}
