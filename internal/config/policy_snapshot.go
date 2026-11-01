package config

import (
	"context"
	"sync"
)

// PolicySnapshot is an immutable configuration copy plus the clustered
// version observed when it was taken. It does not hold the policy lock:
// a browse path must not stall a writer. SharedVersion may already be stale
// after a remote write. A durable authorization must recheck that version
// inside its epoch-fenced transaction before publishing. Close is idempotent
// and does not authorize any operation.
type PolicySnapshot struct {
	config        Config
	sharedVersion int64
	clustered     bool
	release       func()
	once          sync.Once
}

func (s *Store) AcquirePolicySnapshot(ctx context.Context) (*PolicySnapshot, error) {
	for attempt := 0; ; attempt++ {
		snap, lag, err := s.copyPolicySnapshot(ctx)
		if err != nil {
			return nil, err
		}
		if !lag {
			return snap, nil
		}
		if attempt == 1 {
			return nil, errPolicyLag
		}
		if err := s.RefreshShared(ctx); err != nil {
			return nil, err
		}
	}
}

// copyPolicySnapshot copies config under a brief read lock, then checks the
// clustered version after releasing it. Writers are not blocked on that read.
func (s *Store) copyPolicySnapshot(ctx context.Context) (*PolicySnapshot, bool, error) {
	s.policyMu.RLock()
	if s.policyErr != nil {
		err := s.policyErr
		s.policyMu.RUnlock()
		return nil, false, err
	}
	s.mu.RLock()
	cfg, shared, version := clone(s.cfg), s.shared, s.sharedVer
	s.mu.RUnlock()
	s.policyMu.RUnlock()
	if shared != nil {
		current, err := shared.SharedVersion(ctx)
		if err != nil {
			return nil, false, err
		}
		if current != version {
			return nil, true, nil
		}
	}
	return &PolicySnapshot{config: cfg, sharedVersion: version, clustered: shared != nil, release: func() {}}, false, nil
}

// Config returns a detached copy. It is meaningful for authorization only
// until Close and while the durable shared-version and epoch checks succeed.
func (p *PolicySnapshot) Config() Config               { return clone(p.config) }
func (p *PolicySnapshot) SharedVersion() (int64, bool) { return p.sharedVersion, p.clustered }
func (p *PolicySnapshot) Close() {
	if p != nil {
		p.once.Do(p.release)
	}
}
