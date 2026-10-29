package sessions

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

func TestSiloSessionIDsAreRandomUUIDsForVideoAndLive(t *testing.T) {
	ctx := context.Background()
	active := func(_ context.Context, id int64) (*core.Viewer, error) { return &core.Viewer{ID: id}, nil }
	events := NewEventBus(nil, active)
	defer events.Close()
	live := &fakeLive{}
	b, err := NewBroker(BrokerDeps{Live: live, Events: events, Playback: &fakePlayback{sessions: map[string]*core.PlaySession{}, limit: 4}, Progress: &fakeProgress{}, ActiveViewer: active, CanPlay: func(*core.Viewer, bool, time.Time) bool { return true }, BitrateLimit: func(*core.Viewer) int { return 0 }, OnTimeline: func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {}, Lookup: func(_ context.Context, _ *core.Viewer, _ core.Device, k core.RatingKey) (*core.Title, error) {
		return &core.Title{Key: k}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close(ctx)
	pattern := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	viewer := &core.Viewer{ID: 1}
	seen := map[string]bool{}
	for _, channel := range []bool{false, true} {
		device := core.Device{ID: "video"}
		req := frontend.PlanRequest{Viewer: viewer, Device: device, Protocol: "silo", Item: core.MovieKey(1), Intent: frontend.PlayIntent{Options: core.DecideOptions{DirectPlay: true}}}
		if channel {
			device.ID = "live"
			req.Device = device
			req.Item = 0
			req.Channel = "channel"
		}
		plan, err := b.Plan(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		session, err := b.Open(ctx, frontend.OpenRequest{Viewer: viewer, Device: device, Protocol: "silo", PlanID: plan.ID})
		if err != nil {
			t.Fatal(err)
		}
		if !pattern.MatchString(session.ID) || seen[session.ID] {
			t.Fatalf("invalid or repeated UUID %q", session.ID)
		}
		seen[session.ID] = true
		parts := strings.Split(session.MediaPath, "/")
		if len(parts) < 5 || parts[4] == session.ID || parts[4] == "" {
			t.Fatalf("grant not independent: %q", session.MediaPath)
		}
	}
	for i := 0; i < 128; i++ {
		id, err := sessionID("silo")
		if err != nil || !pattern.MatchString(id) || seen[id] {
			t.Fatalf("UUID %q err %v", id, err)
		}
		seen[id] = true
	}
	plex, err := sessionID("plex")
	if err != nil || pattern.MatchString(plex) || len(plex) != 26 {
		t.Fatalf("Plex ID changed: %q %v", plex, err)
	}
}
