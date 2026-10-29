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
