package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/handles/capability"
)

func TestHeaderMaintenanceRotatesAfterFailureAndLeadership(t *testing.T) {
	var sweep headerMaintenance
	now := time.Unix(123, 0)
	var seen []int64
	failure := errors.New("transient")
	prune := func(ctx context.Context, got time.Time, shard int64) error {
		if got != now {
			t.Fatalf("clock: %v", got)
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("missing deadline")
		}
		seen = append(seen, shard)
		if shard == 0 && len(seen) == 1 {
			return failure
		}
		return nil
	}
	clock := func() time.Time { return now }
	if err := sweep.pass(context.Background(), false, clock, prune); err != nil || len(seen) != 0 {
		t.Fatalf("nonleader: %v %v", seen, err)
	}
	if err := sweep.pass(context.Background(), true, clock, prune); !errors.Is(err, failure) {
		t.Fatalf("failure: %v", err)
	}
	for i := int64(1); i <= capability.HeaderShardCount; i++ {
		if err := sweep.pass(context.Background(), true, clock, prune); err != nil {
			t.Fatal(err)
		}
	}
	if len(seen) != int(capability.HeaderShardCount)+1 || seen[1] != 1 || seen[len(seen)-1] != 0 {
		t.Fatalf("rotation: %v", seen)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sweep.pass(ctx, true, clock, prune); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
}
