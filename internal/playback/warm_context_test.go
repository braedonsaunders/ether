package playback

import (
	"context"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

func TestPlayPromotesInFlightWarmPastSpeculativeBudget(t *testing.T) {
	parent, shutdown := context.WithCancel(context.Background())
	defer shutdown()
	live, stopPlay := context.WithTimeout(parent, 2*time.Second)
	defer stopPlay()
	foreground := make(chan context.Context, 1)
	// The actual Play flight is already waiting when warm work starts.
	foreground <- live
	ctx, cancel := warmPreparationContext(parent, 20*time.Millisecond, foreground)
	defer cancel()
	// A probe completing after the speculative limit should still be usable.
	select {
	case <-ctx.Done():
		t.Fatal("Play lost its in-flight warm work to the speculative timeout")
	case <-time.After(80 * time.Millisecond):
	}
	stopPlay()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("abandoned foreground work kept running")
	}
}

func TestUnclaimedWarmStillExpiresAndShutdownCancelsPromotedWarm(t *testing.T) {
	ctx, cancel := warmPreparationContext(t.Context(), 20*time.Millisecond, nil)
	defer cancel()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("speculative work outlived its budget")
	}
	parent, shutdown := context.WithCancel(context.Background())
	foreground := make(chan context.Context, 1)
	foreground <- context.Background()
	promoted, stop := warmPreparationContext(parent, time.Minute, foreground)
	defer stop()
	shutdown()
	select {
	case <-promoted.Done():
	case <-time.After(time.Second):
		t.Fatal("shutdown did not cancel promoted work")
	}
}

// A warm-up runs as speculative work until a Play takes it over; from then
// on the resolver treats it as a real play (#224).
func TestWarmPreparationIsSpeculativeUntilPromoted(t *testing.T) {
	foreground := make(chan context.Context, 1)
	ctx, cancel := warmPreparationContext(context.Background(), time.Minute, foreground)
	defer cancel()
	if !core.IsSpeculative(ctx) {
		t.Fatal("a warm-up is not marked speculative")
	}
	live, stop := context.WithCancel(context.Background())
	defer stop()
	foreground <- live
	deadline := time.Now().Add(5 * time.Second)
	for core.IsSpeculative(ctx) {
		if time.Now().After(deadline) {
			t.Fatal("a promoted warm-up is still speculative")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if core.IsSpeculative(context.Background()) {
		t.Fatal("an ordinary context is speculative")
	}
}

type spySpeculativeResolver struct {
	core.Resolver
	speculative bool
	purpose     string
	remaining   time.Duration
}

func (s *spySpeculativeResolver) Resolve(ctx context.Context, req core.ResolveRequest) (*core.Resolution, error) {
	s.speculative = core.IsSpeculative(ctx) && core.SpeculativeIndexerWork(ctx)
	s.purpose = core.IndexerPurpose(ctx)
	if deadline, ok := ctx.Deadline(); ok {
		s.remaining = time.Until(deadline)
	}
	return s.Resolver.Resolve(ctx, req)
}

func TestCheckFileAvailabilityUsesUpdatedTimeout(t *testing.T) {
	h := newHarness(t, []*fakeRelease{{ID: "r1", Name: "Film.mkv", Size: 1 << 20}}, []string{"r1"})
	defer h.m.Close()
	spy := &spySpeculativeResolver{Resolver: h.resolver}
	h.m.deps.Resolver = spy
	for _, budget := range []time.Duration{13 * time.Second, 300 * time.Millisecond} {
		cfg := h.m.config()
		cfg.Plex.FileCheckTimeout = config.Duration(budget)
		h.m.SetConfig(cfg)
		live, err := h.m.CheckFileAvailability(t.Context(), owner, movieKey, defaultCaps("plex-detail"), "r1")
		if err != nil || !live {
			t.Fatalf("check with %v = (%v, %v)", budget, live, err)
		}
		if spy.remaining <= 0 || spy.remaining > budget || spy.remaining < budget-100*time.Millisecond {
			t.Fatalf("resolver allowance = %v, want approximately %v", spy.remaining, budget)
		}
	}
}

func TestCheckFileAvailabilityRunsAsSpeculativeWarmup(t *testing.T) {
	h := newHarness(t, []*fakeRelease{{ID: "r1", Name: "Film.mkv", Size: 1 << 20}}, []string{"r1"})
	defer h.m.Close()
	spy := &spySpeculativeResolver{Resolver: h.resolver}
	h.m.deps.Resolver = spy
	live, err := h.m.CheckFileAvailability(context.Background(), owner, movieKey, defaultCaps("plex-detail"), "r1")
	if err != nil || !live {
		t.Fatalf("CheckFileAvailability = (%v, %v), want (true, nil)", live, err)
	}
	if !spy.speculative || spy.purpose != core.IndexerPurposeWarmup {
		t.Fatalf("CheckFileAvailability context: speculative=%v purpose=%q, want true / %q", spy.speculative, spy.purpose, core.IndexerPurposeWarmup)
	}
}
