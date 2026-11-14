package experience

import (
	"context"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

func (s *Service) DescriptiveSearchAvailable() bool { return s.d.Search.Vibe.Source != nil }

// DescriptiveSearch selects the collection-reading lane directly, using the
// same policy and availability boundary as ordinary neutral search. Local
// response mode excludes model refinement, remote name/seed lookup and totals.
func (s *Service) DescriptiveSearch(ctx context.Context, viewer *core.User, device core.Device, query string, q frontend.ItemQuery) (frontend.SearchPage, error) {
	if !s.DescriptiveSearchAvailable() {
		return frontend.SearchPage{}, core.ErrNotAvailable
	}
	ctx = core.WithCatalogViewer(core.WithLocalResponse(s.NoProviderWait(ctx)), viewer)
	v, err := s.viewer(ctx, viewer)
	if err != nil {
		return frontend.SearchPage{}, err
	}
	if strings.TrimSpace(query) == "" {
		return frontend.SearchPage{Page: core.Page{Items: []core.Title{}}, Continuation: frontend.ContinuationExhausted}, nil
	}
	if q.Limit <= 0 {
		q.Limit = 50
	}
	q.Limit = min(q.Limit, 50)
	q.NoTotal = true
	items, err := s.vibeMembers(ctx, v, query, q)
	if err != nil {
		return frontend.SearchPage{}, err
	}
	start := min(max(q.Offset, 0), len(items))
	end := start + min(q.Limit, len(items)-start)
	// The vibe source returns a bounded prefix without an exact total.
	return frontend.SearchPage{Page: core.Page{Items: items[start:end]}, Continuation: frontend.ContinuationUnknown}, nil
}
