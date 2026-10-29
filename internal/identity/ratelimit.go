package identity

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/netip"
	"time"

	"github.com/braedonsaunders/ether/internal/store"
)

func init() {
	store.Register("identity_ratelimit", 1, `CREATE TABLE identity_rate_buckets (
 bucket_key TEXT PRIMARY KEY,
 window_start INTEGER NOT NULL,
 attempts INTEGER NOT NULL
 );
CREATE INDEX identity_rate_buckets_window ON identity_rate_buckets(window_start);`)
	store.Register("identity_ratelimit", 2, `ALTER TABLE identity_rate_buckets ADD COLUMN namespace TEXT NOT NULL DEFAULT 'login'; CREATE INDEX identity_rate_buckets_namespace_window ON identity_rate_buckets(namespace,window_start);`)
}

// LoginLimiter counts attempts against both the client IP and the combination
// of identity and IP. Database updates serialize concurrent cluster nodes.
// Success does not reset either bucket. Each call represents one attempt.
type LoginLimiter struct {
	db                   *sql.DB
	now                  func() time.Time
	window               time.Duration
	perIP, perIdentityIP int
	namespace            string
}

func NewLoginLimiter(db *sql.DB, window time.Duration, perIP, perIdentityIP int, now func() time.Time) (*LoginLimiter, error) {
	if db == nil || window < time.Millisecond || perIP <= 0 || perIdentityIP <= 0 {
		return nil, errors.New("identity: invalid login limiter configuration")
	}
	if now == nil {
		now = time.Now
	}
	return &LoginLimiter{db: db, now: now, window: window, perIP: perIP, perIdentityIP: perIdentityIP, namespace: "login"}, nil
}

// NewPollLimiter uses a separate persistent bucket namespace from login
// attempts. Use it for PIN and Quick Connect polling, keyed by the opaque
// handle or polling secret. Multiple nodes share the same limits.
func NewPollLimiter(db *sql.DB, window time.Duration, perIP, perHandleIP int, now func() time.Time) (*LoginLimiter, error) {
	l, err := NewLoginLimiter(db, window, perIP, perHandleIP, now)
	if err != nil {
		return nil, err
	}
	l.namespace = "poll"
	return l, nil
}

func rateKey(kind string, parts ...string) string {
	h := sha256.New()
	for _, part := range append([]string{kind}, parts...) {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Allow records an attempt before verification. It fails closed on database
// errors; callers must not skip this step for valid passwords or PINs.
func (l *LoginLimiter) Allow(ctx context.Context, identity string, ip netip.Addr) (bool, error) {
	if !ip.IsValid() || len(identity) > 1024 {
		return false, errors.New("identity: invalid limiter key")
	}
	addr := ip.Unmap().String()
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	now := l.now().UnixMilli()
	cutoff := now - l.window.Milliseconds()
	update := func(key string) (int, error) {
		var n int
		err := tx.QueryRowContext(ctx, `INSERT INTO identity_rate_buckets(bucket_key,window_start,attempts,namespace) VALUES(?,?,1,?)
ON CONFLICT(bucket_key) DO UPDATE SET
 attempts=CASE WHEN identity_rate_buckets.window_start<=? THEN 1 ELSE identity_rate_buckets.attempts+1 END,
 window_start=CASE WHEN identity_rate_buckets.window_start<=? THEN ? ELSE identity_rate_buckets.window_start END
RETURNING attempts`, key, now, l.namespace, cutoff, cutoff, now).Scan(&n)
		return n, err
	}
	ipCount, err := update(rateKey(l.namespace, "ip", addr))
	if err != nil {
		return false, err
	}
	pairCount, err := update(rateKey(l.namespace, "identity-ip", identity, addr))
	if err != nil {
		return false, err
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	// Best-effort bounded cleanup runs outside the admission transaction; a
	// cleanup failure never resets an active bucket or grants access.
	_, _ = l.Cleanup(ctx, 16)
	return ipCount <= l.perIP && pairCount <= l.perIdentityIP, nil
}

// Cleanup removes at most limit expired buckets. The predicate is repeated
// outside the selection so a bucket refreshed by another node is retained.
func (l *LoginLimiter) Cleanup(ctx context.Context, limit int) (int64, error) {
	if limit <= 0 || limit > 1000 {
		return 0, errors.New("identity: invalid cleanup limit")
	}
	cutoff := l.now().UnixMilli() - l.window.Milliseconds()
	r, err := l.db.ExecContext(ctx, `DELETE FROM identity_rate_buckets WHERE namespace=? AND window_start<=? AND bucket_key IN (SELECT bucket_key FROM identity_rate_buckets WHERE namespace=? AND window_start<=? ORDER BY window_start,bucket_key LIMIT ?)`, l.namespace, cutoff, l.namespace, cutoff, limit)
	if err != nil {
		return 0, err
	}
	return r.RowsAffected()
}
