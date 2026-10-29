package sort

import (
	"github.com/braedonsaunders/ether/internal/core"
	"testing"
)

func TestOrder(t *testing.T) {
	for input, want := range map[string]core.SortOrder{"SortName": core.SortTitle, "DateCreated": core.SortAdded, "PremiereDate": core.SortReleased, "CommunityRating": core.SortRating, "Popularity": core.SortPopularity} {
		got, ok := Order(input)
		if !ok || got != want {
			t.Fatal(input, got, ok)
		}
	}
	if _, ok := Order("unexpected"); ok {
		t.Fatal("accepted unknown")
	}
}
