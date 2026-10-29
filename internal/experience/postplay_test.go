package experience

import (
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestPostPlayOrderAndRelatedDeduplication(t *testing.T) {
	picks := []core.PostPlayPick{{Title: core.Title{Key: 1}}, {Title: core.Title{Key: 2, Adult: true}}, {Title: core.Title{Key: 3}}}
	row := PostPlay(picks, &core.User{}, Visibility{})
	if row.Purpose != core.RowPurpose("recommendation") || len(row.Items) != 2 || row.Items[0].Key != 1 || row.Items[1].Key != 3 {
		t.Fatalf("unexpected row: %+v", row)
	}
	seen := map[core.RatingKey]bool{1: true, 3: true}
	got := UniqueRelated([]core.Title{{Key: 1}, {Key: 2}, {Key: 2}, {Key: 4}}, seen)
	if len(got) != 2 || got[0].Key != 2 || got[1].Key != 4 || !seen[4] {
		t.Fatalf("unexpected related picks: %+v", got)
	}
}
