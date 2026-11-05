package resolver

import (
	"container/heap"
	"time"
)

// availabilityDeadline is independent of cache eviction: an already published
// guide can still reference a title whose resolver cache entry was evicted.
type availabilityDeadline struct {
	key     int64
	expires time.Time
	index   int
}
type availabilityDeadlines []*availabilityDeadline

func (h availabilityDeadlines) Len() int           { return len(h) }
func (h availabilityDeadlines) Less(i, j int) bool { return h[i].expires.Before(h[j].expires) }
func (h availabilityDeadlines) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].index = i
	h[j].index = j
}
func (h *availabilityDeadlines) Push(v any) {
	e := v.(*availabilityDeadline)
	e.index = len(*h)
	*h = append(*h, e)
}
func (h *availabilityDeadlines) Pop() any {
	old := *h
	n := len(old)
	e := old[n-1]
	old[n-1] = nil
	*h = old[:n-1]
	return e
}

// AvailabilityEpoch is an optional constant/heap-bounded capability used by
// background guide snapshots. It never scans the catalogue or queries a provider.
// Expiration work is capped, so a large simultaneous expiry cannot block an
// owner save. Remaining expirations are consumed by subsequent background polls.
func (s *Service) AvailabilityEpoch() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	changed := false
	for n := 0; n < 64 && len(s.availabilityDeadlines) > 0; n++ {
		e := s.availabilityDeadlines[0]
		if !now.After(e.expires) {
			break
		}
		heap.Pop(&s.availabilityDeadlines)
		delete(s.availabilityDeadlineByKey, e.key)
		changed = true
	}
	if changed {
		s.availabilityGeneration++
	}
	return s.availabilityGeneration
}

// Call with s.mu held. Hydration registers expiry without changing the epoch:
// loading the same database fact is not a semantic availability update.
func (s *Service) trackAvailabilityLocked(key int64, expires time.Time) {
	if s.availabilityDeadlineByKey == nil {
		s.availabilityDeadlineByKey = map[int64]*availabilityDeadline{}
	}
	if prior := s.availabilityDeadlineByKey[key]; prior != nil {
		prior.expires = expires
		heap.Fix(&s.availabilityDeadlines, prior.index)
		return
	}
	e := &availabilityDeadline{key: key, expires: expires}
	s.availabilityDeadlineByKey[key] = e
	heap.Push(&s.availabilityDeadlines, e)
}
func (s *Service) forgetAvailabilityEpochLocked(key int64) {
	if prior := s.availabilityDeadlineByKey[key]; prior != nil {
		heap.Remove(&s.availabilityDeadlines, prior.index)
		delete(s.availabilityDeadlineByKey, key)
	}
	s.availabilityGeneration++
}
func (s *Service) clearAvailabilityEpochLocked() {
	s.availabilityGeneration++
	s.availabilityDeadlines = nil
	s.availabilityDeadlineByKey = nil
}
