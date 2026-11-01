package livetv

import (
	"context"
	"net/http"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/streamgrant"
)

// liveMediaSessionID is a playback path under one live session. Session
// metadata and stop routes are not media.
func liveMediaSessionID(path string) (string, bool) {
	rest, ok := strings.CutPrefix(path, "/livetv/sessions/")
	if !ok {
		return "", false
	}
	id, tail, ok := strings.Cut(rest, "/")
	if !ok || id == "" || strings.Contains(id, "/") {
		return "", false
	}
	_, media, ok := strings.Cut(tail, "/")
	if !ok || media == "" {
		return "", false
	}
	switch {
	case media == "index.m3u8":
	case strings.HasPrefix(media, "relay/"), strings.HasPrefix(media, "resource/"), strings.HasPrefix(media, "program/"), strings.HasPrefix(media, "dash/"):
	default:
		return "", false
	}
	return id, true
}

// AuthenticateMediaGrant authorizes GET and HEAD media for the session that
// minted the stream grant. It does not accept account tokens and it does not
// authorize session control.
func (s *Service) AuthenticateMediaGrant(r *http.Request, active func(context.Context, int64) *core.User) (*core.User, bool) {
	if s == nil || r == nil || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
		return nil, false
	}
	id, ok := liveMediaSessionID(r.URL.Path)
	if !ok {
		return nil, false
	}
	s.mu.Lock()
	ss := s.sessions[id]
	s.mu.Unlock()
	if ss == nil {
		return nil, false
	}
	ss.mu.Lock()
	grant, userID, closed := ss.streamGrant, ss.info.UserID, ss.closed
	ss.mu.Unlock()
	if closed || grant == "" {
		return nil, false
	}
	return streamgrant.AuthenticateGrant(r.Context(), r.Method, r.URL.Query().Get(streamgrant.StreamGrantParam), streamgrant.GrantBinding{Token: grant, ViewerID: userID, Started: true}, active)
}
