package sessions

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/state"
	"github.com/braedonsaunders/ether/internal/store"
)

type changingHistoryPlayback struct {
	*fakePlayback
	mu    sync.Mutex
	codec string
}

func (p *changingHistoryPlayback) CurrentSource(_ context.Context, id string) (core.PlaybackSourceObservation, error) {
	if _, ok := p.Session(id); !ok {
		return core.PlaybackSourceObservation{}, core.ErrNotFound
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return core.PlaybackSourceObservation{Revision: p.codec, VideoCodec: p.codec}, nil
}

func TestSourceHistoryRealStateAdvancesOnlyOnAcceptedSamples(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	history := state.New(db, state.Options{})
	active := func(_ context.Context, id int64) (*core.Viewer, error) { return &core.Viewer{ID: id}, nil }
	events := NewEventBus(nil, active)
	defer events.Close()
	playback := &changingHistoryPlayback{fakePlayback: &fakePlayback{sessions: map[string]*core.PlaySession{}, limit: 2}, codec: "h264"}
	b, err := NewBroker(BrokerDeps{Events: events, Playback: playback, Transcoder: fakeBrokerTranscoder{}, Progress: &fakeProgress{}, SourceHistory: history, ActiveViewer: active, CanPlay: func(*core.Viewer, bool, time.Time) bool { return true }, BitrateLimit: func(*core.Viewer) int { return 0 }, OnTimeline: func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {}, Lookup: func(_ context.Context, _ *core.Viewer, _ core.Device, k core.RatingKey) (*core.Title, error) {
		return &core.Title{Key: k}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close(ctx)
	viewer, dev, key := &core.Viewer{ID: 2}, core.Device{ID: "tv"}, core.MovieKey(1)
	plan, err := b.Plan(ctx, frontend.PlanRequest{Viewer: viewer, Device: dev, Protocol: "silo", Item: key, Intent: frontend.PlayIntent{Options: core.DecideOptions{DirectPlay: true}}})
	if err != nil {
		t.Fatal(err)
	}
	session, err := b.Open(ctx, frontend.OpenRequest{Viewer: viewer, Device: dev, Protocol: "silo", PlanID: plan.ID})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := history.LastSource(ctx, viewer.ID, key); err != nil || got == nil || got.VideoCodec != "h264" {
		t.Fatalf("playback start did not record probe facts: %+v %v", got, err)
	}
	access := frontend.SessionAccess{Viewer: viewer, Device: dev, Protocol: "silo"}
	if _, err = b.StartStream(ctx, access, session.ID, frontend.StreamRequest{Format: frontend.StreamDirect}); err != nil {
		t.Fatal(err)
	}
	waitCodec := func(want string) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			got, err := history.LastSource(ctx, viewer.ID, key)
			if err != nil {
				t.Fatal(err)
			}
			if got != nil && got.VideoCodec == want {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("source codec %q was not persisted", want)
	}
	if err = b.Report(ctx, access, session.ID, frontend.Timeline{Sequence: 1, State: "playing"}); err != nil {
		t.Fatal(err)
	}
	waitCodec("h264")
	playback.mu.Lock()
	playback.codec = "hevc"
	playback.mu.Unlock()
	if err = b.Report(ctx, access, session.ID, frontend.Timeline{Sequence: 2, State: "playing"}); err != nil {
		t.Fatal(err)
	}
	waitCodec("hevc")
	playback.mu.Lock()
	playback.codec = "av1"
	playback.mu.Unlock()
	if err = b.Report(ctx, access, session.ID, frontend.Timeline{Sequence: 2, State: "playing"}); err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	sequence := b.sessions[session.ID].historySequence
	b.mu.Unlock()
	if sequence != 3 {
		t.Fatalf("replay reserved source sequence %d", sequence)
	}
	got, err := history.LastSource(ctx, viewer.ID, key)
	if err != nil || got == nil || got.VideoCodec != "hevc" {
		t.Fatalf("replay changed durable source: %+v %v", got, err)
	}
}

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

type stalledSourceHistory struct {
	entered, release chan struct{}
	once             sync.Once
	hold             atomic.Bool
}

type cancelableSourceHistory struct{ entered chan struct{} }

func (h *cancelableSourceHistory) BeginSourceSession(context.Context) (int64, error) { return 1, nil }
func (h *cancelableSourceHistory) RecordLastSource(ctx context.Context, _ int64, _ core.RatingKey, _, _ int64, _ core.PlaybackSourceObservation) error {
	close(h.entered)
	<-ctx.Done()
	return ctx.Err()
}

func TestSourceHistoryWorkerExitsOnCancellation(t *testing.T) {
	h := &cancelableSourceHistory{entered: make(chan struct{})}
	b := &Broker{deps: BrokerDeps{SourceHistory: h}, historyQueue: make(chan sourceHistorySample, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); b.runSourceHistory(ctx) }()
	b.historyQueue <- sourceHistorySample{}
	<-h.entered
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("history worker did not stop after cancellation")
	}
}

func (h *stalledSourceHistory) BeginSourceSession(context.Context) (int64, error) { return 1, nil }
func (h *stalledSourceHistory) RecordLastSource(context.Context, int64, core.RatingKey, int64, int64, core.PlaybackSourceObservation) error {
	if !h.hold.Load() {
		return nil
	}
	h.once.Do(func() { close(h.entered) })
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
	history.hold.Store(true)
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
		select {
		case source := <-history.writes:
			if source.ReleaseID != "actual" {
				t.Fatalf("opened source: %+v", source)
			}
		case <-time.After(time.Second):
			t.Fatal("playback start did not record the opened file")
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
	allowed = true
	session = open()
	if _, err := b.StartStream(ctx, access, session.ID, frontend.StreamRequest{Format: frontend.StreamDirect}); err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	stale := *b.sessions[session.ID]
	b.mu.Unlock()
	b.owners.BindFor("broker", stale.public.ViewerID, stale.clientID, stale.public.Item, "newer", "newer-play", true, time.Now())
	if err := b.Stop(ctx, access, session.ID); err != nil {
		t.Fatal(err)
	}
	// The worker processes this barrier after any erroneous duplicate, revoked,
	// or superseded-session sample.
	b.historyQueue <- sourceHistorySample{source: core.PlaybackSourceObservation{ReleaseID: "barrier"}}
	select {
	case source := <-history.writes:
		if source.ReleaseID != "barrier" {
			t.Fatalf("duplicate or unauthorized cleanup wrote history: %+v", source)
		}
	case <-time.After(time.Second):
		t.Fatal("history worker did not reach barrier")
	}
}
