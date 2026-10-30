package sessions

import (
	"context"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/state"
	"github.com/braedonsaunders/ether/internal/store"
	"testing"
	"time"
)

func TestSiloFinalSampleOrderingControlsStop(t *testing.T) {
	for _, tc := range []struct {
		name     string
		sequence uint64
		position float64
		outcome  frontend.ReportOutcome
		stopped  bool
	}{
		{"stale", 1, 100, frontend.ReportStale, false},
		{"conflict", 2, 121, frontend.ReportConflict, false},
		{"replay", 2, 120, frontend.ReportReplayed, true},
		{"applied", 3, 180, frontend.ReportApplied, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			db := store.OpenTest(t)
			progress := &sampleProgress{State: state.New(db, state.Options{})}
			active := func(_ context.Context, id int64) (*core.Viewer, error) { return &core.Viewer{ID: id}, nil }
			events := NewEventBus(nil, active)
			defer events.Close()
			b, err := NewBroker(BrokerDeps{DB: db, Events: events, Playback: &fakePlayback{sessions: map[string]*core.PlaySession{}, limit: 2}, Progress: progress, ActiveViewer: active, CanPlay: func(*core.Viewer, bool, time.Time) bool { return true }, BitrateLimit: func(*core.Viewer) int { return 0 }, OnTimeline: func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {}, Lookup: func(_ context.Context, _ *core.Viewer, _ core.Device, k core.RatingKey) (*core.Title, error) {
				return &core.Title{Key: k}, nil
			}})
			if err != nil {
				t.Fatal(err)
			}
			defer b.Close(ctx)
			const install = "11111111-1111-4111-8111-111111111111"
			viewer := &core.Viewer{ID: 1}
			device := core.Device{ID: "tv"}
			plan, err := b.Plan(ctx, frontend.PlanRequest{Viewer: viewer, Device: device, Protocol: "silo", Item: core.MovieKey(1), Intent: frontend.PlayIntent{ClientSessionID: install, Options: core.DecideOptions{DirectPlay: true}}})
			if err != nil {
				t.Fatal(err)
			}
			session, err := b.Open(ctx, frontend.OpenRequest{Viewer: viewer, Device: device, Protocol: "silo", PlanID: plan.ID})
			if err != nil {
				t.Fatal(err)
			}
			access := frontend.SessionAccess{Viewer: viewer, Device: device, Protocol: "silo"}

			sample := frontend.ProgressSample{InstallationID: install, Sequence: 2, PositionSeconds: 120}
			if got, err := b.ReportSample(ctx, access, session.ID, sample); err != nil || got != frontend.ReportApplied {
				t.Fatalf("initial %s %v", got, err)
			}
			sample.Sequence = tc.sequence
			sample.PositionSeconds = tc.position
			sample.Stop = true
			if got, err := b.ReportSample(ctx, access, session.ID, sample); err != nil || got != tc.outcome {
				t.Fatalf("final %s %v", got, err)
			}
			b.mu.Lock()
			_, sessionActive := b.sessions[session.ID]
			b.mu.Unlock()
			if sessionActive == tc.stopped {
				t.Fatalf("active=%v stopped=%v", sessionActive, tc.stopped)
			}
			var receipts int
			if err = db.QueryRow(`SELECT COUNT(*) FROM sessions_sample_receipts`).Scan(&receipts); err != nil {
				t.Fatal(err)
			}
			if tc.stopped && receipts != 0 || !tc.stopped && receipts != 1 {
				t.Fatalf("receipts %d", receipts)
			}
		})
	}
}
