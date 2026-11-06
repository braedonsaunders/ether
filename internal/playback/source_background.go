package playback

import (
	"context"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

const maxBackgroundSourceHold = 10 * time.Minute

type backgroundSourceHold struct {
	done  <-chan struct{}
	until time.Time
}

func (h *backgroundSourceHold) active(now time.Time) bool {
	if h == nil || !now.Before(h.until) {
		return false
	}
	select {
	case <-h.done:
		return false
	default:
		return true
	}
}

// HoldBackgroundSource holds an explicitly owned prepared source for one
// deadline-bound background job. It does not touch viewer activity, admit a
// stream, change read priority, or prevent Stop/shutdown from revoking it.
// The caller releases the hold before retiring its source responsibility.
func (m *Manager) HoldBackgroundSource(ctx context.Context, id string, expected core.SourceRetirement) (func(), error) {
	deadline, bounded := ctx.Deadline()
	remaining := time.Until(deadline)
	owner, owned := expected.(*sourceLifetime)
	if !bounded || remaining <= 0 || ctx.Err() != nil || !owned || owner == nil {
		return nil, core.ErrNotAvailable
	}
	s := m.lookup(id)
	if s == nil {
		return nil, core.ErrNotAvailable
	}
	now := m.now()
	hold := &backgroundSourceHold{done: ctx.Done(), until: now.Add(min(remaining, maxBackgroundSourceHold))}
	s.mu.Lock()
	if s.closed || !s.preparedOnly || s.sources != owner || s.backgroundHold.active(now) {
		s.mu.Unlock()
		return nil, core.ErrNotAvailable
	}
	s.backgroundHold = hold
	s.analysisSource = true
	s.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			if s.backgroundHold == hold {
				s.backgroundHold = nil
			}
			s.mu.Unlock()
		})
	}, nil
}
