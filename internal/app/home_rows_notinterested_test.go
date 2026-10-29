package app

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestPreviewAvailabilityExclusionBackfillAndLegacyParity(t *testing.T) {
	ctx := context.Background()
	viewer := &core.User{ID: 1}
	items := []core.Title{{Key: core.MovieKey(1)}, {Key: core.MovieKey(2)}, {Key: core.MovieKey(3)}, {Key: core.MovieKey(4)}}
	var calls []int
	load := func(_ context.Context, n int) []core.Hub {
		calls = append(calls, n)
		return []core.Hub{{ID: "availability", Items: items[:min(n, len(items))], More: true}}
	}
	blocked := false
	filter := func(_ context.Context, _ *core.User, in []core.Title) ([]core.Title, error) {
		if !blocked {
			return in, nil
		}
		out := make([]core.Title, 0, len(in))
		for _, item := range in {
			if item.Key != items[0].Key {
				out = append(out, item)
			}
		}
		return out, nil
	}
	got := previewAvailability(ctx, viewer, 2, load, filter)
	if len(got) != 1 || !reflect.DeepEqual(got[0].Items, items[:2]) || !reflect.DeepEqual(calls, []int{2}) || !got[0].More {
		t.Fatalf("legacy preview changed: %+v %v", got, calls)
	}
	blocked = true
	calls = nil
	got = previewAvailability(ctx, viewer, 2, load, filter)
	if len(got) != 1 || !reflect.DeepEqual(got[0].Items, items[1:3]) || !reflect.DeepEqual(calls, []int{2, 4}) {
		t.Fatalf("preview backfill failed: %+v %v", got, calls)
	}
	got = previewAvailability(ctx, viewer, 2, load, func(context.Context, *core.User, []core.Title) ([]core.Title, error) {
		return nil, errors.New("unavailable")
	})
	if got != nil {
		t.Fatalf("preview failed open: %+v", got)
	}
}
