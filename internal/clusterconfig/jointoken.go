package clusterconfig

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// JoinTokenPrefix starts every join token.
const JoinTokenPrefix = "ether-join-v1:"

// DefaultJoinTokenTTL is how long a join token stays valid.
const DefaultJoinTokenTTL = time.Hour

var (
	// ErrTokenInvalid: the token is malformed or unknown to the cluster.
	ErrTokenInvalid = errors.New("clusterconfig: join token is not valid for this cluster")
	// ErrTokenUsed: the token was already used to join a node.
	ErrTokenUsed = errors.New("clusterconfig: join token has already been used")
	// ErrTokenExpired: the token's validity window has passed.
	ErrTokenExpired = errors.New("clusterconfig: join token has expired")
)

// JoinToken is what a join token string carries. It is a secret: it holds
// the database URL (with its password) and the cluster credential key.
type JoinToken struct {
	DatabaseURL string `json:"d"`
	ID          string `json:"i"`
	Secret      string `json:"s"`
	Key         []byte `json:"k"`
}

// String encodes the token as "ether-join-v1:" + base64url(JSON).
func (t JoinToken) String() string {
	b, _ := json.Marshal(t)
	return JoinTokenPrefix + base64.RawURLEncoding.EncodeToString(b)
}

// ParseJoinToken decodes a token string.
func ParseJoinToken(s string) (JoinToken, error) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, JoinTokenPrefix) {
		return JoinToken{}, fmt.Errorf("%w: it must start with %q", ErrTokenInvalid, JoinTokenPrefix)
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(s, JoinTokenPrefix))
	if err != nil {
		return JoinToken{}, fmt.Errorf("%w: damaged encoding (copy the whole token)", ErrTokenInvalid)
	}
	var t JoinToken
	if err := json.Unmarshal(raw, &t); err != nil || t.ID == "" || t.Secret == "" || t.DatabaseURL == "" {
		return JoinToken{}, fmt.Errorf("%w: incomplete token", ErrTokenInvalid)
	}
	if len(t.Key) != 32 {
		return JoinToken{}, fmt.Errorf("%w: bad credential key", ErrTokenInvalid)
	}
	return t, nil
}

// TokenInfo describes a stored token (never its secret).
type TokenInfo struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"createdAt"`
	ExpiresAt time.Time `json:"expiresAt"`
	UsedAt    time.Time `json:"usedAt,omitzero"`
	UsedBy    string    `json:"usedBy,omitempty"`
}

func secretHash(id, secret string) string {
	h := sha256.Sum256([]byte("ether-join:" + id + ":" + secret))
	return hex.EncodeToString(h[:])
}

func randomString(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// CreateJoinToken stores a new one-time token (only its hash) valid for ttl
// and returns it. databaseURL is the URL the joining node will use and key
// the cluster credential key; both travel only inside the token.
func CreateJoinToken(ctx context.Context, db *sql.DB, databaseURL string, key []byte, ttl time.Duration, now time.Time) (JoinToken, TokenInfo, error) {
	if databaseURL == "" {
		return JoinToken{}, TokenInfo{}, errors.New("clusterconfig: a join token needs the cluster database URL")
	}
	if len(key) != 32 {
		return JoinToken{}, TokenInfo{}, errors.New("clusterconfig: invalid credential key")
	}
	if ttl <= 0 {
		ttl = DefaultJoinTokenTTL
	}
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return JoinToken{}, TokenInfo{}, err
	}
	id := hex.EncodeToString(b)
	secret, err := randomString(32)
	if err != nil {
		return JoinToken{}, TokenInfo{}, err
	}
	info := TokenInfo{ID: id, CreatedAt: now.UTC().Truncate(time.Second), ExpiresAt: now.Add(ttl).UTC().Truncate(time.Second)}
	// Maintenance is lifecycle-triggered, never a scheduled wall-clock sweep.
	if err := PruneExpiredJoinTokens(ctx, db, now); err != nil {
		return JoinToken{}, TokenInfo{}, err
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO clusterconfig_join_tokens(id, secret_hash, created_at, expires_at) VALUES(?, ?, ?, ?)`,
		id, secretHash(id, secret), info.CreatedAt.Unix(), info.ExpiresAt.Unix()); err != nil {
		return JoinToken{}, TokenInfo{}, err
	}
	return JoinToken{DatabaseURL: databaseURL, ID: id, Secret: secret, Key: append([]byte(nil), key...)}, info, nil
}

// ConsumeJoinToken marks the token used by usedBy, atomically: of any
// number of concurrent attempts exactly one succeeds. A used, expired or
// unknown token fails with ErrTokenUsed, ErrTokenExpired or ErrTokenInvalid.
func ConsumeJoinToken(ctx context.Context, db *sql.DB, t JoinToken, usedBy string, now time.Time) error {
	hash := secretHash(t.ID, t.Secret)
	res, err := db.ExecContext(ctx, `UPDATE clusterconfig_join_tokens SET used_at=?, used_by=? WHERE id=? AND secret_hash=? AND used_at=0 AND expires_at>?`,
		now.Unix(), usedBy, t.ID, hash, now.Unix())
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 1 {
		return nil
	}
	var stored string
	var expires, used int64
	var by string
	err = db.QueryRowContext(ctx, `SELECT secret_hash, expires_at, used_at, used_by FROM clusterconfig_join_tokens WHERE id=?`, t.ID).Scan(&stored, &expires, &used, &by)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && subtle.ConstantTimeCompare([]byte(stored), []byte(hash)) != 1) {
		return fmt.Errorf("%w (unknown or revoked); create a new token on a cluster node", ErrTokenInvalid)
	}
	if err != nil {
		return err
	}
	if used != 0 {
		return fmt.Errorf("%w (by %s at %s); create a new token on a cluster node", ErrTokenUsed, by, time.Unix(used, 0).UTC().Format(time.RFC3339))
	}
	return fmt.Errorf("%w (at %s); create a new token on a cluster node", ErrTokenExpired, time.Unix(expires, 0).UTC().Format(time.RFC3339))
}

// PruneExpiredJoinTokens removes at most 256 expired records per owner read or
// creation. Backlogs drain across calls; expiry validation never depends on cleanup.
// A pruned credential remains invalid, though its historical expiry/used reason
// is no longer retained. No goroutine or periodic sweep is started.
func PruneExpiredJoinTokens(ctx context.Context, db *sql.DB, now time.Time) error {
	_, err := db.ExecContext(ctx, `DELETE FROM clusterconfig_join_tokens WHERE id IN (SELECT id FROM clusterconfig_join_tokens WHERE expires_at<=? ORDER BY expires_at, id LIMIT 256)`, now.Unix())
	return err
}

// ActiveJoinTokens performs bounded maintenance and returns only unused tokens
// valid at now, without credential material. UI clients must also filter expiry
// as time passes after the response.
func ActiveJoinTokens(ctx context.Context, db *sql.DB, now time.Time) ([]TokenInfo, error) {
	if err := PruneExpiredJoinTokens(ctx, db, now); err != nil {
		return nil, err
	}
	return listJoinTokens(ctx, db, ` WHERE used_at=0 AND expires_at>?`, now.Unix())
}

// ListJoinTokens returns stored tokens, newest first.
func ListJoinTokens(ctx context.Context, db *sql.DB) ([]TokenInfo, error) {
	return listJoinTokens(ctx, db, "")
}

func listJoinTokens(ctx context.Context, db *sql.DB, filter string, args ...any) ([]TokenInfo, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, created_at, expires_at, used_at, used_by FROM clusterconfig_join_tokens`+filter+` ORDER BY created_at DESC, id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TokenInfo{}
	for rows.Next() {
		var ti TokenInfo
		var created, expires, used int64
		if err := rows.Scan(&ti.ID, &created, &expires, &used, &ti.UsedBy); err != nil {
			return nil, err
		}
		ti.CreatedAt, ti.ExpiresAt = time.Unix(created, 0).UTC(), time.Unix(expires, 0).UTC()
		if used != 0 {
			ti.UsedAt = time.Unix(used, 0).UTC()
		}
		out = append(out, ti)
	}
	return out, rows.Err()
}

// RevokeJoinToken deletes an unused token; it reports whether one existed.
func RevokeJoinToken(ctx context.Context, db *sql.DB, id string) (bool, error) {
	res, err := db.ExecContext(ctx, `DELETE FROM clusterconfig_join_tokens WHERE id=? AND used_at=0`, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}
