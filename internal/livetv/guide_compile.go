package livetv

import (
	"context"
	"sync"
	"time"
)

type guideCompileResult struct {
	ready    chan struct{}
	snapshot *virtualGuideSnapshot
	err      error
}

// guideCompileCache belongs to a single refresh pass, shared by all workers.
// It retains only the current pass's new/shared tables, never a second lineup.
type guideCompileCache struct {
	mu               sync.Mutex
	entries          map[string]*guideCompileResult
	availabilityOnce sync.Once
	availability     uint64
}

func (c *guideCompileCache) fingerprint(s *Service, channel Channel, revision uint64, now time.Time) string {
	c.availabilityOnce.Do(func() { c.availability = s.snapshotAvailabilityEpoch(now) })
	return s.snapshotFingerprintAt(channel, revision, c.availability)
}
func (c *guideCompileCache) remember(key string, snapshot *virtualGuideSnapshot) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[string]*guideCompileResult{}
	}
	if c.entries[key] != nil {
		return
	}
	ready := make(chan struct{})
	close(ready)
	c.entries[key] = &guideCompileResult{ready: ready, snapshot: snapshot}
}
func (c *guideCompileCache) build(ctx context.Context, key string, build func() (*virtualGuideSnapshot, error)) (*virtualGuideSnapshot, error) {
	c.mu.Lock()
	if c.entries == nil {
		c.entries = map[string]*guideCompileResult{}
	}
	if existing := c.entries[key]; existing != nil {
		c.mu.Unlock()
		select {
		case <-existing.ready:
			return existing.snapshot, existing.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	result := &guideCompileResult{ready: make(chan struct{})}
	c.entries[key] = result
	c.mu.Unlock()
	result.snapshot, result.err = build()
	close(result.ready)
	return result.snapshot, result.err
}
