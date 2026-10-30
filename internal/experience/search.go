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
	Vibe       VibeSearch
	Natural    func(context.Context, *core.User, string, int) ([]core.Title, bool)
	Catalog    func(context.Context, string, []core.MediaType, int) ([]core.Title, error)
	// Unrestricted is a trusted host policy capability. Nil uses the
	// restricted, inherited-policy and availability path.
	Unrestricted func(*core.User) bool
}

func (s Search) Find(ctx context.Context, viewer *core.User, query string, types []core.MediaType, limit int) ([]core.Title, error) {
	if strings.TrimSpace(query) == "" || len(types) == 0 {
		return nil, nil
	}
	allowed := make(map[core.MediaType]bool, len(types))
	for _, typ := range types {
		allowed[typ] = true
	}
	if s.Unrestricted != nil && s.Unrestricted(viewer) {
		var items []core.Title
		var natural bool
		if s.Natural != nil {
			items, natural = s.Natural(ctx, viewer, query, limit)
		}
		if !natural {
			var err error
			items, err = s.Catalog(ctx, query, types, limit)
			if err != nil {
				return nil, err
			}
		}
		return s.filterCandidates(viewer, items, allowed, natural), nil
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

// SearchResults retains whether an optional collection source handled the query.
// A handled empty result must not fall back to keyword search.
type SearchResults struct {
	Collection       *core.Collection
	Items            []core.Title
	CollectionSearch bool
}

// Resolve chooses collection search before keyword search when requested.
func (s Search) Resolve(ctx context.Context, viewer *core.User, query string, types []core.MediaType, limit int, collections bool) (SearchResults, error) {
	if collections {
		collection, items, handled, err := s.Vibe.Find(ctx, viewer, query)
		if err != nil || handled {
			return SearchResults{Collection: collection, Items: items, CollectionSearch: handled}, err
		}
	}
	items, err := s.Find(ctx, viewer, query, types, limit)
	return SearchResults{Items: items}, err
}

// Keyword preserves the catalogue's candidate window and applies title policy.
// Section searches use this fixed window rather than natural search or refill.
func (s Search) Keyword(ctx context.Context, viewer *core.User, query string, types []core.MediaType, limit int) ([]core.Title, error) {
	if strings.TrimSpace(query) == "" {
		return nil, nil
	}
	items, err := s.Catalog(ctx, query, types, limit)
	if err != nil {
		return nil, err
	}
	return s.filterCandidates(viewer, items, nil, false), nil
}

func (s Search) filterCandidates(viewer *core.User, items []core.Title, allowed map[core.MediaType]bool, filterTypes bool) []core.Title {
	filtered := make([]core.Title, 0, len(items))
	for i := range items {
		if s.Visibility.TitleAllowed(viewer, &items[i]) && (!filterTypes || allowed[items[i].Type]) {
			filtered = append(filtered, items[i])
		}
	}
	return filtered
}
