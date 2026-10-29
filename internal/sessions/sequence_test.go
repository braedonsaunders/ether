package sessions

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/clusterbus"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/store"
)

type flakySequenceProgress struct{ calls int }

func (f *flakySequenceProgress) Progress(context.Context, int64, core.RatingKey, time.Duration, time.Duration, string) error {
	f.calls++
	if f.calls == 1 {
		return errors.New("temporary progress failure")
	}
	return nil
}

func TestSequenceCanRetryAfterProgressFailure(t *testing.T) {
	for _, clustered := range []bool{false, true} {
		t.Run(map[bool]string{false: "local", true: "clustered"}[clustered], func(t *testing.T) {
			ctx := context.Background()
			active := func(_ context.Context, id int64) (*core.Viewer, error) { return &core.Viewer{ID: id}, nil }
			events := NewEventBus(nil, active)
			defer events.Close()
			progress := &flakySequenceProgress{}
			deps := BrokerDeps{Events: events, Playback: &fakePlayback{sessions: map[string]*core.PlaySession{}, limit: 2}, Transcoder: fakeBrokerTranscoder{}, Progress: progress, ActiveViewer: active, CanPlay: func(*core.Viewer, bool, time.Time) bool { return true }, BitrateLimit: func(*core.Viewer) int { return 0 }, OnTimeline: func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {}, Lookup: func(_ context.Context, _ *core.Viewer, _ core.Device, k core.RatingKey) (*core.Title, error) {
				return &core.Title{Key: k}, nil
			}}
			if clustered {
				deps.DB = store.OpenTest(t)
				deps.Cluster = clusterbus.NewHub().Node("node")
			}
			b, err := NewBroker(deps)
			if err != nil {
				t.Fatal(err)
			}
			defer b.Close(ctx)
			viewer, device := &core.Viewer{ID: 2}, core.Device{ID: "tv"}
			p, err := b.Plan(ctx, frontend.PlanRequest{Viewer: viewer, Device: device, Protocol: "silo", Item: core.MovieKey(1), Intent: frontend.PlayIntent{Options: core.DecideOptions{DirectPlay: true}}})
			if err != nil {
				t.Fatal(err)
			}
			s, err := b.Open(ctx, frontend.OpenRequest{Viewer: viewer, Device: device, Protocol: "silo", PlanID: p.ID})
			if err != nil {
				t.Fatal(err)
			}
			a := frontend.SessionAccess{Viewer: viewer, Device: device, Protocol: "silo"}
			report := frontend.Timeline{Sequence: 1, State: "playing"}
			if err = b.Report(ctx, a, s.ID, report); err == nil {
				t.Fatal("expected transient failure")
			}
			if err = b.Report(ctx, a, s.ID, report); err != nil {
				t.Fatalf("retry: %v", err)
			}
			if err = b.Report(ctx, a, s.ID, report); err != nil || progress.calls != 2 {
				t.Fatalf("duplicate applied: calls=%d err=%v", progress.calls, err)
			}
		})
	}
}
