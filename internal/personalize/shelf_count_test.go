package personalize

import (
	"fmt"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

// Plex count is a card window, not permission to remove otherwise full shelves.
func TestShelfIdentitySurvivesSmallCardWindows(t *testing.T) {
	for _, spec := range []struct {
		kind, id string
		minimum  int
	}{
		{KindTopRated, "toprated", 5}, {KindShowcase, "showcase.comedy", 6},
	} {
		t.Run(spec.id, func(t *testing.T) {
			cat := newFakeCatalog(testNow)
			var keys []core.RatingKey
			for i := 1; i <= 12; i++ {
				keys = append(keys, movieKey(0, i))
			}
			row := testRow(spec.kind, spec.id, items(cat, keys...))
			row.minItems = spec.minimum
			row.coldHome = true
			for _, count := range []int{12, 4, 1, 5, 6, 12} {
				t.Run(fmt.Sprint(count), func(t *testing.T) {
					rows := run(emptyModel(row), func(a *assembly) { a.limit = count; a.minRow = 12 })
					if len(rows) != 1 || rows[0].HubID != "home."+spec.id || len(rows[0].Items) != count {
						t.Fatalf("count=%d: rows=%v; want same shelf with %d cards", count, kindsOf(rows), count)
					}
				})
			}
			// A genuine shortage still suppresses the row at the normal window.
			row.items = row.items[:3]
			if rows := run(emptyModel(row), func(a *assembly) { a.limit = 12; a.minRow = 12 }); len(rows) != 0 {
				t.Fatalf("genuinely thin shelf retained: %v", kindsOf(rows))
			}
		})
	}
}
