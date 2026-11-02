package playback

import (
	"context"
	"testing"
	"time"

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
