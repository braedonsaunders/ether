package sessions

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

func TestCloseWithCanceledContextStopsEncoderAndSource(t *testing.T) {
	ctx := context.Background()
	active := func(_ context.Context, id int64) (*core.Viewer, error) { return &core.Viewer{ID: id}, nil }
	events := NewEventBus(nil, active)
	defer events.Close()
	playback := &fakePlayback{sessions: map[string]*core.PlaySession{}, limit: 2}
	transcoder := &fakeMediaTranscoder{sessions: map[string]*fakeMediaSession{}}
	b, err := NewBroker(BrokerDeps{Events: events, Playback: playback, Transcoder: transcoder, Progress: &fakeProgress{}, ActiveViewer: active, CanPlay: func(*core.Viewer, bool, time.Time) bool { return true }, BitrateLimit: func(*core.Viewer) int { return 0 }, OnTimeline: func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {}, Lookup: func(_ context.Context, _ *core.Viewer, _ core.Device, k core.RatingKey) (*core.Title, error) {
		return &core.Title{Key: k}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	v, d := &core.Viewer{ID: 1}, core.Device{ID: "tv"}
	plan, err := b.Plan(ctx, frontend.PlanRequest{Viewer: v, Device: d, Protocol: "jellyfin", Item: core.MovieKey(1), Intent: frontend.PlayIntent{Options: core.DecideOptions{DirectPlay: true, DirectStream: true}}})
	if err != nil {
		t.Fatal(err)
	}
	s, err := b.Open(ctx, frontend.OpenRequest{Viewer: v, Device: d, Protocol: "jellyfin", PlanID: plan.ID})
	if err != nil {
		t.Fatal(err)
	}
	a := frontend.SessionAccess{Viewer: v, Device: d, Protocol: "jellyfin"}
	if _, err = b.StartStream(ctx, a, s.ID, frontend.StreamRequest{Format: frontend.StreamHLS}); err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	b.Close(canceled)
	if len(transcoder.sessions) != 0 || len(playback.Sessions()) != 0 {
		t.Fatal("canceled shutdown leaked encoder or source")
	}
	if _, err = b.StartStream(ctx, a, s.ID, frontend.StreamRequest{Format: frontend.StreamHLS}); !errors.Is(err, core.ErrNotAvailable) {
		t.Fatalf("closed broker restarted stream: %v", err)
	}
	b.Close(ctx)
}

type delayedStartTranscoder struct {
	*fakeMediaTranscoder
	entered chan struct{}
	release chan struct{}
}

func (d *delayedStartTranscoder) StartHLS(ctx context.Context, r core.HLSRequest) (core.HLSSession, error) {
	close(d.entered)
	<-d.release
	return d.fakeMediaTranscoder.StartHLS(ctx, r)
}
func TestCanceledStreamStartReleasesEncoder(t *testing.T) {
	ctx := context.Background()
	active := func(_ context.Context, id int64) (*core.Viewer, error) { return &core.Viewer{ID: id}, nil }
	events := NewEventBus(nil, active)
	defer events.Close()
	playback := &fakePlayback{sessions: map[string]*core.PlaySession{}, limit: 2}
	transcoder := &delayedStartTranscoder{fakeMediaTranscoder: &fakeMediaTranscoder{sessions: map[string]*fakeMediaSession{}}, entered: make(chan struct{}), release: make(chan struct{})}
	b, err := NewBroker(BrokerDeps{Events: events, Playback: playback, Transcoder: transcoder, Progress: &fakeProgress{}, ActiveViewer: active, CanPlay: func(*core.Viewer, bool, time.Time) bool { return true }, BitrateLimit: func(*core.Viewer) int { return 0 }, OnTimeline: func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {}, Lookup: func(_ context.Context, _ *core.Viewer, _ core.Device, k core.RatingKey) (*core.Title, error) {
		return &core.Title{Key: k}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close(ctx)
	v, d := &core.Viewer{ID: 1}, core.Device{ID: "tv"}
	plan, err := b.Plan(ctx, frontend.PlanRequest{Viewer: v, Device: d, Protocol: "jellyfin", Item: core.MovieKey(1), Intent: frontend.PlayIntent{Options: core.DecideOptions{DirectPlay: true, DirectStream: true}}})
	if err != nil {
		t.Fatal(err)
	}
	session, err := b.Open(ctx, frontend.OpenRequest{Viewer: v, Device: d, Protocol: "jellyfin", PlanID: plan.ID})
	if err != nil {
		t.Fatal(err)
	}
	reqCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		_, err := b.StartStream(reqCtx, frontend.SessionAccess{Viewer: v, Device: d, Protocol: "jellyfin"}, session.ID, frontend.StreamRequest{Format: frontend.StreamHLS})
		done <- err
	}()
	select {
	case <-transcoder.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("start did not enter transcoder")
	}
	cancel()
	close(transcoder.release)
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("start error %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled start did not finish")
	}
	if len(transcoder.sessions) != 0 {
		t.Fatal("canceled start leaked encoder")
	}
}

// App teardown also closes the playback and transcode managers independently.
// A caller that retries broker cleanup after a held owner gate is released must
// not be prevented by the broker's already-closed state.
func TestCloseRetriesCleanupAfterOwnerGateTimeout(t *testing.T) {
	ctx := context.Background()
	active := func(_ context.Context, id int64) (*core.Viewer, error) { return &core.Viewer{ID: id}, nil }
	events := NewEventBus(nil, active)
	defer events.Close()
	playback := &fakePlayback{sessions: map[string]*core.PlaySession{}, limit: 1}
	transcoder := &fakeMediaTranscoder{sessions: map[string]*fakeMediaSession{}}
	b, err := NewBroker(BrokerDeps{Events: events, Playback: playback, Transcoder: transcoder, Progress: &fakeProgress{}, ActiveViewer: active, CanPlay: func(*core.Viewer, bool, time.Time) bool { return true }, BitrateLimit: func(*core.Viewer) int { return 0 }, OnTimeline: func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {}, Lookup: func(_ context.Context, _ *core.Viewer, _ core.Device, k core.RatingKey) (*core.Title, error) {
		return &core.Title{Key: k}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	b.shutdownTimeout = 20 * time.Millisecond
	viewer, device := &core.Viewer{ID: 1}, core.Device{ID: "held"}
	plan, err := b.Plan(ctx, frontend.PlanRequest{Viewer: viewer, Device: device, Protocol: "jellyfin", Item: core.MovieKey(1), Intent: frontend.PlayIntent{Options: core.DecideOptions{DirectPlay: true, DirectStream: true}}})
	if err != nil {
		t.Fatal(err)
	}
	session, err := b.Open(ctx, frontend.OpenRequest{Viewer: viewer, Device: device, Protocol: "jellyfin", PlanID: plan.ID})
	if err != nil {
		t.Fatal(err)
	}
	access := frontend.SessionAccess{Viewer: viewer, Device: device, Protocol: "jellyfin"}
	if _, err = b.StartStream(ctx, access, session.ID, frontend.StreamRequest{Format: frontend.StreamHLS}); err != nil {
		t.Fatal(err)
	}
	unlock, err := b.gates.Acquire(ctx, fmt.Sprintf("broker/owner/%d/%s", viewer.ID, clientKey("jellyfin", device)))
	if err != nil {
		t.Fatal(err)
	}
	b.Close(ctx)
	if len(transcoder.sessions) != 1 || len(playback.Sessions()) != 1 {
		t.Fatal("held gate should defer cleanup")
	}
	unlock()
	b.Close(ctx)
	if len(transcoder.sessions) != 0 || len(playback.Sessions()) != 0 {
		t.Fatal("retry left encoder or source after gate released")
	}
}
