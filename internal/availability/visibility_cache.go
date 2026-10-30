package availability

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/braedonsaunders/ether/internal/core"
	"time"
)

type visibilityEntry struct {
	at      time.Time
	visible bool
}

// A short local memo keeps repeated Plex artwork/list requests inexpensive.
// New observations and invalidations clear it immediately. Other cluster nodes
// see changes within fifteen seconds, without a provider request.
func (s *Service) FilterDiscovery(ctx context.Context, items []core.Title) []core.Title {
	now := s.now()
	keys := make(map[core.RatingKey]string)
	allowed := map[core.RatingKey]bool{}
	var pending []core.Title
	policy := fmt.Sprintf("%s:%+v", s.policy(), s.cfg().Availability)
	feeds := []byte(fmt.Sprintf("%+v", s.d.Feeds.FeedSources()))
	s.visibilityMu.Lock()
	revision := s.visibilityRevision
	for _, t := range items {
		if !evidenceWindow(t, now, s.cfg().Availability) {
			allowed[t.Key] = true
			continue
		}
		if _, seen := keys[t.Key]; seen {
			continue
		}
		raw, _ := json.Marshal(t)
		key := fmt.Sprintf("%s:%x:%x", policy, sha256.Sum256(feeds), sha256.Sum256(raw))
		keys[t.Key] = key
		if e, ok := s.visibilityCache[key]; ok && now.Sub(e.at) < 15*time.Second && !now.Before(e.at) {
			allowed[t.Key] = e.visible
		} else {
			pending = append(pending, t)
		}
	}
	s.visibilityMu.Unlock()
	if len(pending) > 0 {
		for _, t := range s.filterDiscovery(ctx, pending) {
			allowed[t.Key] = true
		}
		s.visibilityMu.Lock()
		if revision == s.visibilityRevision {
			if s.visibilityCache == nil || len(s.visibilityCache)+len(pending) > 8192 {
				s.visibilityCache = map[string]visibilityEntry{}
			}
			for _, t := range pending {
				s.visibilityCache[keys[t.Key]] = visibilityEntry{now, allowed[t.Key]}
			}
		}
		s.visibilityMu.Unlock()
	}
	out := make([]core.Title, 0, len(items))
	for _, t := range items {
		if allowed[t.Key] {
			out = append(out, t)
		}
	}
	return out
}
func (s *Service) clearVisibility() {
	s.visibilityMu.Lock()
	s.visibilityRevision++
	s.visibilityCache = nil
	s.visibilityMu.Unlock()
}

type currentPolicyKey struct{}

// FilterDiscoveryCurrent bypasses node-local visibility and resolver memos for
// one bounded policy preflight batch. It performs no provider lookup.
func (s *Service) FilterDiscoveryCurrent(ctx context.Context, items []core.Title) []core.Title {
	return s.filterDiscovery(context.WithValue(ctx, currentPolicyKey{}, true), items)
}
