package reservations

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/store"
)

// Bookkeeping only: adapters must authorize, insert and remove the domain
// record in the same transaction. Old writers bypass this table; cluster-wide
// quotas need an independently enforced mixed-version fence.
func init() {
	store.Register("handle_reservations", 1, `CREATE TABLE handle_reservations (namespace TEXT NOT NULL,digest TEXT NOT NULL,shard INTEGER NOT NULL,viewer_id BIGINT NOT NULL,surface TEXT NOT NULL,expires BIGINT NOT NULL,PRIMARY KEY(namespace,digest)); CREATE INDEX handle_reservations_shard_expiry ON handle_reservations(namespace,shard,expires); CREATE INDEX handle_reservations_scope ON handle_reservations(namespace,shard,viewer_id,surface,expires);`)
}

type Policy struct {
	Namespace                  string
	Shards                     int64
	PerShard, PerViewerSurface int
	TTL                        time.Duration
}
type Reservation struct {
	Digest   string
	ViewerID int64
	Surface  string
}
type CapacityError struct{ Namespace, Scope string }

func (e *CapacityError) Error() string { return "handles: capacity reached (" + e.Scope + ")" }

type Store struct {
	db     *sql.DB
	policy Policy
}
type Tx struct {
	owner *Store
	tx    *sql.Tx
}

// New fixes policy at the adapter boundary. All upgraded nodes must use the
// same policy for this namespace; changing it requires a separate version fence.
func New(db *sql.DB, p Policy) (*Store, error) {
	if db == nil || p.Namespace == "" || len(p.Namespace) > 64 || p.Shards <= 0 || p.Shards > 1024 || p.PerShard <= 0 || p.PerViewerSurface <= 0 || p.TTL < time.Second || p.TTL > 24*time.Hour {
		return nil, errors.New("handles: invalid reservation policy")
	}
	return &Store{db: db, policy: p}, nil
}
func (s *Store) Begin(ctx context.Context) (*Tx, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	return &Tx{owner: s, tx: tx}, nil
}

// SQL exposes only this store's transaction for the adapter's domain record.
func (t *Tx) SQL() *sql.Tx    { return t.tx }
func (t *Tx) Commit() error   { return t.tx.Commit() }
func (t *Tx) Rollback() error { return t.tx.Rollback() }
func (t *Tx) lock(ctx context.Context, viewer int64) error {
	return store.LockKey(ctx, store.DialectOf(t.owner.db), t.tx, fmt.Sprintf("handles:%s:%d", t.owner.policy.Namespace, viewer%t.owner.policy.Shards))
}
func valid(r Reservation) bool {
	if r.ViewerID <= 0 || r.Surface == "" || len(r.Surface) > 128 || len(r.Digest) != 64 {
		return false
	}
	for _, c := range r.Digest {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}

// Reserve obtains time from the DB on the transaction, never from the client.
// Its returned expiry must also be the maximum expiry of the typed domain row.
// Fresh random handles only; stable retry and active session leases are not
// supported. TTL cleanup removes bookkeeping, not domain data or live sessions.
func (t *Tx) Reserve(ctx context.Context, r Reservation) (int64, error) {
	if !valid(r) {
		return 0, errors.New("handles: invalid reservation")
	}
	p := t.owner.policy
	shard := r.ViewerID % p.Shards
	if err := t.lock(ctx, r.ViewerID); err != nil {
		return 0, err
	}
	// Both dialects round upward to the next whole millisecond.
	query := `SELECT CAST((julianday('now') - 2440587.5)*86400000 + 0.999 AS INTEGER)`
	if store.DialectOf(t.owner.db) == store.Postgres {
		query = `SELECT CAST(CEIL(EXTRACT(EPOCH FROM clock_timestamp())*1000) AS BIGINT)`
	}
	var now int64
	if err := t.tx.QueryRowContext(ctx, query).Scan(&now); err != nil {
		return 0, err
	}
	if _, err := t.tx.ExecContext(ctx, `DELETE FROM handle_reservations WHERE namespace=? AND shard=? AND expires<=?`, p.Namespace, shard, now); err != nil {
		return 0, err
	}
	var count int
	if err := t.tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM handle_reservations WHERE namespace=? AND shard=? AND viewer_id=? AND surface=?`, p.Namespace, shard, r.ViewerID, r.Surface).Scan(&count); err != nil {
		return 0, err
	}
	if count >= p.PerViewerSurface {
		return 0, &CapacityError{Namespace: p.Namespace, Scope: "viewer"}
	}
	if err := t.tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM handle_reservations WHERE namespace=? AND shard=?`, p.Namespace, shard).Scan(&count); err != nil {
		return 0, err
	}
	if count >= p.PerShard {
		return 0, &CapacityError{Namespace: p.Namespace, Scope: "shard"}
	}
	expiry := now + p.TTL.Milliseconds()
	_, err := t.tx.ExecContext(ctx, `INSERT INTO handle_reservations(namespace,digest,shard,viewer_id,surface,expires) VALUES(?,?,?,?,?,?)`, p.Namespace, r.Digest, shard, r.ViewerID, r.Surface, expiry)
	if err != nil {
		return 0, err
	}
	return expiry, nil
}

// Release must accompany a separately authorized domain deletion in this very
// transaction. False means missing OR foreign: it does not authorize domain
// deletion. The adapter must use its own scope-checked conditional deletion.
func (t *Tx) Release(ctx context.Context, r Reservation) (bool, error) {
	if !valid(r) {
		return false, errors.New("handles: invalid reservation")
	}
	if err := t.lock(ctx, r.ViewerID); err != nil {
		return false, err
	}
	result, err := t.tx.ExecContext(ctx, `DELETE FROM handle_reservations WHERE namespace=? AND digest=? AND viewer_id=? AND surface=?`, t.owner.policy.Namespace, r.Digest, r.ViewerID, r.Surface)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}
