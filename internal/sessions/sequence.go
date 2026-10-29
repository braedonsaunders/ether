package sessions

import (
	"context"
	"fmt"
)

func (b *Broker) acceptSequence(ctx context.Context, s *brokerSession, sequence uint64) (bool, error) {
	if sequence == 0 {
		return true, nil
	}
	if b.cluster != nil && s.public.Protocol != "plex" {
		// Fixed-width decimal preserves the entire uint64 range and sorts
		// numerically in both SQLite and PostgreSQL text comparisons.
		encoded := fmt.Sprintf("%020d", sequence)
		result, err := b.cluster.store.db.ExecContext(ctx, `UPDATE plex_sessions SET report_sequence=? WHERE id=? AND node_id=? AND COALESCE(protocol,'plex')<>'plex' AND (report_sequence IS NULL OR report_sequence<?)`, encoded, s.public.ID, b.cluster.store.node, encoded)
		if err != nil {
			return false, err
		}
		n, err := result.RowsAffected()
		return n == 1, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	current := b.sessions[s.public.ID]
	if current == nil || sequence <= current.sequence {
		return false, nil
	}
	current.sequence = sequence
	return true, nil
}
