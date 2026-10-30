package plex

import (
	"context"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/state"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestFrontendWatchSubscriptionFollowsMount(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	ctx := context.Background()
	watch := state.New(store.OpenTest(t), state.Options{})
	defer watch.Close(ctx)
	adapter := Frontend{Server: f.srv, WatchState: watch}
	sub, ok := f.srv.notifier.subscribe([]string{"timeline"}, &ownerUser)
	if !ok {
		t.Fatal("notification subscription failed")
	}
	defer f.srv.notifier.unsubscribe(sub)
	change := func(surface string, want bool) {
		t.Helper()
		if err := watch.Scrobble(state.WithOrigin(ctx, state.Origin{Surface: surface}), ownerUser.ID, core.MovieKey(5)); err != nil {
			t.Fatal(err)
		}
		select {
		case <-sub.ch:
			if !want {
				t.Fatal("unexpected refresh while disabled or from Plex")
			}
		default:
			if want {
				t.Fatal("missing cross-surface refresh")
			}
		}
		select {
		case <-sub.ch:
			t.Fatal("duplicate refresh")
		default:
		}
	}
	change("jellyfin", false)
	for range 2 {
		instance, err := adapter.Mount(ctx, frontend.Host{})
		if err != nil {
			t.Fatal(err)
		}
		change("plex", false)
		change("jellyfin", true)
		if err := instance.Close(ctx); err != nil {
			t.Fatal(err)
		}
		if err := instance.Close(ctx); err != nil {
			t.Fatal(err)
		}
		change("emby", false)
	}
}
