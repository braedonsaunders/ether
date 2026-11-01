package playback

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

const maxOwnedSourceOpens = 1024
const maxOwnedSourceResponsibilities = 1024
const maxSourceCleanup = 128
const sourceCleanupShutdownWait = time.Second

// sourceAccounting charges backend calls through Close, including uncertain
// failures. It never evicts an acquired source to admit another call.
type sourceAccounting struct {
	mu                     sync.Mutex
	opens, owners, cleanup int
	cleanupDone            chan struct{}
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

// sourceOpenTicket reserves physical and ownership capacity independently.
// Proven backend completion refunds only the physical slot. Payload cleanup
// must settle before the lifetime can report successful retirement. A terminal
// failure completes with an error while retaining its responsibility charge.
// Lock order is lifetime.mu -> accounting.mu. Payload callbacks enter this order
// only after releasing all payload/state/gate locks; accounting never calls out.
type sourceOpenTicket struct {
	manager      *Manager
	source       *sourceLifetime
	physicalOnce sync.Once
	settledOnce  sync.Once
}

// reserveSourceOpen runs before every backend call and reserves the eventual
// retry/callback obligation too. No backend, DB or filesystem work holds locks.
func (m *Manager) reserveSourceOpen(s *sourceLifetime) (*sourceOpenTicket, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sealed {
		return nil, core.ErrNotAvailable
	}
	m.sourceAccounting.mu.Lock()
	defer m.sourceAccounting.mu.Unlock()
	if m.sourceAccounting.opens >= maxOwnedSourceOpens || m.sourceAccounting.owners >= maxOwnedSourceResponsibilities {
		return nil, core.ErrNotAvailable
	}
	m.sourceAccounting.opens++
	m.sourceAccounting.owners++
	s.pending++
	return &sourceOpenTicket{manager: m, source: s}, nil
}
func (t *sourceOpenTicket) physical(err error) {
	t.physicalOnce.Do(func() {
		if err == nil {
			t.manager.sourceAccounting.mu.Lock()
			t.manager.sourceAccounting.opens--
			t.manager.sourceAccounting.mu.Unlock()
		}
	})
}
func (t *sourceOpenTicket) settle(err error) {
	t.settledOnce.Do(func() {
		s := t.source
		s.mu.Lock()
		defer s.mu.Unlock()
		if err == nil {
			t.manager.sourceAccounting.mu.Lock()
			t.manager.sourceAccounting.owners--
			t.manager.sourceAccounting.mu.Unlock()
		} else if s.err == nil {
			s.err = err
		}
		s.pending--
		if s.sealed && s.pending == 0 {
			close(s.done)
		}
	})
}

func (m *Manager) openSource(ctx context.Context, s *sourceLifetime, res *core.Resolution) (core.MediaSource, func(error), error) {
	ticket, err := m.reserveSourceOpen(s)
	if err != nil {
		return nil, nil, err
	}
	var owner payloadOwner
	if m.resume != nil {
		owner, err = m.resume.payload.reserve(ctx, s.user, s.surface, s.device, s.id, nzbHash(res.NZB), "source-open")
		if err != nil {
			ticket.physical(nil) // no Engine.Open was called
			if errors.Is(err, errPayloadCommitUnknown) {
				ticket.settle(err)
			} else {
				ticket.settle(nil)
			}
			return nil, nil, err
		}
	}
	var once sync.Once
	finish := func(closeErr error) {
		once.Do(func() {
			ticket.physical(closeErr)
			if closeErr != nil {
				// Unknown backend completion protects both charges and payload.
				// Never retry backend Close or retire its owner on that evidence.
				ticket.settle(closeErr)
				return
			}
			if m.resume != nil {
				// onGone settles only after the durable row is deleted. A resume
				// share of that row is not settlement.
				err := m.resume.payload.releaseSourceLater(context.Background(), owner, m.opts.PayloadReplayGrace, func() { ticket.settle(nil) })
				if errors.Is(err, errPayloadRetained) {
					return
				}
				if err != nil {
					// The exact owner was admitted before Open; it has one bounded
					// callback slot. Rejection/shutdown never settles this ticket.
					m.resume.payload.queueReleaseAfter(owner, func() { ticket.settle(nil) })
					return
				}
			}
			ticket.settle(nil)
		})
	}
	src, err := m.openShared(ctx, res)
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
	if m.encoderInputHolds(id) {
		return fmt.Errorf("%w: encoder input lease", core.ErrNotAvailable)
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
	// Detach returns before Close. Quarantine while this session is no longer
	// visible so a plan that starts after RetireSource cannot reuse its bytes.
	if g != nil {
		m.quarantineShared(g.share)
	}
	m.mu.Unlock()
	go func() { defer release(); m.finishSessionStop(s, g) }()
	return nil
}

func (m *Manager) sourceCleanupContext() (context.Context, context.CancelFunc) {
	if m.sourceCleanupBudget != nil {
		return m.sourceCleanupBudget, func() {}
	}
	return context.WithTimeout(context.Background(), sourceCleanupShutdownWait)
}

// drainSourceCleanup bounds shutdown without starting a goroutine that could
// outlive its wait. Timeout never releases a source or its accounting charge.
// A cleanup that finished as the budget expired is success: both channels
// can be ready together, and the blocked Close is not the wait.
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
		select {
		case <-done:
			return nil
		default:
			return ctx.Err()
		}
	}
}

var _ core.OwnedSourcePlayback = (*Manager)(nil)
