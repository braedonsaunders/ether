package experience

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/braedonsaunders/ether/internal/handles/capability"
)

// PruneReceiptShard retires at most HeaderBatchMax expired receipts, including
// abandoned devices. The header DELETE trigger retires typed payloads and
// claims atomically. It uses the same shard lock as issuance before touching
// either table; live receipts are never evicted to recover capacity.
func PruneReceiptShard(ctx context.Context, db *sql.DB, now time.Time, shard int64) (int64, error) {
	if db == nil || now.UnixMilli() <= 0 || shard < 0 || shard >= capability.HeaderShardCount {
		return 0, errors.New("experience: invalid receipt maintenance scope")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	// Any positive viewer on this shard selects the same lock across surfaces.
	scope := capability.HeaderScope{Namespace: ReceiptCapabilityNamespace, ViewerID: shard + capability.HeaderShardCount, Surface: "maintenance"}
	if err = capability.LockHeaderScopeInTx(ctx, db, tx, scope); err != nil {
		return 0, err
	}
	cutoff := now.UnixMilli()
	res, err := tx.ExecContext(ctx, `DELETE FROM capability_headers WHERE namespace=? AND shard=? AND expires<=? AND digest IN (SELECT digest FROM capability_headers WHERE namespace=? AND shard=? AND expires<=? ORDER BY expires,digest LIMIT ?)`, ReceiptCapabilityNamespace, shard, cutoff, ReceiptCapabilityNamespace, shard, cutoff, capability.HeaderBatchMax)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	return n, nil
}
