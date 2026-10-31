package app

import (
	"context"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// Owner status presents one row per viewer, including passthrough IPTV.
// Programme sources stay private to the playout service.
type ownerPlayback struct {
	core.Playback
	app *App
}

func (p ownerPlayback) Sessions() []*core.PlaySession {
	var live []*core.PlaySession
	if p.app.livetv != nil {
		live = p.app.livetv.ViewingSessions()
	}
	groups := map[string]bool{}
	for _, s := range live {
		groups[s.ID] = true
	}
	out := make([]*core.PlaySession, 0, len(live))
	for _, s := range p.Playback.Sessions() {
		if !s.PreparedOnly && (s.PlaybackGroup == "" || !groups[s.PlaybackGroup]) {
			out = append(out, s)
		}
	}
	out = append(out, live...)
	// Other cluster nodes' streams (nil on a standalone server).
	if p.app != nil && p.app.playback != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		for _, s := range p.app.playback.RemoteSessions(ctx) {
			if !s.PreparedOnly {
				out = append(out, s)
			}
		}
		cancel()
	}
	return out
}
func (p ownerPlayback) Session(id string) (*core.PlaySession, bool) {
	for _, s := range p.Sessions() {
		if s.ID == id {
			return s, true
		}
	}
	return nil, false
}
func (a *App) stopOwnerSession(id string) error {
	if server := a.currentPlexServer(); server != nil {
		server.NoteOwnerStop(id, "The server owner has ended this stream.")
	}
	if a.frontends != nil && a.frontends.broker != nil && a.frontends.broker.StopPlayback(context.Background(), id) {
		return nil
	}
	if a.livetv != nil {
		for _, s := range a.livetv.ViewingSessions() {
			if s.ID == id {
				return a.livetv.Stop(context.Background(), id, &core.User{IsOwner: true})
			}
		}
	}
	if a.playback != nil {
		a.playback.Stop(id)
	}
	return nil
}
