package experience

import (
	"context"
	"slices"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
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
	// Familiar, when set, reports the viewer's affinity (core.AffinityViewer
	// or core.AffinityServer) with titles among keys; the catalogue ranks
	// those first among equal matches.
	Familiar func(ctx context.Context, viewer *core.User, keys []core.RatingKey) map[core.RatingKey]int
	// Details reads in full the titles a candidate search returned partial
	// (core.SearchCandidates); nil when the catalogue cannot.
	Details func(context.Context, []core.Title) ([]core.Title, error)
}

// FamiliarFrom adapts a watch-state store to Search.Familiar; nil when the
// store cannot report it.
func FamiliarFrom(st any) func(context.Context, *core.User, []core.RatingKey) map[core.RatingKey]int {
	src, ok := st.(FamiliarSource)
	if !ok || src == nil {
		return nil
	}
	return func(ctx context.Context, viewer *core.User, keys []core.RatingKey) map[core.RatingKey]int {
		known, err := src.Familiar(ctx, viewer.ID, keys)
		if err != nil {
			return nil
		}
		return known
	}
}

// withViewer attaches the viewer's search affinity to ctx.
func (s Search) withViewer(ctx context.Context, viewer *core.User) context.Context {
	ctx = core.WithCatalogViewer(ctx, viewer)
	ctx = core.WithCatalogViewer(ctx, viewer)
	if s.Familiar == nil || viewer == nil {
		return ctx
	}
	return core.WithSearchAffinity(ctx, func(ctx context.Context, keys []core.RatingKey) map[core.RatingKey]int {
		return s.Familiar(ctx, viewer, keys)
	})
}

// Find is an explicit search: catalogue rules the owner limited to browsing
// do not apply, so those titles can still be found.
func (s Search) Find(ctx context.Context, viewer *core.User, query string, types []core.MediaType, limit int) ([]core.Title, error) {
	ctx = core.WithCatalogViewer(ctx, viewer)
	if strings.TrimSpace(query) == "" || len(types) == 0 {
		return nil, nil
	}
	ctx = s.withViewer(core.WithExplicitSearch(ctx), viewer)
	allowed := make(map[core.MediaType]bool, len(types))
	for _, typ := range types {
		allowed[typ] = true
	}
	if s.Unrestricted != nil && s.Unrestricted(viewer) {
		var items []core.Title
		var natural bool
		if s.Natural != nil {
			items, natural = s.Natural(fullSearch(ctx), viewer, query, limit)
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
			items, ok = s.Natural(fullSearch(ctx), viewer, query, fetch)
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

// fullSearch is ctx for a search that keeps its results: natural search
// caches what it resolves, so it never reads a candidate window.
func fullSearch(ctx context.Context) context.Context {
	if core.SearchCandidatesFrom(ctx) == nil {
		return ctx
	}
	return core.WithSearchCandidates(ctx, nil)
}

// TitleMatchScore is the catalogue relevance (core.Title.SearchScore) from
// which a keyword result is the title the viewer typed, give or take an
// article: such a title outranks a collection reading of the same words
// ("1917" is the film, not the films of 1917).
const TitleMatchScore = 0.96

// Resolve chooses collection search before keyword search when requested,
// unless the query is also a title's name: then the keyword results lead
// and the collection is still offered beside them.
func (s Search) Resolve(ctx context.Context, viewer *core.User, query string, types []core.MediaType, limit int, collections bool) (SearchResults, error) {
	ctx = core.WithCatalogViewer(ctx, viewer)
	if collections {
		collection, items, handled, err := s.Vibe.Find(ctx, viewer, query)
		if err != nil {
			return SearchResults{}, err
		}
		if handled {
			keyword, err := s.Find(ctx, viewer, query, types, limit)
			if err == nil && slices.ContainsFunc(keyword, func(t core.Title) bool { return t.SearchScore >= TitleMatchScore }) {
				items = keyword
			}
			return SearchResults{Collection: collection, Items: items, CollectionSearch: true}, nil
		}
	}
	items, err := s.Find(ctx, viewer, query, types, limit)
	if err == nil && len(items) == 0 && !collections {
		collection, members, handled, vibeErr := s.Vibe.Find(ctx, viewer, query)
		if vibeErr != nil {
			return SearchResults{}, vibeErr
		}
		if handled {
			return SearchResults{Collection: collection, Items: members, CollectionSearch: true}, nil
		}
	}
	return SearchResults{Items: items}, err
}

// Keyword preserves the catalogue's candidate window and applies title policy.
// Section searches use this fixed window rather than natural search or refill.
func (s Search) Keyword(ctx context.Context, viewer *core.User, query string, types []core.MediaType, limit int) ([]core.Title, error) {
	ctx = core.WithCatalogViewer(ctx, viewer)
	if strings.TrimSpace(query) == "" {
		return nil, nil
	}
	items, err := s.Catalog(s.withViewer(core.WithExplicitSearch(ctx), viewer), query, types, limit)
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

type SearchResults = frontend.SearchResults
type FamiliarSource = frontend.FamiliarSource
