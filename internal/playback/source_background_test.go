package playback

import (
	"context"
	"testing"
	"time"
)

func TestBackgroundSourceHoldIsBoundedAndRevocable(t *testing.T) {
	for _, end := range []string{"release", "cancel", "deadline", "stop", "viewer"} {
		t.Run(end, func(t *testing.T) {
			h, ps, outcome := inputHarness(t)
			ctx, cancel := context.WithTimeout(t.Context(), time.Hour)
			defer cancel()
			if _, err := h.m.HoldBackgroundSource(context.Background(), ps.ID, outcome); err == nil {
				t.Fatal("unbounded job was admitted")
			}
			release, err := h.m.HoldBackgroundSource(ctx, ps.ID, outcome)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			if _, err := h.m.HoldBackgroundSource(ctx, ps.ID, outcome); err == nil {
				t.Fatal("duplicate hold was admitted")
			}
			s := h.m.lookup(ps.ID)
			s.mu.Lock()
			lastActive, lastRead := s.lastActive, s.lastRead
			s.mu.Unlock()
			h.clock.Advance(3 * time.Minute)
			h.m.expire()
			if _, ok := h.m.Session(ps.ID); !ok {
				t.Fatal("held source was reaped by viewer idle timer")
			}
			s.mu.Lock()
			changed := !s.lastActive.Equal(lastActive) || !s.lastRead.Equal(lastRead)
			s.mu.Unlock()
			if changed {
				t.Fatal("hold changed viewer activity")
			}
			switch end {
			case "release":
				release()
				release()
			case "cancel":
				cancel()
			case "deadline":
				h.clock.Advance(maxBackgroundSourceHold)
			case "stop":
				h.m.Stop(ps.ID)
			case "viewer":
				h.m.MarkSurfaceOpened(ps.ID)
				h.clock.Advance(3 * time.Minute)
			}
			h.m.expire()
			if _, ok := h.m.Session(ps.ID); ok {
				t.Fatal("job hold retained a released, cancelled, expired, stopped or viewer source")
			}
		})
	}
}
