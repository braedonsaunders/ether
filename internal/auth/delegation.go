package auth

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

const delegationPrefix = "ether-cast-"
const delegationTTL = 15 * time.Minute

func init() {
	store.Register("auth", 3, `
CREATE TABLE auth_delegations (
 token_hash TEXT PRIMARY KEY,
 user_id INTEGER NOT NULL REFERENCES auth_users(id) ON DELETE CASCADE,
 expires_at INTEGER NOT NULL
);
CREATE INDEX auth_delegations_user ON auth_delegations(user_id);
`)
}

var _ core.DelegationAuth = (*Service)(nil)

// IssueDelegation creates a server-local credential, never a plex.tv account
// token. Only hashes persist; expiry and membership are checked on every use,
// including when another cluster node receives the credential.
func (s *Service) IssueDelegation(ctx context.Context, userID int64) (string, error) {
	now := s.o.Now()
	if _, err := s.db.ExecContext(ctx, `DELETE FROM auth_delegations WHERE expires_at<=?`, now.Unix()); err != nil {
		return "", err
	}
	token := delegationPrefix + rand.Text()
	result, err := s.db.ExecContext(ctx, `INSERT INTO auth_delegations(token_hash,user_id,expires_at)
 SELECT ?,id,? FROM auth_users WHERE id=? AND active=1 AND origin<>'dlna_guest'
 AND (SELECT COUNT(*) FROM auth_delegations WHERE user_id=?)<64`, HashToken(token), now.Add(delegationTTL).Unix(), userID, userID)
	if err != nil {
		return "", err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return "", err
	}
	if n != 1 {
		return "", core.ErrUnauthorized
	}
	return token, nil
}

func (s *Service) authenticateDelegation(ctx context.Context, token string) (*core.User, time.Time, error) {
	var id, expiresAt int64
	err := s.db.QueryRowContext(ctx, `SELECT user_id, expires_at FROM auth_delegations WHERE token_hash=? AND expires_at>?`, HashToken(token), s.o.Now().Unix()).Scan(&id, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, time.Time{}, core.ErrUnauthorized
	}
	if err != nil {
		return nil, time.Time{}, err
	}
	r, err := scanRow(s.db.QueryRowContext(ctx, `SELECT `+userCols+` FROM auth_users WHERE id=? AND active=1 AND origin<>'dlna_guest'`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, time.Time{}, core.ErrUnauthorized
	}
	if err != nil {
		return nil, time.Time{}, err
	}
	u := r.effective()
	u.IsOwner, u.Delegated = false, true
	return &u, time.Unix(expiresAt, 0).UTC(), nil
}
