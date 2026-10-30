package experience

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/handles/capability"
	"github.com/braedonsaunders/ether/internal/store"
)

// IssueBatch atomically issues a bounded set of receipts in input order. It is
// not yet wired to row responses: response-level policy revalidation and
// stale-row handling must be resolved before adopting it there.
func (s *Receipts) IssueBatch(ctx context.Context, viewer *core.User, device core.Device, protocol core.Protocol, subjects []ReceiptSubject) ([]string, error) {
	return s.issueBatchGuarded(ctx, viewer, device, protocol, subjects, nil)
}

// guard must only use the supplied transaction; it must never call external
// policy callbacks or acquire a second connection while the write lock is held.
// A nil guard leaves caller-specific response freshness outside this method.
func (s *Receipts) issueBatchGuarded(ctx context.Context, viewer *core.User, device core.Device, protocol core.Protocol, subjects []ReceiptSubject, guard func(context.Context, *sql.Tx) error) ([]string, error) {
	if viewer == nil || viewer.ID <= 0 || device.ID == "" || protocol == "" || len(subjects) == 0 || len(subjects) > s.opts.MaxPerDevice {
		return nil, ErrReceipt
	}
	snapshots := make([]string, len(subjects))
	encoded := make(map[*core.NeutralImpression]string)
	for i, subject := range subjects {
		if subject.Kind == "" || subject.ID == "" {
			return nil, ErrReceipt
		}
		if imp := subject.Impression; imp != nil {
			if !snapshotMatches(subject, imp) || len(imp.Items) > 1000 || len(imp.Entries) > 1000 || len(imp.Placement) > 128 || len(imp.ViewID) > 512 || len(imp.RowID) > 512 || len(imp.Kind) > 512 || len(imp.Title) > 2048 {
				return nil, ErrReceipt
			}
			value, ok := encoded[imp]
			if !ok {
				b, err := json.Marshal(imp)
				if err != nil || len(b) > 32768 {
					return nil, ErrReceipt
				}
				value = string(b)
				encoded[imp] = value
			}
			snapshots[i] = value
		}
	}
	active, err := s.opts.Active(ctx, viewer.ID)
	if err != nil || active == nil || active.ID != viewer.ID {
		return nil, ErrReceipt
	}
	for _, subject := range subjects {
		if !s.opts.Allowed(ctx, active, device, protocol, subject) {
			return nil, ErrReceipt
		}
	}
	var generation int64
	if s.opts.Generation != nil {
		generation, err = s.opts.Generation(ctx, active, device, protocol)
		if err != nil || generation < 0 {
			return nil, ErrReceipt
		}
	}
	tx, err := s.opts.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err = store.LockKey(ctx, store.DialectOf(s.opts.DB), tx, fmt.Sprintf("experience:receipts:%d:%s", viewer.ID, device.ID)); err != nil {
		return nil, err
	}
	now := s.opts.Now().Unix()
	if err = s.sweepExpired(ctx, tx, viewer.ID, device.ID, now); err != nil {
		return nil, err
	}
	var count int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM experience_receipts WHERE viewer_id=? AND device_id=?`, viewer.ID, device.ID).Scan(&count); err != nil {
		return nil, err
	}
	if count+len(subjects) > s.opts.MaxPerDevice {
		return nil, errors.New("experience: receipt limit reached")
	}
	tokens := make([]string, 0, len(subjects))
	headers := make([]capability.Header, 0, min(len(subjects), capability.HeaderBatchMax))
	expires := s.opts.Now().Add(s.opts.TTL).Unix()
	for i, subject := range subjects {
		var token, hash string
		inserted := false
		for attempt := 0; attempt < 3; attempt++ {
			token, hash, err = s.codec.Issue()
			if err != nil {
				return nil, err
			}
			var resCount int64
			res, e := tx.ExecContext(ctx, `INSERT INTO experience_receipts(hash,viewer_id,device_id,protocol,expires,kind,subject,purpose,rank,impression,generation) VALUES(?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(hash) DO NOTHING`, hash, viewer.ID, device.ID, string(protocol), expires, subject.Kind, subject.ID, string(subject.Purpose), subject.Rank, snapshots[i], generation)
			if e != nil {
				return nil, e
			}
			resCount, e = res.RowsAffected()
			if e != nil {
				return nil, e
			}
			if resCount == 1 {
				inserted = true
				break
			}
		}
		if !inserted {
			return nil, errors.New("experience: receipt collision")
		}
		tokens = append(tokens, token)
		if s.opts.MirrorHeaders {
			millis, e := receiptExpiresMillis(expires)
			if e != nil {
				return nil, e
			}
			headers = append(headers, capability.Header{HeaderScope: capability.HeaderScope{Namespace: ReceiptCapabilityNamespace, ViewerID: viewer.ID, Surface: string(protocol), DeviceID: device.ID}, Digest: hash, ExpiresMillis: millis})
			if len(headers) == capability.HeaderBatchMax {
				if err = s.bindHeaders(ctx, tx, headers); err != nil {
					return nil, err
				}
				headers = headers[:0]
			}
		}
	}
	if len(headers) > 0 {
		if err = s.bindHeaders(ctx, tx, headers); err != nil {
			return nil, err
		}
	}
	if guard != nil {
		if err = guard(ctx, tx); err != nil {
			return nil, err
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return tokens, nil
}

// guardRowViewSnapshot is only the database portion of row-view freshness.
// It does not validate current viewer policy or entry visibility and therefore
// must not be used alone to authorize or publish a response.
func guardRowViewSnapshot(ctx context.Context, tx *sql.Tx, db *sql.DB, id string, viewerID int64, deviceID string, protocol core.Protocol, nowMillis int64) error {
	if tx == nil || db == nil || id == "" || viewerID <= 0 || deviceID == "" || protocol == "" {
		return ErrRowViewStale
	}
	query := `SELECT snapshot,expires FROM experience_row_views WHERE snapshot=? AND viewer_id=? AND device_id=? AND protocol=?`
	if store.DialectOf(db) == store.Postgres {
		query += ` FOR UPDATE`
	}
	var got string
	var expiry int64
	if err := tx.QueryRowContext(ctx, query, id, viewerID, deviceID, string(protocol)).Scan(&got, &expiry); err != nil || got != id || expiry <= nowMillis {
		return ErrRowViewStale
	}
	return nil
}
