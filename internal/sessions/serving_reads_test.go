package sessions

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

type stallingPlayback struct {
	*fakePlayback
	entered chan struct{}
	release chan struct{}
}

func (p *stallingPlayback) ServeRange(w http.ResponseWriter, r *http.Request, _ string) {
	tracker := core.ReadRetirementFrom(r.Context())
	if tracker == nil {
		http.Error(w, "missing read retirement", http.StatusInternalServerError)
		return
	}
	done, ok := tracker.Begin()
	if !ok {
		http.Error(w, "sealed", http.StatusServiceUnavailable)
		return
	}
	defer done()
	close(p.entered)
	<-p.release
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("x"))
}

func TestServingRequestWaitsForStalledReader(t *testing.T) {
	ctx := context.Background()
	playback := &stallingPlayback{
		fakePlayback: &fakePlayback{sessions: map[string]*core.PlaySession{}, limit: 2},
		entered:      make(chan struct{}),
		release:      make(chan struct{}),
	}
	viewer := &core.Viewer{ID: 4}
	device := core.Device{ID: "stalled"}
	active := func(_ context.Context, id int64) (*core.Viewer, error) { return &core.Viewer{ID: id}, nil }
	bus := NewEventBus(nil, active)
	t.Cleanup(bus.Close)
	broker, err := NewBroker(BrokerDeps{
		Lookup: func(_ context.Context, _ *core.Viewer, _ core.Device, key core.RatingKey) (*core.Title, error) {
			return &core.Title{Key: key}, nil
		},
		Playback: playback, Transcoder: fakeBrokerTranscoder{}, Progress: &fakeProgress{},
		ActiveViewer: active, CanPlay: func(*core.Viewer, bool, time.Time) bool { return true },
		BitrateLimit: func(*core.Viewer) int { return 0 },
		OnTimeline:   func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {},
		Events:       bus,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { broker.Close(context.Background()) })
	plan, err := broker.Plan(ctx, frontend.PlanRequest{Viewer: viewer, Device: device, Protocol: "jellyfin", Item: core.MovieKey(9), Intent: frontend.PlayIntent{Options: core.DecideOptions{DirectPlay: true}}})
	if err != nil {
		t.Fatal(err)
	}
	session, err := broker.Open(ctx, frontend.OpenRequest{Viewer: viewer, Device: device, Protocol: "jellyfin", PlanID: plan.ID})
	if err != nil {
		t.Fatal(err)
	}
	access := frontend.SessionAccess{Viewer: viewer, Device: device, Protocol: "jellyfin"}
	path, err := broker.StartStream(ctx, access, session.ID, frontend.StreamRequest{Format: frontend.StreamDirect})
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan int, 1)
	go func() {
		rec := httptest.NewRecorder()
		broker.ServeMedia(rec, httptest.NewRequest(http.MethodGet, path, nil))
		served <- rec.Code
	}()
	select {
	case <-playback.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("reader did not start")
	}
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		if err := broker.Stop(ctx, access, session.ID); err != nil {
			t.Errorf("stop: %v", err)
		}
	}()
	select {
	case <-stopped:
		t.Fatal("stop returned while the reader held the request")
	case code := <-served:
		t.Fatalf("serve returned while the reader was stalled: %d", code)
	case <-time.After(200 * time.Millisecond):
	}
	close(playback.release)
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("stop did not return after the reader finished")
	}
	select {
	case code := <-served:
		if code != http.StatusOK {
			t.Fatalf("serve status %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return")
	}
}
