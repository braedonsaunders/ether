package playback

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestExactVariantNeverSubstitutes(t *testing.T) {
	ctx := context.Background()
	t.Run("missing selection", func(t *testing.T) {
		h := newHarness(t, []*fakeRelease{mkv("A", 10000, time.Minute)}, []string{"A"})
		if _, err := h.m.PrepareExactVariant(ctx, owner, movieKey, defaultCaps("exact"), "B"); !errors.Is(err, core.ErrNotAvailable) {
			t.Fatalf("missing exact selection: %v", err)
		}
		if len(h.m.Sessions()) != 0 {
			t.Fatal("fallback session was installed")
		}
	})
	t.Run("unhealthy selection", func(t *testing.T) {
		broken := mkv("B", 10000, time.Minute)
		broken.setFail(0, broken.Size, nil)
		h := newHarness(t, []*fakeRelease{mkv("A", 10000, time.Minute), broken}, []string{"A", "B"})
		if _, err := h.m.PrepareExactVariant(ctx, owner, movieKey, defaultCaps("exact"), "B"); !errors.Is(err, core.ErrNotAvailable) {
			t.Fatalf("broken exact selection: %v", err)
		}
		if len(h.m.Sessions()) != 0 {
			t.Fatal("unhealthy exact selection fell back")
		}
	})
	t.Run("failover and reuse", func(t *testing.T) {
		h := newHarness(t, []*fakeRelease{mkv("A", 10000, time.Minute), mkv("B", 20000, time.Minute)}, []string{"A", "B"})
		caps := defaultCaps("exact")
		old, err := h.m.PrepareVariant(ctx, owner, movieKey, caps, "B")
		if err != nil {
			t.Fatal(err)
		}
		exact, err := h.m.PrepareExactVariant(ctx, owner, movieKey, caps, "B")
		if err != nil {
			t.Fatal(err)
		}
		if exact.ID == old.ID || exact.Resolution.Release.ID != "B" {
			t.Fatal("exact selection reused a fallback-enabled session")
		}
		if _, err = h.m.Failover(ctx, exact.ID, 0); !errors.Is(err, core.ErrNotAvailable) {
			t.Fatalf("exact failover: %v", err)
		}
		current, ok := h.m.Session(exact.ID)
		if !ok || current.Resolution.Release.ID != "B" {
			t.Fatalf("exact session changed source: %+v", current)
		}
	})
}

func TestConcurrentExactFlightNeverInheritsFallbackSession(t *testing.T) {
	for i := 0; i < 5; i++ {
		h := newHarness(t, []*fakeRelease{mkv("A", 10000, time.Minute)}, []string{"A"})
		h.resolver.delay = 30 * time.Millisecond
		caps := defaultCaps("same")
		start := make(chan struct{})
		exactResult := make(chan *core.PlaySession, 1)
		errs := make(chan error, 1)
		normalDone := make(chan struct{})
		go func() {
			<-start
			ps, err := h.m.PrepareExactVariant(context.Background(), owner, movieKey, caps, "A")
			exactResult <- ps
			errs <- err
		}()
		go func() {
			defer close(normalDone)
			<-start
			_, _ = h.m.PrepareVariant(context.Background(), owner, movieKey, caps, "A")
		}()
		close(start)
		ps, err := <-exactResult, <-errs
		<-normalDone
		if err == nil {
			if ps == nil {
				t.Fatal("nil exact session")
			}
			s := h.m.lookup(ps.ID)
			if s == nil || !s.exact {
				t.Fatal("exact caller inherited fallback-enabled flight")
			}
		}
	}
}
