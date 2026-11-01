package experience

import (
	"context"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestPostPlayOrderAndRelatedDeduplication(t *testing.T) {
	picks := []core.PostPlayPick{{Title: core.Title{Key: 1}}, {Title: core.Title{Key: 2, Adult: true}}, {Title: core.Title{Key: 3}}}
	row := PostPlay(context.Background(), picks, &core.User{}, Visibility{}, map[core.RatingKey]bool{}, 0)
	if row.Purpose != core.RowPurpose("recommendation") || len(row.Items) != 2 || row.Items[0].Key != 1 || row.Items[1].Key != 3 {
		t.Fatalf("unexpected row: %+v", row)
	}
	seen := map[core.RatingKey]bool{1: true, 3: true}
	got := UniqueRelated(context.Background(), &core.User{}, Visibility{}, []core.Title{{Key: 1}, {Key: 2}, {Key: 2}, {Key: 4}}, seen, 0)
	if len(got) != 2 || got[0].Key != 2 || got[1].Key != 4 || !seen[4] {
		t.Fatalf("unexpected related picks: %+v", got)
	}
}

func TestPostPlayAndRelatedFilterBeforeDedupeAndLimit(t *testing.T) {
	ctx := context.Background()
	viewer := &core.User{ID: 7}
	show := core.ShowKey(42)
	v := Visibility{
		GetTitle: func(context.Context, core.RatingKey) (*core.Title, error) { return &core.Title{Adult: true}, nil },
		FilterAvailable: func(_ context.Context, ts []core.Title) []core.Title {
			if ts[0].Key == 2 {
				return nil
			}
			return ts
		},
	}
	seen := map[core.RatingKey]bool{}
	picks := []core.PostPlayPick{{Title: core.Title{Key: 1, ShowKey: show}}, {Title: core.Title{Key: 2}}, {Title: core.Title{Key: 1}}, {Title: core.Title{Key: 3}}}
	row := PostPlay(ctx, picks, viewer, v, seen, 1)
	if len(row.Items) != 1 || row.Items[0].Key != 1 || seen[2] || !seen[1] {
		t.Fatalf("filtered row: %+v, seen=%v", row, seen)
	}
	related := UniqueRelated(ctx, viewer, v, []core.Title{{Key: 2}, {Key: 3, ShowKey: show}, {Key: 3}, {Key: 4}}, seen, 1)
	if len(related) != 1 || related[0].Key != 3 || seen[2] || !seen[3] || seen[4] {
		t.Fatalf("filtered related: %+v, seen=%v", related, seen)
	}
}
