package sessions

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestDeviceCapabilitiesPersistAndStayScoped(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	active := func(_ context.Context, id int64) (*core.Viewer, error) { return &core.Viewer{ID: id}, nil }
	events := NewEventBus(nil, active)
	defer events.Close()
	mk := func() *Broker {
		b, err := NewBroker(BrokerDeps{DB: db, Events: events, Playback: &fakePlayback{sessions: map[string]*core.PlaySession{}, limit: 2}, Transcoder: fakeBrokerTranscoder{}, Progress: &fakeProgress{}, ActiveViewer: active, CanPlay: func(*core.Viewer, bool, time.Time) bool { return true }, BitrateLimit: func(*core.Viewer) int { return 0 }, OnTimeline: func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {}, Lookup: func(_ context.Context, _ *core.Viewer, _ core.Device, k core.RatingKey) (*core.Title, error) {
			return &core.Title{Key: k}, nil
		}})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { b.Close(ctx) })
		return b
	}
	a, b := mk(), mk()
	access := frontend.SessionAccess{Viewer: &core.Viewer{ID: 2}, Protocol: "jellyfin", Device: core.Device{ID: "phone"}}
	yes := true
	declared := frontend.SessionCapabilities{PlayableMediaTypes: []string{"Video"}, SupportedCommands: []string{"Pause"}, SupportsRemoteControl: &yes, DeviceProfile: []byte(`{"Name":"Profile"}`), IconURL: "https://example.invalid/icon"}
	if err := a.UpdateCapabilities(ctx, access, declared); err != nil {
		t.Fatal(err)
	}
	declared.SupportedCommands[0] = "changed"
	yes = false
	got, err := b.Capabilities(ctx, access)
	if err != nil || got.SupportedCommands[0] != "Pause" || got.SupportsRemoteControl == nil || !*got.SupportsRemoteControl || got.SupportsMediaControl != nil || got.IconURL != "https://example.invalid/icon" {
		t.Fatalf("round trip %+v %v", got, err)
	}
	got.SupportedCommands[0] = "modified"
	again, err := a.Capabilities(ctx, access)
	if err != nil || again.SupportedCommands[0] != "Pause" {
		t.Fatalf("caller mutated saved capabilities %+v %v", again, err)
	}
	plan, err := b.Plan(ctx, frontend.PlanRequest{Viewer: access.Viewer, Device: access.Device, Protocol: access.Protocol, Item: core.MovieKey(1), Intent: frontend.PlayIntent{Options: core.DecideOptions{DirectPlay: true}}})
	if err != nil {
		t.Fatal(err)
	}
	opened, err := b.Open(ctx, frontend.OpenRequest{Viewer: access.Viewer, Device: access.Device, Protocol: access.Protocol, PlanID: plan.ID})
	if err != nil || opened.Capabilities == nil || opened.Capabilities.SupportedCommands[0] != "Pause" {
		t.Fatalf("session capability metadata %+v %v", opened, err)
	}
	opened.Capabilities.SupportedCommands[0] = "tampered"
	b.mu.Lock()
	kept := b.sessions[opened.ID].public.Capabilities.SupportedCommands[0]
	b.mu.Unlock()
	if kept != "Pause" {
		t.Fatal("caller mutated broker-owned session capabilities")
	}
	for _, other := range []frontend.SessionAccess{{Viewer: &core.Viewer{ID: 3}, Protocol: "jellyfin", Device: core.Device{ID: "phone"}}, {Viewer: access.Viewer, Protocol: "emby", Device: access.Device}, {Viewer: access.Viewer, Protocol: "jellyfin", Device: core.Device{ID: "other"}}} {
		v, err := b.Capabilities(ctx, other)
		if err != nil || len(v.SupportedCommands) != 0 {
			t.Fatalf("scope leaked %+v %v", v, err)
		}
	}
	if err := a.UpdateCapabilities(ctx, frontend.SessionAccess{Viewer: access.Viewer, Protocol: "jellyfin"}, declared); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("empty device accepted %v", err)
	}
	declared.DeviceProfile = []byte(`{`)
	if err := a.UpdateCapabilities(ctx, access, declared); !errors.Is(err, core.ErrNotAvailable) {
		t.Fatalf("invalid profile accepted %v", err)
	}
}
