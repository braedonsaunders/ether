package auth

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/plextv"
)

type blockedValidator struct {
	stage   string
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (v *blockedValidator) wait(ctx context.Context, stage string) error {
	if v.stage != stage {
		return nil
	}
	v.once.Do(func() { close(v.entered) })
	select {
	case <-v.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (v *blockedValidator) Account(ctx context.Context, _ string) (*plextv.Account, error) {
	if err := v.wait(ctx, "account"); err != nil {
		return nil, err
	}
	return &plextv.Account{ID: 100, Username: "viewer"}, nil
}

func (v *blockedValidator) HasServer(ctx context.Context, _ string) (bool, error) {
	return true, v.wait(ctx, "resources")
}

func TestSlowValidationDoesNotBlockRevocation(t *testing.T) {
	for _, stage := range []string{"account", "resources"} {
		t.Run(stage, func(t *testing.T) {
			s, _, _ := setup(t)
			// Model a viewer originally admitted by personal-token validation.
			if _, err := s.db.Exec(`UPDATE auth_users SET origin='validated' WHERE plex_id=100`); err != nil {
				t.Fatal(err)
			}
			if err := s.reload(context.Background()); err != nil {
				t.Fatal(err)
			}
			v := &blockedValidator{stage: stage, entered: make(chan struct{}), release: make(chan struct{})}
			s.o.Validator = v
			validated := make(chan error, 1)
			go func() { _, err := s.Authenticate(context.Background(), "new-device"); validated <- err }()
			<-v.entered
			synced := make(chan error, 1)
			go func() { synced <- s.SyncFriends(context.Background(), nil) }()
			var syncErr error
			select {
			case syncErr = <-synced:
			case <-time.After(2 * time.Second):
				close(v.release)
				<-validated
				<-synced
				t.Fatal("revocation waited for an untrusted token's network request")
			}
			close(v.release)
			validationErr := <-validated
			if syncErr != nil {
				t.Fatal(syncErr)
			}
			if !errors.Is(validationErr, core.ErrUnauthorized) {
				t.Fatalf("in-flight validation restored revoked access: %v", validationErr)
			}
			if _, err := s.Authenticate(context.Background(), "alice-access"); !errors.Is(err, core.ErrUnauthorized) {
				t.Fatalf("revoked access token accepted: %v", err)
			}
		})
	}
}

func TestUnknownTokenValidationCapacity(t *testing.T) {
	s, _, _ := setup(t)
	v := &blockedValidator{stage: "account", entered: make(chan struct{}), release: make(chan struct{})}
	s.o.Validator = v
	s.o.MaxConcurrentValidations = 1
	first := make(chan error, 1)
	go func() { _, err := s.Authenticate(context.Background(), "first-device"); first <- err }()
	<-v.entered
	var pending chan error
	defer func() {
		close(v.release)
		<-first
		if pending != nil {
			<-pending
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	rejected := make(chan error, 1)
	go func() { _, err := s.Authenticate(ctx, "another-device"); rejected <- err }()
	select {
	case err := <-rejected:
		if err == nil || errors.Is(err, core.ErrUnauthorized) {
			t.Fatalf("capacity should be a temporary failure, got %v", err)
		}
	case <-ctx.Done():
		// The regression must release both workers even when the bound fails.
		t.Error("excess token validation queued instead of failing fast")
		pending = rejected
	}
	if u, err := s.Authenticate(context.Background(), "server-token"); err != nil || !u.IsOwner {
		t.Fatalf("known owner token unavailable at capacity: %v", err)
	}
	s.mu.RLock()
	n := s.inflight.Len()
	s.mu.RUnlock()
	if n != 1 {
		t.Errorf("unbounded in-flight validations: %d", n)
	}
}

// A permissive validator simulates plex.tv accepting arbitrary credentials.
// Ether grants must be rejected before they can reach it.
type acceptingValidator struct{ calls int }

func (v *acceptingValidator) Account(context.Context, string) (*plextv.Account, error) {
	v.calls++
	return &plextv.Account{ID: 100}, nil
}
func (v *acceptingValidator) HasServer(context.Context, string) (bool, error) { return true, nil }
func TestPlexRejectsSurfaceGrantBeforeValidation(t *testing.T) {
	s, _, _ := setup(t)
	v := &acceptingValidator{}
	s.o.Validator = v
	if _, err := s.Authenticate(context.Background(), "eg1_arbitrary"); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("got %v", err)
	}
	if v.calls != 0 {
		t.Fatal("grant reached Plex validator")
	}
}
