package transcode

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

type retirementRunner struct {
	Runner
	start func(Command) (Process, error)
	run   func(context.Context, Command) error
}

func (r retirementRunner) Start(c Command) (Process, error) {
	if r.start != nil {
		return r.start(c)
	}
	return r.Runner.Start(c)
}
func (r retirementRunner) Run(ctx context.Context, c Command) error {
	if r.run != nil {
		return r.run(ctx, c)
	}
	return r.Runner.Run(ctx, c)
}

type retirementProcess struct {
	Process
	stopEntered chan struct{}
	stopRelease chan struct{}
	once        sync.Once
}

func (p *retirementProcess) Stop() {
	p.once.Do(func() { close(p.stopEntered) })
	<-p.stopRelease
	p.Process.Stop()
}
func encoderLatch() (chan struct{}, func()) {
	c := make(chan struct{})
	var once sync.Once
	return c, func() { once.Do(func() { close(c) }) }
}

func joinEncoderResources(t *testing.T, m *Manager, release func()) {
	t.Helper()
	t.Cleanup(func() {
		release()
		m.mu.Lock()
		var lives []*encoderLifetime
		for life := range m.encoders {
			lives = append(lives, life)
		}
		m.mu.Unlock()
		for _, life := range lives {
			life.seal()
			encoderSignal(t, life.done)
		}
		if err := m.Close(); err != nil {
			t.Error(err)
		}
	})
}

func encoderSignal(t *testing.T, c <-chan struct{}) {
	t.Helper()
	select {
	case <-c:
	case <-time.After(5 * time.Second):
		t.Fatal("encoder test resource did not progress")
	}
}
func assertEncoderPending(t *testing.T, outcome core.EncoderRetirement) {
	t.Helper()
	if !errors.Is(outcome.EncoderErr(), core.ErrEncoderPending) {
		t.Fatalf("pending result=%v", outcome.EncoderErr())
	}
	select {
	case <-outcome.EncoderDone():
		t.Fatal("unproved encoder retirement completed")
	default:
	}
}

func TestEncoderRetirementRetainsStalledStopAndShutdown(t *testing.T) {
	fr := newFakeRunner()
	entered := make(chan struct{})
	release, unblock := encoderLatch()
	started := make(chan Process, 1)
	runner := retirementRunner{Runner: fr, start: func(c Command) (Process, error) {
		p, err := fr.Start(c)
		started <- p
		return &retirementProcess{Process: p, stopEntered: entered, stopRelease: release}, err
	}}
	m := newTestManager(t, fr, config.Transcode{MaxTranscodes: 1}, WithRunner(runner))
	joinEncoderResources(t, m, unblock)
	m.encoderTimeout = 20 * time.Millisecond
	s := startSession(t, m, transcodeRequest(t, "stuck"))
	outcome := s.EncoderRetirement()

	if err := s.RetireEncoder(); err != nil {
		t.Fatal(err)
	}
	encoderSignal(t, entered)
	// Even a separately confirmed process exit cannot prove that Stop returned.
	p := <-started
	p.Stop()
	encoderSignal(t, p.Done())
	assertEncoderPending(t, outcome)
	if err := s.Close(); !errors.Is(err, core.ErrEncoderPending) {
		t.Fatalf("stalled Close=%v", err)
	}
	if _, err := os.Stat(s.dir); err != nil {
		t.Fatalf("removed directory of live process: %v", err)
	}
	if _, err := m.StartHLS(context.Background(), transcodeRequest(t, "other")); !errors.Is(err, ErrTooManyTranscodes) {
		t.Fatalf("stalled process lost encoder charge: %v", err)
	}
	if err := m.Close(); !errors.Is(err, core.ErrEncoderPending) {
		t.Fatalf("stalled manager shutdown=%v", err)
	}
	assertEncoderPending(t, outcome)
}

func TestEncoderRetirementLateStartCannotPublish(t *testing.T) {
	for _, startErr := range []error{nil, errors.New("returned process with failure")} {
		t.Run(fmt.Sprint(startErr), func(t *testing.T) {
			fr := newFakeRunner()
			entered := make(chan struct{})
			release, unblock := encoderLatch()
			runner := retirementRunner{Runner: fr, start: func(c Command) (Process, error) {
				close(entered)
				<-release
				p, err := fr.Start(c)
				if err != nil {
					return p, err
				}
				return p, startErr
			}}
			m := newTestManager(t, fr, config.Transcode{}, WithRunner(runner))
			joinEncoderResources(t, m, unblock)
			m.encoderTimeout = 20 * time.Millisecond
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			type result struct {
				outcome core.EncoderRetirement
				err     error
			}
			returned := make(chan result, 1)
			req := remuxRequest(t, "late", time.Hour)
			go func() {
				s, outcome, err := m.StartHLSOwned(ctx, req)
				if s != nil {
					t.Error("canceled start returned a session")
				}
				returned <- result{outcome, err}
			}()
			encoderSignal(t, entered)
			cancel()
			r := <-returned
			if !errors.Is(r.err, context.Canceled) || r.outcome == nil {
				t.Fatalf("canceled result=%+v", r)
			}
			assertEncoderPending(t, r.outcome)
			if err := m.Close(); !errors.Is(err, core.ErrEncoderPending) {
				t.Fatalf("late start shutdown=%v", err)
			}
			unblock()
			encoderSignal(t, r.outcome.EncoderDone())
			if _, ok := m.Session("late"); ok {
				t.Fatal("late encoder was published")
			}
			if !errors.Is(r.outcome.EncoderErr(), core.ErrEncoderInputUnverified) {
				t.Fatalf("unverified input result=%v", r.outcome.EncoderErr())
			}
		})
	}
}

func TestEncoderRetirementRepeatedLegacyCloseReleasesCapacity(t *testing.T) {
	m := newTestManager(t, newFakeRunner(), config.Transcode{MaxTranscodes: 1})
	m.encoderLimit = 2
	for i := 0; i < 12; i++ {
		s := startSession(t, m, transcodeRequest(t, fmt.Sprintf("repeat-%d", i)))
		outcome := s.EncoderRetirement()
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		encoderSignal(t, outcome.EncoderDone())
		if !errors.Is(outcome.EncoderErr(), core.ErrEncoderInputUnverified) {
			t.Fatalf("missing producer proof=%v", outcome.EncoderErr())
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(s.dir); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("completed output retained: %v", err)
		}
		m.mu.Lock()
		n, charged := len(m.encoders), m.encodingLocked("")
		m.mu.Unlock()
		if n != 0 || charged != 0 {
			t.Fatalf("completed work retained capacity: lifetimes=%d encoders=%d", n, charged)
		}
	}
}

func TestEncoderRetirementBoundsStalledStarts(t *testing.T) {
	fr := newFakeRunner()
	entered := make(chan struct{}, 2)
	release, unblock := encoderLatch()
	runner := retirementRunner{Runner: fr, start: func(c Command) (Process, error) {
		entered <- struct{}{}
		<-release
		return fr.Start(c)
	}}
	m := newTestManager(t, fr, config.Transcode{}, WithRunner(runner))
	m.encoderLimit = 2
	joinEncoderResources(t, m, unblock)
	var outcomes []core.EncoderRetirement
	for i := 0; i < 2; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		returned := make(chan core.EncoderRetirement, 1)
		req := remuxRequest(t, fmt.Sprintf("stalled-%d", i), time.Hour)
		go func() { _, o, _ := m.StartHLSOwned(ctx, req); returned <- o }()
		encoderSignal(t, entered)
		cancel()
		outcomes = append(outcomes, <-returned)
	}
	if _, _, err := m.StartHLSOwned(context.Background(), remuxRequest(t, "over", time.Hour)); !errors.Is(err, ErrEncoderCapacity) {
		t.Fatalf("capacity=%v", err)
	}
	unblock()
	for _, o := range outcomes {
		encoderSignal(t, o.EncoderDone())
	}
}

func TestEncoderRetirementJoinsSessionPreflight(t *testing.T) {
	fr := newFakeRunner()
	entered := make(chan struct{})
	release, unblock := encoderLatch()
	runner := retirementRunner{Runner: fr, run: func(ctx context.Context, c Command) error {
		if c.Path != "owned-sample" {
			return fr.Run(ctx, c)
		}
		close(entered)
		<-release // deliberately ignores request cancellation
		return nil
	}}
	m := newTestManager(t, fr, config.Transcode{}, WithRunner(runner))
	joinEncoderResources(t, m, unblock)
	m.encoderTimeout = 20 * time.Millisecond
	s := startSession(t, m, remuxRequest(t, "preflight", time.Hour))
	joined := make(chan error, 1)
	go func() { joined <- s.encoderRun(context.Background(), Command{Path: "owned-sample"}) }()
	encoderSignal(t, entered)
	if err := s.Close(); !errors.Is(err, core.ErrEncoderPending) {
		t.Fatalf("unjoined sample Close=%v", err)
	}
	assertEncoderPending(t, s.EncoderRetirement())
	unblock()
	if err := <-joined; err != nil {
		t.Fatal(err)
	}
	encoderSignal(t, s.encoder.done)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestEncoderRetirementKnownFailedStartRollsBack(t *testing.T) {
	fr := newFakeRunner()
	startErr := errors.New("known failed start")
	fr.startError = startErr
	m := newTestManager(t, fr, config.Transcode{MaxTranscodes: 1})
	_, outcome, err := m.StartHLSOwned(context.Background(), transcodeRequest(t, "failed"))
	if !errors.Is(err, startErr) || outcome == nil {
		t.Fatalf("failed start=%v outcome=%v", err, outcome)
	}
	encoderSignal(t, outcome.EncoderDone())
	if err := outcome.EncoderErr(); err != nil {
		t.Fatalf("known no-process rollback=%v", err)
	}
	m.mu.Lock()
	left, charge := len(m.encoders), m.encodingLocked("")
	m.mu.Unlock()
	if left != 0 || charge != 0 {
		t.Fatalf("failed start retained lifetime/charge: %d/%d", left, charge)
	}
}

func TestEncoderRetirementBoundsCleanupWorkersAndQueuedDetach(t *testing.T) {
	m := newTestManager(t, newFakeRunner(), config.Transcode{})
	release, unblock := encoderLatch()
	joinEncoderResources(t, m, unblock)
	m.encoderTimeout = 20 * time.Millisecond
	entered := make(chan struct{}, 8)
	m.encoderRemove = func(dir string) error {
		entered <- struct{}{}
		<-release
		return removeAllRetry(dir)
	}
	var sessions []*Session
	for i := 0; i < encoderCleanupWorkers+3; i++ {
		sessions = append(sessions, startSession(t, m, remuxRequest(t, fmt.Sprintf("cleanup-%d", i), time.Hour)))
	}
	for _, s := range sessions {
		if err := s.RetireEncoder(); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < encoderCleanupWorkers; i++ {
		encoderSignal(t, entered)
	}
	if err := m.Close(); !errors.Is(err, core.ErrEncoderPending) {
		t.Fatalf("stalled filesystem shutdown=%v", err)
	}
	select {
	case <-entered:
		t.Fatal("cleanup spawned beyond its fixed worker bound")
	default:
	}
	for _, s := range sessions {
		assertEncoderPending(t, s.EncoderRetirement())
	}
	unblock()
	for _, s := range sessions {
		encoderSignal(t, s.encoder.done)
	}
}

func TestEncoderRetirementProtectsOutputAcrossManagerReplacement(t *testing.T) {
	fr := newFakeRunner()
	entered := make(chan struct{})
	release, unblock := encoderLatch()
	runner := retirementRunner{Runner: fr, start: func(c Command) (Process, error) {
		p, err := fr.Start(c)
		return &retirementProcess{Process: p, stopEntered: entered, stopRelease: release}, err
	}}
	m := newTestManager(t, fr, config.Transcode{}, WithRunner(runner))
	joinEncoderResources(t, m, unblock)
	m.encoderTimeout = 20 * time.Millisecond
	s := startSession(t, m, remuxRequest(t, "protected", time.Hour))
	if err := s.RetireEncoder(); err != nil {
		t.Fatal(err)
	}
	encoderSignal(t, entered)
	// Startup cleanup in another manager in this process cannot reclaim an
	// output directory still rooted by a stalled exact lifetime.
	other := newTestManager(t, newFakeRunner(), config.Transcode{TempDir: m.tempDir})
	if _, err := os.Stat(s.dir); err != nil {
		t.Fatalf("replacement manager deleted protected output: %v", err)
	}
	if err := other.Close(); err != nil {
		t.Fatal(err)
	}
	assertEncoderPending(t, s.EncoderRetirement())
}

func TestEncoderRetirementCleanupErrorIsTerminalAndRetainsCharge(t *testing.T) {
	m := newTestManager(t, newFakeRunner(), config.Transcode{MaxTranscodes: 1})
	cleanupErr := errors.New("unproved directory cleanup")
	m.encoderRemove = func(string) error { return cleanupErr }
	s := startSession(t, m, transcodeRequest(t, "cleanup-error"))
	// This fixture repairs and verifies the filesystem explicitly after testing
	// retained responsibility. Production has no implicit success conversion.
	t.Cleanup(func() {
		encoderSignal(t, s.encoder.done)
		if err := removeAllRetry(s.dir); err != nil {
			t.Fatal(err)
		}
		encoderCleanup.dirs.Delete(s.dir)
		m.mu.Lock()
		delete(m.encoders, s.encoder)
		delete(m.encoderIDs, s.id)
		m.mu.Unlock()
		<-encoderCleanup.slots
	})
	if err := s.Close(); !errors.Is(err, cleanupErr) {
		t.Fatalf("cleanup result=%v", err)
	}
	encoderSignal(t, s.encoder.done)
	if !errors.Is(s.encoder.EncoderErr(), cleanupErr) {
		t.Fatalf("terminal proof=%v", s.encoder.EncoderErr())
	}
	if err := s.RetireEncoder(); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(s.encoder.EncoderErr(), cleanupErr) {
		t.Fatal("terminal error changed into successful proof")
	}
	m.mu.Lock()
	lifetimes, charge := len(m.encoders), m.encodingLocked("")
	m.mu.Unlock()
	if lifetimes != 1 || charge != 1 {
		t.Fatalf("cleanup error lost capacity: %d/%d", lifetimes, charge)
	}
	if _, err := m.StartHLS(context.Background(), transcodeRequest(t, "replacement")); !errors.Is(err, ErrTooManyTranscodes) {
		t.Fatalf("cleanup error allowed overcommit: %v", err)
	}
}

type countedEncoderProcess struct {
	Process
	active *atomic.Int32
	once   sync.Once
}

func (p *countedEncoderProcess) Stop() {
	p.Process.Stop()
	p.once.Do(func() { p.active.Add(-1) })
}

func TestEncoderRetirementSerializesSameIDBeforeStart(t *testing.T) {
	fr := newFakeRunner()
	firstEntered := make(chan struct{})
	release, unblock := encoderLatch()
	var calls, active atomic.Int32
	overlap := make(chan int32, 4)
	runner := retirementRunner{Runner: fr, start: func(c Command) (Process, error) {
		if calls.Add(1) == 1 {
			close(firstEntered)
			<-release
		}
		p, err := fr.Start(c)
		if p == nil {
			return nil, err
		}
		if n := active.Add(1); n > 1 {
			overlap <- n
		}
		return &countedEncoderProcess{Process: p, active: &active}, err
	}}
	m := newTestManager(t, fr, config.Transcode{}, WithRunner(runner))
	joinEncoderResources(t, m, unblock)
	type result struct {
		session core.HLSSession
		outcome core.EncoderRetirement
		err     error
	}
	first, second := make(chan result, 1), make(chan result, 1)
	req := remuxRequest(t, "same", time.Hour)
	start := func(out chan<- result) {
		s, o, err := m.StartHLSOwned(context.Background(), req)
		out <- result{s, o, err}
	}
	go start(first)
	encoderSignal(t, firstEntered)
	m.mu.Lock()
	prior := m.encoderIDs["same"]
	m.mu.Unlock()
	if prior == nil {
		t.Fatal("Start ran before exact ID reservation")
	}
	go start(second)
	encoderSignal(t, prior.cancel) // second start reached and superseded the first
	if n := calls.Load(); n != 1 {
		t.Fatalf("second Start overlapped pending first: %d calls", n)
	}
	unblock()
	one, two := <-first, <-second
	if one.session != nil || one.err == nil {
		t.Fatalf("superseded start published: %+v", one)
	}
	encoderSignal(t, one.outcome.EncoderDone())
	if two.session == nil || two.err != nil {
		t.Fatalf("replacement failed: %+v", two)
	}
	if got, ok := m.Session("same"); !ok || got != two.session {
		t.Fatal("late old publication replaced the winner")
	}
	if err := two.session.Close(); err != nil {
		t.Fatal(err)
	}
	encoderSignal(t, two.outcome.EncoderDone())
	if n := active.Load(); n != 0 {
		t.Fatalf("actual processes not joined: %d", n)
	}
	select {
	case n := <-overlap:
		t.Fatalf("overlapping exact processes: %d", n)
	default:
	}
	m.mu.Lock()
	left := len(m.encoderIDs)
	m.mu.Unlock()
	if left != 0 {
		t.Fatalf("completed ID reservations=%d", left)
	}
}

func TestEncoderRetirementCapacityFollowsDecisionAcrossRuns(t *testing.T) {
	m := newTestManager(t, newFakeRunner(), config.Transcode{MaxTranscodes: 1})
	s := startSession(t, m, remuxRequest(t, "decision", time.Hour))
	change := func(copyVideo bool) {
		t.Helper()
		s.mu.Lock()
		s.decision.VideoCopy = copyVideo
		if copyVideo {
			s.decision.Kind = core.DecisionRemux
		} else {
			s.decision.Kind = core.DecisionTranscode
		}
		if len(s.rungs) > 0 {
			s.rungs[0].decision = s.decision
		}
		err := s.startRunLocked(0, false)
		s.mu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
	}
	change(false)
	s.mu.Lock()
	s.stopRunLocked()
	s.mu.Unlock()
	s.watchers.Wait() // cooperative test processes actually joined, no waiter
	m.mu.Lock()
	charged := m.encodingLocked("")
	m.mu.Unlock()
	if charged != 1 {
		t.Fatalf("noncopy decision lost reserved capacity between runs: %d", charged)
	}
	if _, err := m.StartHLS(context.Background(), transcodeRequest(t, "over")); !errors.Is(err, ErrTooManyTranscodes) {
		t.Fatalf("noncopy decision overcommitted: %v", err)
	}
	change(true)
	m.mu.Lock()
	charged = m.encodingLocked("")
	m.mu.Unlock()
	if charged != 0 {
		t.Fatalf("verified copy replacement retained completed encoder charge: %d", charged)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}
