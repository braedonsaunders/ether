package identity

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"time"
)

// CreateOwner atomically creates the first local owner and their credential.
// The database's single-owner constraint arbitrates concurrent setup requests.
// Callers must refresh auth's viewer cache after successful creation.
func (l *Local) CreateOwner(ctx context.Context, username, password string) (int64, error) {
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
	err = tx.QueryRowContext(ctx, `INSERT INTO auth_users(plex_id,username,title,is_owner,active,origin,created_at,updated_at) VALUES(NULL,?,?,1,1,'local',?,?) RETURNING id`, username, username, now, now).Scan(&id)
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

// OwnerCredential is the owner's local sign-in: what a failed start's
// recovery page verifies against while the database cannot be used.
type OwnerCredential struct {
	ViewerID   int64  `json:"viewerId"`
	IdentityID int64  `json:"identityId"`
	Username   string `json:"username"`
	Hash       string `json:"hash"`
}

// OwnerLocalCredential reads the active owner's local credential from q (a
// database, possibly opened read-only). ok is false when the owner has no
// local password.
func OwnerLocalCredential(ctx context.Context, q interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}) (cred OwnerCredential, ok bool, err error) {
	err = q.QueryRowContext(ctx, `SELECT l.viewer_id,l.id,l.subject,l.password_hash FROM identity_links l JOIN auth_users u ON u.id=l.viewer_id WHERE l.provider='local' AND u.is_owner=1 AND u.active=1 AND l.password_hash<>'' ORDER BY l.id LIMIT 1`).
		Scan(&cred.ViewerID, &cred.IdentityID, &cred.Username, &cred.Hash)
	if errors.Is(err, sql.ErrNoRows) {
		return OwnerCredential{}, false, nil
	}
	if err != nil {
		return OwnerCredential{}, false, err
	}
	return cred, true, nil
}

// VerifyOwnerCredential checks username and password against cred with the
// same Argon2id verification and constant-time comparison as Authenticate.
func VerifyOwnerCredential(cred OwnerCredential, username, password string) bool {
	nameOK := subtle.ConstantTimeCompare([]byte(cred.Username), []byte(username)) == 1
	return verifyPassword(cred.Hash, password) && nameOK
}
