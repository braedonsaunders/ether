package plex

import (
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/state"
)

// State emits only after committing, including deferred cluster writes. Do not
// refresh the library on every playing heartbeat: it churns recommendation rows
// and duplicates the separate session-status notifications. Explicit scrobbles
// already publish their own library notification in the Plex handler.
func subscribeWatchRefresh(watch *state.State, refresh func(int64, core.RatingKey), explicitOnly bool) func() {
	return watch.Subscribe(func(ev state.Event) {
		// Cross-surface explicit state changes previously came through the shared
		// event publisher; Plex's own handlers already send these notifications.
		if ev.Origin.Surface != "plex" && (ev.Kind == state.EventScrobble || ev.Kind == state.EventUnscrobble) {
			refresh(ev.UserID, ev.Key)
			return
		}
		if explicitOnly || ev.Imported {
			return
		}
		if ev.Kind == state.EventCompleted ||
			(ev.Kind == state.EventProgress && !ev.Completed && (ev.State == "paused" || ev.State == "stopped")) {
			refresh(ev.UserID, ev.Key)
		}
	})
}
