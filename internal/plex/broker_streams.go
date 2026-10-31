package plex

import (
	"context"
	"errors"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/sessions"
)

// UsePlexStreams binds visible stream ids and sidecar reads to the broker.
// Pass sessions.BindPlexStreams(broker).
func (s *Server) UsePlexStreams(api sessions.PlexStreamAPI) {
	if s != nil {
		s.boundStreams = api
	}
}

func (s *Server) plexStreamAccess(u *core.User) frontend.SessionAccess {
	return frontend.SessionAccess{Viewer: u, Protocol: "plex"}
}

func (s *Server) assignStream(ctx context.Context, u *core.User, key core.RatingKey, index int, info core.StreamInfo, sessionID string) int64 {
	if s == nil || s.boundStreams == nil {
		return 0
	}
	id, err := s.boundStreams.BindPlexStream(ctx, s.plexStreamAccess(u), sessions.PlexStreamBinding{
		Key: key, Index: index, Info: info, SessionID: sessionID,
	})
	if err != nil {
		return 0
	}
	return id
}

func (s *Server) resolveStream(ctx context.Context, u *core.User, id int64) (sessions.PlexStreamBinding, error) {
	if s == nil || s.boundStreams == nil {
		return sessions.PlexStreamBinding{}, core.ErrNotFound
	}
	return s.boundStreams.ResolvePlexStream(ctx, s.plexStreamAccess(u), id)
}

func streamMiss(err error) bool {
	return err == nil || errors.Is(err, core.ErrNotFound)
}
