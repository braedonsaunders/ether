package catalog

import (
	"testing"

	"github.com/braedonsaunders/ether/internal/contentrating"
)

func TestTitleRatingRankUsesStricterEnforcementRating(t *testing.T) {
	pg13 := int(contentrating.LevelOf("PG-13"))
	rRank := int(contentrating.EnforceLevelOf("R"))
	if got := titleRatingRank("PG-13", "R"); got != rRank {
		t.Fatalf("dual-cut PG-13/R rank = %d, want %d (PG-13=%d)", got, rRank, pg13)
	}
	if got := titleRatingRank("PG-13", contentrating.UncertifiedAdult); got < rRank {
		t.Fatalf("PG-13/UncertifiedAdult rank = %d, want >= %d", got, rRank)
	}
	if got := titleRatingRank("PG-13", ""); got != pg13 {
		t.Fatalf("PG-13 with empty enforcement rank = %d, want %d", got, pg13)
	}
}
