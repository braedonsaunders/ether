package mediabrowser

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"github.com/braedonsaunders/ether/internal/frontend/kit/paging"
	"github.com/braedonsaunders/ether/internal/frontend/kit/route"
)

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
	r = withBrowseMemo(withListMediaEstimate(withSeriesStudioCache(r)))
	viewer, _, err := i.viewer(r)
	if err != nil || viewer == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	service, ok := i.host.Experience.(frontend.LibraryActivity)
	if !ok {
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
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
			jsonReply(w, []any{})
		} else {
			jsonReply(w, i.queryResult([]any{}, 0, offset))
		}
		return
	}
	if offset > 500 || limit > 500 || offset > 500-limit {
		http.Error(w, "Unsupported page", http.StatusBadRequest)
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
	items := make([]any, 0, len(titles))
	for _, title := range titles {
		item := i.itemDTO(title, states[title.Key])
		if at, ok := added[title.Key]; ok {
			item["DateCreated"] = at
		}
		if path := paths[title.Key]; path != "" {
			item["Path"] = path
		}
		for key, value := range images[title.Key] {
			item[key] = value
		}
		i.shapeJellyfinItem(r, viewer, item, title)
		i.jellyfinSeasonListFields(r, item, title)
		items = append(items, item)
	}
	if latest {
		jsonReply(w, items)
	} else {
		result := i.queryResult(items, total, offset)
		if !totalExact {
			delete(result, "TotalRecordCount")
		}
		jsonReply(w, result)
	}
}
