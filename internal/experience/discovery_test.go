package experience

import (
	"context"
	"reflect"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestDiscoverySelectionTrustedAndRestricted(t *testing.T) {
	ctx := context.Background()
	parent := core.ShowKey(8)
	child := core.Title{Key: core.EpisodeKey(8, 1, 1), ShowKey: parent, Type: core.TypeEpisode}
	adult := core.Title{Key: core.MovieKey(1), Adult: true}
	visible := core.Title{Key: core.MovieKey(2)}
	vis := Visibility{GetTitle: func(context.Context, core.RatingKey) (*core.Title, error) { return &core.Title{Adult: true}, nil }}
	all := []core.Title{child, adult, visible}
	var calls []int
	load := func(_ context.Context, n int) []DiscoveryCandidate {
		calls = append(calls, n)
		return []DiscoveryCandidate{{Index: 0, Items: all[:min(n, len(all))]}, {Index: 1}}
	}
	owner := DiscoverySelection{Visibility: vis, Unrestricted: func(*core.User) bool { return true }}
	rows := owner.Load(ctx, &core.User{IsOwner: true}, 2, load)
	if !reflect.DeepEqual(calls, []int{2}) || len(rows) != 1 || len(rows[0].Items) != 1 || rows[0].Items[0].Key != child.Key {
		t.Fatalf("trusted rows=%+v calls=%v", rows, calls)
	}
	calls = nil
	restricted := DiscoverySelection{Visibility: vis}
	rows = restricted.Load(ctx, &core.User{ID: 1}, 1, load)
	if !reflect.DeepEqual(calls, []int{1, 2, 4}) || len(rows) != 1 || len(rows[0].Items) != 1 || rows[0].Items[0].Key != visible.Key {
		t.Fatalf("restricted rows=%+v calls=%v", rows, calls)
	}
}
