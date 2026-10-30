package plex

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/state"
	"github.com/braedonsaunders/ether/internal/store"
)

// Reproduce finishing a resumed episode, autoplaying ten seconds of the next,
// and returning home. Notifications must observe committed state, not the old
// halfway resume point; ordinary heartbeats must not reshuffle library shelves.
func TestWatchRefreshAfterCompletionAndAutoplayStop(t *testing.T) {
	db := store.OpenTest(t)
	watch := state.New(db, state.Options{})
	ctx := context.Background()
	first, next := core.EpisodeKey(40290, 16, 3), core.EpisodeKey(40290, 16, 4)
	const user = int64(7)
	var notifications []core.WatchStatus
	unsubscribe := subscribeWatchRefresh(watch, func(u int64, k core.RatingKey) {
		if u != user {
			t.Fatalf("wrong user %d", u)
		}
		st, err := watch.Get(ctx, u, k)
		if err != nil {
			t.Fatal(err)
		}
		notifications = append(notifications, st)
	}, false)
	defer unsubscribe()
	progress := func(k core.RatingKey, sec int, mode string) {
		t.Helper()
		if err := watch.Progress(ctx, user, k, time.Duration(sec)*time.Second, 40*time.Minute, mode); err != nil {
			t.Fatal(err)
		}
	}
	progress(first, 1200, "playing")
	progress(first, 1250, "playing")
	if len(notifications) != 0 {
		t.Fatal("ordinary playing heartbeats refresh the library")
	}
	progress(first, 2390, "playing")
	if len(notifications) != 1 || notifications[0].ViewCount != 1 || notifications[0].ViewOffset != 0 {
		t.Fatalf("completion missing or delivered before commit: %+v", notifications)
	}
	progress(first, 2400, "stopped")
	progress(next, 0, "playing")
	progress(next, 10, "playing")
	progress(next, 10, "stopped")
	if len(notifications) != 3 || notifications[2].Key != next || notifications[2].ViewCount != 0 || notifications[2].ViewOffset != 0 {
		t.Fatalf("autoplay exit did not invalidate the next episode: %+v", notifications)
	}
	progress(next, 600, "paused")
	if len(notifications) != 4 || notifications[3].ViewOffset != 10*time.Minute {
		t.Fatalf("pause refresh: %+v", notifications)
	}
	// Crossing completion on a stop emits both Progress and Completed internally,
	// but should produce one library invalidation.
	progress(next, 2400, "stopped")
	if len(notifications) != 5 {
		t.Fatalf("duplicate completion invalidations: %d", len(notifications))
	}
	unsubscribe()
	progress(next, 600, "paused")
	if len(notifications) != 5 {
		t.Fatal("subscription leaked after shutdown")
	}
}

func TestWatchRefreshUnmountWaitsForExecutingCallback(t *testing.T) {
	ctx := context.Background()
	watch := state.New(store.OpenTest(t), state.Options{})
	defer watch.Close(ctx)
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	releaseCallback := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseCallback()
	var calls atomic.Int32
	unsub := subscribeWatchRefresh(watch, func(int64, core.RatingKey) { calls.Add(1); close(entered); <-release }, false)
	updateDone := make(chan error, 1)
	go func() {
		updateDone <- watch.Scrobble(state.WithOrigin(ctx, state.Origin{Surface: "jellyfin"}), 1, core.MovieKey(9))
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("callback did not run")
	}
	closed := make(chan struct{})
	go func() { unsub(); close(closed) }()
	select {
	case <-closed:
		t.Fatal("unmount returned with executing callback")
	case <-time.After(20 * time.Millisecond):
	}
	releaseCallback()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("unmount did not finish")
	}
	if err := <-updateDone; err != nil {
		t.Fatal(err)
	}
	unsub()
	if err := watch.Unscrobble(ctx, 1, core.MovieKey(9)); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatal("callback executed after unmount")
	}
}
