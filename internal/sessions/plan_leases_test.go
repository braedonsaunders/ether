package sessions

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

type leaseHarness struct {
	b        *Broker
	playback *fakePlayback
	tc       *fakeMediaTranscoder
	now      atomic.Int64
	allowed  atomic.Bool
	viewer   *core.Viewer
	device   core.Device
	req      frontend.PlanRequest
}

func newLeaseHarness(t *testing.T) *leaseHarness {
	t.Helper()
	h := &leaseHarness{playback: &fakePlayback{sessions: map[string]*core.PlaySession{}, limit: 10}, tc: &fakeMediaTranscoder{sessions: map[string]*fakeMediaSession{}}, viewer: &core.Viewer{ID: 2}, device: core.Device{ID: "device"}}
	h.now.Store(time.Now().UnixNano())
	h.allowed.Store(true)
	active := func(_ context.Context, id int64) (*core.Viewer, error) { return &core.Viewer{ID: id}, nil }
	bus := NewEventBus(nil, active)
	t.Cleanup(bus.Close)
	b, err := NewBroker(BrokerDeps{Events: bus, Playback: h.playback, Transcoder: h.tc, Progress: &fakeProgress{}, ActiveViewer: active, CanPlay: func(*core.Viewer, bool, time.Time) bool { return true }, BitrateLimit: func(*core.Viewer) int { return 0 }, OnTimeline: func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {}, Now: func() time.Time { return time.Unix(0, h.now.Load()) }, Lookup: func(_ context.Context, _ *core.Viewer, _ core.Device, k core.RatingKey) (*core.Title, error) {
		if !h.allowed.Load() {
			return nil, core.ErrNotFound
		}
		return &core.Title{Key: k}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	b.planSweepEvery = 10 * time.Millisecond
	h.b = b
	t.Cleanup(func() { b.Close(context.Background()) })
	h.req = frontend.PlanRequest{Viewer: h.viewer, Device: h.device, Protocol: "jellyfin", Item: core.MovieKey(1), Intent: frontend.PlayIntent{Options: core.DecideOptions{DirectPlay: true, DirectStream: true}}}
	return h
}
func (h *leaseHarness) plan(t *testing.T) frontend.PlayPlan {
	t.Helper()
	p, e := h.b.Plan(context.Background(), h.req)
	if e != nil {
		t.Fatal(e)
	}
	return p
}
func (h *leaseHarness) open(t *testing.T, p frontend.PlayPlan) frontend.Session {
	t.Helper()
	s, e := h.b.Open(context.Background(), frontend.OpenRequest{Viewer: h.viewer, Device: h.device, Protocol: h.req.Protocol, PlanID: p.ID})
	if e != nil {
		t.Fatal(e)
	}
	return s
}
func (h *leaseHarness) access() frontend.SessionAccess {
	return frontend.SessionAccess{Viewer: h.viewer, Device: h.device, Protocol: h.req.Protocol}
}
func waitLease(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.After(time.Second)
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for !cond() {
		select {
		case <-deadline:
			t.Fatal("lease did not drain within one second")
		case <-ticker.C:
		}
	}
}

func TestPlanLeaseFailureAndSharedPlans(t *testing.T) {
	h := newLeaseHarness(t)
	ctx := context.Background()
	first, second := h.plan(t), h.plan(t)
	if len(h.playback.Sessions()) != 1 {
		t.Fatal("plans did not share source")
	}
	h.allowed.Store(false)
	if _, err := h.b.Open(ctx, frontend.OpenRequest{Viewer: h.viewer, Device: h.device, Protocol: h.req.Protocol, PlanID: first.ID}); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("failure injection: %v", err)
	}
	if len(h.playback.Sessions()) != 1 {
		t.Fatal("failed plan stopped another plan source")
	}
	h.allowed.Store(true)
	session := h.open(t, second)
	if err := h.b.Stop(ctx, h.access(), session.ID); err != nil {
		t.Fatal(err)
	}
	if len(h.playback.Sessions()) != 0 || len(h.tc.sessions) != 0 {
		t.Fatal("last session did not release source")
	}
}

func TestPlanLeaseRetainsSourceWhileSessionOrPlanActive(t *testing.T) {
	h := newLeaseHarness(t)
	ctx := context.Background()
	session := h.open(t, h.plan(t))
	if _, err := h.b.StartStream(ctx, h.access(), session.ID, frontend.StreamRequest{Format: frontend.StreamHLS}); err != nil {
		t.Fatal(err)
	}
	pending := h.plan(t)
	if err := h.b.Stop(ctx, h.access(), session.ID); err != nil {
		t.Fatal(err)
	}
	if len(h.playback.Sessions()) != 1 || len(h.tc.sessions) != 0 {
		t.Fatal("stop failed to retain shared plan source or release encoder")
	}
	next := h.open(t, pending)
	if err := h.b.Stop(ctx, h.access(), next.ID); err != nil {
		t.Fatal(err)
	}
	if len(h.playback.Sessions()) != 0 {
		t.Fatal("last lease leaked source")
	}
	waitLease(t, func() bool {
		h.b.mu.Lock()
		defer h.b.mu.Unlock()
		return h.b.planSweepStop == nil
	})
}

func TestPlanLeaseExpiresAndForeignOpenCannotCancel(t *testing.T) {
	h := newLeaseHarness(t)
	p := h.plan(t)
	ctx := context.Background()
	_, err := h.b.Open(ctx, frontend.OpenRequest{Viewer: &core.Viewer{ID: 3}, Device: h.device, Protocol: h.req.Protocol, PlanID: p.ID})
	if !errors.Is(err, core.ErrUnauthorized) || len(h.playback.Sessions()) != 1 {
		t.Fatalf("foreign open affected lease: %v", err)
	}
	h.now.Add(int64(3 * time.Minute))
	waitLease(t, func() bool { return len(h.playback.Sessions()) == 0 })
	h.b.mu.Lock()
	remaining := len(h.b.plans)
	h.b.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("expired plans retained: %d", remaining)
	}
}

func TestPlanLeaseViewerCapAndShutdown(t *testing.T) {
	h := newLeaseHarness(t)
	for i := 0; i < maxViewerPlans; i++ {
		h.plan(t)
	}
	if _, err := h.b.Plan(context.Background(), h.req); !errors.Is(err, core.ErrStreamLimit) {
		t.Fatalf("owner plan cap: %v", err)
	}
	if len(h.playback.Sessions()) != 1 {
		t.Fatal("cap admitted extra sources")
	}
	h.b.Close(context.Background())
	h.b.mu.Lock()
	plans, leases := len(h.b.plans), len(h.b.sourceLeases)
	h.b.mu.Unlock()
	if plans != 0 || leases != 0 || len(h.playback.Sessions()) != 0 {
		t.Fatalf("shutdown retained plans=%d leases=%d sources=%d", plans, leases, len(h.playback.Sessions()))
	}
	waitLease(t, func() bool { h.b.mu.Lock(); defer h.b.mu.Unlock(); return h.b.planSweepDone == nil })
}

func TestPlanLeaseShutdownRetriesHeldOwnerGate(t *testing.T) {
	h := newLeaseHarness(t)
	h.plan(t)
	unlock, err := h.b.gates.Acquire(context.Background(), fmt.Sprintf("broker/owner/%d/%s", h.viewer.ID, clientKey(h.req.Protocol, h.device)))
	if err != nil {
		t.Fatal(err)
	}
	h.b.shutdownTimeout = 20 * time.Millisecond
	h.b.Close(context.Background())
	if len(h.playback.Sessions()) != 1 {
		t.Fatal("shutdown stopped source without owner gate")
	}
	unlock()
	waitLease(t, func() bool { return len(h.playback.Sessions()) == 0 })
	waitLease(t, func() bool {
		h.b.mu.Lock()
		defer h.b.mu.Unlock()
		return len(h.b.plans) == 0 && len(h.b.sourceLeases) == 0 && h.b.planSweepDone == nil
	})
}

func TestPlanLeaseConcurrentPlansShareAndDrain(t *testing.T) {
	h := newLeaseHarness(t)
	const n = 8
	start := make(chan struct{})
	plans := make(chan frontend.PlayPlan, n)
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); <-start; p, e := h.b.Plan(context.Background(), h.req); plans <- p; errs <- e }()
	}
	close(start)
	wg.Wait()
	close(plans)
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	var all []frontend.PlayPlan
	for p := range plans {
		all = append(all, p)
	}
	if len(h.playback.Sessions()) != 1 {
		t.Fatal("concurrent plans did not share source")
	}
	h.allowed.Store(false)
	for _, p := range all {
		_, _ = h.b.Open(context.Background(), frontend.OpenRequest{Viewer: h.viewer, Device: h.device, Protocol: h.req.Protocol, PlanID: p.ID})
	}
	if len(h.playback.Sessions()) != 0 {
		t.Fatal("failed concurrent plans leaked source")
	}
}

func TestCanceledOpenExpiresWithoutStoppingForeignOrSharedSource(t *testing.T) {
	h := newLeaseHarness(t)
	ctx := context.Background()
	first, second := h.plan(t), h.plan(t)
	unlock, err := h.b.gates.Acquire(ctx, fmt.Sprintf("broker/owner/%d/%s", h.viewer.ID, clientKey(h.req.Protocol, h.device)))
	if err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err = h.b.Open(canceled, frontend.OpenRequest{Viewer: h.viewer, Device: h.device, Protocol: h.req.Protocol, PlanID: first.ID}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled open: %v", err)
	}
	unlock()
	waitLease(t, func() bool { h.b.mu.Lock(); defer h.b.mu.Unlock(); _, ok := h.b.plans[first.ID]; return !ok })
	if len(h.playback.Sessions()) != 1 {
		t.Fatal("canceled open stopped another plan source")
	}
	session := h.open(t, second)
	if err := h.b.Stop(ctx, h.access(), session.ID); err != nil {
		t.Fatal(err)
	}
	if len(h.playback.Sessions()) != 0 {
		t.Fatal("source remained after last owner stopped")
	}
}

func TestFailedPlanNeverStopsPreexistingSource(t *testing.T) {
	h := newLeaseHarness(t)
	ctx := context.Background()
	caps := core.ClientCaps{ClientID: clientKey(h.req.Protocol, h.device), Protocol: h.req.Protocol}
	source, err := h.playback.Prepare(ctx, h.viewer, h.req.Item, caps)
	if err != nil {
		t.Fatal(err)
	}
	p := h.plan(t)
	h.allowed.Store(false)
	if _, err = h.b.Open(ctx, frontend.OpenRequest{Viewer: h.viewer, Device: h.device, Protocol: h.req.Protocol, PlanID: p.ID}); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("open failure: %v", err)
	}
	if _, ok := h.playback.Session(source.ID); !ok {
		t.Fatal("failed plan stopped preexisting source")
	}
	h.b.Close(ctx)
	if _, ok := h.playback.Session(source.ID); !ok {
		t.Fatal("broker shutdown stopped an unowned source")
	}
}
