package experience

import (
	"context"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestDiscoveryFiltersAdult(t *testing.T) {
	ctx := context.Background()
	rows := []DiscoveryCandidate{{Index: 3, Purpose: core.RowPurpose("watchlist"), Items: []core.Title{{Key: 1, Adult: true}, {Key: 3}}}}
	got := FilterDiscovery(ctx, &core.User{}, Visibility{}, rows)
	if len(got) != 1 || got[0].Purpose != "watchlist" || len(got[0].Items) != 1 || got[0].Items[0].Key != 3 {
		t.Fatalf("unexpected rows: %+v", got)
	}
}
func TestRelatedBackfillsBeforeDeduplication(t *testing.T) {
	ctx := context.Background()
	seen := map[core.RatingKey]bool{1: true}
	calls := 0
	rows, err := LoadRelated(ctx, &core.User{}, Visibility{}, 1, seen, func(_ context.Context, n int) ([]RelatedCandidate, error) {
		calls++
		all := []core.Title{{Key: 1}, {Key: 2}}
		return []RelatedCandidate{{Index: 7, Items: all[:min(n, len(all))]}}, nil
	})
	if err != nil || len(rows) != 1 || len(rows[0].Items) != 1 || rows[0].Items[0].Key != 2 || calls != 2 || !seen[2] {
		t.Fatalf("unexpected rows %+v calls %d err %v", rows, calls, err)
	}
}
