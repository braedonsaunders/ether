package experience

import (
	"context"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
)

// Search resolves discovery-first keyword searches for the viewer's libraries.
// Natural is optional; false falls back to the catalogue.
type Search struct {
	Visibility Visibility
	Natural    func(context.Context, *core.User, string, int) ([]core.Title, bool)
	Catalog    func(context.Context, string, []core.MediaType, int) ([]core.Title, error)
}

func (s Search) Find(ctx context.Context, viewer *core.User, query string, types []core.MediaType, limit int) ([]core.Title, error) {
	if strings.TrimSpace(query) == "" || len(types) == 0 {
		return nil, nil
	}
	if s.Natural != nil {
		if items, ok := s.Natural(ctx, viewer, query, limit); ok {
			allowed := make(map[core.MediaType]bool, len(types))
			for _, typ := range types {
				allowed[typ] = true
			}
			filtered := items[:0:0]
			for _, item := range items {
				if allowed[item.Type] && s.Visibility.TitleAllowed(viewer, &item) {
					filtered = append(filtered, item)
				}
			}
			return filtered, nil
		}
	}
	items, err := s.Catalog(ctx, query, types, limit)
	if err != nil {
		return nil, err
	}
	filtered := make([]core.Title, 0, len(items))
	for i := range items {
		if s.Visibility.TitleAllowed(viewer, &items[i]) {
			filtered = append(filtered, items[i])
		}
	}
	return filtered, nil
}
