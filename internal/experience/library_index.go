package experience

import (
	"context"

	"github.com/braedonsaunders/ether/internal/contentrating"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

const maxIndexedExclusions = 2000

// indexedLibraryPage asks the catalogue for the requested offset and limit.
// It is used only when the SQL predicate already includes this viewer's
// exclusions. A page that still fails the in-process checks is refused so the
// caller scans, which is the path for filters SQL cannot see.
func (s *Service) indexedLibraryPage(ctx context.Context, v *core.User, library *Library, q frontend.ItemQuery) (core.Page, bool, error) {
	if s == nil || s.d.PageExclusions == nil || s.d.Browse == nil || library == nil || v == nil {
		return core.Page{}, false, nil
	}
	if !indexedBrowseQuery(library, q) {
		return core.Page{}, false, nil
	}
	ex := s.d.PageExclusions(ctx, v)
	if len(ex.Keys) > maxIndexedExclusions || len(ex.ExcludeGenres) > 64 || len(ex.ExcludeKeywords) > 64 {
		return core.Page{}, false, nil
	}
	filter := q.Filter
	filter.Type = library.Type
	filter.ExcludeKeys = append(append([]core.RatingKey{}, q.Filter.ExcludeKeys...), ex.Keys...)
	filter.ExcludeGenres = append(append([]string{}, ex.ExcludeGenres...), q.Filter.ExcludeGenres...)
	filter.ExcludeKeywords = append(append([]string{}, ex.ExcludeKeywords...), q.Filter.ExcludeKeywords...)
	filter.MaxContentRating = tightenRating(filter.MaxContentRating, v.MaxContentRating)
	filter.MaxContentRating = tightenRating(filter.MaxContentRating, ex.MaxContentRating)
	share := ex.Movie
	if library.Type == core.TypeShow || library.Type == core.TypeSeason || library.Type == core.TypeEpisode {
		share = ex.Television
	}
	filter.ContentRatings = append(append([]string{}, share.ContentRatings...), filter.ContentRatings...)
	filter.ExcludeContentRatings = append(append([]string{}, share.ExcludeContentRatings...), filter.ExcludeContentRatings...)
	filter.MinContentRating = firstNonEmpty(filter.MinContentRating, share.MinContentRating)
	filter.MaxContentRating = tightenRating(filter.MaxContentRating, share.MaxContentRating)
	filter.IncludeCollections = append(append([]string{}, share.IncludeCollections...), filter.IncludeCollections...)
	filter.ExcludeCollections = append(append([]string{}, share.ExcludeCollections...), filter.ExcludeCollections...)
	page, err := s.d.Browse(ctx, core.BrowseQuery{Filter: filter, Sort: q.Sort, Desc: q.Desc, Offset: max(q.Offset, 0), Limit: q.Limit})
	if err != nil {
		return core.Page{}, false, err
	}
	if !s.indexedPageHolds(ctx, v, page.Items, q) {
		return core.Page{}, false, nil
	}
	return page, true, nil
}

func tightenRating(current, extra string) string {
	if extra != "" && (current == "" || int(contentrating.LevelOf(extra)) < int(contentrating.LevelOf(current))) {
		return extra
	}
	return current
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func (s *Service) indexedPageHolds(ctx context.Context, v *core.User, items []core.Title, q frontend.ItemQuery) bool {
	if len(items) == 0 {
		return true
	}
	kept := s.d.Visibility.Available(ctx, v, items)
	have := make(map[core.RatingKey]bool, len(kept))
	for _, title := range kept {
		have[title.Key] = true
	}
	for i := range items {
		title := items[i]
		if !have[title.Key] || !s.d.Visibility.TitleAllowed(v, &title) || !s.d.Visibility.Playable(ctx, v, &title) {
			return false
		}
		ok, err := s.matchesUserQuery(ctx, v, title, q)
		if err != nil || !ok {
			return false
		}
	}
	return true
}

// indexedBrowseQuery is a single catalogue page. A recursive movie library is
// still that page: movies have no children. A recursive show library is not,
// once the request can include seasons or episodes.
func indexedBrowseQuery(library *Library, q frontend.ItemQuery) bool {
	if library == nil || q.Watched != nil || q.Favorite != nil || q.Season != nil || q.AdjacentTo != 0 || q.StartAt != 0 || q.Limit <= 0 {
		return false
	}
	for _, typ := range q.Types {
		if typ != library.Type {
			return false
		}
	}
	if library.Type == core.TypeShow && q.Recursive && (len(q.Types) == 0 || containsMediaType(q.Types, core.TypeSeason) || containsMediaType(q.Types, core.TypeEpisode)) {
		return false
	}
	return true
}
