package sessions

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestVersionHandlesBindParentViewerDeviceAndSurface(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	active := func(_ context.Context, id int64) (*core.Viewer, error) {
		if id == 3 {
			return nil, core.ErrUnauthorized
		}
		return &core.Viewer{ID: id}, nil
	}
	events := NewEventBus(nil, active)
	defer events.Close()
	offered := true
	playback := &fakePlayback{sessions: map[string]*core.PlaySession{}, limit: 2}
	b, err := NewBroker(BrokerDeps{Events: events, Playback: playback, Progress: &fakeProgress{}, ActiveViewer: active, CanPlay: func(*core.Viewer, bool, time.Time) bool { return true }, BitrateLimit: func(*core.Viewer) int { return 0 }, OnTimeline: func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {}, Lookup: func(_ context.Context, _ *core.Viewer, _ core.Device, k core.RatingKey) (*core.Title, error) {
		return &core.Title{Key: k}, nil
	}, Variants: func(context.Context, *core.Viewer, *core.Title, core.ClientCaps) ([]core.ReleaseVariant, error) {
		if !offered {
			return nil, nil
		}
		return []core.ReleaseVariant{{ID: "release-secret"}}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close(ctx)
	jelly, err := NewVersionHandles(db, b, "jellyfin")
	if err != nil {
		t.Fatal(err)
	}
	emby, err := NewVersionHandles(db, b, "emby")
	if err != nil {
		t.Fatal(err)
	}
	scope := frontend.VersionScope{Viewer: &core.Viewer{ID: 1}, Device: core.Device{ID: "device"}, Protocol: "jellyfin"}
	item := core.MovieKey(1)
	// Concurrent issuance must return one stable handle without opening a source.
	var wg sync.WaitGroup
	tokens := make(chan string, 4)
	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			token, err := jelly.Issue(ctx, scope, item, "release-secret")
			tokens <- token
			errs <- err
		}()
	}
	wg.Wait()
	close(tokens)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var handle string
	for token := range tokens {
		if handle == "" {
			handle = token
		}
		if token != handle || strings.Contains(token, "release-secret") {
			t.Fatalf("unstable/leaking handle %q", token)
		}
	}
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(handle) {
		t.Fatalf("version handle is not a canonical UUIDv4: %q", handle)
	}
	if playback.prepares != 0 {
		t.Fatal("handle issuance resolved a release")
	}
	target, err := jelly.Resolve(ctx, scope, handle)
	if err != nil || target.Item != item {
		t.Fatalf("resolve: %+v %v", target, err)
	}
	target, err = jelly.Resolve(ctx, scope, strings.ToUpper(strings.ReplaceAll(handle, "-", "")))
	if err != nil || target.Item != item {
		t.Fatalf("resolve undashed UUID: %+v %v", target, err)
	}
	second, err := jelly.Issue(ctx, scope, core.MovieKey(2), "release-secret")
	if err != nil || second == handle {
		t.Fatalf("same release aliased another parent: %q %v", second, err)
	}
	for _, bad := range []frontend.VersionScope{{Viewer: &core.Viewer{ID: 2}, Device: scope.Device, Protocol: "jellyfin"}, {Viewer: scope.Viewer, Device: core.Device{ID: "other"}, Protocol: "jellyfin"}, {Viewer: scope.Viewer, Device: scope.Device, Protocol: "emby"}} {
		h := jelly
		if bad.Protocol == "emby" {
			h = emby
		}
		if _, err := h.Resolve(ctx, bad, handle); !errors.Is(err, core.ErrNotFound) {
			t.Fatalf("cross-scope resolved: %+v %v", bad, err)
		}
	}
	offered = false
	if _, err = jelly.Resolve(ctx, scope, handle); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("withdrawn version resolved: %v", err)
	}
}

type exactVersionPlayback struct {
	*fakePlayback
	calls int
	wrong bool
}

func (p *exactVersionPlayback) PrepareExactVariant(ctx context.Context, v *core.User, key core.RatingKey, caps core.ClientCaps, release string) (*core.PlaySession, error) {
	p.calls++
	ps, err := p.fakePlayback.Prepare(ctx, v, key, caps)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	selected := release
	if p.wrong {
		selected = "other-release"
	}
	ps.Resolution = &core.Resolution{Release: core.Release{ID: selected}}
	return ps, nil
}

func TestExactVersionHandlePlanOpenAndSessionBinding(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	active := func(_ context.Context, id int64) (*core.Viewer, error) { return &core.Viewer{ID: id}, nil }
	bus := NewEventBus(nil, active)
	defer bus.Close()
	playback := &exactVersionPlayback{fakePlayback: &fakePlayback{sessions: map[string]*core.PlaySession{}, limit: 4}}
	variantCalls := 0
	broker, err := NewBroker(BrokerDeps{DB: db, Events: bus, Playback: playback, Transcoder: fakeBrokerTranscoder{}, Progress: &fakeProgress{}, ActiveViewer: active, CanPlay: func(*core.Viewer, bool, time.Time) bool { return true }, BitrateLimit: func(*core.Viewer) int { return 0 }, OnTimeline: func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {}, Lookup: func(_ context.Context, _ *core.Viewer, _ core.Device, k core.RatingKey) (*core.Title, error) {
		return &core.Title{Key: k}, nil
	}, Variants: func(context.Context, *core.Viewer, *core.Title, core.ClientCaps) ([]core.ReleaseVariant, error) {
		variantCalls++
		return []core.ReleaseVariant{{ID: "release-secret"}}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer broker.Close(ctx)
	service, err := NewVersionHandles(db, broker, "jellyfin")
	if err != nil {
		t.Fatal(err)
	}
	viewer, device := &core.Viewer{ID: 1}, core.Device{ID: "tv"}
	scope := frontend.VersionScope{Viewer: viewer, Device: device, Protocol: "jellyfin"}
	item := core.MovieKey(7)
	handle, err := service.Issue(ctx, scope, item, "release-secret")
	if err != nil {
		t.Fatal(err)
	}
	req := frontend.PlanRequest{Viewer: viewer, Device: device, Protocol: "jellyfin", Item: item, Version: handle, Intent: frontend.PlayIntent{ExactVersion: true, Estimate: true, Options: core.DecideOptions{DirectPlay: true}}}
	before := variantCalls
	estimate, err := broker.Plan(ctx, req)
	if err != nil || !estimate.Estimated || playback.calls != 0 || variantCalls != before {
		t.Fatalf("estimate resolved candidate: %+v %v", estimate, err)
	}
	req.Intent.Estimate = false
	plan, err := broker.Plan(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	session, err := broker.Open(ctx, frontend.OpenRequest{Protocol: "jellyfin", PlanID: plan.ID, Viewer: viewer, Device: device})
	if err != nil {
		t.Fatal(err)
	}
	if err = service.MatchSession(ctx, scope, handle, session.ID); err != nil {
		t.Fatalf("valid match: %v", err)
	}
	if err = service.MatchSession(ctx, scope, strings.ToUpper(strings.ReplaceAll(handle, "-", "")), session.ID); err != nil {
		t.Fatalf("undashed UUID match: %v", err)
	}
	other := scope
	other.Device.ID = "foreign"
	if err = service.MatchSession(ctx, other, handle, session.ID); err == nil {
		t.Fatal("foreign device matched")
	}
	other = scope
	other.Viewer = &core.Viewer{ID: 2}
	if err = service.MatchSession(ctx, other, handle, session.ID); err == nil {
		t.Fatal("foreign viewer matched")
	}
	if err = service.MatchSession(ctx, scope, handle, "missing"); err == nil {
		t.Fatal("missing session matched")
	}
	playback.mu.Lock()
	for _, ps := range playback.sessions {
		ps.Resolution = &core.Resolution{Release: core.Release{ID: "different"}}
	}
	playback.mu.Unlock()
	if err = service.MatchSession(ctx, scope, handle, session.ID); err == nil {
		t.Fatal("changed source matched")
	}
	req.Device = core.Device{ID: "another"}
	if _, err = broker.Plan(ctx, req); err == nil {
		t.Fatal("foreign device planned with handle")
	}
	otherScope := scope
	otherScope.Device = req.Device
	otherHandle, err := service.Issue(ctx, otherScope, item, "release-secret")
	if err != nil {
		t.Fatal(err)
	}
	req.Version = otherHandle
	playback.wrong = true
	beforeSessions := len(playback.Sessions())
	if _, err = broker.Plan(ctx, req); !errors.Is(err, core.ErrNotAvailable) {
		t.Fatalf("incorrect source accepted: %v", err)
	}
	if len(playback.Sessions()) != beforeSessions {
		t.Fatal("incorrect source was left open")
	}
}
