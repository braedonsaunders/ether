package availability

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

type visibilityEntry struct {
	at      time.Time
	visible bool
}

// A short local memo keeps repeated artwork/shelf checks inexpensive while
// browse/rows/search query availability_gate directly in SQL. Opening a gate
// clears this memo immediately.
func (s *Service) FilterDiscovery(ctx context.Context, items []core.Title) []core.Title {
	if len(items) == 0 {
		return items
	}
	snap := s.snapshotFor(ctx)
	for _, t := range items {
		if evidenceWindow(t, snap.now, snap.availability) {
			return s.filterDiscoverySnapshot(ctx, items, snap)
		}
	}
	out := make([]core.Title, len(items))
	copy(out, items)
	return out
}

func (s *Service) filterDiscoverySnapshot(ctx context.Context, items []core.Title, input discoverySnapshot) []core.Title {
	now := input.now
	keys := make(map[core.RatingKey]string)
	allowed := map[core.RatingKey]bool{}
	var pending []core.Title
	policy := fmt.Sprintf("%s:%s:%+v", input.gatePolicy, input.policy, input.availability)
	s.visibilityMu.Lock()
	revision := s.visibilityRevision
	for _, t := range items {
		if input.upcomingMovie(t) || !evidenceWindow(t, now, input.availability) {
			allowed[t.Key] = true
			continue
		}
		if _, seen := keys[t.Key]; seen {
			continue
		}
		raw, _ := json.Marshal(t)
		key := fmt.Sprintf("%s:%x", policy, sha256.Sum256(raw))
		keys[t.Key] = key
		if e, ok := s.visibilityCache[key]; ok && now.Sub(e.at) < 15*time.Second && !now.Before(e.at) {
			allowed[t.Key] = e.visible
		} else {
			pending = append(pending, t)
		}
	}
	s.visibilityMu.Unlock()
	if len(pending) > 0 {
		for _, t := range s.filterDiscovery(ctx, pending, input) {
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
	return s.filterDiscovery(context.WithValue(ctx, currentPolicyKey{}, true), items, s.discoverySnapshot())
}

// JudgedOnFacts returns, for one request, whether FilterDiscovery judges a
// title on its facts: one inside the evidence window, whose releases are
// matched against its names, genres, keywords, countries and directors. A
// caller holding partial titles passes those in full.
func (s *Service) JudgedOnFacts(ctx context.Context) func(core.Title) bool {
	snap := s.snapshotFor(ctx)
	return func(t core.Title) bool {
		// Digital-mode movies use indexed home-date evidence exclusively.
		// Their genres, names and credits cannot affect this decision. Old
		// read-only schemas still run the legacy release matcher.
		if s.hasGateTable && t.Type == core.TypeMovie && snap.availability.ModeFor(hiddenMovies) == config.ModeDigital {
			return false
		}
		return !snap.upcomingMovie(t) && evidenceWindow(t, snap.now, snap.availability)
	}
}
