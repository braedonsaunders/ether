package playback

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// Tail metadata reads map to duration, but a channel has its own programme
// clock and no HLS swap target. Only that clock may replace its decoder.
func TestChannelTailProbeDoesNotOwnRecovery(t *testing.T) {
	h := newHarness(t, []*fakeRelease{mkv("A", 100_000, 100*time.Second), mkv("B", 100_000, 100*time.Second)}, []string{"A", "B"})
	ps := h.prepare(owner, movieKey, "channel")
	s := h.m.lookup(ps.ID)
	s.caps.BroadcastBitrateKbps = 6000
	g := s.current()
	h.m.backgroundFailover(s, g, 100*time.Second)
	if g.backgroundFailover.Load() || s.current() != g {
		t.Fatal("tail metadata read initiated channel recovery")
	}
}

type deadlineChannelResolver struct {
	core.Resolver
	canceled chan struct{}
}

func (r *deadlineChannelResolver) Resolve(ctx context.Context, _ core.ResolveRequest) (*core.Resolution, error) {
	<-ctx.Done()
	close(r.canceled)
	return nil, ctx.Err()
}
func TestChannelColdAdmission(t *testing.T) {
	t.Run("stored and remembered remux obey internal channel budget", func(t *testing.T) {
		h := newHarness(t, []*fakeRelease{mkv("REMUX", 200_000_000, 100*time.Second)}, []string{"REMUX"})
		ps := h.prepare(owner, movieKey, "ordinary")
		h.m.Stop(ps.ID)
		caps := defaultCaps("channel")
		caps.BroadcastBitrateKbps = 6000
		profile := h.m.selectProfile(caps, owner, movieKey)
		if got := h.m.resumeResolution(context.Background(), owner.ID, movieKey, profile, caps, ""); got != nil {
			t.Fatal("remembered remux bypassed channel budget")
		}
		h.m.resume.freshMu.Lock()
		clear(h.m.resume.live)
		h.m.resume.freshMu.Unlock()
		if got := h.m.resumeResolution(context.Background(), owner.ID, movieKey, profile, caps, ""); got != nil {
			t.Fatal("stored remux bypassed channel budget")
		}
	})
	t.Run("deadline cancels channel flight without player grace", func(t *testing.T) {
		h := newHarness(t, []*fakeRelease{mkv("A", 100_000, 100*time.Second)}, []string{"A"})
		r := &deadlineChannelResolver{Resolver: h.resolver, canceled: make(chan struct{})}
		h.m.deps.Resolver = r
		caps := defaultCaps("cold-source")
		caps.BroadcastBitrateKbps = 6000
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		if _, err := h.m.PrepareChannel(ctx, owner, movieKey, caps, "broadcast-cold-source"); err == nil || !strings.Contains(err.Error(), "deadline") {
			t.Fatalf("prepare err=%v", err)
		}
		select {
		case <-r.canceled:
		case <-time.After(time.Second):
			t.Fatal("channel flight survived deadline under player grace")
		}
	})
}
