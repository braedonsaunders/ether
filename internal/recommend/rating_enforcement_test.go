package recommend

import (
	"testing"

	"github.com/braedonsaunders/ether/internal/contentrating"
	"github.com/braedonsaunders/ether/internal/core"
)

func TestFilterTitlesEnforcesDualCutAndUncertifiedAdult(t *testing.T) {
	titles := []core.Title{
		{Key: core.MovieKey(1), Title: "Allowed", ContentRating: "PG-13", EnforcementRating: "PG-13"},
		{Key: core.MovieKey(2), Title: "DualCut", ContentRating: "PG-13", EnforcementRating: "R"},
		{Key: core.MovieKey(3), Title: "AdultFlag", ContentRating: "PG", EnforcementRating: contentrating.UncertifiedAdult},
	}
	viewer := &core.User{ID: 1, MaxContentRating: "PG-13"}
	got := FilterTitles(titles, viewer)
	if len(got) != 1 || got[0].Key != core.MovieKey(1) {
		t.Fatalf("FilterTitles returned %+v, want only movie 1", got)
	}
}
