package sessions

import (
	"context"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

type guardedHistoryPlayback struct{ *fakePlayback }

func (p guardedHistoryPlayback) WithCurrentSource(ctx context.Context, id string, f func(context.Context, core.PlaybackSourceObservation) error) error {
	if _, ok := p.Session(id); !ok {
		return core.ErrNotFound
	}
	return f(ctx, core.PlaybackSourceObservation{Revision: "source", ReleaseID: "actual", VideoCodec: "h264", Height: 720})
}

type fakeSourceHistory struct {
	epochs, writes int
	last           core.PlaybackSourceObservation
}

func (h *fakeSourceHistory) BeginSourceSession(context.Context) (int64, error) {
	h.epochs++
	return int64(h.epochs), nil
}
func (h *fakeSourceHistory) RecordLastSource(_ context.Context, _ int64, _ core.RatingKey, _ int64, source core.PlaybackSourceObservation) error {
	h.writes++
	h.last = source
	return nil
}

func TestSourceHistoryRecordsOnlyAcceptedSamplesAndAuthorizedStop(t *testing.T) {
	ctx := context.Background()
	active := func(_ context.Context, id int64) (*core.Viewer, error) { return &core.Viewer{ID: id}, nil }
	events := NewEventBus(nil, active)
	defer events.Close()
	history := &fakeSourceHistory{}
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
	if history.writes != 1 || history.last.ReleaseID != "actual" {
		t.Fatalf("accepted source: %+v", history)
	}
	if err := b.Report(ctx, access, session.ID, report); err != nil || history.writes != 1 {
		t.Fatalf("duplicate wrote history: %+v %v", history, err)
	}
	if err := b.Stop(ctx, access, session.ID); err != nil || history.writes != 2 {
		t.Fatalf("bare stop source: %+v %v", history, err)
	}
	session = open()
	if _, err := b.StartStream(ctx, access, session.ID, frontend.StreamRequest{Format: frontend.StreamDirect}); err != nil {
		t.Fatal(err)
	}
	allowed = false
	if err := b.Stop(ctx, access, session.ID); err != nil || history.writes != 2 {
		t.Fatalf("revoked cleanup wrote history: %+v %v", history, err)
	}
}
