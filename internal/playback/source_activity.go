package playback

import (
	"context"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// SourcePlaybackProgress applies trusted remote activity to one exact source.
// Unlike frontend timeline matching, it cannot refresh another surface or a
// replacement generation belonging to the same viewer/device/title.
func (m *Manager) SourcePlaybackProgress(ctx context.Context, id, revision string, viewer int64, client string, key core.RatingKey, position time.Duration, state string) error {
	if position < 0 || (state != "playing" && state != "paused") || revision == "" {
		return core.ErrSourceChanged
	}
	s := m.lookup(id)
	if s == nil {
		return core.ErrSourceChanged
	}
	if err := lock(ctx, s.failSem); err != nil {
		return err
	}
	defer unlock(s.failSem)
	ps := s.snapshot(m.now())
	if ps.UserID != viewer || ps.ClientID != client || ps.Key != key || core.SourceRevision(ps) != revision {
		return core.ErrSourceChanged
	}
	now := m.now()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return core.ErrSourceChanged
	}
	if err := ctx.Err(); err != nil {
		s.mu.Unlock()
		return err
	}
	s.viewerPosition = position
	s.viewerAt = time.Now()
	s.viewerState = state
	if measuredProtocol(s.caps.Protocol) {
		s.exp.observe(now, reportsBuffering(s.caps.Protocol), position, state)
	}
	s.lastActive = now
	s.stopAt = time.Time{}
	var pausedRelease string
	if state == "paused" && s.cur != nil {
		pausedRelease = s.cur.releaseID()
	}
	s.mu.Unlock()
	m.startSubtitleCheck(s)
	m.noteCause(pausedRelease, core.CacheCausePaused)
	m.maintainReserve(s, position, state)
	return nil
}
