package sessions

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

func TestReportItemProgressWritesJellyfinWatchState(t *testing.T) {
	ctx := context.Background()
	active := func(_ context.Context, id int64) (*core.Viewer, error) { return &core.Viewer{ID: id}, nil }
	events := NewEventBus(nil, active)
	defer events.Close()
	progress := &fakeProgress{}
	key := core.MovieKey(9)
	b, err := NewBroker(BrokerDeps{
		Events: events, Playback: &fakePlayback{sessions: map[string]*core.PlaySession{}, limit: 1}, Progress: progress, ActiveViewer: active,
		CanPlay: func(*core.Viewer, bool, time.Time) bool { return true }, BitrateLimit: func(*core.Viewer) int { return 0 },
		OnTimeline: func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {},
		Lookup: func(_ context.Context, _ *core.Viewer, _ core.Device, k core.RatingKey) (*core.Title, error) {
			if k != key {
				return nil, core.ErrNotFound
			}
			return &core.Title{Key: k, Type: core.TypeMovie}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close(ctx)
	access := frontend.SessionAccess{Viewer: &core.Viewer{ID: 1}, Device: core.Device{ID: "appletv"}, Protocol: "jellyfin"}
	timeline := frontend.Timeline{PlaybackID: "infuse-session", Position: 1745 * time.Second, Duration: time.Hour, State: "paused"}
	if err := b.ReportItemProgress(ctx, access, key, timeline); err != nil {
		t.Fatal(err)
	}
	if progress.calls != 1 || progress.viewer != 1 || progress.key != key || progress.position != 1745*time.Second || progress.duration != time.Hour || progress.state != "paused" {
		t.Fatalf("progress: %+v", progress)
	}
	if err := b.ReportItemProgress(ctx, frontend.SessionAccess{Viewer: access.Viewer, Device: access.Device, Protocol: "plex"}, key, timeline); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatal(err)
	}
	if err := b.ReportItemProgress(ctx, access, core.MovieKey(3), timeline); !errors.Is(err, core.ErrNotFound) || progress.calls != 1 {
		t.Fatal(err, progress.calls)
	}
}
