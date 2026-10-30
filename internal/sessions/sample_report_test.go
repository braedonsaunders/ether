package sessions

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/state"
	"github.com/braedonsaunders/ether/internal/store"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSiloSampleDurableOutcomes(t *testing.T) {
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
	zero := frontend.ProgressSample{InstallationID: install, Sequence: 1, PositionSeconds: math.Copysign(0, -1)}
	if got, err := b.ReportSample(ctx, access, session.ID, zero); err != nil || got != frontend.ReportApplied {
		t.Fatalf("negative zero %s %v", got, err)
	}
	zero.PositionSeconds = 0
	if got, err := b.ReportSample(ctx, access, session.ID, zero); err != nil || got != frontend.ReportReplayed {
		t.Fatalf("canonical zero replay %s %v", got, err)
	}

	for _, tc := range []struct {
		sequence uint64
		position float64
		out      frontend.ReportOutcome
	}{{2, 120, frontend.ReportApplied}, {2, 120, frontend.ReportReplayed}, {1, 100, frontend.ReportStale}, {2, 121, frontend.ReportConflict}, {3, 180, frontend.ReportApplied}} {
		sample.Sequence = tc.sequence
		sample.PositionSeconds = tc.position
		got, err := b.ReportSample(ctx, access, session.ID, sample)
		if err != nil || got != tc.out {
			t.Fatalf("%+v got=%s err=%v", tc, got, err)
		}
	}
	// With unknown duration, the existing state reducer records zero as a start.
	// Only zero and the first meaningful offset create history; replays never do.
	var history int
	if err = db.QueryRow(`SELECT COUNT(*) FROM state_history`).Scan(&history); err != nil || history != 2 {
		t.Fatalf("history %d %v", history, err)
	}
	sample.Sequence = 4
	sample.PositionSeconds = 240
	var wg sync.WaitGroup
	for n := 0; n < 8; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := b.ReportSample(ctx, access, session.ID, sample)
			if err != nil || (got != frontend.ReportApplied && got != frontend.ReportReplayed) {
				t.Errorf("concurrent %s %v", got, err)
			}
		}()
	}
	wg.Wait()
	// A failed write must not consume the sequence or leave a durable receipt.
	progress.fail.Store(true)
	sample.Sequence = 5
	if got, err := b.ReportSample(ctx, access, session.ID, sample); err == nil || got == frontend.ReportApplied {
		t.Fatalf("failure %s %v", got, err)
	}
	var seq string
	db.QueryRow(`SELECT sequence FROM sessions_sample_receipts WHERE session_id=?`, session.ID).Scan(&seq)
	if seq != "00000000000000000004" {
		t.Fatal(seq)
	}
	progress.fail.Store(false)
	if got, err := b.ReportSample(ctx, access, session.ID, sample); err != nil || got != frontend.ReportApplied {
		t.Fatalf("retry %s %v", got, err)
	}

	sample.InstallationID = "22222222-2222-4222-8222-222222222222"
	if _, err = b.ReportSample(ctx, access, session.ID, sample); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatal(err)
	}
	sample.InstallationID = install

	progress.entered = make(chan struct{})
	progress.release = make(chan struct{})
	sample.Sequence = 6
	reported := make(chan error, 1)
	go func() {
		got, err := b.ReportSample(ctx, access, session.ID, sample)
		if err == nil && got != frontend.ReportApplied {
			err = fmt.Errorf("outcome %s", got)
		}
		reported <- err
	}()
	select {
	case <-progress.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("receipt transaction did not enter")
	}
	stopped := make(chan error, 1)
	go func() { stopped <- b.Stop(ctx, access, session.ID) }()
	select {
	case err := <-stopped:
		close(progress.release)
		t.Fatalf("Stop bypassed in-flight owner gate: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(progress.release)
	if err := <-reported; err != nil {
		t.Fatal(err)
	}
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
	if got, err := b.ReportSample(ctx, access, session.ID, sample); err == nil || got == frontend.ReportApplied {
		t.Fatalf("retired sample %s %v", got, err)
	}

	var remaining int
	if err = db.QueryRow(`SELECT COUNT(*) FROM sessions_sample_receipts`).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("stop cleanup %d %v", remaining, err)
	}

}

var _ interface {
	ProgressWithReceipt(context.Context, int64, core.RatingKey, time.Duration, time.Duration, string, func(*sql.Tx) error) error
} = (*state.State)(nil)

type sampleProgress struct {
	*state.State
	fail    atomic.Bool
	entered chan struct{}
	release chan struct{}
}

func (s *sampleProgress) ProgressWithReceipt(ctx context.Context, user int64, key core.RatingKey, position, duration time.Duration, status string, receipt func(*sql.Tx) error) error {
	return s.State.ProgressWithReceipt(ctx, user, key, position, duration, status, func(tx *sql.Tx) error {
		if err := receipt(tx); err != nil {
			return err
		}
		if s.fail.Load() {
			return errors.New("injected write failure")
		}
		if s.entered != nil {
			close(s.entered)
			<-s.release
		}
		return nil
	})
}
