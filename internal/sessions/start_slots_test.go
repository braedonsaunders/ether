package sessions

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

func TestStreamStartAdmissionIsBoundedAndReleasedOnCancellation(t *testing.T) {
	ctx := context.Background()
	active := func(_ context.Context, id int64) (*core.Viewer, error) { return &core.Viewer{ID: id}, nil }
	events := NewEventBus(nil, active)
	defer events.Close()
	playback := &fakePlayback{sessions: map[string]*core.PlaySession{}, limit: 3}
	transcoder := &delayedStartTranscoder{fakeMediaTranscoder: &fakeMediaTranscoder{sessions: map[string]*fakeMediaSession{}}, entered: make(chan struct{}), release: make(chan struct{})}
	b, err := NewBroker(BrokerDeps{MaxPendingStarts: 1, Events: events, Playback: playback, Transcoder: transcoder, Progress: &fakeProgress{}, ActiveViewer: active, CanPlay: func(*core.Viewer, bool, time.Time) bool { return true }, BitrateLimit: func(*core.Viewer) int { return 0 }, OnTimeline: func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {}, Lookup: func(_ context.Context, _ *core.Viewer, _ core.Device, k core.RatingKey) (*core.Title, error) {
		return &core.Title{Key: k}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close(ctx)
	viewer := &core.Viewer{ID: 1}
	open := func(device core.Device) (frontend.Session, frontend.SessionAccess) {
		t.Helper()
		plan, err := b.Plan(ctx, frontend.PlanRequest{Viewer: viewer, Device: device, Protocol: "jellyfin", Item: core.MovieKey(1), Intent: frontend.PlayIntent{Options: core.DecideOptions{DirectPlay: true, DirectStream: true}}})
		if err != nil {
			t.Fatal(err)
		}
		s, err := b.Open(ctx, frontend.OpenRequest{Viewer: viewer, Device: device, Protocol: "jellyfin", PlanID: plan.ID})
		if err != nil {
			t.Fatal(err)
		}
		return s, frontend.SessionAccess{Viewer: viewer, Device: device, Protocol: "jellyfin"}
	}
	first, firstAccess := open(core.Device{ID: "one"})
	second, secondAccess := open(core.Device{ID: "two"})
	third, thirdAccess := open(core.Device{ID: "three"})
	reqCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		_, err := b.StartStream(reqCtx, firstAccess, first.ID, frontend.StreamRequest{Format: frontend.StreamHLS})
		done <- err
	}()
	select {
	case <-transcoder.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("start did not enter transcoder")
	}
	_, err = b.StartStream(ctx, secondAccess, second.ID, frontend.StreamRequest{Format: frontend.StreamDirect})
	var busy *frontend.SessionBusyError
	if !errors.Is(err, frontend.ErrSessionBusy) || !errors.As(err, &busy) || busy.RetryAfter <= 0 {
		t.Fatalf("expected retryable capacity error, got %v", err)
	}
	if err := b.Stop(ctx, secondAccess, second.ID); err != nil {
		t.Fatalf("stop blocked by saturated admission: %v", err)
	}
	cancel()
	close(transcoder.release)
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled start: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("start did not finish")
	}
	if len(transcoder.sessions) != 0 {
		t.Fatal("canceled start leaked encoder")
	}
	if _, err = b.StartStream(ctx, thirdAccess, third.ID, frontend.StreamRequest{Format: frontend.StreamDirect}); err != nil {
		t.Fatalf("admission not released: %v", err)
	}
}
