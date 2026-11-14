package experience

import (
	"context"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

// indexedRecursivePage merges bounded stored-title pages. A recursive TV query
// must not expand every series (which refreshes seasons and can hold SQLite's
// writer while sign-ins wait). Catalogue child queries carry parent policy and
// availability in SQL; only the returned page needs host authorization.
func (s *Service) indexedRecursivePage(ctx context.Context, v *core.User, library *Library, q frontend.ItemQuery) (core.Page, bool, error) {
	if library == nil || library.Type != core.TypeShow || !q.Recursive || q.Limit <= 0 || q.Season != nil || q.StartAt != 0 || q.AdjacentTo != 0 ||
		!plainItemFilter(q.Filter) || s.d.Browse == nil || s.d.PageExclusions == nil || s.d.ParentTitles == nil || !s.d.StoredEpisodes.complete() || (q.Favorite != nil && *q.Favorite) {
		return core.Page{}, false, nil
	}
	// Custom episode groups have synthetic children not in the stored index.
	if custom, err := s.d.StoredEpisodes.Customized(ctx, v); err != nil || custom {
		return core.Page{}, false, err
	}
	types := q.Types
	if len(types) == 0 {
		types = []core.MediaType{core.TypeShow, core.TypeSeason, core.TypeEpisode}
	}
	// Each type needs at most the requested window, never a library-size slice.
	window := max(q.Offset, 0) + q.Limit
	single := len(types) == 1
	if single {
		window = q.Limit
	}
	if window > 1000 {
		return core.Page{}, false, nil
	}
	var items []core.Title
	total := 0
	seen := map[core.MediaType]bool{}
	for _, typ := range types {
		if seen[typ] || containsMediaType(q.ExcludedTypes, typ) || (typ != core.TypeShow && typ != core.TypeSeason && typ != core.TypeEpisode) {
			continue
		}
		seen[typ] = true
		// Series played state is derived from episodes, unlike child watch rows.
		if typ == core.TypeShow && q.Watched != nil {
			return core.Page{}, false, nil
		}
		ex := s.d.PageExclusions(ctx, v, typ)
		if ex.Err != nil {
			return core.Page{}, true, ex.Err
		}
		filter := q.Filter
		filter.Type = typ
		filter, fits, none := viewerBrowseFilter(filter, v, ex, typ)
		if !fits {
			return core.Page{}, false, nil
		}
		if none {
			continue
		}
		filter, fits, none, err := s.userStateFilter(ctx, v, filter, q)
		if err != nil {
			return core.Page{}, true, err
		}
		if !fits {
			return core.Page{}, false, nil
		}
		if none {
			continue
		}
		offset := 0
		if single {
			offset = max(q.Offset, 0)
		}
		page, err := s.d.Browse(ctx, core.BrowseQuery{Filter: filter, Sort: q.Sort, Desc: q.Desc, Offset: offset, Limit: window, NoTotal: q.NoTotal})
		if err != nil {
			return core.Page{}, true, err
		}
		kept := page.Items
		check := q
		check.Types = []core.MediaType{typ}
		if typ == core.TypeEpisode || typ == core.TypeSeason {
			kept, err = s.visibleEpisodes(ctx, v, page.Items, check, ex.AvailabilityInSQL)
			if err != nil {
				return core.Page{}, true, err
			}
		} else if !s.indexedPageHolds(ctx, v, page.Items, check, ex.AvailabilityInSQL) {
			// A policy race must not turn this request into a full library walk.
			kept = nil
		}
		items = append(items, kept...)
		total += page.Total
	}
	if !single {
		if err := s.sortTitles(ctx, items, q); err != nil {
			return core.Page{}, true, err
		}
	}
	offset := max(q.Offset, 0)
	if single {
		offset = 0
	}
	start := min(offset, len(items))
	end := min(start+q.Limit, len(items))
	return core.Page{Items: items[start:end], Total: total}, true, nil
}
