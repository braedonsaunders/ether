package livetv

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/store"
)

// Both clock and timer spies stay silent after construction while disabled.
// This guards all worker families at their single lifetime admission point.
func TestChannelExistenceControlsBackgroundWork(t *testing.T) {
	db, err := store.OpenMemory(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var clocks, timers atomic.Int64
	s, err := New(Options{DB: db, Showcase: true, Now: func() time.Time { clocks.Add(1); return time.Unix(0, 0) }, NewTimer: func(d time.Duration) *time.Timer { timers.Add(1); return time.NewTimer(d) }, NewTicker: func(d time.Duration) *time.Ticker { timers.Add(1); return time.NewTicker(d) }})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	initial := clocks.Load()
	s.ReconcileEnabled()
	if err := s.RefreshGuide(t.Context()); err != nil {
		t.Fatal(err)
	}
	if timers.Load() != 0 || clocks.Load() != initial || s.running {
		t.Fatal("disabled service admitted background work")
	}
	if s.VisibleTo(t.Context(), nil) {
		t.Fatal("empty service visible")
	}
	// Disabled channels still make Live TV active; individual visibility is
	// a separate policy. The first save starts workers, the last delete drains
	// them, and a subsequent save can restart their lifetime.
	for range 2 {
		_, err := s.SaveChannel(context.Background(), Channel{ID: "exists", Name: "Saved channel", Number: "1", Kind: "virtual", Enabled: false, OffAir: true})
		if err != nil {
			t.Fatal(err)
		}
		if !s.Enabled() {
			t.Fatal("saved channel did not activate Live TV")
		}
		waitRunning := func(want bool) {
			t.Helper()
			deadline := time.Now().Add(3 * time.Second)
			for {
				s.lifecycleMu.Lock()
				running := s.running
				s.lifecycleMu.Unlock()
				if running == want {
					return
				}
				if time.Now().After(deadline) {
					t.Fatalf("worker lifetime did not reach %v", want)
				}
				time.Sleep(time.Millisecond)
			}
		}
		waitRunning(true)
		s.virtualGuide.mu.Lock()
		s.virtualGuide.lastDemand = time.Unix(0, 0)
		s.virtualGuide.mu.Unlock()
		if err := s.DeleteChannel(context.Background(), "exists"); err != nil {
			t.Fatal(err)
		}
		if s.Enabled() {
			t.Fatal("last deletion left Live TV active")
		}
		waitRunning(false)
		before := clocks.Load()
		s.ReconcileEnabled()
		if clocks.Load() != before {
			t.Fatal("empty service ran clock work")
		}
	}
}
