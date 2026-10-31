package plex

import (
	"sync"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/state"
)

// State emits only after committing, including deferred cluster writes. Do not
// refresh the library on every playing heartbeat: it churns recommendation rows
// and duplicates the separate session-status notifications. Explicit scrobbles
// already publish their own library notification in the Plex handler.
func subscribeWatchRefresh(watch *state.State, refresh func(int64, core.RatingKey), explicitOnly bool) func() {
	var mu sync.Mutex
	active := true
	unsubscribe := watch.Subscribe(func(ev state.Event) {
		mu.Lock()
		defer mu.Unlock()
		if !active {
			return
		}
		// A Plex-account import is not a local playback timeline. Refresh the
		// library for the mark or rating, and do not treat it as an upload.
		if ev.Account && (ev.Kind == state.EventScrobble || ev.Kind == state.EventUnscrobble || ev.Kind == state.EventRated) {
			refresh(ev.UserID, ev.Key)
			return
		}
		if ev.Imported {
			return
		}

		// Cross-surface explicit state changes previously came through the shared
		// event publisher; Plex's own handlers already send these notifications.
		if ev.Origin.Surface != "plex" && (ev.Kind == state.EventScrobble || ev.Kind == state.EventUnscrobble) {
			refresh(ev.UserID, ev.Key)
			return
		}
		if explicitOnly {
			return
		}
		if ev.Kind == state.EventCompleted ||
			(ev.Kind == state.EventProgress && !ev.Completed && (ev.State == "paused" || ev.State == "stopped")) {
			refresh(ev.UserID, ev.Key)
		}
	})
	return func() {
		// State may already have copied a subscriber before removing it. Fence that
		// copy and wait for an executing refresh before returning from unmount.
		mu.Lock()
		active = false
		mu.Unlock()
		unsubscribe()
	}
}
