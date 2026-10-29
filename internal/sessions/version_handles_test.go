package sessions

import (
	"context"
	"errors"
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
	if playback.prepares != 0 {
		t.Fatal("handle issuance resolved a release")
	}
	target, err := jelly.Resolve(ctx, scope, handle)
	if err != nil || target.Item != item {
		t.Fatalf("resolve: %+v %v", target, err)
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
