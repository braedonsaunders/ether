package identity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"math/big"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func init() {
	store.Register("identity_quickconnect", 1, `
CREATE TABLE identity_quickconnect (
 code TEXT PRIMARY KEY, secret_hash TEXT NOT NULL UNIQUE,
 surface TEXT NOT NULL, device_id TEXT NOT NULL, client TEXT NOT NULL,
 product TEXT NOT NULL, version TEXT NOT NULL,
 expires INTEGER NOT NULL, viewer_id INTEGER, consumed INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX identity_quickconnect_expiry ON identity_quickconnect(expires);
`)
}

// QuickConnect is a shared, one-use approval broker. HTTP handlers must rate
// limit code approval and secret polling; codes are intentionally short.
type QuickConnect struct {
	db            *sql.DB
	ttl, grantTTL time.Duration
	now           func() time.Time
}

func NewQuickConnect(db *sql.DB, ttl, grantTTL time.Duration, now func() time.Time) (*QuickConnect, error) {
	if db == nil || ttl < time.Second || grantTTL < time.Second {
		return nil, errors.New("identity: invalid quick connect configuration")
	}
	if now == nil {
		now = time.Now
	}
	return &QuickConnect{db, ttl, grantTTL, now}, nil
}

const codeAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

func quickCode() (string, error) {
	b := make([]byte, 6)
	for i := range b {
		n, e := rand.Int(rand.Reader, big.NewInt(int64(len(codeAlphabet))))
		if e != nil {
			return "", e
		}
		b[i] = codeAlphabet[n.Int64()]
	}
	return string(b), nil
}
func secretHash(secret string) (string, bool) {
	b, e := base64.RawURLEncoding.Strict().DecodeString(secret)
	if e != nil || len(b) != 32 || base64.RawURLEncoding.EncodeToString(b) != secret {
		return "", false
	}
	h := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(h[:]), true
}

// Initiate creates a code to be shown on the device and a separate secret for polling.
func (q *QuickConnect) Initiate(ctx context.Context, surface string, device core.Device) (code, secret string, err error) {
	if surface == "" {
		return "", "", errors.New("identity: surface required")
	}
	var raw [32]byte
	if _, err = rand.Read(raw[:]); err != nil {
		return
	}
	secret = base64.RawURLEncoding.EncodeToString(raw[:])
	hash, _ := secretHash(secret)
	// Expired codes are removed before allocating so codes can eventually be reused.
	if _, err = q.db.ExecContext(ctx, `DELETE FROM identity_quickconnect WHERE expires<=?`, q.now().UnixMilli()); err != nil {
		return "", "", err
	}
	for i := 0; i < 10; i++ {
		code, err = quickCode()
		if err != nil {
			return
		}
		res, e := q.db.ExecContext(ctx, `INSERT INTO identity_quickconnect(code,secret_hash,surface,device_id,client,product,version,expires) VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(code) DO NOTHING`, code, hash, surface, device.ID, device.Client, device.Product, device.Version, q.now().Add(q.ttl).UnixMilli())
		if e != nil {
			return "", "", e
		}
		n, e := res.RowsAffected()
		if e != nil {
			return "", "", e
		}
		if n == 1 {
			return code, secret, nil
		}
	}
	return "", "", errors.New("identity: could not allocate quick connect code")
}

// Approve binds a pending, unexpired code to an active viewer exactly once.
func (q *QuickConnect) Approve(ctx context.Context, code string, viewerID int64) error {
	return q.approve(ctx, "", code, viewerID)
}

// ApproveSurface restricts an approval to a request for the given surface.
func (q *QuickConnect) ApproveSurface(ctx context.Context, surface, code string, viewerID int64) error {
	if surface == "" {
		return core.ErrUnauthorized
	}
	return q.approve(ctx, surface, code, viewerID)
}

func (q *QuickConnect) approve(ctx context.Context, surface, code string, viewerID int64) error {
	res, err := q.db.ExecContext(ctx, `UPDATE identity_quickconnect SET viewer_id=? WHERE code=? AND (?='' OR surface=?) AND viewer_id IS NULL AND consumed=0 AND expires>? AND EXISTS (SELECT 1 FROM auth_users WHERE id=? AND active=1)`, viewerID, code, surface, surface, q.now().UnixMilli(), viewerID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return core.ErrUnauthorized
	}
	return nil
}

// Poll consumes an approved request and creates its grant in one transaction.
// ErrQuickConnectPending means that approval has not yet happened.
var ErrQuickConnectPending = errors.New("identity: quick connect pending")

func (q *QuickConnect) Poll(ctx context.Context, secret string) (string, error) {
	return q.poll(ctx, "", secret)
}

// PollSurface verifies the surface atomically before consuming the request.
func (q *QuickConnect) PollSurface(ctx context.Context, surface, secret string) (string, error) {
	if surface == "" {
		return "", core.ErrUnauthorized
	}
	return q.poll(ctx, surface, secret)
}

func (q *QuickConnect) poll(ctx context.Context, requestedSurface, secret string) (string, error) {
	hash, ok := secretHash(secret)
	if !ok {
		return "", core.ErrUnauthorized
	}
	tx, err := q.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	now := q.now().UnixMilli()
	var viewer int64
	var surface, device, client, product, version string
	err = tx.QueryRowContext(ctx, `UPDATE identity_quickconnect SET consumed=1 WHERE secret_hash=? AND (?='' OR surface=?) AND viewer_id IS NOT NULL AND consumed=0 AND expires>? AND EXISTS (SELECT 1 FROM auth_users WHERE id=identity_quickconnect.viewer_id AND active=1) RETURNING viewer_id,surface,device_id,client,product,version`, hash, requestedSurface, requestedSurface, now).Scan(&viewer, &surface, &device, &client, &product, &version)
	if errors.Is(err, sql.ErrNoRows) {
		var pending int
		e := tx.QueryRowContext(ctx, `SELECT 1 FROM identity_quickconnect WHERE secret_hash=? AND (?='' OR surface=?) AND viewer_id IS NULL AND consumed=0 AND expires>?`, hash, requestedSurface, requestedSurface, now).Scan(&pending)
		if e == nil {
			return "", ErrQuickConnectPending
		}
		if !errors.Is(e, sql.ErrNoRows) {
			return "", e
		}
		return "", core.ErrUnauthorized
	}
	if err != nil {
		return "", err
	}
	var raw [32]byte
	if _, err = rand.Read(raw[:]); err != nil {
		return "", err
	}
	token := grantPrefix + base64.RawURLEncoding.EncodeToString(raw[:])
	grantHashValue, _ := grantHash(token)
	_, err = tx.ExecContext(ctx, `INSERT INTO identity_grants(hash,viewer_id,surface,device_id,client,product,version,created,last_used,expires) VALUES(?,?,?,?,?,?,?,?,?,?)`, grantHashValue, viewer, surface, device, client, product, version, now, now, now+q.grantTTL.Milliseconds())
	if err != nil {
		return "", err
	}
	if err = tx.Commit(); err != nil {
		return "", err
	}
	return token, nil
}
