package plexwatch

import (
	"context"

	"github.com/braedonsaunders/ether/internal/state"
)

// completeSend records acceptance, import echo and acknowledgement as one
// transaction. Until commit is known to succeed the immutable admission keeps
// conflicting writes fenced, even if the original journal row was superseded,
// reclaimed, cancelled or compacted while the request was in flight.
func (s *Service) completeSend(ctx context.Context, row state.SyncRow, id, guid, kind string, at int64, rating int) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO plexwatch_sent(user_id,guid,kind,at_ms,rating,sent_at) VALUES(?,?,?,?,?,?) ON CONFLICT DO NOTHING`, row.UserID, guid, kind, at, rating, s.now().UnixNano()); err != nil {
		return err
	}
	if s.receiptFault != nil {
		if err := s.receiptFault("echo"); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE state_sync_journal SET delivered_at=?,claimed_by='',claimed_at=0,dead=0,last_error='' WHERE seq=?`, s.now().UnixNano(), row.Seq); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE plexwatch_attempt SET receipt_at=? WHERE attempt_id=?`, s.now().UnixNano(), id); err != nil {
		return err
	}
	if s.receiptFault != nil {
		if err := s.receiptFault("commit"); err != nil {
			return err
		}
	}
	if s.receiptCommit != nil {
		return s.receiptCommit(tx)
	}
	return tx.Commit()
}
