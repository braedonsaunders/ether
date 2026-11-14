package sessions

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

// A stale node can still hold the old decision in memory. Both its stop and
// its refresh must preserve the newer generation committed by another node.
func TestDecisionGenerationAcrossNodes(t *testing.T) {
	db := store.OpenTest(t)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	defer func() { cancel(); wg.Wait() }()
	now := time.Now()
	clock := func() time.Time { return now }
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := NewDecisionArms(ctx, db, clock, "a", logger, &wg)
	b := NewDecisionArms(ctx, db, clock, "b", logger, &wg)
	old := DecisionOwner{ID: "public-player", UserID: 1, ClientID: "tv", SessionID: "player", Generation: "old-encoder"}
	fresh := old
	fresh.Generation = "new-encoder"
	key := core.MovieKey(1)
	a.Arm(old, key)
	b.Arm(fresh, key)
	if a.Current(old, key) || !a.Current(fresh, key) {
		t.Fatal("stale node failed to read committed generation")
	}
	a.Disarm(old, key)
	a.Refresh(now)
	a.ClearOwn() // A stale refresh must not transfer ownership of the new row.
	if !a.Armed(fresh, key) || !b.Armed(fresh, key) || !a.Current(fresh, key) {
		t.Fatal("old stop or refresh revoked new generation")
	}
	b.Disarm(fresh, key)
	a.Refresh(now)
	b.Refresh(now)
	if a.Armed(fresh, key) || b.Armed(fresh, key) {
		t.Fatal("current stop did not revoke its generation")
	}
	if !a.Armed(fresh, 0) {
		t.Fatal("stop erased the decision-seen latch")
	}
}
