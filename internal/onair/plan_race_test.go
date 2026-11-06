package onair

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

// Plans run concurrently (prewarm, Ensure, playout) against the same stored
// style while the owner saves it. Planning must never write into the stored
// scenes, and the plan for an unchanged request must hash identically.
// Run with -race to catch shared-slice writes.
func TestConcurrentPlansDoNotMutateStoredStyle(t *testing.T) {
	s := testService(t, Options{})
	ctx := context.Background()
	l, err := s.Look(ctx, "neon")
	if err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(l.Scenes)
	r := core.ClipRequest{LookID: l.ID, Moment: "ident", ChannelID: "c1", Bind: map[string]string{"channel.name": "One", "slate.start": "60"}}
	first, ok := s.plan(r)
	if !ok {
		t.Fatal("ident did not plan")
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 25; n++ {
				if j, ok := s.plan(r); !ok || j.Hash != first.Hash {
					t.Errorf("unstable plan hash")
					return
				}
				s.Ensure(ctx, r)
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for n := 0; n < 5; n++ {
			if _, err := s.SaveLook(ctx, l); err != nil {
				t.Errorf("save: %v", err)
				return
			}
		}
	}()
	wg.Wait()
	s.mu.Lock()
	stored := s.looks[l.ID]
	s.mu.Unlock()
	after, _ := json.Marshal(stored.Scenes)
	if string(before) != string(after) {
		t.Fatal("planning mutated the stored style's scenes")
	}
}
