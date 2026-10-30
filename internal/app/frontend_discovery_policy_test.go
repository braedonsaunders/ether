package app

import (
	"context"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
)

func TestExperienceDiscoveryBackfillsRestrictedOnly(t *testing.T) {
	ctx := context.Background()
	viewer := &core.User{ID: 1}
	items := []core.Title{{Key: core.MovieKey(1), Adult: true}, {Key: core.MovieKey(2), Adult: true}, {Key: core.MovieKey(3)}, {Key: core.MovieKey(4)}}
	calls := 0
	load := func(_ context.Context, _ *core.User, limit int) ([]core.Hub, error) {
		calls++
		return []core.Hub{{ID: "watchlist", Items: items[:min(limit, len(items))]}}, nil
	}
	got, err := controlLegacyLoadExperienceDiscovery(ctx, viewer, 2, true, experience.Visibility{}, load)
	if err != nil || calls != 2 || len(got) != 1 || len(got[0].Items) != 2 || got[0].Items[0].Key != items[2].Key || got[0].Items[1].Key != items[3].Key {
		t.Fatalf("restricted discovery did not backfill: %+v calls=%d err=%v", got, calls, err)
	}
	calls = 0
	got, err = controlLegacyLoadExperienceDiscovery(ctx, viewer, 2, false, experience.Visibility{}, load)
	if err != nil || calls != 1 || len(got) != 1 || len(got[0].Items) != 2 || got[0].Items[0].Key != items[0].Key {
		t.Fatalf("unrestricted discovery changed: %+v calls=%d err=%v", got, calls, err)
	}
}
