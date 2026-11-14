package experience

import (
	"context"

	"github.com/braedonsaunders/ether/internal/contentrating"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

// libraryBrowseFilter composes the source pool and visible rules in indexed SQL.
func (s *Service) libraryBrowseFilter(ctx context.Context, v *core.User, lib core.Library, req core.Filter) core.Filter {
	f := req
	f.CatalogueSources = lib.Sources
	for _, source := range f.CatalogueSources {
		if source == "tmdb-movies" || source == "tmdb-tv" {
			f.CatalogueSources = nil // The union includes the full catalogue.
			break
		}
	}
	// Unrestricted built-ins impose their type at each browse boundary.
	// Keeping it out of the request filter preserves recursive indexed paging.
	if (lib.ID != core.LibraryMovies && lib.ID != core.LibraryShows) || len(f.CatalogueSources) > 0 {
		f.Type = filterMediaType(lib)
	}
	if lib.Source == core.LibraryCollection {
		f.CollectionMembers = lib.CollectionID
		f.CatalogueSources = nil
	}
	if lib.ID != core.LibraryMovies && lib.ID != core.LibraryShows {
		f.ExtraRules = lib.Rules
		f.ExtraFallback = lib.VisibilityDefault
	}
	return f
}

// collectionLibraryScan is the in-process fallback for a collection library
// page the indexed path could not answer (a sort or request shape SQL cannot
// page). It walks the collection's ids in their stored order in bounded
// batches, applies the viewer policy and the request, and pages the kept
// titles. Each batch is bounded; the walk is the scan path every filter
// library already uses when its rules cannot be pushed into SQL.
func (s *Service) collectionLibraryScan(ctx context.Context, v *core.User, library *Library, q frontend.ItemQuery, collectionID int64) (core.Page, error) {
	var base core.Filter
	base.CollectionMembers = collectionID
	filter := base
	filter.Type = library.Type
	if v.MaxContentRating != "" && (filter.MaxContentRating == "" || int(contentrating.LevelOf(v.MaxContentRating)) < int(contentrating.LevelOf(filter.MaxContentRating))) {
		filter.MaxContentRating = v.MaxContentRating
	}
	var kept []core.Title
	total := 0
	for offset := 0; ; {
		page, err := s.d.Browse(ctx, core.BrowseQuery{Filter: filter, Sort: q.Sort, Desc: q.Desc, Offset: offset, Limit: 500})
		if err != nil {
			return core.Page{}, err
		}
		for i := range page.Items {
			title := &page.Items[i]
			if !s.visible(ctx, v, title) {
				continue
			}
			ok, e := s.matchesUserQuery(ctx, v, *title, q)
			if e != nil {
				return core.Page{}, e
			}
			if !ok {
				continue
			}
			kept = append(kept, *title)
			total++
		}
		if page.Total == 0 || len(page.Items) == 0 {
			break
		}
		offset += len(page.Items)
		if offset >= page.Total {
			break
		}
		if err := ctx.Err(); err != nil {
			return core.Page{}, err
		}
	}
	if q.AdjacentTo != 0 {
		return adjacentTitles(kept, q)
	}
	if q.StartAt != 0 {
		return startingTitles(kept, q)
	}
	start := min(max(q.Offset, 0), total)
	end := total
	if q.Limit > 0 {
		end = min(start+q.Limit, total)
	}
	return core.Page{Items: kept[start:end], Total: total}, nil
}
