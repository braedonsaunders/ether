package playback

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

const maxOwnedSourceOpens = 1024
const maxSourceCleanup = 128
const sourceCleanupShutdownWait = time.Second

// sourceAccounting charges backend calls through Close, including uncertain
// failures. It never evicts an acquired source to admit another call.
type sourceAccounting struct {
	mu             sync.Mutex
	opens, cleanup int
	cleanupDone    chan struct{}
}

// sourceLifetime belongs to the actual manager session/preparation flight.
// It holds aggregate outcomes rather than retaining every old generation.
type sourceLifetime struct {
	user                int64
	surface, device, id string
	mu                  sync.Mutex
	done                chan struct{}
	pending             int
	sealed              bool
	err                 error
}

func newSourceLifetime(user int64, surface, device string) *sourceLifetime {
	return &sourceLifetime{user: user, surface: surface, device: device, id: randomID(32), done: make(chan struct{})}
}
func (s *sourceLifetime) Done() <-chan struct{} { return s.done }
func (s *sourceLifetime) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-s.done:
		return s.err
	default:
		return core.ErrNotAvailable
	}
}
func (s *sourceLifetime) seal() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.sealed {
		s.sealed = true
		if s.pending == 0 {
			close(s.done)
		}
	}
}

// reserveSourceOpen runs before every Engine.Open, never under manager/session
// locks. A Close error retains its accounting charge for the manager lifetime.
func (m *Manager) reserveSourceOpen(s *sourceLifetime) (func(error), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sealed {
		return nil, core.ErrNotAvailable
	}
	m.sourceAccounting.mu.Lock()
	if m.sourceAccounting.opens >= maxOwnedSourceOpens {
		m.sourceAccounting.mu.Unlock()
		return nil, core.ErrNotAvailable
	}
	m.sourceAccounting.opens++
	m.sourceAccounting.mu.Unlock()
	s.pending++
	var once sync.Once
	return func(err error) {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			if err == nil {
				m.sourceAccounting.mu.Lock()
				m.sourceAccounting.opens--
				m.sourceAccounting.mu.Unlock()
			} else if s.err == nil {
				s.err = err
			}
			s.pending--
			if s.sealed && s.pending == 0 {
				close(s.done)
			}
		})
	}, nil
}

func (m *Manager) openSource(ctx context.Context, s *sourceLifetime, res *core.Resolution) (core.MediaSource, func(error), error) {
	finish, err := m.reserveSourceOpen(s)
	if err != nil {
		return nil, nil, err
	}
	var owner payloadOwner
	if m.resume != nil {
		owner, err = m.resume.payload.reserve(ctx, s.user, s.surface, s.device, s.id, nzbHash(res.NZB), "source-open")
		if err != nil {
			if errors.Is(err, errPayloadCommitUnknown) {
				finish(err)
			} else {
				finish(nil)
			}
			return nil, nil, err
		}
		backendFinished := finish
		var once sync.Once
		finish = func(closeErr error) {
			once.Do(func() {
				if closeErr == nil {
					closeErr = m.resume.payload.release(context.Background(), owner)
				}
				backendFinished(closeErr)
			})
		}
	}
	src, err := m.deps.Engine.Open(ctx, res.Release.ID, res.NZB)
	if err != nil || src == nil {
		if src != nil {
			finish(src.Close())
		} else {
			finish(nil)
		}
		if err == nil {
			err = errors.New("playback: backend returned no source")
		}
		return nil, nil, err
	}
	return src, finish, nil
}

func (m *Manager) SourceRetirement(id string) (core.SourceRetirement, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.sessions[id]
	if s == nil {
		return nil, false
	}
	return s.sources, true
}

func (m *Manager) PrepareSurfaceSource(ctx context.Context, user *core.User, key core.RatingKey, caps core.ClientCaps, release string, exact bool) (*core.PlaySession, core.SourceRetirement, error) {
	if exact && release == "" {
		return nil, nil, core.ErrNotAvailable
	}
	return m.prepareOwnedEntry(ctx, user, key, caps, release, exact, true)
}

// RetireSource reserves cleanup before detaching. No backend call runs while
// holding a manager/session lock; callers must also hold no broker owner gate.
func (m *Manager) RetireSource(id string, outcome core.SourceRetirement) error {
	expected, ok := outcome.(*sourceLifetime)
	if !ok || expected == nil {
		return core.ErrNotAvailable
	}
	m.sourceAccounting.mu.Lock()
	if m.sourceAccounting.cleanup >= maxSourceCleanup {
		m.sourceAccounting.mu.Unlock()
		return core.ErrNotAvailable
	}
	if m.sourceAccounting.cleanup == 0 {
		m.sourceAccounting.cleanupDone = make(chan struct{})
	}
	m.sourceAccounting.cleanup++
	m.sourceAccounting.mu.Unlock()
	release := func() {
		m.sourceAccounting.mu.Lock()
		m.sourceAccounting.cleanup--
		if m.sourceAccounting.cleanup == 0 {
			close(m.sourceAccounting.cleanupDone)
		}
		m.sourceAccounting.mu.Unlock()
	}
	m.mu.Lock()
	s := m.sessions[id]
	if s == nil || s.sources != expected {
		m.mu.Unlock()
		release()
		// A sealed exact lifetime is already detached; never touch a new one.
		expected.mu.Lock()
		sealed := expected.sealed
		expected.mu.Unlock()
		if sealed {
			return nil
		}
		return core.ErrNotAvailable
	}
	if m.closed {
		m.mu.Unlock()
		release()
		return core.ErrNotAvailable
	}
	g := m.detachSessionLocked(s)
	m.mu.Unlock()
	go func() { defer release(); m.finishSessionStop(s, g) }()
	return nil
}

// drainSourceCleanup bounds shutdown without starting a goroutine that could
// outlive its wait. Timeout never releases a source or its accounting charge.
func (m *Manager) drainSourceCleanup(ctx context.Context) error {
	m.sourceAccounting.mu.Lock()
	pending, done := m.sourceAccounting.cleanup, m.sourceAccounting.cleanupDone
	m.sourceAccounting.mu.Unlock()
	if pending == 0 {
		return nil
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

var _ core.OwnedSourcePlayback = (*Manager)(nil)
