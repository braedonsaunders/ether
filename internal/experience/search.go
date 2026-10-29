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
	allowed := make(map[core.MediaType]bool, len(types))
	for _, typ := range types {
		allowed[typ] = true
	}
	// The catalogue and natural-search APIs limit candidates, not permitted
	// results. Expand only when policy removed candidates and the source may
	// have more; cap work for sources which never exhaust.
	want := limit
	if want <= 0 {
		want = 20
	}
	fetch := want
	for {
		var items []core.Title
		var ok bool
		if s.Natural != nil {
			items, ok = s.Natural(ctx, viewer, query, fetch)
		}
		if !ok {
			var err error
			items, err = s.Catalog(ctx, query, types, fetch)
			if err != nil {
				return nil, err
			}
		}
		filtered := make([]core.Title, 0, len(items))
		for i := range items {
			if allowed[items[i].Type] && s.Visibility.Visible(ctx, viewer, &items[i]) {
				filtered = append(filtered, items[i])
			}
		}
		if len(filtered) >= want {
			return filtered[:want], nil
		}
		if len(items) < fetch || fetch >= 1000 || ctx.Err() != nil {
			return filtered, nil
		}
		fetch = min(fetch*2, 1000)
	}
}
