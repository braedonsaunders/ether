package config

import (
	"context"
	"sync"
)

// PolicySnapshot owns a local policy read lease and an immutable configuration
// copy. SharedVersion is the exact clustered version checked at acquisition;
// it may already be stale after a remote write. A durable authorization must
// recheck that version inside its epoch-fenced transaction before publishing.
// The caller must Close; this type does not itself authorize any operation.
type PolicySnapshot struct {
	config        Config
	sharedVersion int64
	clustered     bool
	release       func()
	once          sync.Once
}

func (s *Store) AcquirePolicySnapshot(ctx context.Context) (*PolicySnapshot, error) {
	release, err := s.AcquirePolicy(ctx)
	if err != nil {
		return nil, err
	}
	s.mu.RLock()
	cfg, version, clustered := clone(s.cfg), s.sharedVer, s.shared != nil
	s.mu.RUnlock()
	return &PolicySnapshot{config: cfg, sharedVersion: version, clustered: clustered, release: release}, nil
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
