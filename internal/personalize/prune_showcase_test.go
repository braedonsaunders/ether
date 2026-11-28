package personalize

import (
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/contentrating"
	"github.com/braedonsaunders/ether/internal/core"
)

func TestSharedShowcaseCacheHonorsEpochAndTTL(t *testing.T) {
	e := &Engine{}
	now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	input := map[string][]core.Title{
		"Drama": {{Key: core.MovieKey(1), Title: "Drama One"}},
	}
	e.sharedShowcasePut(7, now, input)

	got, ok := e.sharedShowcaseGet(7, now.Add(time.Minute))
	if !ok || len(got["Drama"]) != 1 || got["Drama"][0].Key != core.MovieKey(1) {
		t.Fatalf("cache hit = %v %+v", ok, got)
	}
	if _, ok := e.sharedShowcaseGet(8, now.Add(time.Minute)); ok {
		t.Fatal("stale policy epoch should miss")
	}
	if _, ok := e.sharedShowcaseGet(7, now.Add(sharedBrowseTTL+time.Second)); ok {
		t.Fatal("expired showcase cache should miss")
	}
}

func TestTitleAllowedByRatingUsesEnforcementRating(t *testing.T) {
	dualCut := core.Title{Key: core.MovieKey(2), ContentRating: "PG-13", EnforcementRating: "R"}
	if titleAllowedByRating(&dualCut, "PG-13") {
		t.Fatal("dual-cut PG-13/R should be blocked for PG-13 viewer")
	}
	adult := core.Title{Key: core.MovieKey(3), ContentRating: "PG", EnforcementRating: contentrating.UncertifiedAdult}
	if titleAllowedByRating(&adult, "PG-13") {
		t.Fatal("UncertifiedAdult should be blocked for PG-13 viewer")
	}
	safe := core.Title{Key: core.MovieKey(1), ContentRating: "PG-13", EnforcementRating: "PG-13"}
	if !titleAllowedByRating(&safe, "PG-13") {
		t.Fatal("PG-13/PG-13 should be allowed for PG-13 viewer")
	}
}
