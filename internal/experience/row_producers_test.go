package experience

import (
	"context"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestNextUpEnforcesParentAndAvailability(t *testing.T) {
	ctx := context.Background()
	current := core.EpisodeKey(42, 1, 1)
	title := &core.Title{Key: core.EpisodeKey(42, 1, 2), ShowKey: core.ShowKey(42), Type: core.TypeEpisode}
	parent := &core.Title{Adult: true}
	vis := Visibility{GetTitle: func(context.Context, core.RatingKey) (*core.Title, error) { return parent, nil }}
	next := func(context.Context, core.RatingKey) (*core.Title, error) { return title, nil }
	if len(NextUp(ctx, &core.User{}, current, next, vis).Items) != 0 {
		t.Fatal("restricted parent leaked")
	}
	parent.Adult = false
	if len(NextUp(ctx, &core.User{}, current, next, vis).Items) != 1 {
		t.Fatal("permitted next episode missing")
	}
	vis.FilterAvailable = func(context.Context, []core.Title) []core.Title { return nil }
	if len(NextUp(ctx, &core.User{}, current, next, vis).Items) != 0 {
		t.Fatal("unavailable episode leaked")
	}
}

func TestLoadPostPlayBackfills(t *testing.T) {
	picks := []core.PostPlayPick{{Title: core.Title{Key: 1, Adult: true}}, {Title: core.Title{Key: 2}}}
	calls := 0
	seen := map[core.RatingKey]bool{}
	row := LoadPostPlay(context.Background(), &core.User{}, Visibility{}, 1, seen, func(_ context.Context, n int) []core.PostPlayPick { calls++; return picks[:min(n, len(picks))] })
	if calls != 2 || len(row.Items) != 1 || row.Items[0].Key != 2 || seen[1] || !seen[2] {
		t.Fatalf("unexpected row: %+v, seen %+v, calls %d", row, seen, calls)
	}
}
