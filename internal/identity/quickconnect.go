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
	store.Register("identity_quickconnect", 1, `
CREATE TABLE identity_quickconnect (
 code TEXT PRIMARY KEY, secret_hash TEXT NOT NULL UNIQUE,
 surface TEXT NOT NULL, device_id TEXT NOT NULL, client TEXT NOT NULL,
 product TEXT NOT NULL, version TEXT NOT NULL,
 expires INTEGER NOT NULL, viewer_id INTEGER, consumed INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX identity_quickconnect_expiry ON identity_quickconnect(expires);
`)
	store.Register("identity_quickconnect", 2, `ALTER TABLE identity_quickconnect ADD COLUMN device_name TEXT NOT NULL DEFAULT ''; ALTER TABLE identity_quickconnect ADD COLUMN created_at INTEGER NOT NULL DEFAULT 0;`)
}

// QuickConnect is a shared, one-use approval broker. HTTP handlers must rate
// limit code approval and secret polling; codes are intentionally short.
type QuickConnect struct {
	db             *sql.DB
	ttl, grantTTL  time.Duration
	now            func() time.Time
	expire, insert *sql.Stmt
}

const (
	quickExpireSQL = `DELETE FROM identity_quickconnect WHERE expires<=?`
	quickInsertSQL = `INSERT INTO identity_quickconnect(code,secret_hash,surface,device_id,client,product,version,device_name,created_at,expires) VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(code) DO NOTHING`
)

func NewQuickConnect(db *sql.DB, ttl, grantTTL time.Duration, now func() time.Time) (*QuickConnect, error) {
	if db == nil || ttl < time.Second || grantTTL < time.Second {
		return nil, errors.New("identity: invalid quick connect configuration")
	}
	if now == nil {
		now = time.Now
	}
	expire, err := db.Prepare(quickExpireSQL)
	if err != nil {
		return nil, err
	}
	insert, err := db.Prepare(quickInsertSQL)
	if err != nil {
		return nil, err
	}
	q := &QuickConnect{db: db, ttl: ttl, grantTTL: grantTTL, now: now, expire: expire, insert: insert}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	// A committed cleanup parses the statement and flushes the journal before
	// a viewer is waiting. Later browse traffic can evict an unpinned plan.
	_, _ = expire.ExecContext(ctx, int64(0))
	return q, nil
}

const codeAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

func quickCode() (string, error) {
	var raw [6]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	b := make([]byte, 6)
	for i, v := range raw {
		b[i] = codeAlphabet[int(v)%len(codeAlphabet)]
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
	// One transaction: the expired-code delete and the insert share a journal
	// flush. Expired codes are removed so a short code can be reused.
	tx, err := q.db.BeginTx(ctx, nil)
	if err != nil {
		return "", "", err
	}
	defer tx.Rollback()
	expire := tx.StmtContext(ctx, q.expire)
	insert := tx.StmtContext(ctx, q.insert)
	if _, err = expire.ExecContext(ctx, q.now().UnixMilli()); err != nil {
		return "", "", err
	}
	for i := 0; i < 10; i++ {
		code, err = quickCode()
		if err != nil {
			return
		}
		now := q.now()
		res, e := insert.ExecContext(ctx, code, hash, surface, device.ID, device.Client, device.Product, device.Version, device.DeviceName, now.UnixMilli(), now.Add(q.ttl).UnixMilli())
		if e != nil {
			return "", "", e
		}
		n, e := res.RowsAffected()
		if e != nil {
			return "", "", e
		}
		if n == 1 {
			if err = tx.Commit(); err != nil {
				return "", "", err
			}
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
	res, err := q.db.ExecContext(ctx, `UPDATE identity_quickconnect SET viewer_id=? WHERE code=? AND (?='' OR surface=?) AND viewer_id IS NULL AND consumed=0 AND expires>? AND EXISTS (SELECT 1 FROM auth_users WHERE id=? AND active=1 AND origin<>'dlna_guest')`, viewerID, code, surface, surface, q.now().UnixMilli(), viewerID)
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

// StatusSurface reports approval without consuming the secret or issuing a grant.
func (q *QuickConnect) StatusSurface(ctx context.Context, surface, secret string) (bool, error) {
	details, err := q.DetailsSurface(ctx, surface, secret)
	return details.Approved, err
}

type QuickConnectDetails struct {
	Approved  bool
	Code      string
	Device    core.Device
	CreatedAt time.Time
	ExpiresAt time.Time
}

// DetailsSurface reads metadata without consuming a valid surface-bound secret.
func (q *QuickConnect) DetailsSurface(ctx context.Context, surface, secret string) (QuickConnectDetails, error) {
	var details QuickConnectDetails
	hash, ok := secretHash(secret)
	if !ok || surface == "" {
		return details, core.ErrUnauthorized
	}
	var viewer sql.NullInt64
	var active bool
	var created, expires int64
	err := q.db.QueryRowContext(ctx, `SELECT code,device_id,client,product,version,device_name,created_at,expires,viewer_id, EXISTS (SELECT 1 FROM auth_users WHERE id=identity_quickconnect.viewer_id AND active=1) FROM identity_quickconnect WHERE secret_hash=? AND surface=? AND consumed=0 AND expires>?`, hash, surface, q.now().UnixMilli()).Scan(&details.Code, &details.Device.ID, &details.Device.Client, &details.Device.Product, &details.Device.Version, &details.Device.DeviceName, &created, &expires, &viewer, &active)
	if errors.Is(err, sql.ErrNoRows) {
		return details, core.ErrUnauthorized
	}
	if err != nil {
		return details, err
	}
	if viewer.Valid && !active {
		return details, core.ErrUnauthorized
	}
	if created == 0 {
		created = expires - q.ttl.Milliseconds()
	}
	details.CreatedAt = time.UnixMilli(created)
	details.ExpiresAt = time.UnixMilli(expires)
	details.Approved = viewer.Valid
	return details, nil
}

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
	err = tx.QueryRowContext(ctx, `UPDATE identity_quickconnect SET consumed=1 WHERE secret_hash=? AND (?='' OR surface=?) AND viewer_id IS NOT NULL AND consumed=0 AND expires>? AND EXISTS (SELECT 1 FROM auth_users WHERE id=identity_quickconnect.viewer_id AND active=1 AND origin<>'dlna_guest') RETURNING viewer_id,surface,device_id,client,product,version`, hash, requestedSurface, requestedSurface, now).Scan(&viewer, &surface, &device, &client, &product, &version)
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
	if err = recordLogin(ctx, tx, viewer, now); err != nil {
		return "", err
	}
	if err = tx.Commit(); err != nil {
		return "", err
	}
	return token, nil
}
