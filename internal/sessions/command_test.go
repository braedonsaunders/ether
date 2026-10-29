package sessions

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

func TestCommandAuthorizationAndTargetedDelivery(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	active := func(_ context.Context, id int64) (*core.Viewer, error) {
		return &core.Viewer{ID: id, IsOwner: id == 1}, nil
	}
	events := NewEventBus(nil, active)
	defer events.Close()
	b, err := NewBroker(BrokerDeps{Events: events, Playback: &fakePlayback{sessions: map[string]*core.PlaySession{}, limit: 2}, Transcoder: fakeBrokerTranscoder{}, Progress: &fakeProgress{}, ActiveViewer: active, CanPlay: func(*core.Viewer, bool, time.Time) bool { return true }, BitrateLimit: func(*core.Viewer) int { return 0 }, OnTimeline: func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {}, Lookup: func(_ context.Context, _ *core.Viewer, _ core.Device, k core.RatingKey) (*core.Title, error) {
		return &core.Title{Key: k}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close(context.Background())
	viewer, device := &core.Viewer{ID: 2}, core.Device{ID: "phone"}
	p, err := b.Plan(ctx, frontend.PlanRequest{Viewer: viewer, Device: device, Protocol: "jellyfin", Item: core.MovieKey(1), Intent: frontend.PlayIntent{Options: core.DecideOptions{DirectPlay: true}}})
	if err != nil {
		t.Fatal(err)
	}
	s, err := b.Open(ctx, frontend.OpenRequest{Viewer: viewer, Device: device, Protocol: "jellyfin", PlanID: p.ID})
	if err != nil {
		t.Fatal(err)
	}
	target, err := events.Subscribe(ctx, frontend.EventScope{Viewer: viewer, Protocol: "jellyfin", SessionID: s.ID})
	if err != nil {
		t.Fatal(err)
	}
	other, err := events.Subscribe(ctx, frontend.EventScope{Viewer: viewer, Protocol: "jellyfin", SessionID: "other"})
	if err != nil {
		t.Fatal(err)
	}
	wrongSurface, err := events.Subscribe(ctx, frontend.EventScope{Viewer: viewer, Protocol: "plex", SessionID: s.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err = b.Command(ctx, &core.Viewer{ID: 3}, s.ID, frontend.Command{Kind: frontend.CommandPause}); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("foreign command %v", err)
	}
	if err = b.Command(ctx, viewer, s.ID, frontend.Command{Kind: frontend.CommandSeek, Position: time.Minute}); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-target:
		if got.Command == nil || got.Command.Kind != frontend.CommandSeek || got.Command.Position != time.Minute {
			t.Fatalf("command %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("missing target command")
	}
	for _, ch := range []<-chan frontend.Event{other, wrongSurface} {
		select {
		case got := <-ch:
			t.Fatalf("command leaked %+v", got)
		case <-time.After(20 * time.Millisecond):
		}
	}
	if err = b.Command(ctx, &core.Viewer{ID: 1}, s.ID, frontend.Command{Kind: frontend.CommandStop}); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-target:
		if got.Command == nil || got.Command.Kind != frontend.CommandStop {
			t.Fatalf("stop %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("missing stop command")
	}
	list, err := b.Sessions(ctx, frontend.SessionAccess{Viewer: viewer})
	if err != nil || len(list) != 0 {
		t.Fatalf("stop did not end session: %+v %v", list, err)
	}
}
