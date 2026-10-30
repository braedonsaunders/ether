package auth

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/plextv"
)

// validate checks cancellation once itself, then again at flight entry. Hold
// both callers at that second check to expose the old check-before-join race.
type admissionContext struct {
	context.Context
	checks  atomic.Int32
	ready   chan<- struct{}
	proceed <-chan struct{}
}

func (c *admissionContext) Err() error {
	if c.checks.Add(1) == 2 {
		c.ready <- struct{}{}
		select {
		case <-c.proceed:
		case <-time.After(time.Second):
			return context.DeadlineExceeded
		}
	}
	return c.Context.Err()
}

type admissionValidator struct {
	calls   atomic.Int32
	release <-chan struct{}
}

func (v *admissionValidator) Account(ctx context.Context, _ string) (*plextv.Account, error) {
	v.calls.Add(1)
	select {
	case <-v.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(5 * time.Second):
		return nil, context.DeadlineExceeded
	}
	return nil, core.ErrUnauthorized
}
func (*admissionValidator) HasServer(context.Context, string) (bool, error) { return false, nil }

func TestValidationAdmissionIsAtomic(t *testing.T) {
	s, _, _ := setup(t)
	s.o.MaxConcurrentValidations = 1
	synctest.Test(t, func(t *testing.T) {
		ready := make(chan struct{}, 2)
		proceed := make(chan struct{})
		release := make(chan struct{})
		v := &admissionValidator{release: release}
		s.o.Validator = v
		results := make(chan error, 2)
		for _, token := range []string{"audit-one", "audit-two"} {
			go func() {
				ctx := &admissionContext{Context: context.Background(), ready: ready, proceed: proceed}
				_, err := s.validate(ctx, token, HashToken(token))
				results <- err
			}()
		}
		for range 2 {
			select {
			case <-ready:
			case <-time.After(time.Second):
				t.Fatal("admission barrier timed out")
			}
		}
		close(proceed)
		synctest.Wait()
		calls := v.calls.Load()
		pending := s.inflight.Len()
		close(release)
		synctest.Wait()
		temporary, unauthorized := 0, 0
		for range 2 {
			var err error
			select {
			case err = <-results:
			case <-time.After(time.Second):
				t.Fatal("validation result timed out")
			}
			if errors.Is(err, core.ErrUnauthorized) {
				unauthorized++
			} else if err != nil {
				temporary++
			}
		}
		if calls != 1 || pending != 1 || temporary != 1 || unauthorized != 1 {
			t.Fatalf("calls=%d pending=%d temporary=%d unauthorized=%d; want 1 each", calls, pending, temporary, unauthorized)
		}
		if s.inflight.Len() != 0 {
			t.Fatal("validation capacity was not released")
		}
	})
}

func TestValidationAdmissionRetainsStaleFallback(t *testing.T) {
	s, _, clock := setup(t)
	owner, err := s.Authenticate(context.Background(), "server-token")
	if err != nil {
		t.Fatal(err)
	}
	s.o.MaxConcurrentValidations = 1
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		v := &admissionValidator{release: release}
		s.o.Validator = v
		done := make(chan error, 1)
		go func() { _, err := s.validate(context.Background(), "occupied", HashToken("occupied")); done <- err }()
		synctest.Wait()
		hash := HashToken("stale-device")
		s.mu.Lock()
		s.pos[hash] = cacheEntry{userID: owner.ID, expires: clock.now().Add(-time.Second)}
		s.mu.Unlock()
		u, err := s.validate(context.Background(), "stale-device", hash)
		if err != nil || u == nil || u.ID != owner.ID {
			t.Errorf("capacity lost valid stale fallback: user=%v err=%v", u, err)
		}
		s.mu.Lock()
		s.pos[hash] = cacheEntry{userID: owner.ID, expires: clock.now().Add(-2 * s.o.StaleTTL)}
		s.mu.Unlock()
		if u, err := s.validate(context.Background(), "stale-device", hash); u != nil || err == nil || errors.Is(err, core.ErrUnauthorized) {
			t.Errorf("expired stale should be temporary failure: user=%v err=%v", u, err)
		}
		if v.calls.Load() != 1 {
			t.Error("refused validation reached upstream")
		}
		close(release)
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("validation did not finish")
		}
	})
}
