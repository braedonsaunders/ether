package app

import (
	"context"
	"time"

	"github.com/braedonsaunders/ether/internal/handles/capability"
)

// headerMaintenance rotates even on error so one blocked shard cannot starve
// all others. The cursor is deliberately local; a new leader starts at zero.
// An unsuccessful shard is retried on the next rotation, not assumed clean.
type headerMaintenance struct{ next int64 }

type headerShardPruner func(context.Context, time.Time, int64) error

func (m *headerMaintenance) pass(ctx context.Context, leader bool, now func() time.Time, prune headerShardPruner) error {
	if !leader || ctx.Err() != nil {
		return ctx.Err()
	}
	shard := m.next
	m.next = (m.next + 1) % capability.HeaderShardCount
	deadline, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return prune(deadline, now(), shard)
}
