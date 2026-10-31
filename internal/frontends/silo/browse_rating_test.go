package silo

import (
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

// Silo shows other countries' ratings with their country, as Jellyfin does.
func TestTitleContentRatingKeepsCountry(t *testing.T) {
	for stored, want := range map[string]string{"PG-13": "PG-13", "gb/12A": "GB-12A"} {
		got := titleDTO(frontend.Title{Key: core.MovieKey(1), Type: "movie", Title: "T", ContentRating: stored}, frontend.WatchStatus{})["content_rating"]
		if got != want {
			t.Errorf("%q: content_rating %v, want %q", stored, got, want)
		}
	}
}
