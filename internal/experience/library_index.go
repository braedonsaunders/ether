package experience

import (
	"context"
	"strings"

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
	filter := q.Filter
	filter.Type = library.Type
	filter, fits, none := viewerBrowseFilter(filter, v, s.d.PageExclusions(ctx, v), library.Type)
	if !fits {
		return core.Page{}, false, nil
	}
	if none {
		return core.Page{Items: []core.Title{}}, true, nil
	}
	page, err := s.d.Browse(ctx, core.BrowseQuery{Filter: filter, Sort: q.Sort, Desc: q.Desc, Offset: max(q.Offset, 0), Limit: q.Limit})
	if err != nil {
		return core.Page{}, false, err
	}
	if !s.indexedPageHolds(ctx, v, page.Items, q) {
		return core.Page{}, false, nil
	}
	return page, true, nil
}

// viewerBrowseFilter folds what the viewer may not see into filter for a
// library of typ, so SQL pages and totals match the in-process checks:
// hidden keys, blocked genres and keywords, the tightest rating cap and the
// plex.tv share predicate. fits is false when the exclusions are too large
// for one predicate; none reports that no title can match.
func viewerBrowseFilter(filter core.Filter, v *core.User, ex LibraryPageExclusions, typ core.MediaType) (out core.Filter, fits, none bool) {
	if len(ex.Keys) > maxIndexedExclusions || len(ex.ExcludeGenres) > 64 || len(ex.ExcludeKeywords) > 64 {
		return filter, false, false
	}
	filter.ExcludeKeys = append(append([]core.RatingKey{}, filter.ExcludeKeys...), ex.Keys...)
	filter.ExcludeGenres = append(append([]string{}, ex.ExcludeGenres...), filter.ExcludeGenres...)
	filter.ExcludeKeywords = append(append([]string{}, ex.ExcludeKeywords...), filter.ExcludeKeywords...)
	if v != nil {
		filter.MaxContentRating = tightenRating(filter.MaxContentRating, v.MaxContentRating)
	}
	filter.MaxContentRating = tightenRating(filter.MaxContentRating, ex.MaxContentRating)
	share := ex.Movie
	if typ == core.TypeShow || typ == core.TypeSeason || typ == core.TypeEpisode {
		share = ex.Television
	}
	filter, none = MergeShareFilter(filter, share)
	return filter, true, none
}

// MergeShareFilter narrows filter by a plex.tv share predicate. Allow lists
// intersect, so a client's own contentRating filter can only narrow a share;
// deny lists and collection names add; rating bounds tighten. none reports
// that the two allow lists have no rating in common.
func MergeShareFilter(filter, share core.Filter) (out core.Filter, none bool) {
	filter.ContentRatings, none = intersectFold(filter.ContentRatings, share.ContentRatings)
	filter.ExcludeContentRatings = append(append([]string{}, share.ExcludeContentRatings...), filter.ExcludeContentRatings...)
	filter.MinContentRating = firstNonEmpty(filter.MinContentRating, share.MinContentRating)
	filter.MaxContentRating = tightenRating(filter.MaxContentRating, share.MaxContentRating)
	filter.IncludeCollectionNames = append(append([]string{}, share.IncludeCollectionNames...), filter.IncludeCollectionNames...)
	filter.ExcludeCollectionNames = append(append([]string{}, share.ExcludeCollectionNames...), filter.ExcludeCollectionNames...)
	return filter, none
}

// intersectFold is the case-insensitive intersection of two allow lists,
// where an empty list allows everything. none reports an empty intersection
// of two non-empty lists.
func intersectFold(a, b []string) ([]string, bool) {
	switch {
	case len(b) == 0:
		return a, false
	case len(a) == 0:
		return append([]string(nil), b...), false
	}
	var out []string
	for _, x := range a {
		for _, y := range b {
			if strings.EqualFold(strings.TrimSpace(x), strings.TrimSpace(y)) {
				out = append(out, x)
				break
			}
		}
	}
	return out, len(out) == 0
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
