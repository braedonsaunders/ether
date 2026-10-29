package plex

import (
	"context"
	"fmt"
	"net/http"
	"sync"

	"github.com/braedonsaunders/ether/internal/core"
)

// Lifecycle operations lock the owner first, then at most one encoder ID.
// Response streaming never holds these gates. ID gates also serialize reuse
// across titles or owners while the table's ownership claim is rechecked.
type playbackLifecycle struct {
	mu    sync.Mutex
	gates map[string]*playbackGate
}
type playbackGate struct {
	token chan struct{}
	refs  int
}

func newPlaybackLifecycle() *playbackLifecycle {
	return &playbackLifecycle{gates: make(map[string]*playbackGate)}
}
func (l *playbackLifecycle) acquire(ctx context.Context, key string) (func(), error) {
	l.mu.Lock()
	g := l.gates[key]
	if g == nil {
		g = &playbackGate{token: make(chan struct{}, 1)}
		g.token <- struct{}{}
		l.gates[key] = g
	}
	g.refs++
	l.mu.Unlock()
	drop := func() {
		l.mu.Lock()
		g.refs--
		if g.refs == 0 {
			delete(l.gates, key)
		}
		l.mu.Unlock()
	}
	select {
	case <-ctx.Done():
		drop()
		return nil, ctx.Err()
	case <-g.token:
	}
	if err := ctx.Err(); err != nil {
		g.token <- struct{}{}
		drop()
		return nil, err
	}
	var once sync.Once
	return func() { once.Do(func() { g.token <- struct{}{}; drop() }) }, nil
}
func (s *Server) lockPlaybackOwner(ctx context.Context, user int64, client string) (func(), error) {
	return s.playbackLifecycle.acquire(ctx, fmt.Sprintf("owner/%d/%s", user, client))
}
func (s *Server) lockPlaybackRequest(w http.ResponseWriter, r *http.Request) (func(), bool) {
	pr := FromContext(r.Context())
	if pr.User == nil {
		writeHTTPError(w, http.StatusUnauthorized)
		return nil, false
	}
	unlock, err := s.lockPlaybackOwner(r.Context(), pr.User.ID, pr.ClientID)
	if err != nil {
		return nil, false
	}
	return unlock, true
}
func (s *Server) lockEncoder(ctx context.Context, id string) (func(), error) {
	return s.playbackLifecycle.acquire(ctx, "encoder/"+id)
}

// The caller holds the owner gate. Never release the shared playback source.
func (s *Server) cleanSupersededEncoders(ctx context.Context, user int64, client string, key core.RatingKey, playbackID string) {
	for _, id := range s.transcodes.forClient(user, client, key) {
		unlock, err := s.lockEncoder(ctx, id)
		if err != nil {
			return
		}
		e, ok := s.transcodes.get(id)
		if ok && e.userID == user && e.clientID == client && e.key == key &&
			e.playbackID == playbackID && s.playbackOwners.Stale(user, client, key, playbackID) {
			if s.d.Transcoder != nil {
				s.d.Transcoder.Stop(id)
			}
			s.transcodes.remove(id)
		}
		unlock()
	}
}
