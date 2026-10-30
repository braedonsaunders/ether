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

func TestSiloStopBoundsReceiptCleanupWhenDatabaseStalls(t *testing.T) {
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

	sample := frontend.ProgressSample{InstallationID: install, Sequence: 1, PositionSeconds: 120}
	if got, err := b.ReportSample(ctx, access, session.ID, sample); err != nil || got != frontend.ReportApplied {
		t.Fatalf("sample %s %v", got, err)
	}
	db.SetMaxOpenConns(1)
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	stopped := make(chan error, 1)
	go func() { stopped <- b.Stop(ctx, access, session.ID) }()
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		conn.Close()
		t.Fatal("Stop blocked on receipt cleanup")
	}
	conn.Close()
	var count int
	if err = db.QueryRow(`SELECT COUNT(*) FROM sessions_sample_receipts WHERE session_id=?`, session.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("uncertain receipt erased %d %v", count, err)
	}
}
