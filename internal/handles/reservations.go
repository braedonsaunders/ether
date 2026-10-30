package handles

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/braedonsaunders/ether/internal/store"
)

// Reservations have no domain payload and do not themselves authorize access.
// A policy adapter must validate authority and write its typed record in the
// same transaction. Older nodes do not reserve slots; cluster-wide enforcement
// requires a separate all-node version fence.
func init() {
	store.Register("handle_reservations", 1, `CREATE TABLE handle_reservations (
 namespace TEXT NOT NULL, digest TEXT NOT NULL, shard INTEGER NOT NULL,
 viewer_id BIGINT NOT NULL, surface TEXT NOT NULL, expires BIGINT NOT NULL,
 PRIMARY KEY(namespace,digest)
 ); CREATE INDEX handle_reservations_shard_expiry ON handle_reservations(namespace,shard,expires);
 CREATE INDEX handle_reservations_scope ON handle_reservations(namespace,shard,viewer_id,surface,expires);`)
}

// ReservationPolicy is fixed by an adapter, not supplied by a client. Fixed
// shards bound global rows without a single global hot lock. Changing shard
// count or limits while old writers run needs a separate migration/fence.
type ReservationPolicy struct {
	Namespace        string
	Shards           int64
	PerShard         int
	PerViewerSurface int
}

type Reservation struct {
	Digest       string
	ViewerID     int64
	Surface      string
	Expires, Now int64
}

type CapacityError struct {
	Namespace string
	Scope     string
}

func (e *CapacityError) Error() string { return "handles: capacity reached (" + e.Scope + ")" }

// Reserve uses the caller's transaction, so quota and the adapter's domain
// insert commit or roll back together. Expiry cleanup only removes reservation
// bookkeeping; it must never be used to infer playback/session retirement.
func Reserve(ctx context.Context, db *sql.DB, tx *sql.Tx, policy ReservationPolicy, r Reservation) error {
	if db == nil || tx == nil || policy.Namespace == "" || len(policy.Namespace) > 64 || policy.Shards <= 0 || policy.Shards > 1024 || policy.PerShard <= 0 || policy.PerViewerSurface <= 0 || r.ViewerID <= 0 || r.Surface == "" || len(r.Surface) > 128 || r.Expires <= r.Now || len(r.Digest) != 64 {
		return errors.New("handles: invalid reservation")
	}
	for _, c := range r.Digest {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return errors.New("handles: invalid digest")
		}
	}
	shard := r.ViewerID % policy.Shards
	if err := store.LockKey(ctx, store.DialectOf(db), tx, fmt.Sprintf("handles:%s:%d", policy.Namespace, shard)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM handle_reservations WHERE namespace=? AND shard=? AND expires<=?`, policy.Namespace, shard, r.Now); err != nil {
		return err
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM handle_reservations WHERE namespace=? AND shard=? AND viewer_id=? AND surface=?`, policy.Namespace, shard, r.ViewerID, r.Surface).Scan(&count); err != nil {
		return err
	}
	if count >= policy.PerViewerSurface {
		return &CapacityError{Namespace: policy.Namespace, Scope: "viewer"}
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM handle_reservations WHERE namespace=? AND shard=?`, policy.Namespace, shard).Scan(&count); err != nil {
		return err
	}
	if count >= policy.PerShard {
		return &CapacityError{Namespace: policy.Namespace, Scope: "shard"}
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO handle_reservations(namespace,digest,shard,viewer_id,surface,expires) VALUES(?,?,?,?,?,?)`, policy.Namespace, r.Digest, shard, r.ViewerID, r.Surface, r.Expires)
	return err
}

// Release removes bookkeeping in the same transaction as a successful consume
// or revocation. Absence is allowed for records issued by older binaries.
func Release(ctx context.Context, tx *sql.Tx, namespace, digest string) error {
	if tx == nil || namespace == "" || digest == "" {
		return errors.New("handles: invalid release")
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM handle_reservations WHERE namespace=? AND digest=?`, namespace, digest)
	return err
}
