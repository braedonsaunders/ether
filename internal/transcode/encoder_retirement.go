package transcode

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

const encoderLifetimeCapacity = 1024
const encoderWorkCapacity = 64
const encoderCleanupWorkers = 4

var ErrEncoderCapacity = errors.New("transcode: encoder retirement capacity exhausted")

// Admission is process-wide: a stuck manager cannot escape the bound by being
// replaced. Every queued detach/cleanup already owns one of these slots.
var encoderCleanup = struct {
	once  sync.Once
	slots chan struct{}
	jobs  chan *encoderLifetime
	dirs  sync.Map
}{slots: make(chan struct{}, encoderLifetimeCapacity), jobs: make(chan *encoderLifetime, encoderLifetimeCapacity)}

type encoderLifetime struct {
	m               *Manager
	id              string
	mu              sync.Mutex
	session         *Session
	work            int
	sealed          bool
	detached        bool
	queued          bool
	terminal        bool
	started         bool
	input           core.EncoderInputLease
	inputClaimed    bool
	inputDrained    bool
	inputSealIssued bool
	inputError      error
	cleanup         error
	proof           error
	done            chan struct{}
	cancel          chan struct{}
	encoding        atomic.Bool
	runs            atomic.Int32
}

type encoderWork struct {
	once  sync.Once
	life  *encoderLifetime
	after func()
}

func (m *Manager) reserveEncoder() (*encoderLifetime, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, errors.New("transcode: manager closed")
	}
	limit := m.encoderLimit
	if limit <= 0 {
		limit = encoderLifetimeCapacity
	}
	if len(m.encoders) >= limit {
		return nil, ErrEncoderCapacity
	}
	select {
	case encoderCleanup.slots <- struct{}{}:
	default:
		return nil, ErrEncoderCapacity
	}
	encoderCleanup.once.Do(func() {
		for i := 0; i < encoderCleanupWorkers; i++ {
			go func() {
				for life := range encoderCleanup.jobs {
					life.advance()
				}
			}()
		}
	})
	// Start and detach each have a reserved ticket. No post-seal allocation
	// is necessary to stop a process that returns late from Start.
	l := &encoderLifetime{m: m, work: 2, done: make(chan struct{}), cancel: make(chan struct{})}
	if m.encoders == nil {
		m.encoders = make(map[*encoderLifetime]struct{})
	}
	m.encoders[l] = struct{}{}
	return l, nil
}

func (l *encoderLifetime) attach(s *Session) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.sealed {
		return false
	}
	if _, exists := encoderCleanup.dirs.LoadOrStore(s.dir, l); exists {
		return false
	}
	l.session = s
	return true
}

func (l *encoderLifetime) reserve(n int) ([]*encoderWork, error) {
	if l == nil { // package tests may construct a non-running Session directly
		return make([]*encoderWork, n), nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.sealed {
		return nil, errors.New("transcode: session closed")
	}
	if l.work+n > encoderWorkCapacity {
		return nil, ErrEncoderCapacity
	}
	l.work += n
	work := make([]*encoderWork, n)
	for i := range work {
		work[i] = &encoderWork{life: l}
	}
	return work, nil
}

func (w *encoderWork) finish() {
	if w == nil {
		return
	}
	w.once.Do(func() {
		if w.after != nil {
			w.after()
		}
		w.life.finishWork()
	})
}
func (l *encoderLifetime) finishWork() {
	l.mu.Lock()
	l.work--
	l.queueLocked()
	l.mu.Unlock()
}
func (l *encoderLifetime) queueLocked() {
	if l.sealed && !l.queued && !l.terminal && (!l.detached || l.work == 0 || (l.inputClaimed && !l.inputDrained && !l.inputSealIssued && l.work == 1)) {
		l.queued = true
		encoderCleanup.jobs <- l // one queued job per admitted lifetime
	}
}
func (l *encoderLifetime) seal() {
	l.mu.Lock()
	if !l.sealed {
		l.sealed = true
		close(l.cancel)
	}
	l.queueLocked()
	l.mu.Unlock()
}
func (l *encoderLifetime) advance() {
	l.mu.Lock()
	s, detached := l.session, l.detached
	l.mu.Unlock()
	if !detached {
		if s != nil {
			s.mu.Lock()
			s.closed = true
			s.stopRunLocked()
			s.broadcastLocked()
			s.mu.Unlock()
		}
		l.mu.Lock()
		l.detached = true
		l.queued = false
		l.work-- // reserved detach ticket
		l.queueLocked()
		l.mu.Unlock()
		return
	}
	l.mu.Lock()
	if l.inputClaimed && !l.inputDrained && !l.inputSealIssued && l.work == 1 {
		l.inputSealIssued = true
		l.queued = false
		input := l.input
		l.mu.Unlock()
		// Actual work has drained excluding its reserved input ticket. Never
		// park a cleanup worker on input producers or wait on our own ticket.
		input.SealInput()
		return
	}
	err := l.inputError
	l.mu.Unlock()
	if err == nil && s != nil {
		remove := s.m.encoderRemove
		if remove == nil {
			remove = removeAllRetry
		}
		err = remove(s.dir)
		if err == nil {
			encoderCleanup.dirs.Delete(s.dir)
			s.m.forget(s)
			s.log.Info("session closed", "age", time.Since(s.created).Round(time.Second))
		}
	}
	l.mu.Lock()
	l.cleanup = err
	l.proof = err
	if l.proof == nil && l.started && !l.inputClaimed {
		// Missing optional input proof cannot authorize full retirement.
		// Proven legacy process/directory cleanup can release its own charge.
		l.proof = core.ErrEncoderInputUnverified
	}
	l.terminal = true
	if err == nil {
		l.m.mu.Lock()
		delete(l.m.encoders, l)
		if l.m.encoderIDs[l.id] == l {
			delete(l.m.encoderIDs, l.id)
		}
		l.m.mu.Unlock()
		<-encoderCleanup.slots
	}
	close(l.done)
	l.mu.Unlock()
}
func (l *encoderLifetime) EncoderDone() <-chan struct{} { return l.done }
func (l *encoderLifetime) EncoderErr() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.terminal {
		return core.ErrEncoderPending
	}
	return l.proof
}
func (l *encoderLifetime) legacyResult() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.terminal {
		return core.ErrEncoderPending
	}
	return l.cleanup
}
func (m *Manager) encoderWait() time.Duration {
	if m.encoderTimeout > 0 {
		return m.encoderTimeout
	}
	return 10 * time.Second
}
func (s *Session) EncoderRetirement() core.EncoderRetirement { return s.encoder }
func (s *Session) RetireEncoder() error {
	if s.encoder == nil {
		return errors.New("transcode: session has no encoder responsibility")
	}
	s.encoder.seal()
	return nil
}
func (m *Manager) StartHLSOwned(ctx context.Context, req core.HLSRequest) (core.HLSSession, core.EncoderRetirement, error) {
	l, err := m.reserveEncoder()
	if err != nil {
		return nil, nil, err
	}
	type result struct {
		s   core.HLSSession
		err error
	}
	resultCh := make(chan result, 1)
	go func() {
		s, err := m.startHLS(ctx, req, l)
		if ctx.Err() != nil {
			l.seal()
			s, err = nil, ctx.Err()
		}
		resultCh <- result{s, err}
	}()
	select {
	case r := <-resultCh:
		return r.s, l, r.err
	case <-ctx.Done():
		l.seal()
		return nil, l, ctx.Err()
	}
}

var _ core.OwnedTranscoder = (*Manager)(nil)
var _ core.OwnedEncoderSession = (*Session)(nil)

func (l *encoderLifetime) isSealed() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.sealed
}
func (l *encoderLifetime) markStarted() {
	if l != nil {
		l.mu.Lock()
		l.started = true
		l.mu.Unlock()
	}
}
func (l *encoderLifetime) runWork(encoding bool) ([]*encoderWork, error) {
	work, err := l.reserve(2)
	if err != nil || l == nil {
		return work, err
	}
	if encoding {
		l.runs.Add(1)
		var remaining atomic.Int32
		remaining.Store(2)
		for _, w := range work {
			w.after = func() {
				if remaining.Add(-1) == 0 {
					l.runs.Add(-1)
				}
			}
		}
	}
	return work, nil
}

func (s *Session) encoderRun(ctx context.Context, cmd Command) error {
	work, err := s.encoder.reserve(1)
	if err != nil {
		return err
	}
	defer work[0].finish()
	s.encoder.markStarted()
	return s.m.runner.Run(ctx, cmd)
}

// claimEncoderID linearizes same-ID replacement within this Manager before
// any backend/directory work. A prior admitted start must retire too, even if it never published.
func (m *Manager) claimEncoderID(ctx context.Context, id string, life *encoderLifetime) error {
	timer := time.NewTimer(m.encoderWait())
	defer timer.Stop()
	for {
		life.mu.Lock()
		m.mu.Lock()
		if life.sealed || m.closed {
			m.mu.Unlock()
			life.mu.Unlock()
			return errors.New("transcode: session closed")
		}
		if m.encoderIDs == nil {
			m.encoderIDs = make(map[string]*encoderLifetime)
		}
		prior := m.encoderIDs[id]
		if prior == nil {
			life.id = id
			m.encoderIDs[id] = life
			m.mu.Unlock()
			life.mu.Unlock()
			return nil
		}
		m.mu.Unlock()
		life.mu.Unlock()
		prior.seal()
		select {
		case <-prior.done:
			if err := prior.legacyResult(); err != nil {
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		case <-life.cancel:
			return errors.New("transcode: session closed")
		case <-timer.C:
			return core.ErrEncoderPending
		}
	}
}

func (l *encoderLifetime) installInput(input core.EncoderInputLease) error {
	work, err := l.reserve(1)
	if err != nil {
		return err
	}
	// Reserve before claim, including a callback racing ClaimInput's return.
	l.mu.Lock()
	l.input = input
	l.mu.Unlock()
	err = input.ClaimInput(func(inputErr error) {
		l.mu.Lock()
		l.inputDrained = true
		l.inputError = inputErr
		l.mu.Unlock()
		work[0].finish()
	})
	if err != nil {
		l.mu.Lock()
		l.input = nil
		l.mu.Unlock()
		work[0].finish() // failed claim guarantees no later callback
		return err
	}
	l.mu.Lock()
	l.inputClaimed = true
	l.queueLocked()
	l.mu.Unlock()
	return nil
}
