package playback

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/sessions"
)

func TestDetailPlanSingleFlightAndPlayReusesProbe(t *testing.T) {
	ctx := context.Background()
	release := &fakeRelease{ID: "cold", Name: "cold.mp4", Size: 15360, Duration: time.Minute}
	alternate := mkv("alternate", 16384, time.Minute)
	h := newHarness(t, []*fakeRelease{release, alternate}, []string{"cold", "alternate"})
	h.resolver.delay = 100 * time.Millisecond
	var allowed atomic.Bool
	allowed.Store(true)
	active := func(_ context.Context, id int64) (*core.Viewer, error) {
		if id != friend.ID {
			return nil, core.ErrUnauthorized
		}
		v := *friend
		return &v, nil
	}
	events := sessions.NewEventBus(nil, active)
	defer events.Close()
	lookup := func(ctx context.Context, _ *core.Viewer, _ core.Device, key core.RatingKey) (*core.Title, error) {
		if !allowed.Load() {
			return nil, core.ErrNotFound
		}
		return h.catalog.Get(ctx, key)
	}
	b, err := sessions.NewBroker(sessions.BrokerDeps{Lookup: lookup, MediaLookup: lookup, Playback: h.m, Transcoder: h.tr, Progress: h.state, ActiveViewer: active, CanPlay: func(*core.Viewer, bool, time.Time) bool { return true }, BitrateLimit: func(*core.Viewer) int { return 0 }, OnTimeline: func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {}, Events: events, Now: h.clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close(ctx)
	device := core.Device{ID: "device"}
	req := frontend.PlanRequest{Viewer: friend, Device: device, Protocol: "jellyfin", Item: movieKey, Caps: defaultCaps(device.ID), Intent: frontend.PlayIntent{Options: core.DecideOptions{DirectPlay: true, DirectStream: true}}}
	const count = 12
	results := make(chan frontend.PlayPlan, count)
	errs := make(chan error, count)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); <-start; p, e := b.DetailPlan(ctx, req); results <- p; errs <- e }()
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	for p := range results {
		if p.ID != "" || p.Estimated || p.Media.Estimated || p.Media.Container != "mp4" || p.Media.Size != release.Size || !p.DirectPlay {
			t.Fatalf("invalid detail plan: %+v", p)
		}
	}
	if h.engine.openCount() != 1 {
		t.Fatalf("detail opened %d sources", h.engine.openCount())
	}
	if sources := h.m.Sessions(); len(sources) != 1 || !sources[0].PreparedOnly {
		t.Fatalf("detail source must stay hidden from Activity: %+v", sources)
	}
	h.tr.mu.Lock()
	probes := h.tr.probes
	h.tr.mu.Unlock()
	if probes != 1 {
		t.Fatalf("detail probed %d times", probes)
	}
	rows, err := b.Sessions(ctx, frontend.SessionAccess{Viewer: friend, Device: device, Protocol: req.Protocol})
	if err != nil || len(rows) != 0 {
		t.Fatalf("detail created frontend session: %+v %v", rows, err)
	}
	plan, err := b.Plan(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	session, err := b.Open(ctx, frontend.OpenRequest{Viewer: friend, Device: device, Protocol: req.Protocol, PlanID: plan.ID})
	if err != nil {
		t.Fatal(err)
	}
	if sources := h.m.Sessions(); len(sources) != 1 || sources[0].PreparedOnly {
		t.Fatalf("opened playback must be visible in Activity: %+v", sources)
	}
	h.tr.mu.Lock()
	probes = h.tr.probes
	h.tr.mu.Unlock()
	if probes != 1 || h.engine.openCount() != 1 {
		t.Fatalf("play repeated open/probe: opens=%d probes=%d", h.engine.openCount(), probes)
	}
	sources := h.m.Sessions()
	if len(sources) != 1 {
		t.Fatalf("unexpected sources: %+v", sources)
	}
	if _, err := h.m.Failover(ctx, sources[0].ID, 0); err != nil {
		t.Fatal(err)
	}
	refreshed, err := b.DetailPlan(ctx, req)
	if err != nil || refreshed.Media.Container != "mkv" || refreshed.Media.Size != alternate.Size {
		t.Fatalf("detail cache survived source failover: %+v %v", refreshed, err)
	}
	if err := b.Stop(ctx, frontend.SessionAccess{Viewer: friend, Device: device, Protocol: req.Protocol}, session.ID); err != nil {
		t.Fatal(err)
	}
	if sources := h.m.Sessions(); len(sources) != 1 || !sources[0].PreparedOnly {
		t.Fatalf("remaining plan source must be hidden after stop: %+v", sources)
	}
	allowed.Store(false)
	if _, err := b.DetailPlan(ctx, req); err == nil {
		t.Fatal("cached detail ignored policy revocation")
	}
	allowed.Store(true)
	h.resolver.mu.Lock()
	h.resolver.delay = 250 * time.Millisecond
	h.resolver.mu.Unlock()
	timed, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	req.Item = movie2
	if _, err := b.DetailPlan(timed, req); err == nil {
		t.Fatal("canceled detail unexpectedly succeeded")
	}
	time.Sleep(300 * time.Millisecond)
	if h.engine.openCount() != 2 {
		t.Fatalf("canceled detail opened an extra source: %d", h.engine.openCount())
	}
}
