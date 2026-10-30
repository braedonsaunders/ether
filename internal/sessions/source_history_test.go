package sessions

import (
	"context"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

type guardedHistoryPlayback struct{ *fakePlayback }

func (p guardedHistoryPlayback) CurrentSource(ctx context.Context, id string) (core.PlaybackSourceObservation, error) {
	if _, ok := p.Session(id); !ok {
		return core.PlaybackSourceObservation{}, core.ErrNotFound
	}
	return core.PlaybackSourceObservation{Revision: "source", ReleaseID: "actual", VideoCodec: "h264", Height: 720}, nil
}

type fakeSourceHistory struct {
	epochs int
	writes chan core.PlaybackSourceObservation
}

type stalledSourceHistory struct{ entered, release chan struct{} }

func (h *stalledSourceHistory) BeginSourceSession(context.Context) (int64, error) { return 1, nil }
func (h *stalledSourceHistory) RecordLastSource(context.Context, int64, core.RatingKey, int64, int64, core.PlaybackSourceObservation) error {
	close(h.entered)
	<-h.release // Deliberately ignore cancellation to model an uncooperative driver.
	return nil
}

func TestSourceHistoryStalledStorageDoesNotBlockStop(t *testing.T) {
	ctx := context.Background()
	active := func(_ context.Context, id int64) (*core.Viewer, error) { return &core.Viewer{ID: id}, nil }
	events := NewEventBus(nil, active)
	defer events.Close()
	history := &stalledSourceHistory{entered: make(chan struct{}), release: make(chan struct{})}
	defer close(history.release)
	playback := guardedHistoryPlayback{&fakePlayback{sessions: map[string]*core.PlaySession{}, limit: 2}}
	b, err := NewBroker(BrokerDeps{Events: events, Playback: playback, Transcoder: fakeBrokerTranscoder{}, Progress: &fakeProgress{}, SourceHistory: history, ActiveViewer: active, CanPlay: func(*core.Viewer, bool, time.Time) bool { return true }, BitrateLimit: func(*core.Viewer) int { return 0 }, OnTimeline: func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {}, Lookup: func(_ context.Context, _ *core.Viewer, _ core.Device, k core.RatingKey) (*core.Title, error) {
		return &core.Title{Key: k}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close(ctx)
	viewer, device := &core.Viewer{ID: 2}, core.Device{ID: "tv"}
	plan, err := b.Plan(ctx, frontend.PlanRequest{Viewer: viewer, Device: device, Protocol: "silo", Item: core.MovieKey(1), Intent: frontend.PlayIntent{Options: core.DecideOptions{DirectPlay: true}}})
	if err != nil {
		t.Fatal(err)
	}
	session, err := b.Open(ctx, frontend.OpenRequest{Viewer: viewer, Device: device, Protocol: "silo", PlanID: plan.ID})
	if err != nil {
		t.Fatal(err)
	}
	access := frontend.SessionAccess{Viewer: viewer, Device: device, Protocol: "silo"}
	if _, err = b.StartStream(ctx, access, session.ID, frontend.StreamRequest{Format: frontend.StreamDirect}); err != nil {
		t.Fatal(err)
	}
	if err = b.Report(ctx, access, session.ID, frontend.Timeline{Sequence: 1, State: "playing"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-history.entered:
	case <-time.After(time.Second):
		t.Fatal("writer did not enter")
	}
	stopCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if err = b.Stop(stopCtx, access, session.ID); err != nil {
		t.Fatalf("stop blocked on storage: %v", err)
	}
}

func (h *fakeSourceHistory) BeginSourceSession(context.Context) (int64, error) {
	h.epochs++
	return int64(h.epochs), nil
}
func (h *fakeSourceHistory) RecordLastSource(_ context.Context, _ int64, _ core.RatingKey, _, _ int64, source core.PlaybackSourceObservation) error {
	h.writes <- source
	return nil
}

func TestSourceHistoryRecordsOnlyAcceptedSamplesAndAuthorizedStop(t *testing.T) {
	ctx := context.Background()
	active := func(_ context.Context, id int64) (*core.Viewer, error) { return &core.Viewer{ID: id}, nil }
	events := NewEventBus(nil, active)
	defer events.Close()
	history := &fakeSourceHistory{writes: make(chan core.PlaybackSourceObservation, 3)}
	allowed := true
	playback := guardedHistoryPlayback{&fakePlayback{sessions: map[string]*core.PlaySession{}, limit: 2}}
	b, err := NewBroker(BrokerDeps{Events: events, Playback: playback, Transcoder: fakeBrokerTranscoder{}, Progress: &fakeProgress{}, SourceHistory: history, ActiveViewer: active, CanPlay: func(*core.Viewer, bool, time.Time) bool { return true }, BitrateLimit: func(*core.Viewer) int { return 0 }, OnTimeline: func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {}, Lookup: func(_ context.Context, _ *core.Viewer, _ core.Device, k core.RatingKey) (*core.Title, error) {
		if !allowed {
			return nil, core.ErrNotFound
		}
		return &core.Title{Key: k}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close(ctx)
	viewer, device := &core.Viewer{ID: 2}, core.Device{ID: "tv"}
	open := func() frontend.Session {
		p, err := b.Plan(ctx, frontend.PlanRequest{Viewer: viewer, Device: device, Protocol: "silo", Item: core.MovieKey(1), Intent: frontend.PlayIntent{Options: core.DecideOptions{DirectPlay: true}}})
		if err != nil {
			t.Fatal(err)
		}
		s, err := b.Open(ctx, frontend.OpenRequest{Viewer: viewer, Device: device, Protocol: "silo", PlanID: p.ID})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	access := frontend.SessionAccess{Viewer: viewer, Device: device, Protocol: "silo"}
	session := open()
	if _, err := b.StartStream(ctx, access, session.ID, frontend.StreamRequest{Format: frontend.StreamDirect}); err != nil {
		t.Fatal(err)
	}
	report := frontend.Timeline{Sequence: 1, State: "playing"}
	if err := b.Report(ctx, access, session.ID, report); err != nil {
		t.Fatal(err)
	}
	select {
	case source := <-history.writes:
		if source.ReleaseID != "actual" {
			t.Fatalf("accepted source: %+v", source)
		}
	case <-time.After(time.Second):
		t.Fatal("accepted source not persisted")
	}
	if err := b.Report(ctx, access, session.ID, report); err != nil {
		t.Fatal(err)
	}
	if err := b.Stop(ctx, access, session.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-history.writes:
	case <-time.After(time.Second):
		t.Fatal("stop source missing")
	}
	session = open()
	if _, err := b.StartStream(ctx, access, session.ID, frontend.StreamRequest{Format: frontend.StreamDirect}); err != nil {
		t.Fatal(err)
	}
	allowed = false
	if err := b.Stop(ctx, access, session.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case source := <-history.writes:
		t.Fatalf("duplicate or revoked cleanup wrote history: %+v", source)
	default:
	}
}
