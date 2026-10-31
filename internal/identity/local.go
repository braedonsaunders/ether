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

// argonProfile is one accepted Argon2id parameter set. New passwords use the
// OWASP minimum (19 MiB, two iterations, one lane). The older 64 MiB profile
// still verifies so existing credentials keep working, and a successful check
// rewrites them. Parameters outside these two sets are rejected: a stored
// hash must not be able to demand unbounded memory.
type argonProfile struct {
	memory, time uint32
	threads      uint8
}

func (p argonProfile) prefix() string {
	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$", p.memory, p.time, p.threads)
}

var (
	argonCurrent = argonProfile{memory: 19 * 1024, time: 2, threads: 1}
	argonLegacy  = argonProfile{memory: 64 * 1024, time: 3, threads: 1}
)

// hashPassword uses a fresh salt and the current Argon2id profile.
func hashPassword(password string) (string, error) {
	return hashPasswordWith(argonCurrent, password)
}

func hashPasswordWith(profile argonProfile, password string) (string, error) {
	if len(password) == 0 || len(password) > 1024 {
		return "", errors.New("identity: invalid password length")
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	hash := argon2.IDKey([]byte(password), salt, profile.time, profile.memory, profile.threads, 32)
	return profile.prefix() + base64.RawStdEncoding.EncodeToString(salt) + "$" + base64.RawStdEncoding.EncodeToString(hash), nil
}

func verifyPassword(encoded, password string) bool {
	if len(password) == 0 || len(password) > 1024 {
		return false
	}
	profile, rest, ok := argonProfileOf(encoded)
	if !ok {
		return false
	}
	fields := strings.Split(rest, "$")
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
	got := argon2.IDKey([]byte(password), salt, profile.time, profile.memory, profile.threads, 32)
	return subtle.ConstantTimeCompare(got, want) == 1
}

func argonProfileOf(encoded string) (argonProfile, string, bool) {
	for _, profile := range []argonProfile{argonCurrent, argonLegacy} {
		prefix := profile.prefix()
		if strings.HasPrefix(encoded, prefix) {
			return profile, strings.TrimPrefix(encoded, prefix), true
		}
	}
	return argonProfile{}, "", false
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
	err = l.db.QueryRowContext(ctx, `SELECT l.viewer_id,l.id,l.password_hash FROM identity_links l JOIN auth_users u ON u.id=l.viewer_id WHERE l.provider='local' AND l.subject=? AND u.active=1 AND u.origin<>'dlna_guest'`, username).Scan(&viewerID, &identityID, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, core.ErrUnauthorized
	}
	if err != nil {
		return 0, 0, err
	}
	if !verifyPassword(hash, password) {
		return 0, 0, core.ErrUnauthorized
	}
	if strings.HasPrefix(hash, argonLegacy.prefix()) {
		go l.rehash(viewerID, username, password)
	}
	return viewerID, identityID, nil
}

// rehash replaces a legacy Argon2id encoding after it has already verified.
// A failed rewrite leaves the old hash in place for the next sign-in.
func (l *Local) rehash(viewerID int64, username, password string) {
	hash, err := hashPassword(password)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = l.db.ExecContext(ctx, `UPDATE identity_links SET password_hash=? WHERE viewer_id=? AND provider='local' AND subject=?`, hash, viewerID, username)
}

// HasPassword reports whether an active viewer has a local password identity.
func (l *Local) HasPassword(ctx context.Context, viewerID int64) (bool, error) {
	var found bool
	err := l.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM identity_links i JOIN auth_users u ON u.id=i.viewer_id WHERE i.viewer_id=? AND i.provider='local' AND i.password_hash<>'' AND u.active=1 AND u.origin<>'dlna_guest')`, viewerID).Scan(&found)
	return found, err
}
