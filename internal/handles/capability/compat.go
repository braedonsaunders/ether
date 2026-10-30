package capability

import (
	"context"
	"database/sql"
	"errors"
)

// LegacyTicketConsumer is the existing scoped, single-use legacy operation.
// Its authorization and atomicity remain the adapter's responsibility.
type LegacyTicketConsumer func(context.Context, string, string, string) (Ticket, error)

// ConsumeCompatible only falls back when the new header is absent. A present
// but foreign, expired, revoked, or malformed header is never treated as a
// legacy miss. Callers must avoid duplicate handles across the two stores;
// mixed-version writes and quota enforcement require a separate node fence.
func (s *Tickets) ConsumeCompatible(ctx context.Context, surface, purpose, digest string, legacy LegacyTicketConsumer) (Ticket, error) {
	out, err := s.Consume(ctx, surface, purpose, digest)
	if !errors.Is(err, sql.ErrNoRows) || legacy == nil {
		return out, err
	}
	return legacy(ctx, surface, purpose, digest)
}
