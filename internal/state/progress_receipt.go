package state

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

type progressReceiptKey struct{}

// ProgressWithReceipt commits receipt, watch state and history together. The
// callback may run again after a row conflict and must use only the supplied
// transaction. This synchronous path never acknowledges an outbox-only write.
func (s *State) ProgressWithReceipt(ctx context.Context, user int64, key core.RatingKey, position, duration time.Duration, status string, receipt func(*sql.Tx) error) error {
	if receipt == nil {
		return errors.New("state: missing progress receipt")
	}
	if position < 0 {
		position = 0
	}
	ctx = context.WithValue(ctx, progressReceiptKey{}, receipt)
	return s.progressAt(ctx, user, key, progressWrite{offset: position, duration: duration, state: status, origin: originFrom(ctx)})
}
