package identity

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/argon2"

	"github.com/braedonsaunders/ether/internal/core"
	_ "github.com/braedonsaunders/ether/internal/identity/schema"
)

// Local authenticates unique local usernames, independent of display names.
// Membership is checked at authentication time against the viewer record.
type Local struct{ db *sql.DB }

func NewLocal(db *sql.DB) (*Local, error) {
	if db == nil {
		return nil, errors.New("identity: database required")
	}
	return &Local{db: db}, nil
}

func validUsername(s string) bool {
	return len(s) > 0 && len(s) <= 128 && strings.TrimSpace(s) == s && !strings.ContainsAny(s, "\x00\r\n")
}

const localPrefix = "$argon2id$v=19$m=65536,t=3,p=1$"

// hashPassword uses a fresh salt and a fixed, bounded Argon2id profile.
func hashPassword(password string) (string, error) {
	if len(password) == 0 || len(password) > 1024 {
		return "", errors.New("identity: invalid password length")
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	hash := argon2.IDKey([]byte(password), salt, 3, 65536, 1, 32)
	return localPrefix + base64.RawStdEncoding.EncodeToString(salt) + "$" + base64.RawStdEncoding.EncodeToString(hash), nil
}

func verifyPassword(encoded, password string) bool {
	if len(password) == 0 || len(password) > 1024 || !strings.HasPrefix(encoded, localPrefix) {
		return false
	}
	fields := strings.Split(strings.TrimPrefix(encoded, localPrefix), "$")
	if len(fields) != 2 || len(fields[0]) != 22 || len(fields[1]) != 43 {
		return false
	}
	salt, err := base64.RawStdEncoding.Strict().DecodeString(fields[0])
	if err != nil || len(salt) != 16 {
		return false
	}
	want, err := base64.RawStdEncoding.Strict().DecodeString(fields[1])
	if err != nil || len(want) != 32 {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, 3, 65536, 1, 32)
	return subtle.ConstantTimeCompare(got, want) == 1
}

// Create links a username and password to an existing viewer. Database
// uniqueness prevents two identities from claiming the same username.
func (l *Local) Create(ctx context.Context, viewerID int64, username, password string) error {
	if viewerID <= 0 || !validUsername(username) {
		return errors.New("identity: invalid local identity")
	}
	hash, err := hashPassword(password)
	if err != nil {
		return err
	}
	result, err := l.db.ExecContext(ctx, `INSERT INTO identity_links (viewer_id,provider,subject,display,verified_at,password_hash) SELECT ?, 'local', ?, ?, ?, ? WHERE EXISTS(SELECT 1 FROM auth_users WHERE id=? AND origin<>'dlna_guest')`, viewerID, username, username, time.Now().Unix(), hash, viewerID)
	if err != nil {
		return fmt.Errorf("identity: create local identity: %w", err)
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return core.ErrUnauthorized
	}
	return nil
}

// SetPassword resets a linked local credential by viewer and username.
func (l *Local) SetPassword(ctx context.Context, viewerID int64, username, password string) error {
	if viewerID <= 0 || !validUsername(username) {
		return errors.New("identity: invalid local identity")
	}
	hash, err := hashPassword(password)
	if err != nil {
		return err
	}
	result, err := l.db.ExecContext(ctx, `UPDATE identity_links SET password_hash=? WHERE viewer_id=? AND provider='local' AND subject=? AND EXISTS(SELECT 1 FROM auth_users WHERE id=? AND origin<>'dlna_guest')`, hash, viewerID, username, viewerID)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return core.ErrNotFound
	}
	return nil
}

// Authenticate returns the viewer and identity IDs only for active members.
func (l *Local) Authenticate(ctx context.Context, username, password string) (viewerID, identityID int64, err error) {
	if !validUsername(username) || len(password) == 0 || len(password) > 1024 {
		return 0, 0, core.ErrUnauthorized
	}
	var hash string
	err = l.db.QueryRowContext(ctx, `SELECT l.viewer_id,l.id,l.password_hash FROM identity_links l JOIN auth_users u ON u.id=l.viewer_id WHERE l.provider='local' AND l.subject=? AND u.active=1`, username).Scan(&viewerID, &identityID, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, core.ErrUnauthorized
	}
	if err != nil {
		return 0, 0, err
	}
	if !verifyPassword(hash, password) {
		return 0, 0, core.ErrUnauthorized
	}
	return viewerID, identityID, nil
}

// HasPassword reports whether an active viewer has a local password identity.
func (l *Local) HasPassword(ctx context.Context, viewerID int64) (bool, error) {
	var found bool
	err := l.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM identity_links i JOIN auth_users u ON u.id=i.viewer_id WHERE i.viewer_id=? AND i.provider='local' AND i.password_hash<>'' AND u.active=1)`, viewerID).Scan(&found)
	return found, err
}
