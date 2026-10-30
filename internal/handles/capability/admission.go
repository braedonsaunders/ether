package capability

import (
	"context"
	"database/sql"
	"fmt"
	"hash/fnv"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/store"
)

// Limits are fixed by the issuing domain, never by a request. All writers for
// a namespace must use the same limits. Expired rows remain charged until the
// domain safely retires them; admission never evicts a live resource.
type Limits struct{ PerScope, PerShard int }

// AdmitHeadersInTx reserves bounded storage in the domain transaction. A batch
// has one namespace/viewer/surface, but may contain different devices. The
// caller authorizes and inserts its typed records in this same transaction and
// must roll back on any error. Existing rows (including pre-cutover headers)
// count toward admission. This does not fence direct SQL or old binaries.
func AdmitHeadersInTx(ctx context.Context, db *sql.DB, tx *sql.Tx, headers []Header, limits Limits) error {
	if db == nil || tx == nil || len(headers) == 0 || len(headers) > HeaderBatchMax || limits.PerScope <= 0 || limits.PerShard < limits.PerScope {
		return ErrInvalidHeader
	}
	first := headers[0]
	seen := make(map[string]bool, len(headers))
	for _, h := range headers {
		scope := h.HeaderScope
		// Tickets use the same primitive; the low-level mirror binder still rejects them.
		if scope.Namespace == ticketNamespace {
			scope.Namespace = "ticket.admission"
		}
		if !validHeaderScope(scope) || !digestValid(h.Digest) || h.ExpiresMillis <= 0 || h.Namespace != first.Namespace || h.ViewerID != first.ViewerID || h.Surface != first.Surface || seen[h.Digest] {
			return ErrInvalidHeader
		}
		seen[h.Digest] = true
	}
	if err := lockHeaders(ctx, db, tx, first.HeaderScope); err != nil {
		return err
	}
	shard := first.ViewerID % HeaderShardCount
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM capability_headers WHERE namespace=? AND shard=? AND viewer_id=? AND surface=?`, first.Namespace, shard, first.ViewerID, first.Surface).Scan(&n); err != nil {
		return err
	}
	if n+len(headers) > limits.PerScope {
		return &CapacityError{Scope: "viewer"}
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM capability_headers WHERE namespace=? AND shard=?`, first.Namespace, shard).Scan(&n); err != nil {
		return err
	}
	if n+len(headers) > limits.PerShard {
		return &CapacityError{Scope: "shard"}
	}
	return insertHeadersInTx(ctx, tx, headers)
}

func lockHeaders(ctx context.Context, db *sql.DB, tx *sql.Tx, scope HeaderScope) error {
	return store.LockKey(ctx, store.DialectOf(db), tx, fmt.Sprintf("capability:admission:%s:%d", scope.Namespace, scope.ViewerID%HeaderShardCount))
}

// ConsumeHeaderInTx removes exactly one live, scoped reservation. The domain
// must revalidate authorization and consume its typed payload in this same
// transaction. A false result never authorizes a fallback. Reusable handles
// should be revoked with this operation only when their domain lifetime ends.
func ConsumeHeaderInTx(ctx context.Context, db *sql.DB, tx *sql.Tx, h Header, nowMillis int64) (bool, error) {
	scope := h.HeaderScope
	if scope.Namespace == ticketNamespace {
		scope.Namespace = "ticket.admission"
	}
	if db == nil || tx == nil || !validHeaderScope(scope) || !digestValid(h.Digest) || nowMillis <= 0 {
		return false, ErrInvalidHeader
	}
	if err := lockHeaders(ctx, db, tx, h.HeaderScope); err != nil {
		return false, err
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM capability_headers WHERE namespace=? AND digest=? AND shard=? AND viewer_id=? AND surface=? AND device_id=? AND expires>?`, h.Namespace, h.Digest, h.ViewerID%HeaderShardCount, h.ViewerID, h.Surface, h.DeviceID, nowMillis)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}

// ClaimHeaderInTx atomically claims one purpose on a reusable handle. A false
// result is an already-claimed purpose; absent, foreign or expired handles fail
// closed. Typed domain authorization must be checked in this transaction.
func ClaimHeaderInTx(ctx context.Context, db *sql.DB, tx *sql.Tx, h Header, purpose string, nowMillis int64) (bool, error) {
	if db == nil || tx == nil || !validHeaderScope(h.HeaderScope) || !digestValid(h.Digest) || purpose == "" || len(purpose) > 128 || nowMillis <= 0 {
		return false, ErrInvalidHeader
	}
	if err := lockHeaders(ctx, db, tx, h.HeaderScope); err != nil {
		return false, err
	}
	var found int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM capability_headers WHERE namespace=? AND digest=? AND viewer_id=? AND surface=? AND device_id=? AND expires>?`, h.Namespace, h.Digest, h.ViewerID, h.Surface, h.DeviceID, nowMillis).Scan(&found); err != nil {
		return false, err
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO capability_claims(namespace,digest,purpose) VALUES(?,?,?) ON CONFLICT(namespace,digest,purpose) DO NOTHING`, h.Namespace, h.Digest, purpose)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}

// AdmissionLockSQL returns a PostgreSQL advisory-lock expression matching the
// Go admission lock. Inputs must be trusted migration constants, never request
// text. Keeping both forms here prevents domain triggers taking a different lock.
func AdmissionLockSQL(namespace, viewerSQL string) string {
	var out strings.Builder
	out.WriteString("(ARRAY[")
	for shard := int64(0); shard < HeaderShardCount; shard++ {
		if shard > 0 {
			out.WriteByte(',')
		}
		hash := fnv.New64a()
		fmt.Fprintf(hash, "ether:capability:admission:%s:%d", namespace, shard)
		out.WriteString(strconv.FormatInt(int64(hash.Sum64()), 10))
		out.WriteString("::bigint")
	}
	out.WriteString("])[(")
	out.WriteString(viewerSQL)
	fmt.Fprintf(&out, " %% %d)+1]", HeaderShardCount)
	return out.String()
}

// RenewHeaderInTx extends one live, exact-scoped header under the admission
// lock without adding a reservation. Existing active lifetimes remain renewable
// when migrated state is over quota; fresh admission still fails closed. The
// domain must authorize renewal and update its typed payload in the same tx.
func RenewHeaderInTx(ctx context.Context, db *sql.DB, tx *sql.Tx, h Header, nowMillis int64) (bool, error) {
	scope := h.HeaderScope
	if scope.Namespace == ticketNamespace {
		scope.Namespace = "ticket.admission"
	}
	if db == nil || tx == nil || !validHeaderScope(scope) || !digestValid(h.Digest) || nowMillis <= 0 || h.ExpiresMillis <= nowMillis {
		return false, ErrInvalidHeader
	}
	if err := lockHeaders(ctx, db, tx, h.HeaderScope); err != nil {
		return false, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE capability_headers SET expires=? WHERE namespace=? AND digest=? AND shard=? AND viewer_id=? AND surface=? AND device_id=? AND expires>?`, h.ExpiresMillis, h.Namespace, h.Digest, h.ViewerID%HeaderShardCount, h.ViewerID, h.Surface, h.DeviceID, nowMillis)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	return affected == 1, err
}
