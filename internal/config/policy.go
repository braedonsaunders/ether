package config

import (
	"context"
	"errors"
	"reflect"
)

// SetPolicyEpoch binds durable invalidation after a visibility configuration
// transition. The lease excludes local publication through receipt commit.
func (s *Store) SetPolicyEpoch(bump func() error) {
	s.policyMu.Lock()
	defer s.policyMu.Unlock()
	s.policyBump = bump
}

func samePolicy(a, b Config) bool {
	return reflect.DeepEqual(a.Catalog, b.Catalog) && reflect.DeepEqual(a.Personalize, b.Personalize) && reflect.DeepEqual(a.Availability, b.Availability)
}

func (s *Store) policyUpdate(update func() error) error {
	s.policyMu.Lock()
	defer s.policyMu.Unlock()
	before := s.Get()
	err := update()
	if !samePolicy(before, s.Get()) && s.policyBump != nil {
		s.policyErr = s.policyBump()
		if s.policyErr != nil {
			return s.policyErr
		}
	}
	return err
}

// AcquirePolicy keeps local policy stable and refuses a cluster cache whose
// shared document has not caught up with the authoritative database version.
func (s *Store) AcquirePolicy(ctx context.Context) (func(), error) {
	for attempt := 0; ; attempt++ {
		release, err := s.acquirePolicy(ctx)
		if !errors.Is(err, errPolicyLag) || attempt == 1 {
			return release, err
		}
		if err := s.RefreshShared(ctx); err != nil {
			return nil, err
		}
	}
}

var errPolicyLag = errors.New("config: policy cache is behind shared settings")

func (s *Store) acquirePolicy(ctx context.Context) (func(), error) {
	s.policyMu.RLock()
	fail := func(err error) (func(), error) { s.policyMu.RUnlock(); return nil, err }
	if s.policyErr != nil {
		return fail(s.policyErr)
	}
	s.mu.RLock()
	b, version := s.shared, s.sharedVer
	s.mu.RUnlock()
	if b != nil {
		current, err := b.SharedVersion(ctx)
		if err != nil {
			return fail(err)
		}
		if current != version {
			return fail(errPolicyLag)
		}
	}
	return s.policyMu.RUnlock, nil
}
