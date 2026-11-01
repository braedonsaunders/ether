package playback

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/sessions"
)

func TestLegacyPlexAndBrokerShareAdmission(t *testing.T) {
	for _, tc := range []struct {
		name      string
		configure harnessOpt
	}{
		{"server", func(h *harness, _ *Options) { h.cfg.Server.MaxStreams = 1 }},
		{"viewer", func(h *harness, _ *Options) { h.cfg.Server.DefaultStreamLimit = 1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			h := newHarness(t, []*fakeRelease{mkv("A", 10000, time.Minute)}, []string{"A"}, tc.configure)
			active := func(_ context.Context, id int64) (*core.Viewer, error) {
				if id == friend.ID {
					v := *friend
					return &v, nil
				}
				if id == owner.ID {
					v := *owner
					return &v, nil
				}
				return nil, core.ErrUnauthorized
			}
			events := sessions.NewEventBus(nil, active)
			defer events.Close()
			b, err := sessions.NewBroker(sessions.BrokerDeps{Lookup: func(ctx context.Context, _ *core.Viewer, _ core.Device, k core.RatingKey) (*core.Title, error) {
				return h.catalog.Get(ctx, k)
			}, Playback: h.m, Transcoder: h.tr, Progress: h.state, ActiveViewer: active, CanPlay: func(*core.Viewer, bool, time.Time) bool { return true }, BitrateLimit: func(*core.Viewer) int { return 0 }, OnTimeline: func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {}, Events: events, Now: h.clock.Now})
			if err != nil {
				t.Fatal(err)
			}
			defer b.Close(ctx)
			legacyCaps := defaultCaps("plex-device")
			legacyCaps.Protocol = "plex"
			legacy, err := h.m.Prepare(ctx, friend, movieKey, legacyCaps)
			if err != nil {
				t.Fatal(err)
			}
			device := core.Device{ID: "jf-device"}
			req := frontend.PlanRequest{Viewer: friend, Device: device, Protocol: "jellyfin", Item: movie2, Caps: defaultCaps("jf-device"), Intent: frontend.PlayIntent{Options: core.DecideOptions{DirectPlay: true, DirectStream: true}}}
			if _, err := b.Plan(ctx, req); !errors.Is(err, core.ErrStreamLimit) {
				t.Fatalf("Plex did not consume broker slot: %v", err)
			}
			h.m.Stop(legacy.ID)
			plan, err := b.Plan(ctx, req)
			if err != nil {
				t.Fatal(err)
			}
			session, err := b.Open(ctx, frontend.OpenRequest{Viewer: friend, Device: device, Protocol: "jellyfin", PlanID: plan.ID})
			if err != nil {
				t.Fatal(err)
			}
			rows := h.m.Sessions()
			if len(rows) != 1 || rows[0].Client.Protocol != "jellyfin" {
				t.Fatalf("broker activity rows: %+v", rows)
			}
			if _, err := h.m.Prepare(ctx, friend, movie3, defaultCaps("another-plex")); !errors.Is(err, core.ErrStreamLimit) {
				t.Fatalf("broker did not consume Plex slot: %v", err)
			}
			if err := b.Stop(ctx, frontend.SessionAccess{Viewer: friend, Device: device, Protocol: "jellyfin"}, session.ID); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestBrokerScopeCannotShareOrStopCollidingPlexSource(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, []*fakeRelease{mkv("A", 10000, time.Minute)}, []string{"A"})
	caps := defaultCaps("8:jellyfincolliding-device")
	caps.Protocol = "plex"
	plex, err := h.m.Prepare(ctx, friend, movieKey, caps)
	if err != nil {
		t.Fatal(err)
	}
	other := caps
	other.Protocol = "jellyfin"
	broker, err := h.m.PrepareSurface(ctx, friend, movieKey, other, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if plex.ID == broker.ID {
		t.Fatal("colliding legacy and broker identities shared a source")
	}
	again, err := h.m.PrepareSurface(ctx, friend, movieKey, other, "", false)
	if err != nil || again.ID != broker.ID {
		t.Fatalf("broker scope did not reuse its source: %v", err)
	}
	h.m.Stop(broker.ID)
	if _, ok := h.m.Session(plex.ID); !ok {
		t.Fatal("stopping broker source stopped Plex")
	}
	if got, ok := h.m.SessionFor(friend.ID, caps.ClientID, movieKey); !ok || got.ID != plex.ID {
		t.Fatal("legacy lookup lost Plex source")
	}
	h.m.Stop(plex.ID)
}

func TestCollidingScopesEachConsumeStreamLimit(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, []*fakeRelease{mkv("A", 10000, time.Minute)}, []string{"A"}, func(h *harness, _ *Options) { h.cfg.Server.MaxStreams = 1 })
	caps := defaultCaps("8:jellyfincolliding-device")
	caps.Protocol = "plex"
	plex, err := h.m.Prepare(ctx, friend, movieKey, caps)
	if err != nil {
		t.Fatal(err)
	}
	other := caps
	other.Protocol = "jellyfin"
	if _, err := h.m.PrepareSurface(ctx, friend, movieKey, other, "", false); !errors.Is(err, core.ErrStreamLimit) {
		t.Fatalf("collision bypassed limit: %v", err)
	}
	h.m.Stop(plex.ID)
}

func TestRemoteAdmissionCountsCollidingScopesSeparately(t *testing.T) {
	legacy := identity{userID: 7, clientID: "8:jellyfindevice", key: movieKey}
	broker := legacy
	broker.surface = "jellyfin"
	legacyRow := unitRow{id: "legacy", user: legacy.userID, client: clusterClientID(legacy), kind: unitSession}
	brokerRow := unitRow{id: "broker", user: broker.userID, client: clusterClientID(broker), kind: unitSession}
	if user, total := remoteCounts([]unitRow{legacyRow}, broker); user != 1 || total != 1 {
		t.Fatalf("broker omitted Plex unit: %d %d", user, total)
	}
	if user, total := remoteCounts([]unitRow{brokerRow}, legacy); user != 1 || total != 1 {
		t.Fatalf("Plex omitted broker unit: %d %d", user, total)
	}
	if pendingID("node", legacy) == pendingID("node", broker) {
		t.Fatal("scope reservations collided")
	}
}

func TestSurfacePlansDoNotReplaceOtherPreparedSources(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, []*fakeRelease{mkv("A", 10000, time.Minute), mkv("B", 10000, time.Minute)}, []string{"A", "B"})
	caps := defaultCaps("same-device")
	caps.Protocol = "jellyfin"
	first, err := h.m.PrepareSurface(ctx, friend, movieKey, caps, "A", true)
	if err != nil {
		t.Fatal(err)
	}
	second, err := h.m.PrepareSurface(ctx, friend, movieKey, caps, "B", true)
	if err != nil {
		t.Fatal(err)
	}
	third, err := h.m.PrepareSurface(ctx, friend, movie2, caps, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == second.ID || first.ID == third.ID {
		t.Fatal("different plans shared one source")
	}
	for _, id := range []string{first.ID, second.ID, third.ID} {
		if _, ok := h.m.Session(id); !ok {
			t.Fatalf("planning retired source %s", id)
		}
	}
	h.m.Stop(first.ID)
	h.m.Stop(second.ID)
	h.m.Stop(third.ID)
}

// A playback request without a selected version must resolve the real source;
// an estimate is only suitable for browse and cannot describe its bytes.
func TestColdHandlelessBrokerPlaybackUsesOpenedSource(t *testing.T) {
	ctx := context.Background()
	release := &fakeRelease{ID: "cold", Name: "cold.mp4", Size: 15360, Duration: time.Minute}
	h := newHarness(t, []*fakeRelease{release}, []string{"cold"})
	active := func(_ context.Context, id int64) (*core.Viewer, error) {
		if id != friend.ID {
			return nil, core.ErrUnauthorized
		}
		v := *friend
		return &v, nil
	}
	events := sessions.NewEventBus(nil, active)
	defer events.Close()
	b, err := sessions.NewBroker(sessions.BrokerDeps{Lookup: func(ctx context.Context, _ *core.Viewer, _ core.Device, k core.RatingKey) (*core.Title, error) {
		return h.catalog.Get(ctx, k)
	}, Playback: h.m, Transcoder: h.tr, Progress: h.state, ActiveViewer: active, CanPlay: func(*core.Viewer, bool, time.Time) bool { return true }, BitrateLimit: func(*core.Viewer) int { return 0 }, OnTimeline: func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {}, Events: events, Now: h.clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close(ctx)
	device := core.Device{ID: "cold-device"}
	req := frontend.PlanRequest{Viewer: friend, Device: device, Protocol: "jellyfin", Item: movieKey, Caps: defaultCaps(device.ID), Intent: frontend.PlayIntent{Options: core.DecideOptions{DirectPlay: true, DirectStream: true}}}
	if h.engine.openCount() != 0 {
		t.Fatal("fixture must begin cold")
	}
	plan, err := b.Plan(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Estimated || plan.Media.Estimated || plan.Media.Container != "mp4" || plan.Media.Size != release.Size || h.engine.openCount() != 1 {
		t.Fatalf("plan does not describe opened source: %+v opens=%d", plan, h.engine.openCount())
	}
	if !plan.DirectPlay && !plan.DirectStream && !plan.Transcoding {
		t.Fatalf("no playable alternative: %+v", plan)
	}
	session, err := b.Open(ctx, frontend.OpenRequest{Viewer: friend, Device: device, Protocol: req.Protocol, PlanID: plan.ID})
	if err != nil || session.ID == "" {
		t.Fatalf("open: %+v %v", session, err)
	}
	path, err := b.StartStream(ctx, frontend.SessionAccess{Viewer: friend, Device: device, Protocol: req.Protocol}, session.ID, frontend.StreamRequest{Format: frontend.StreamDirect})
	if err != nil || path == "" {
		t.Fatalf("stream: %q %v", path, err)
	}
	if err := b.Stop(ctx, frontend.SessionAccess{Viewer: friend, Device: device, Protocol: req.Protocol}, session.ID); err != nil {
		t.Fatal(err)
	}
}
