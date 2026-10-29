package sessions

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

func TestForeignJellyfinStopCannotTouchPlexTranscode(t *testing.T) {
	ctx := context.Background()
	active := func(_ context.Context, id int64) (*core.Viewer, error) { return &core.Viewer{ID: id}, nil }
	events := NewEventBus(nil, active)
	defer events.Close()
	playback := &fakePlayback{sessions: map[string]*core.PlaySession{}, limit: 2}
	transcoder := &fakeMediaTranscoder{sessions: map[string]*fakeMediaSession{}}
	broker, err := NewBroker(BrokerDeps{Events: events, Playback: playback, Transcoder: transcoder, Progress: &fakeProgress{}, ActiveViewer: active, CanPlay: func(*core.Viewer, bool, time.Time) bool { return true }, BitrateLimit: func(*core.Viewer) int { return 0 }, OnTimeline: func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {}, Lookup: func(_ context.Context, _ *core.Viewer, _ core.Device, k core.RatingKey) (*core.Title, error) {
		return &core.Title{Key: k}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer broker.Close(ctx)
	plexViewer, plexDevice := &core.Viewer{ID: 1}, core.Device{ID: "plex-tv"}
	plan, err := broker.Plan(ctx, frontend.PlanRequest{Viewer: plexViewer, Device: plexDevice, Protocol: "plex", Item: core.MovieKey(1), Intent: frontend.PlayIntent{ClientSessionID: "plex-client-session", Options: core.DecideOptions{DirectPlay: true, DirectStream: true}}})
	if err != nil {
		t.Fatal(err)
	}
	session, err := broker.Open(ctx, frontend.OpenRequest{Viewer: plexViewer, Device: plexDevice, Protocol: "plex", PlanID: plan.ID})
	if err != nil {
		t.Fatal(err)
	}
	plexAccess := frontend.SessionAccess{Viewer: plexViewer, Device: plexDevice, Protocol: "plex"}
	if _, err = broker.StartStream(ctx, plexAccess, session.ID, frontend.StreamRequest{Format: frontend.StreamHLS}); err != nil {
		t.Fatal(err)
	}
	original := transcoder.sessions[session.ID]
	if original == nil {
		t.Fatal("Plex transcode missing")
	}
	for _, access := range []frontend.SessionAccess{{Viewer: &core.Viewer{ID: 2}, Device: core.Device{ID: "jellyfin-tv"}, Protocol: "jellyfin"}, {Viewer: plexViewer, Device: plexDevice, Protocol: "jellyfin"}} {
		err = broker.Report(ctx, access, session.ID, frontend.Timeline{State: "stopped", PlaybackID: session.ID})
		if !errors.Is(err, core.ErrUnauthorized) {
			t.Fatalf("foreign report: %v", err)
		}
		err = broker.Stop(ctx, access, session.ID)
		if !errors.Is(err, core.ErrUnauthorized) {
			t.Fatalf("foreign stop: %v", err)
		}
		// A client-supplied identifier is not an alias for the broker session.
		_ = broker.Report(ctx, access, "plex-client-session", frontend.Timeline{State: "stopped", PlaybackID: "plex-client-session"})
		_ = broker.Stop(ctx, access, "plex-client-session")
		if transcoder.sessions[session.ID] != original || len(playback.Sessions()) != 1 {
			t.Fatal("foreign ID stopped or replaced Plex playback")
		}
	}
}
