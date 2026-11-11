package mediabrowser

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"github.com/braedonsaunders/ether/internal/frontend/kit/paging"
	"github.com/braedonsaunders/ether/internal/frontend/kit/route"
)

func latestPlayedQuery(r *http.Request) (bool, bool, error) {
	values := route.Query(r.URL.Query(), "IsPlayed")
	if len(values) == 0 || (len(values) == 1 && values[0] == "") {
		return false, false, nil
	}
	if len(values) != 1 {
		return false, false, errors.New("ambiguous IsPlayed")
	}
	played, err := strconv.ParseBool(values[0])
	if err != nil {
		return false, false, err
	}
	return played, true, nil
}

// activityView reads ParentId as a library view. parentId naming an item
// (a series or season) is not a view and reports exists=false so the resume
// path can scope to that item instead of 404ing: Jellyfin for Roku asks
// /UserItems/Resume?parentId=<series> to show a resume button (#421).
func activityView(r *http.Request) (string, bool, error) {
	values := route.Query(r.URL.Query(), "ParentId")
	if len(values) > 1 {
		return "", false, errors.New("ambiguous parent")
	}
	if len(values) == 0 || values[0] == "" {
		return "", true, nil
	}
	n, err := ids.ParseGUID(ids.View, values[0])
	if err != nil {
		if _, keyErr := ids.ParseRatingKeyGUID(values[0]); keyErr == nil {
			return "", false, nil
		}
		return "", false, err
	}
	return ids.Decimal(n), true, nil
}

// activityParentKey is the series or season a resume query is scoped to, or 0
// when ParentId is absent or names a library view.
func activityParentKey(r *http.Request) frontend.RatingKey {
	values := route.Query(r.URL.Query(), "ParentId")
	if len(values) != 1 || values[0] == "" {
		return 0
	}
	if _, err := ids.ParseGUID(ids.View, values[0]); err == nil {
		return 0
	}
	key, err := ids.ParseRatingKeyGUID(values[0])
	if err != nil {
		return 0
	}
	return key
}

func (i *instance) libraryActivity(w http.ResponseWriter, r *http.Request, latest bool) {
	r = withJellyfinListFiles(withBrowseMemo(withListMediaEstimate(withSeriesStudioCache(r))))
	viewer, _, err := i.viewer(r)
	if err != nil || viewer == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	view, isView, err := activityView(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if !isView {
		// A parentId naming an item scopes a resume row to that series or
		// season. Latest has no item-parent form here, so it stays a 404.
		if latest {
			http.NotFound(w, r)
			return
		}
		i.resumeUnderParent(w, r, viewer)
		return
	}
	defaultLimit := 50
	if latest {
		// Jellyfin's and Emby's Latest answer 20 rows when no Limit is sent
		// (Jellyfin's GetLatestMedia "int limit = 20", its OpenAPI default;
		// Emby's GetLatestMedia sets Limit = 20).
		defaultLimit = 20
	}
	offset, limit := queryInt(r, "StartIndex", 0), queryInt(r, "Limit", defaultLimit)
	if limit == 0 {
		if latest {
			i.replyItems(w, r, []any{})
		} else {
			i.replyItems(w, r, i.queryResult([]any{}, 0, offset))
		}
		return
	}
	if offset > 500 || limit > 500 || offset > 500-limit {
		http.Error(w, "Unsupported page", http.StatusBadRequest)
		return
	}
	if latest {
		played, set, err := latestPlayedQuery(r)
		if err != nil {
			http.Error(w, "Invalid IsPlayed", http.StatusBadRequest)
			return
		}
		if !set {
			// Both servers default HidePlayedInLatest to true, so an omitted
			// IsPlayed hides played items. An explicit query value wins.
			played = false
		}
		r = r.WithContext(frontend.WithLatestPlayed(r.Context(), played))
	}
	if view != "" {
		if n, e := strconv.ParseInt(view, 10, 64); e == nil && ids.PersonalView(n) {
			i.writePersonalActivity(w, r, viewer, n, latest, offset, limit)
			return
		}
	}
	service, ok := i.host.Experience.(frontend.LibraryActivity)
	if !ok {
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		return
	}
	count := offset + limit
	var titles []frontend.Title
	total := 0
	totalExact := true
	added := map[frontend.RatingKey]any{}
	if latest {
		groups := route.Query(r.URL.Query(), "GroupItems")
		if len(groups) > 1 {
			http.Error(w, "Invalid grouping", http.StatusBadRequest)
			return
		}
		group := true
		if len(groups) == 1 {
			group, err = strconv.ParseBool(groups[0])
			if err != nil {
				http.Error(w, "Invalid grouping", http.StatusBadRequest)
				return
			}
		}
		var rows []frontend.LatestItem
		if grouped, ok := i.host.Experience.(frontend.LibraryActivityGrouping); ok {
			rows, err = grouped.LatestAddedGrouped(r.Context(), viewer, device(r), view, count, group)
		} else if group {
			rows, err = service.LatestAdded(r.Context(), viewer, device(r), view, count)
		} else {
			http.Error(w, "Unavailable", http.StatusServiceUnavailable)
			return
		}
		for _, row := range rows {
			titles = append(titles, row.Title)
			if row.AddedAt != nil {
				added[row.Title.Key] = wireTime(*row.AddedAt)
			}
		}
		total = len(titles)
		titles = paging.Slice(titles, offset, limit)
	} else {
		var types []frontend.MediaType
		asked := false
		for _, value := range route.Query(r.URL.Query(), "MediaTypes") {
			for _, typ := range strings.Split(value, ",") {
				if strings.TrimSpace(typ) == "" {
					continue
				}
				asked = true
				if strings.EqualFold(strings.TrimSpace(typ), "Video") {
					types = []frontend.MediaType{"movie", "episode"}
				}
			}
		}
		if asked && len(types) == 0 {
			// Jellyfin Web asks for resumable Book and Audio rows on its home
			// screen. Ether resumes only video; other media types have no
			// resume items, which Jellyfin answers as an empty result.
			i.replyItems(w, r, i.queryResult([]any{}, 0, offset))
			return
		}
		requireExact := true
		values := route.Query(r.URL.Query(), "EnableTotalRecordCount")
		if len(values) > 1 {
			http.Error(w, "Invalid total option", http.StatusBadRequest)
			return
		}
		if len(values) == 1 {
			requireExact, err = strconv.ParseBool(values[0])
			if err != nil {
				http.Error(w, "Invalid total option", http.StatusBadRequest)
				return
			}
		}
		if len(types) == 0 {
			// Jellyfin and Emby resume rows are playable movies and episodes.
			// A season or series progress row is not a resume item.
			types = []frontend.MediaType{"movie", "episode"}
		}
		page, e := service.Resume(r.Context(), viewer, device(r), view, types, offset, limit, requireExact)
		err, titles, total, totalExact = e, page.Items, page.Total, page.TotalExact
		if err == nil && requireExact && !totalExact {
			http.Error(w, "Unavailable", http.StatusServiceUnavailable)
			return
		}
	}
	if err != nil {
		if latest && errors.Is(err, frontend.ErrNotFound) && view != "" && i.viewerHasView(r, viewer, view) {
			// A library the viewer sees but that has no latest feed (a
			// plugin library) has nothing recently added; Jellyfin Web asks
			// for every view on its home screen.
			i.replyItems(w, r, []any{})
			return
		}
		if errors.Is(err, frontend.ErrNotFound) {
			http.NotFound(w, r)
		} else {
			http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		}
		return
	}
	i.writeListedTitles(w, r, viewer, titles, added, total, totalExact, offset, latest)
}

// viewerHasView reports whether view (a decimal view id) is one of the
// viewer's library views.
func (i *instance) viewerHasView(r *http.Request, viewer *frontend.Viewer, view string) bool {
	views, err := i.host.Experience.Views(r.Context(), viewer)
	if err != nil {
		return false
	}
	for _, v := range views {
		if v.ID == view {
			return true
		}
	}
	return false
}

// resumeUnderParent answers /UserItems/Resume?parentId=<series or season>:
// the resumable descendants of that item. Jellyfin for Roku asks this for the
// resume button on a series or season page (#421). A parent that is not an
// item, or an item with no resumable child, is an empty result, never an
// error: some clients treat any error here as fatal.
func (i *instance) resumeUnderParent(w http.ResponseWriter, r *http.Request, viewer *frontend.Viewer) {
	parent := activityParentKey(r)
	if parent == 0 {
		http.NotFound(w, r)
		return
	}
	offset, limit := queryInt(r, "StartIndex", 0), queryInt(r, "Limit", 50)
	if limit == 0 {
		i.replyItems(w, r, i.queryResult([]any{}, 0, offset))
		return
	}
	if i.host.Experience == nil {
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		return
	}
	requireExact := true
	if values := route.Query(r.URL.Query(), "EnableTotalRecordCount"); len(values) == 1 {
		requireExact, _ = strconv.ParseBool(values[0])
	}
	q := frontend.ItemQuery{Types: resumeUnderParentTypes(r), Recursive: true, Limit: showChildPageMax}
	page, err := i.host.Experience.Children(r.Context(), viewer, device(r), parent, q)
	if err != nil {
		if errors.Is(err, frontend.ErrNotFound) {
			i.replyItems(w, r, i.queryResult([]any{}, 0, offset))
			return
		}
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		return
	}
	keys := make([]frontend.RatingKey, 0, len(page.Items))
	for _, title := range page.Items {
		keys = append(keys, title.Key)
	}
	states, err := i.host.Experience.UserData(r.Context(), viewer, keys)
	if err != nil {
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		return
	}
	var resumable []frontend.Title
	for _, title := range page.Items {
		if states[title.Key].ViewOffset > 0 {
			resumable = append(resumable, title)
		}
	}
	total := len(resumable)
	start := min(offset, total)
	end := min(start+limit, total)
	i.writeListedTitles(w, r, viewer, resumable[start:end], nil, total, requireExact, offset, false)
}

// resumeUnderParentTypes mirrors the resume row: playable movies and episodes.
func resumeUnderParentTypes(r *http.Request) []frontend.MediaType {
	for _, value := range route.Query(r.URL.Query(), "MediaTypes") {
		for _, typ := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(typ), "Video") {
				return []frontend.MediaType{"movie", "episode"}
			}
		}
	}
	return []frontend.MediaType{"movie", "episode"}
}

func (i *instance) writeListedTitles(w http.ResponseWriter, r *http.Request, viewer *frontend.Viewer, titles []frontend.Title, added map[frontend.RatingKey]any, total int, totalExact bool, offset int, latest bool) {
	if latest {
		r = i.noteUnplayedCounts(r, viewer, titles)
	}
	keys := make([]frontend.RatingKey, 0, len(titles))
	for _, title := range titles {
		keys = append(keys, title.Key)
	}
	states, err := i.host.Experience.UserData(r.Context(), viewer, keys)
	if err != nil {
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		return
	}
	images, paths := i.gridImagesFor(r, titles), i.virtualPaths(r, viewer, keys)
	i.prefetchParentArtwork(r, titles)
	ratingFacts := i.pageRatingFacts(r, viewer, titles)
	i.fillSeriesTitles(r, viewer, titles)
	r = i.pageEditions(r, viewer, keys)
	items := make([]any, 0, len(titles))
	for _, title := range titles {
		item := i.itemDTO(title, states[title.Key])
		if at, ok := added[title.Key]; ok {
			item["DateCreated"] = at
		}
		path := paths[title.Key]
		if path != "" {
			item["Path"] = path
		}
		if fieldSet(r)["mediasources"] {
			editions := i.jellyfinEditions(r, viewer, title)
			if len(editions) == 1 {
				setSourcePath(editions[0].source, path)
				item["MediaSources"] = []any{editions[0].source}
				applyJellyfinEditionFacts(item, editions[0].media)
			}
		}
		for key, value := range images[title.Key] {
			item[key] = value
		}
		i.shapeJellyfinItem(r, viewer, item, title)
		i.jellyfinListFacts(r, item, title)
		i.jellyfinSeasonListFields(r, item, title)
		if !latest {
			i.jellyfinResumeFields(r, item)
		}
		i.applyListedRatings(item, ratingFacts, title.Key)
		items = append(items, item)
	}
	if latest {
		i.replyItems(w, r, items)
	} else {
		result := i.queryResult(items, total, offset)
		if !totalExact {
			delete(result, "TotalRecordCount")
		}
		i.replyItems(w, r, result)
	}
}

type unplayedCountKey struct{}

// unplayedCounts are a list's UnplayedItemCount badges, worked out for the
// whole page before its items are shaped. absent names the series whose
// badge stays off (a capped or unreadable episode list).
type unplayedCounts struct {
	n      map[frontend.RatingKey]int
	absent map[frontend.RatingKey]bool
}

func withUnplayedCounts(r *http.Request, counts map[frontend.RatingKey]int) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), unplayedCountKey{}, unplayedCounts{n: counts}))
}

func unplayedCountsFrom(r *http.Request) (unplayedCounts, bool) {
	if r == nil {
		return unplayedCounts{}, false
	}
	counts, ok := r.Context().Value(unplayedCountKey{}).(unplayedCounts)
	return counts, ok && counts.n != nil
}

// noteLocalUnplayedCounts is attachUnplayedCount for every series and
// season of a page at once: the experience counts every badge of the page
// in a few statements instead of listing each series' episodes. The badge
// counts are those attachUnplayedCount works out for one series.
func (i *instance) noteLocalUnplayedCounts(r *http.Request, viewer *frontend.Viewer, titles []frontend.Title) *http.Request {
	if i == nil || i.protocol != "jellyfin" || r == nil || viewer == nil || i.host.Experience == nil {
		return r
	}
	local, ok := i.host.Experience.(interface {
		LocalUnplayedCounts(context.Context, *frontend.Viewer, []frontend.RatingKey) (map[frontend.RatingKey]int, error)
	})
	if !ok {
		return r
	}
	var keys []frontend.RatingKey
	for _, title := range titles {
		if title.Type == "show" || title.Type == "season" {
			keys = append(keys, title.Key)
		}
	}
	if len(keys) == 0 {
		return r
	}
	n, err := local.LocalUnplayedCounts(r.Context(), viewer, keys)
	if err != nil {
		return r
	}
	counts := unplayedCounts{n: n, absent: map[frontend.RatingKey]bool{}}
	for _, key := range keys {
		if _, ok := n[key]; !ok {
			counts.absent[key] = true
		}
	}
	return r.WithContext(context.WithValue(r.Context(), unplayedCountKey{}, counts))
}

func (i *instance) noteUnplayedCounts(r *http.Request, viewer *frontend.Viewer, titles []frontend.Title) *http.Request {
	counter, ok := i.host.Experience.(interface {
		UnplayedEpisodeCounts(context.Context, *frontend.Viewer, []frontend.RatingKey) (map[frontend.RatingKey]int, error)
	})
	if !ok {
		return r
	}
	keys := make([]frontend.RatingKey, 0, len(titles))
	for _, title := range titles {
		if title.Type == "show" || title.Type == "season" {
			keys = append(keys, title.Key)
		}
	}
	if len(keys) == 0 {
		return r
	}
	counts, err := counter.UnplayedEpisodeCounts(r.Context(), viewer, keys)
	if err != nil {
		return r
	}
	return withUnplayedCounts(r, counts)
}
