package plex

import (
	"context"
	"errors"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/sessions"
)

// UsePlexStreams binds visible stream ids and sidecar reads to the broker.
// Pass sessions.BindPlexStreams(broker). Fixtures without a broker keep
// the local map.
func (s *Server) UsePlexStreams(api sessions.PlexStreamAPI) {
	if s != nil {
		s.boundStreams = api
	}
}

func (s *Server) plexStreamAccess(u *core.User) frontend.SessionAccess {
	return frontend.SessionAccess{Viewer: u, Protocol: "plex"}
}

func (s *Server) assignStream(ctx context.Context, u *core.User, key core.RatingKey, index int, info core.StreamInfo, sessionID string) int64 {
	if s.boundStreams != nil {
		id, err := s.boundStreams.BindPlexStream(ctx, s.plexStreamAccess(u), sessions.PlexStreamBinding{
			Key: key, Index: index, Info: info, SessionID: sessionID,
		})
		if err != nil {
			return 0
		}
		return id
	}
	return s.streams.register(key, index, info)
}

func (s *Server) resolveStream(ctx context.Context, u *core.User, id int64) (sessions.PlexStreamBinding, error) {
	if s.boundStreams != nil {
		return s.boundStreams.ResolvePlexStream(ctx, s.plexStreamAccess(u), id)
	}
	ref, ok := s.streams.lookup(id)
	if !ok {
		return sessions.PlexStreamBinding{}, core.ErrNotFound
	}
	return sessions.PlexStreamBinding{WireID: id, Key: ref.key, Index: ref.index, Info: ref.info}, nil
}

func streamMiss(err error) bool {
	return err == nil || errors.Is(err, core.ErrNotFound)
}
