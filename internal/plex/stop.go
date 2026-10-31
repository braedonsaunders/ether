package plex

import (
	"context"

	"github.com/braedonsaunders/ether/internal/core"
)

// StopPlayback ends an owner-selected source and tells the official app why.
// On a clustered node a session this node does not serve (a playback or
// live TV session ID from the cluster-wide status) is stopped by the node
// that does.
// NoteOwnerStop tells the Plex client why a stream is ending. The shared
// broker, or the playback manager, performs the stop.
func (s *Server) NoteOwnerStop(id, reason string) {
	if s.d.LiveTV != nil {
		for _, v := range s.d.LiveTV.ViewingSessions() {
			if v.ID == id {
				return
			}
		}
	}
	if s.d.Playback != nil {
		if ps, ok := s.d.Playback.Session(id); ok && ps != nil {
			unlock, err := s.lockPlaybackOwner(s.ctx, ps.UserID, ps.ClientID)
			if err != nil {
				return
			}
			defer unlock()
			for _, e := range s.sessions.list() {
				if found, ok := s.d.Playback.SessionFor(e.user.ID, e.client.ClientID, e.key); ok && found.ID == id {
					s.setTermination(e.id, reason)
				}
			}
			return
		}
	}
	if s.cl != nil {
		s.cl.publish(topicTerminate, terminateEvent{Scope: "playback", ID: id, Reason: reason})
	}
}

// StopPreparedSource stops a source this server prepared that the shared
// broker does not own. Published sessions stop through the broker.
func (s *Server) StopPreparedSource(id string) bool {
	if s == nil || s.d.Playback == nil || id == "" {
		return false
	}
	if _, ok := s.d.Playback.Session(id); !ok {
		return false
	}
	s.d.Playback.Stop(id)
	return true
}

func (s *Server) StopPlayback(id, reason string) {
	if s.cl == nil {
		s.stopPlayback(id, reason)
		return
	}
	if !s.stopLocalPlayback(id, reason) {
		s.cl.publish(topicTerminate, terminateEvent{Scope: "playback", ID: id, Reason: reason})
	}
}

func (s *Server) stopPlayback(id, reason string) {
	if s.d.Playback == nil {
		return
	}
	ps, ok := s.d.Playback.Session(id)
	if !ok {
		return
	}
	unlock, err := s.lockPlaybackOwner(s.ctx, ps.UserID, ps.ClientID)
	if err != nil {
		return
	}
	defer unlock()
	for _, e := range s.sessions.list() {
		if ps, ok := s.d.Playback.SessionFor(e.user.ID, e.client.ClientID, e.key); ok && ps.ID == id {
			s.setTermination(e.id, reason)
		}
	}
	s.d.Playback.Stop(id)
}

// stopLocalPlayback stops id if this node serves it, as a live TV viewer
// or a playback session.
func (s *Server) stopLocalPlayback(id, reason string) bool {
	if s.d.LiveTV != nil {
		for _, v := range s.d.LiveTV.ViewingSessions() {
			if v.ID == id {
				_ = s.d.LiveTV.Stop(context.Background(), id, &core.User{IsOwner: true})
				return true
			}
		}
	}
	if s.d.Playback == nil {
		return false
	}
	if _, ok := s.d.Playback.Session(id); !ok {
		return false
	}
	s.stopPlayback(id, reason)
	return true
}

func (s *Server) liveTVAPIVersion() int {
	if s.liveTVEnabled() {
		return 4
	}
	return 0
}
