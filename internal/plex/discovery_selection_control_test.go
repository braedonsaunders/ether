package plex

import (
	"context"
	"github.com/braedonsaunders/ether/internal/core"
)

// Frozen pre-consolidation dependency adapters, only for legacy controls.
func (s *Server) discoveryContinue(ctx context.Context, pr *Request, items []core.Title) []core.Title {
	if s.d.Discovery == nil || s.d.Discovery.Personal == nil {
		return items
	}
	return s.d.Discovery.Personal.FilterContinue(ctx, pr.User, items)
}

// discoverySearch's bool is false when the owner has left NL search disabled.
func (s *Server) discoverySearch(ctx context.Context, pr *Request, query string, limit int) ([]core.Title, bool) {
	d := s.d.Discovery
	if d == nil || d.LLM == nil || d.Watchlist == nil {
		return nil, false
	}
	cfg, e := d.Watchlist.Settings(ctx)
	if e != nil || !cfg.NaturalSearch {
		return nil, false
	}
	items, e := d.LLM.NaturalSearch(ctx, pr.User, query, limit)
	if e != nil {
		return nil, false
	}
	return items, true
}
func (s *Server) vibeSearch(ctx context.Context, pr *Request, query string) (*core.Collection, []core.Title, bool, error) {
	if pr == nil {
		return nil, nil, false, nil
	}
	return s.searchExperience().Vibe.Find(ctx, pr.User, query)
}
