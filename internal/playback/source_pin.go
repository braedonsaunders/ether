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

// BindSource records why an exact session holds its release. An Auto or
// remembered start opens its release exactly, yet only a viewer's own pick
// stays on a release that cannot be read.
func (m *Manager) BindSource(id string, binding core.SourceBinding) {
	s := m.lookup(id)
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.binding != core.SourceBindingExplicit {
		s.binding = binding
	}
	auto := s.binding == core.SourceBindingAuto
	s.mu.Unlock()
	if auto {
		// Opened by id, yet Auto: its preference is the Auto one.
		pref, _ := m.preferredRelease(s)
		s.mu.Lock()
		if s.cur != nil {
			s.notePreferenceLocked(pref, s.cur.releaseID())
		}
		s.mu.Unlock()
	}
}

// replaceableLocked reports whether a failing exact or pinned session may
// move to another release. A pin holds the client's track and revision
// promises, so only an unpinned Auto start fails over freely and an
// unpinned remembered start only when its release is proven unreadable.
// Caller holds s.mu.
func (s *session) replaceableLocked(unreadable bool) bool {
	if s.sourcePins > 0 || s.originalRevision != "" {
		return false
	}
	if !s.exact {
		return true
	}
	switch s.binding {
	case core.SourceBindingAuto:
		return true
	case core.SourceBindingRemembered:
		return unreadable
	}
	return false
}
