package transcode

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

type testEncoderInput struct {
	mu                sync.Mutex
	allowed           map[string]bool
	claimed           bool
	sealed            bool
	drained           bool
	blocked           bool
	immediate         bool
	claimErr          error
	result            error
	callback          func(error)
	done, sealEntered chan struct{}
	sealOnce          sync.Once
	callbacks         atomic.Int32
}

func newTestEncoderInput(input string, blocked bool) *testEncoderInput {
	return &testEncoderInput{allowed: map[string]bool{input: true, input + "?background=1": true}, blocked: blocked, done: make(chan struct{}), sealEntered: make(chan struct{})}
}
func (s *testEncoderInput) ClaimInput(cb func(error)) error {
	s.mu.Lock()
	if s.claimed || s.sealed || s.claimErr != nil {
		err := s.claimErr
		s.mu.Unlock()
		if err == nil {
			err = core.ErrNotAvailable
		}
		return err
	}
	s.claimed, s.callback = true, cb
	immediate := s.immediate
	s.mu.Unlock()
	if immediate {
		s.SealInput()
	} // callback may beat the successful return
	return nil
}
func (s *testEncoderInput) HeadersForInput(input string) (map[string]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.claimed || s.sealed || !s.allowed[input] {
		return nil, core.ErrNotAvailable
	}
	return map[string]string{core.EncoderInputScopeHeader: strings.Repeat("ab", 32)}, nil
}
func (s *testEncoderInput) SealInput() {
	s.mu.Lock()
	s.sealed = true
	s.mu.Unlock()
	s.sealOnce.Do(func() { close(s.sealEntered) })
	s.drain()
}
func (s *testEncoderInput) release() { s.mu.Lock(); s.blocked = false; s.mu.Unlock(); s.drain() }
func (s *testEncoderInput) drain() {
	s.mu.Lock()
	if !s.sealed || s.blocked || s.drained {
		s.mu.Unlock()
		return
	}
	s.drained = true
	cb := s.callback
	close(s.done)
	s.mu.Unlock()
	if cb != nil {
		s.callbacks.Add(1)
		cb(s.result)
	}
}
func (s *testEncoderInput) InputDone() <-chan struct{} { return s.done }
func (s *testEncoderInput) InputErr() error {
	select {
	case <-s.done:
		return s.result
	default:
		return core.ErrEncoderInputPending
	}
}

func TestEncoderInputTicketClaimRollbackAndImmediateDrain(t *testing.T) {
	for _, mode := range []string{"failed-claim", "immediate-callback", "failed-start", "live-playlist"} {
		t.Run(mode, func(t *testing.T) {
			fr := newFakeRunner()
			req := remuxRequest(t, mode, time.Hour)
			lease := newTestEncoderInput(req.InputURL, false)
			t.Cleanup(lease.SealInput)
			req.InputLease = lease
			if mode == "live-playlist" {
				req.LiveInput = true
			}
			if mode == "failed-claim" {
				lease.claimErr = errors.New("claim refused")
			}
			if mode == "immediate-callback" {
				lease.immediate = true
			}
			if mode == "failed-start" {
				fr.startError = errors.New("Start failed")
			}
			m := newTestManager(t, fr, config.Transcode{})
			_, outcome, err := m.StartHLSOwned(context.Background(), req)
			if err == nil || outcome == nil {
				t.Fatalf("failure=%v outcome=%v", err, outcome)
			}
			encoderSignal(t, outcome.EncoderDone())
			if outcome.EncoderErr() != nil {
				t.Fatalf("proved no-resource rollback=%v", outcome.EncoderErr())
			}
			lease.SealInput()
			wantCallbacks := int32(1)
			if mode == "failed-claim" || mode == "live-playlist" {
				wantCallbacks = 0
			}
			if lease.callbacks.Load() != wantCallbacks {
				t.Fatalf("callbacks=%d want%d", lease.callbacks.Load(), wantCallbacks)
			}
			m.mu.Lock()
			remaining := len(m.encoders)
			m.mu.Unlock()
			if remaining != 0 {
				t.Fatalf("input ticket leaked after rollback: %d", remaining)
			}
		})
	}
}

func TestEncoderInputVerifiedDrainAndOneShot(t *testing.T) {
	m := newTestManager(t, newFakeRunner(), config.Transcode{MaxTranscodes: 1})
	m.encoderLimit = 2
	for i := 0; i < 12; i++ {
		req := transcodeRequest(t, fmt.Sprintf("verified-%d", i))
		lease := newTestEncoderInput(req.InputURL, false)
		t.Cleanup(lease.SealInput)
		req.InputLease = lease
		s, outcome, err := m.StartHLSOwned(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		// A failed second claim cannot seal or retire the first encoder, even
		// when its caller supplies the same public session ID.
		_, failed, err := m.StartHLSOwned(context.Background(), req)
		if err == nil || failed == nil {
			t.Fatal("second encoder reused one-shot input")
		}
		encoderSignal(t, failed.EncoderDone())
		if current, ok := m.Session(req.SessionID); !ok || current != s {
			t.Fatal("failed claim retired another encoder")
		}
		if _, err := lease.HeadersForInput(req.InputURL); err != nil || lease.callbacks.Load() != 0 {
			t.Fatal("failed claim sealed another encoder input")
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		encoderSignal(t, outcome.EncoderDone())
		if err := outcome.EncoderErr(); err != nil {
			t.Fatalf("verified terminal proof=%v", err)
		}
		if lease.callbacks.Load() != 1 {
			t.Fatalf("successful claim callbacks=%d", lease.callbacks.Load())
		}
	}
}

func TestEncoderInputStallsWithoutOccupyingCleanupWorkers(t *testing.T) {
	m := newTestManager(t, newFakeRunner(), config.Transcode{})
	m.encoderLimit = encoderCleanupWorkers + 2
	m.encoderTimeout = 20 * time.Millisecond
	var leases []*testEncoderInput
	var outcomes []core.EncoderRetirement
	t.Cleanup(func() {
		for _, lease := range leases {
			lease.release()
			lease.SealInput()
		}
		for _, outcome := range outcomes {
			encoderSignal(t, outcome.EncoderDone())
		}
	})
	for i := 0; i < encoderCleanupWorkers+1; i++ {
		req := remuxRequest(t, fmt.Sprintf("stalled-input-%d", i), time.Hour)
		lease := newTestEncoderInput(req.InputURL, true)
		leases = append(leases, lease)
		req.InputLease = lease
		s, outcome, err := m.StartHLSOwned(context.Background(), req)
		if outcome != nil {
			outcomes = append(outcomes, outcome)
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Close(); !errors.Is(err, core.ErrEncoderPending) {
			t.Fatalf("stalled producer Close=%v", err)
		}
		encoderSignal(t, lease.sealEntered)
		assertEncoderPending(t, outcome)
		if _, err := os.Stat(s.(*Session).dir); err != nil {
			t.Fatalf("stalled input output removed: %v", err)
		}
	}
	// More than four stuck scopes cannot park the four cleanup workers. The
	// one remaining lifetime slot can be reused by ordinary legacy closes.
	m.encoderTimeout = 5 * time.Second
	for i := 0; i < 10; i++ {
		s := startSession(t, m, remuxRequest(t, fmt.Sprintf("legacy-%d", i), time.Hour))
		if err := s.Close(); err != nil {
			t.Fatalf("stalled inputs occupied cleanup workers: %v", err)
		}
	}
	m.encoderTimeout = 20 * time.Millisecond
	if err := m.Close(); !errors.Is(err, core.ErrEncoderPending) {
		t.Fatalf("stalled input shutdown=%v", err)
	}
}

func TestEncoderInputCommandHeadersAndDiagnostics(t *testing.T) {
	fr := newFakeRunner()
	m := newTestManager(t, fr, config.Transcode{})
	var logs bytes.Buffer
	m.log = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	req := remuxRequest(t, "scoped-command", time.Hour)
	lease := newTestEncoderInput(req.InputURL, false)
	req.InputLease = lease
	s, outcome, err := m.StartHLSOwned(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	session := s.(*Session)
	if len(session.inputHeaders) != 0 {
		t.Fatal("private input proof became provider metadata and disabled audio matching")
	}
	secret := strings.Repeat("ab", 32)
	if got := argValue(fr.lastStart(), "-headers"); got != core.EncoderInputScopeHeader+": "+secret+"\r\n" {
		t.Fatalf("command did not capture private scope: %q", got)
	}
	sample, err := session.encoderSampleArgs(req.InputURL+"?background=1", []string{"-hide_banner", "-i", req.InputURL + "?background=1", "-f", "rawvideo", "-"})
	if err != nil || argValue(sample, "-headers") == "" {
		t.Fatalf("preflight missing its own scoped binding: %v %v", sample, err)
	}
	if _, err := session.encoderSampleArgs("https://provider.invalid/redirect", []string{"-i", "https://provider.invalid/redirect"}); err == nil {
		t.Fatal("private scope entered provider/redirect command")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	encoderSignal(t, outcome.EncoderDone())
	if strings.Contains(logs.String(), secret) || strings.Contains(logs.String(), req.InputURL) {
		t.Fatal("private token/locator leaked into diagnostics")
	}
	cause := fmt.Errorf("start %s headers %s", req.InputURL, secret)
	redacted := redactScopedCommandError(cause, runSpec{Input: req.InputURL, InputScopeHeaders: map[string]string{core.EncoderInputScopeHeader: secret}})
	if strings.Contains(redacted.Error(), secret) || strings.Contains(redacted.Error(), req.InputURL) || !errors.Is(redacted, cause) {
		t.Fatal("command error leaked secrets or lost error identity")
	}
	if _, err := validatedInputHeaders(map[string]string{core.EncoderInputScopeHeader: secret}); err == nil {
		t.Fatal("provider header minted an internal input scope")
	}
}

func TestEncoderInputProofErrorNeverBecomesSuccess(t *testing.T) {
	m := newTestManager(t, newFakeRunner(), config.Transcode{MaxTranscodes: 1})
	req := transcodeRequest(t, "input-proof-error")
	lease := newTestEncoderInput(req.InputURL, false)
	proofErr := errors.New("input drain outcome unknown")
	lease.result = proofErr
	req.InputLease = lease
	hs, outcome, err := m.StartHLSOwned(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	s := hs.(*Session)
	t.Cleanup(func() {
		encoderSignal(t, outcome.EncoderDone())
		if err := removeAllRetry(s.dir); err != nil {
			t.Fatal(err)
		}
		encoderCleanup.dirs.Delete(s.dir)
		m.mu.Lock()
		delete(m.encoders, s.encoder)
		delete(m.encoderIDs, s.id)
		m.mu.Unlock()
		<-encoderCleanup.slots // fixture-only repair after all actual work joined
	})
	if err := s.Close(); !errors.Is(err, proofErr) {
		t.Fatalf("proof error lost: %v", err)
	}
	encoderSignal(t, outcome.EncoderDone())
	lease.SealInput()
	lease.release()
	if !errors.Is(outcome.EncoderErr(), proofErr) {
		t.Fatal("terminal input proof changed into success")
	}
	m.mu.Lock()
	retained, charge := len(m.encoders), m.encodingLocked("")
	m.mu.Unlock()
	if retained != 1 || charge != 1 {
		t.Fatalf("unproved input lost charges: %d/%d", retained, charge)
	}
	if _, err := os.Stat(s.dir); err != nil {
		t.Fatalf("unproved input output removed: %v", err)
	}
}
