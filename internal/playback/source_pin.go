package playback

import (
	"context"

	"github.com/braedonsaunders/ether/internal/core"
)

// PinSourceRevision validates and pins under the same semaphore as failover,
// so no source swap can race the revision check.
func (m *Manager) PinSourceRevision(ctx context.Context, id, revision string) error {
	if revision == "" {
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
	if core.SourceRevision(ps) != revision {
		return core.ErrSourceChanged
	}
	if ps.Resolution == nil || ps.Resolution.Release.ID == "" {
		return core.ErrSourceChanged
	}
	if checker, ok := m.deps.Resolver.(core.ReleaseBlockChecker); ok {
		blocked, err := checker.ReleaseBlocked(ctx, ps.Resolution.Release.ID)
		if err != nil {
			return err
		}
		if blocked {
			return core.ErrSourceChanged
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return core.ErrSourceChanged
	}
	s.sourcePins++
	return nil
}

func (m *Manager) UnpinSourceRevision(id string) {
	s := m.lookup(id)
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.sourcePins > 0 {
		s.sourcePins--
	}
	s.mu.Unlock()
}
