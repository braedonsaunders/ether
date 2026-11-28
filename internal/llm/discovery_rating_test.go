package llm

import (
	"testing"

	"github.com/braedonsaunders/ether/internal/contentrating"
	"github.com/braedonsaunders/ether/internal/core"
)

func TestDiscoverySafeEnforcesEnforcementRating(t *testing.T) {
	viewer := &core.User{ID: 1, MaxContentRating: "PG-13"}
	titles := []core.Title{
		{Key: core.MovieKey(1), Title: "Safe", ContentRating: "PG-13", EnforcementRating: "PG-13"},
		{Key: core.MovieKey(2), Title: "DualCut", ContentRating: "PG-13", EnforcementRating: "R"},
		{Key: core.MovieKey(3), Title: "Adult", ContentRating: "PG", EnforcementRating: contentrating.UncertifiedAdult},
	}
	got := discoverySafe(viewer, titles)
	if len(got) != 1 || got[0].Key != core.MovieKey(1) {
		t.Fatalf("discoverySafe returned %+v, want only movie 1", got)
	}
}
