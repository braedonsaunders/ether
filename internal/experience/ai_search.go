package experience

import (
	"context"
	"fmt"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

func (s *Service) descriptiveSource() core.DescriptiveSearcher {
	source, _ := s.d.Search.Vibe.Source.(core.DescriptiveSearcher)
	return source
}

func (s *Service) AISearchAvailable() bool {
	source := s.descriptiveSource()
	return source != nil && source.DescriptiveSearchAvailable()
}

func (s *Service) DescriptiveSearchAvailable() bool { return s.AISearchAvailable() }

// AISearch is for an explicit protocol job, whose lifetime permits waiting.
// Ordinary title search never calls this waiting path.
func (s *Service) AISearch(ctx context.Context, v *core.Viewer, d core.Device, query string, q frontend.ItemQuery) (frontend.SearchPage, error) {
	return s.describeSearch(ctx, v, d, query, q, true)
}

// DescriptiveSearch reads/schedules the shared job and returns immediately.
func (s *Service) DescriptiveSearch(ctx context.Context, v *core.Viewer, d core.Device, query string, q frontend.ItemQuery) (frontend.SearchPage, error) {
	return s.describeSearch(ctx, v, d, query, q, false)
}

func (s *Service) describeSearch(ctx context.Context, v *core.Viewer, _ core.Device, query string, q frontend.ItemQuery, wait bool) (frontend.SearchPage, error) {
	source := s.descriptiveSource()
	if source == nil || !source.DescriptiveSearchAvailable() {
		return frontend.SearchPage{}, fmt.Errorf("AI search is unavailable")
	}
	viewer, err := s.viewer(ctx, v)
	if err != nil {
		return frontend.SearchPage{}, err
	}
	result, err := source.DescribeSearch(core.WithCatalogViewer(ctx, viewer), viewer, query, wait)
	if err != nil {
		return frontend.SearchPage{}, err
	}
	if result.State == "busy" {
		return frontend.SearchPage{}, fmt.Errorf("AI search is busy")
	}
	if result.State != "ready" {
		return frontend.SearchPage{Continuation: frontend.ContinuationUnknown}, nil
	}
	var items []core.Title
	for _, title := range result.Items {
		if !s.visible(ctx, viewer, &title) {
			continue
		}
		match, e := s.matchesUserQuery(ctx, viewer, title, q)
		if e != nil {
			return frontend.SearchPage{}, e
		}
		if match {
			items = append(items, title)
		}
	}
	offset := max(q.Offset, 0)
	limit := q.Limit
	if limit <= 0 {
		limit = 50
	}
	limit = min(limit, 200)
	total := len(items)
	offset = min(offset, total)
	return frontend.SearchPage{Page: core.Page{Items: items[offset:min(offset+limit, total)], Total: total}}, nil
}
