package sessions

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// acceptSequence reserves a monotonic sequence. Its rollback is conditional:
// a later accepted value can never be overwritten after a progress failure.
func (b *Broker) acceptSequence(ctx context.Context, s *brokerSession, sequence uint64) (bool, func(), error) {
	noop := func() {}
	if sequence == 0 {
		return true, noop, nil
	}
	if b.cluster != nil && s.public.Protocol != "plex" {
		encoded := fmt.Sprintf("%020d", sequence)
		for attempts := 0; attempts < 16; attempts++ {
			var old sql.NullString
			err := b.cluster.store.db.QueryRowContext(ctx, `SELECT report_sequence FROM plex_sessions WHERE id=? AND node_id=? AND COALESCE(protocol,'plex')<>'plex'`, s.public.ID, b.cluster.store.node).Scan(&old)
			if err != nil {
				return false, noop, err
			}
			if old.Valid && old.String >= encoded {
				return false, noop, nil
			}
			var previous any
			if old.Valid {
				previous = old.String
			}
			result, err := b.cluster.store.db.ExecContext(ctx, `UPDATE plex_sessions SET report_sequence=? WHERE id=? AND node_id=? AND COALESCE(protocol,'plex')<>'plex' AND (report_sequence=? OR (report_sequence IS NULL AND CAST(? AS TEXT) IS NULL))`, encoded, s.public.ID, b.cluster.store.node, previous, previous)
			if err != nil {
				return false, noop, err
			}
			n, err := result.RowsAffected()
			if err != nil {
				return false, noop, err
			}
			if n == 0 {
				continue
			}
			rollback := func() {
				rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
				defer cancel()
				_, err := b.cluster.store.db.ExecContext(rctx, `UPDATE plex_sessions SET report_sequence=? WHERE id=? AND node_id=? AND report_sequence=?`, previous, s.public.ID, b.cluster.store.node, encoded)
				if err != nil && b.deps.OnEventError != nil {
					b.deps.OnEventError(err)
				}
			}
			return true, rollback, nil
		}
		return false, noop, fmt.Errorf("sessions: report sequence contention")
	}
	b.mu.Lock()
	current := b.sessions[s.public.ID]
	if current == nil || sequence <= current.sequence {
		b.mu.Unlock()
		return false, noop, nil
	}
	old := current.sequence
	current.sequence = sequence
	b.mu.Unlock()
	rollback := func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if current := b.sessions[s.public.ID]; current != nil && current.sequence == sequence {
			current.sequence = old
		}
	}
	return true, rollback, nil
}
