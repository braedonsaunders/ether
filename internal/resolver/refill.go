package resolver

import (
	"context"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

const (
	maxSearchRefills   = 2
	searchRefillBudget = 45 * time.Second
)

var _ core.SearchCacheRefiller = (*Service)(nil)

// RefillSearchCache recovers candidates when a detail warm-up reuses a
// remembered resolution instead of searching. It never waits for capacity or
// performs resolution, descriptor fetching, health checks or provider work.
// The existing search path retains indexer limits and cache lifetimes, including
// the short empty-result lifetime. Errors are best-effort: no retries or queue.
func (s *Service) RefillSearchCache(title *core.Title) <-chan struct{} {
	if title == nil {
		return nil
	}
	req := searchRequestFor(title)
	key := searchCacheKey(req)
	s.bgMu.Lock()
	defer s.bgMu.Unlock()
	if s.bgClosed {
		return nil
	}
	if done := s.refills[key]; done != nil {
		return done
	}
	if _, ok := s.peekSearchCache(key); ok || len(s.refills) >= maxSearchRefills {
		return nil
	}
	if s.refills == nil {
		s.refills = make(map[string]chan struct{})
	}
	done := make(chan struct{})
	s.refills[key] = done
	// Use the service lifetime, not the metadata request or source warm-up.
	ctx, cancel := context.WithTimeout(s.bgCtx, searchRefillBudget)
	s.bgWG.Add(1)
	go func() {
		defer s.bgWG.Done()
		defer func() {
			cancel()
			s.bgMu.Lock()
			delete(s.refills, key)
			close(done)
			s.bgMu.Unlock()
		}()
		// Recheck the cache: a foreground search may have filled it since
		// admission. No admin-console discovery/observation side effects.
		_, _, _ = s.searchCached(ctx, req, false)
	}()
	return done
}
