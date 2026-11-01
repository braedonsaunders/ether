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

func activityView(r *http.Request) (string, error) {
	values := route.Query(r.URL.Query(), "ParentId")
	if len(values) > 1 {
		return "", errors.New("ambiguous parent")
	}
	if len(values) == 0 || values[0] == "" {
		return "", nil
	}
	n, err := ids.ParseGUID(ids.View, values[0])
	if err != nil {
		return "", err
	}
	return ids.Decimal(n), nil
}

func (i *instance) libraryActivity(w http.ResponseWriter, r *http.Request, latest bool) {
	r = withJellyfinListFiles(withBrowseMemo(withListMediaEstimate(withSeriesStudioCache(r))))
	viewer, _, err := i.viewer(r)
	if err != nil || viewer == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	view, err := activityView(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	offset, limit := queryInt(r, "StartIndex", 0), queryInt(r, "Limit", 50)
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
		for _, value := range route.Query(r.URL.Query(), "MediaTypes") {
			for _, typ := range strings.Split(value, ",") {
				if !strings.EqualFold(strings.TrimSpace(typ), "Video") {
					http.Error(w, "Unsupported media type", http.StatusBadRequest)
					return
				}
				types = []frontend.MediaType{"movie", "episode"}
			}
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
		if errors.Is(err, frontend.ErrNotFound) {
			http.NotFound(w, r)
		} else {
			http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		}
		return
	}
	i.writeListedTitles(w, r, viewer, titles, added, total, totalExact, offset, latest)
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

func withUnplayedCounts(r *http.Request, counts map[frontend.RatingKey]int) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), unplayedCountKey{}, counts))
}

func unplayedCountsFrom(r *http.Request) (map[frontend.RatingKey]int, bool) {
	if r == nil {
		return nil, false
	}
	counts, ok := r.Context().Value(unplayedCountKey{}).(map[frontend.RatingKey]int)
	return counts, ok && counts != nil
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
