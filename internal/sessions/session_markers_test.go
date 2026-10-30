package sessions

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

func TestSessionMarkersAreAuthorizedAndBoundToSourceRevision(t *testing.T) {
	ctx := context.Background()
	active := func(_ context.Context, id int64) (*core.Viewer, error) { return &core.Viewer{ID: id}, nil }
	events := NewEventBus(nil, active)
	defer events.Close()
	source := &revisionSource{Reader: bytes.NewReader(nil), id: "release-one", name: "movie.mkv", size: 100}
	playback := &revisionPlayback{fakePlayback: &fakePlayback{sessions: map[string]*core.PlaySession{}, limit: 2}, source: source}
	var called int
	var known bool
	var change bool
	var duringLookup func()
	var ranges []frontend.Marker
	provider := func(_ context.Context, _ *core.Viewer, _ core.RatingKey, revision string, _ core.MediaInfo) ([]frontend.Marker, bool, error) {
		called++
		if revision == "" {
			t.Fatal("missing revision")
		}
		if change {
			source.id = "release-two"
		}
		if duringLookup != nil {
			duringLookup()
		}
		return ranges, known, nil
	}
	b, err := NewBroker(BrokerDeps{Events: events, Playback: playback, Progress: &fakeProgress{}, ActiveViewer: active, CanPlay: func(*core.Viewer, bool, time.Time) bool { return true }, BitrateLimit: func(*core.Viewer) int { return 0 }, OnTimeline: func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {}, Lookup: func(_ context.Context, _ *core.Viewer, _ core.Device, k core.RatingKey) (*core.Title, error) {
		return &core.Title{Key: k}, nil
	}, SourceMarkers: provider})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close(ctx)
	viewer, device := &core.Viewer{ID: 1}, core.Device{ID: "tv"}
	plan, err := b.Plan(ctx, frontend.PlanRequest{Viewer: viewer, Device: device, Protocol: "silo", Item: core.MovieKey(1), Intent: frontend.PlayIntent{Options: core.DecideOptions{DirectPlay: true}}})
	if err != nil {
		t.Fatal(err)
	}
	session, err := b.Open(ctx, frontend.OpenRequest{Viewer: viewer, Device: device, Protocol: "silo", PlanID: plan.ID})
	if err != nil {
		t.Fatal(err)
	}
	access := frontend.SessionAccess{Viewer: viewer, Device: device, Protocol: "silo"}
	foreign := frontend.SessionAccess{Viewer: &core.Viewer{ID: 2}, Device: device, Protocol: "silo"}
	if _, err = b.Markers(ctx, foreign, session.ID); !errors.Is(err, core.ErrUnauthorized) || called != 0 {
		t.Fatalf("foreign lookup: %v calls=%d", err, called)
	}
	result, err := b.Markers(ctx, access, session.ID)
	if err != nil || result.Known || result.Markers != nil {
		t.Fatalf("unknown: %+v %v", result, err)
	}
	known = true
	result, err = b.Markers(ctx, access, session.ID)
	if err != nil || !result.Known || result.Markers == nil || len(result.Markers) != 0 || result.SourceETag == "" {
		t.Fatalf("known empty: %+v %v", result, err)
	}
	change = true
	result, err = b.Markers(ctx, access, session.ID)
	if err != nil || result.Known || result.SourceETag != "" {
		t.Fatalf("failover accepted stale markers: %+v %v", result, err)
	}
	change = false
	ranges = []frontend.Marker{{Type: "intro", Start: -time.Second, End: time.Second}}
	if _, err = b.Markers(ctx, access, session.ID); !errors.Is(err, core.ErrNotAvailable) {
		t.Fatalf("invalid ranges %v", err)
	}
	ranges = nil
	duringLookup = func() {
		if err := b.Stop(ctx, access, session.ID); err != nil {
			t.Fatal(err)
		}
	}
	if result, err = b.Markers(ctx, access, session.ID); err == nil && result.Known {
		t.Fatalf("stopped owner returned markers %+v", result)
	}

}
