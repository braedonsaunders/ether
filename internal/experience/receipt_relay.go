package experience

import (
	"context"
	"database/sql"
	"math"

	"github.com/braedonsaunders/ether/internal/handles/capability"
)

// ReceiptCapabilityNamespace is the shared header namespace for experience
// receipts. It is not the ticket namespace and it is not an authority.
const ReceiptCapabilityNamespace = "experience.receipt"

// Lock order for a mirrored issue, all inside one transaction on the receipt
// database:
//
//  1. LockKey("experience:receipts:{viewer}:{device}")
//  2. Delete expired experience_receipts for that viewer and device only
//  3. DropHeadersInTx for those digests, at most HeaderBatchMax per call,
//     grouped by surface
//  4. Count remaining experience_receipts for the quota
//  5. Insert the legacy receipt
//  6. BindHeadersInTx for that one digest
//
// Report locks only "experience:receipt:{hash}" and does not read or write
// headers. BindHeadersInTx, DropHeadersInTx, and PruneHeadersInTx are called
// only when they take no advisory lock. This adapter never calls PruneHeadersInTx
// and never calls the ticket namespace: a namespace-wide prune would touch
// other viewers while this transaction holds only the viewer-device lock.
//
// MirrorHeaders defaults off. Turning it on is not a production cutover.

func receiptExpiresMillis(expiresUnix int64) (int64, error) {
	if expiresUnix <= 0 || expiresUnix > math.MaxInt64/1000 {
		return 0, ErrReceipt
	}
	return expiresUnix * 1000, nil
}

func chunkDigests(in []string, size int) [][]string {
	if size <= 0 || len(in) == 0 {
		return nil
	}
	out := make([][]string, 0, (len(in)+size-1)/size)
	for i := 0; i < len(in); i += size {
		j := i + size
		if j > len(in) {
			j = len(in)
		}
		out = append(out, in[i:j])
	}
	return out
}

func (s *Receipts) bindHeaders(ctx context.Context, tx *sql.Tx, headers []capability.Header) error {
	if s.opts.bindHeaders != nil {
		return s.opts.bindHeaders(ctx, tx, headers)
	}
	return capability.BindHeadersInTx(ctx, tx, headers)
}

func (s *Receipts) dropHeaders(ctx context.Context, tx *sql.Tx, scope capability.HeaderScope, digests []string) error {
	if s.opts.dropHeaders != nil {
		return s.opts.dropHeaders(ctx, tx, scope, digests)
	}
	return capability.DropHeadersInTx(ctx, tx, scope, digests)
}

// sweepExpired removes expired authority rows for the locked viewer and device.
// Header drops name only those digests, so an orphan or foreign header cannot
// change the quota and a drop failure rolls the delete back.
func (s *Receipts) sweepExpired(ctx context.Context, tx *sql.Tx, viewer int64, device string, now int64) error {
	// Recheck outer scope and expiry after any wait on an old writer's row.
	// At most 128 expired authority rows and their mirrors are touched per issue.
	const cleanupMax = 128
	query := `DELETE FROM experience_receipts WHERE viewer_id=? AND device_id=? AND expires<=? AND hash IN (SELECT hash FROM experience_receipts WHERE viewer_id=? AND device_id=? AND expires<=? ORDER BY expires,hash LIMIT ?)`
	args := []any{viewer, device, now, viewer, device, now, cleanupMax}
	if !s.opts.MirrorHeaders {
		_, err := tx.ExecContext(ctx, query, args...)
		return err
	}
	rows, err := tx.QueryContext(ctx, query+` RETURNING hash, protocol`, args...)
	if err != nil {
		return err
	}
	grouped := map[string][]string{}
	for rows.Next() {
		var hash, surface string
		if err = rows.Scan(&hash, &surface); err != nil {
			rows.Close()
			return err
		}
		grouped[surface] = append(grouped[surface], hash)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err = rows.Close(); err != nil {
		return err
	}
	for surface, digests := range grouped {
		for _, part := range chunkDigests(digests, capability.HeaderBatchMax) {
			if err = s.dropHeaders(ctx, tx, capability.HeaderScope{
				Namespace: ReceiptCapabilityNamespace,
				ViewerID:  viewer,
				Surface:   surface,
				DeviceID:  device,
			}, part); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Receipts) mirrorIssued(ctx context.Context, tx *sql.Tx, viewer int64, deviceID, surface, digest string, expiresUnix int64) error {
	if !s.opts.MirrorHeaders {
		return nil
	}
	millis, err := receiptExpiresMillis(expiresUnix)
	if err != nil {
		return err
	}
	return s.bindHeaders(ctx, tx, []capability.Header{{
		HeaderScope: capability.HeaderScope{
			Namespace: ReceiptCapabilityNamespace,
			ViewerID:  viewer,
			Surface:   surface,
			DeviceID:  deviceID,
		},
		Digest:        digest,
		ExpiresMillis: millis,
	}})
}
