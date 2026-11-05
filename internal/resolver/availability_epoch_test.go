package resolver

import (
	"context"
	"testing"
	"time"
)

func TestAvailabilityEpochTracksFactsAndExpiry(t *testing.T) {
	now := time.Unix(1000, 0)
	s := &Service{now: func() time.Time { return now }, avail: map[int64]availEntry{}}
	s.mu.Lock()
	s.trackAvailabilityLocked(1, now.Add(time.Minute))
	s.trackAvailabilityLocked(1, now.Add(2*time.Minute))
	s.mu.Unlock()
	if got := s.AvailabilityEpoch(); got != 0 {
		t.Fatalf("hydration changed epoch: %d", got)
	}
	if len(s.availabilityDeadlines) != 1 {
		t.Fatal("refresh accumulated deadline copies")
	}
	now = now.Add(time.Minute + time.Second)
	if got := s.AvailabilityEpoch(); got != 0 {
		t.Fatal("superseded expiry changed epoch")
	}
	now = now.Add(time.Minute)
	if got := s.AvailabilityEpoch(); got != 1 {
		t.Fatalf("expiry did not change epoch: %d", got)
	}
	if got := s.AvailabilityEpoch(); got != 1 {
		t.Fatal("expired fact invalidated twice")
	}
	s.mu.Lock()
	s.forgetAvailabilityEpochLocked(1)
	s.mu.Unlock()
	if got := s.AvailabilityEpoch(); got != 2 {
		t.Fatal("forget did not change epoch")
	}
	// A simultaneous expiry has bounded work even for a large fact set.
	s.mu.Lock()
	for key := int64(0); key < 1000; key++ {
		s.trackAvailabilityLocked(key, now.Add(-time.Second))
	}
	s.mu.Unlock()
	s.AvailabilityEpoch()
	if got := len(s.availabilityDeadlines); got != 1000-64 {
		t.Fatalf("expiry batch unbounded: %d", got)
	}
	s.mu.Lock()
	s.clearAvailabilityEpochLocked()
	s.mu.Unlock()
	if len(s.availabilityDeadlines) != 0 || len(s.availabilityDeadlineByKey) != 0 {
		t.Fatal("clear retained deadlines")
	}
}

func TestAvailabilityEpochIgnoresEquivalentRefresh(t *testing.T) {
	now := time.Unix(1000, 0)
	s := &Service{now: func() time.Time { return now }, avail: map[int64]availEntry{}}
	s.setAvailability(context.Background(), 1, false, time.Hour)
	epoch := s.AvailabilityEpoch()
	now = now.Add(time.Minute)
	s.setAvailability(context.Background(), 1, false, time.Hour)
	if s.AvailabilityEpoch() != epoch {
		t.Fatal("equivalent refresh rebuilt availability consumers")
	}
	s.setAvailability(context.Background(), 1, true, time.Hour)
	if s.AvailabilityEpoch() == epoch {
		t.Fatal("changed fact did not invalidate consumers")
	}
}
