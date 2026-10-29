package mediabrowser

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"github.com/braedonsaunders/ether/internal/frontend/kit/paging"
	"github.com/braedonsaunders/ether/internal/frontend/kit/route"
	kitsort "github.com/braedonsaunders/ether/internal/frontend/kit/sort"
)

func queryInt(r *http.Request, name string, def int) int {
	v := route.Query(r.URL.Query(), name)
	if len(v) != 1 {
		return def
	}
	n, e := strconv.Atoi(v[0])
	if e != nil || n < 0 {
		return def
	}
	return n
}
func (i *instance) userDataDTO(id string, st frontend.WatchStatus) map[string]any {
	data := map[string]any{"PlaybackPositionTicks": st.ViewOffset.Nanoseconds() / 100, "PlayCount": st.ViewCount, "Played": st.ViewCount > 0, "IsFavorite": st.IsFavorite, "PlayedPercentage": float64(0)}
	if st.Duration > 0 {
		data["PlayedPercentage"] = min(100, float64(st.ViewOffset)*100/float64(st.Duration))
	}
	if !st.LastViewedAt.IsZero() {
		data["LastPlayedDate"] = st.LastViewedAt.UTC()
	}
	if st.UserRating > 0 {
		data["Rating"] = st.UserRating
		if st.UserRating >= 7 {
			data["Likes"] = true
		} else if st.UserRating <= 3 {
			data["Likes"] = false
		}
	}
	if i.protocol == "jellyfin" {
		data["Key"] = id
		data["ItemId"] = id
	}
	return data
}
func (i *instance) itemDTO(t frontend.Title, state frontend.WatchStatus) map[string]any {
	kind := map[frontend.MediaType]string{"movie": "Movie", "show": "Series", "season": "Season", "episode": "Episode"}[t.Type]
	m := map[string]any{"Id": ids.RatingKeyGUID(t.Key), "Name": t.Title, "Type": kind, "LocationType": "FileSystem", "IsFolder": t.Type == "show" || t.Type == "season", "Overview": t.Summary, "ProductionYear": t.Year, "RunTimeTicks": t.Runtime.Nanoseconds() / 100, "UserData": i.userDataDTO(ids.RatingKeyGUID(t.Key), state)}
	providers := map[string]string{}
	if t.IDs.TMDB > 0 {
		providers["Tmdb"] = strconv.FormatInt(t.IDs.TMDB, 10)
	}
	if t.IDs.IMDB != "" {
		providers["Imdb"] = t.IDs.IMDB
	}
	if t.IDs.TVDB > 0 {
		providers["Tvdb"] = strconv.FormatInt(t.IDs.TVDB, 10)
	}
	m["ProviderIds"] = providers
	m["ServerId"] = i.info.ID
	sortName := t.SortTitle
	if sortName == "" {
		sortName = t.Title
	}
	m["SortName"] = sortName
	if t.OriginalTitle != "" {
		m["OriginalTitle"] = t.OriginalTitle
	}
	if t.Language != "" {
		m["OriginalLanguage"] = t.Language
	}
	if t.ContentRating != "" {
		m["OfficialRating"] = t.ContentRating
	}
	if t.Rating > 0 {
		m["CommunityRating"] = t.Rating
	}
	if !t.Released.IsZero() {
		m["PremiereDate"] = t.Released.UTC()
	}
	if len(t.Genres) > 0 {
		m["Genres"] = t.Genres
	}
	if len(t.Countries) > 0 {
		m["ProductionLocations"] = t.Countries
	}
	if t.Tagline != "" {
		m["Taglines"] = []string{t.Tagline}
	}
	if t.Type == "movie" || t.Type == "episode" {
		m["MediaType"] = "Video"
	}
	if t.ShowKey != 0 {
		m["SeriesId"] = ids.RatingKeyGUID(t.ShowKey)
		m["SeriesName"] = t.ShowTitle
	}
	if t.Type == "episode" {
		if parent := t.Key.Parent(); parent != 0 {
			m["SeasonId"] = ids.RatingKeyGUID(parent)
			m["ParentId"] = ids.RatingKeyGUID(parent)
		}
		if t.SeasonTitle != "" {
			m["SeasonName"] = t.SeasonTitle
		}
		m["ParentIndexNumber"] = t.Season
		m["IndexNumber"] = t.Episode

	}
	if t.Type == "season" {
		if parent := t.Key.Parent(); parent != 0 {
			m["ParentId"] = ids.RatingKeyGUID(parent)
		}
		m["IndexNumber"] = t.Season
	}
	return m
}
func (i *instance) queryResult(items []any, total, offset int) map[string]any {
	result := map[string]any{"Items": items, "TotalRecordCount": total}
	if i.protocol == "jellyfin" {
		result["StartIndex"] = offset
	}
	return result
}
func (i *instance) browseRoutes() error {
	add := func(method, path string, h http.HandlerFunc) error { return i.router.Handle(method, path, h) }
	views := func(w http.ResponseWriter, r *http.Request) {
		v, _, err := i.viewer(r)
		if err != nil || v == nil {
			http.Error(w, "Unauthorized", 401)
			return
		}
		if i.host.Experience == nil {
			http.Error(w, "Unavailable", 503)
			return
		}
		vs, err := i.host.Experience.Views(r.Context(), v)
		if err != nil {
			http.Error(w, "Unavailable", 503)
			return
		}
		items := make([]any, 0, len(vs))
		for _, view := range vs {
			viewNum, e := ids.ParseDecimal(view.ID)
			if e != nil {
				continue
			}
			viewID, e := ids.GUID(ids.View, viewNum)
			if e != nil {
				continue
			}
			kind := "movies"
			if view.Type == "show" {
				kind = "tvshows"
			}
			items = append(items, map[string]any{"Id": viewID, "Name": view.Name, "Type": "CollectionFolder", "CollectionType": kind, "IsFolder": true, "ServerId": i.info.ID, "SortName": view.Name, "DisplayPreferencesId": viewID, "CanDelete": false, "CanDownload": false, "ProviderIds": map[string]string{}})
		}
		jsonReply(w, i.queryResult(items, len(items), 0))
	}
	rowHandler := func(purpose frontend.RowPurpose, asArray bool) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			v, _, err := i.viewer(r)
			if err != nil || v == nil {
				http.Error(w, "Unauthorized", 401)
				return
			}
			if i.host.Experience == nil {
				http.Error(w, "Unavailable", 503)
				return
			}
			rows, err := i.host.Experience.Rows(r.Context(), frontend.RowRequest{Viewer: v, Device: device(r), Placement: frontend.PlacementHome})
			if err != nil {
				http.Error(w, "Unavailable", 503)
				return
			}
			type rowSpan struct {
				start, end int
				handle     string
			}
			spans := []rowSpan{}
			titles := []frontend.Title{}
			for _, row := range rows {
				if row.Purpose != purpose {
					continue
				}
				start := len(titles)
				if len(row.Entries) > 0 {
					for _, entry := range row.Entries {
						if entry.Kind == frontend.RowEntryTitle && entry.Title != nil {
							titles = append(titles, *entry.Title)
						}
					}
				} else {
					titles = append(titles, row.Items...)
				}
				if row.Handle != "" && len(titles) > start {
					spans = append(spans, rowSpan{start, len(titles), row.Handle})
				}
			}
			offset := queryInt(r, "StartIndex", 0)
			limit := queryInt(r, "Limit", 50)
			page := paging.Slice(titles, offset, limit)
			keys := make([]frontend.RatingKey, 0, len(page))
			for _, title := range page {
				keys = append(keys, title.Key)
			}
			state, err := i.host.Experience.UserData(r.Context(), v, keys)
			if err != nil {
				http.Error(w, "Unavailable", 503)
				return
			}
			items := make([]any, 0, len(page))
			for _, title := range page {
				items = append(items, i.itemDTO(title, state[title.Key]))
			}
			if reporter, ok := i.host.Experience.(frontend.RowInteractionReporter); ok && len(page) > 0 {
				start, end := paging.Bounds(len(titles), offset, limit)
				for _, span := range spans {
					if span.start < end && span.end > start {
						err := reporter.ReportRowInteraction(r.Context(), frontend.RowInteraction{Viewer: v, Device: device(r), Protocol: i.protocol, Handle: span.handle, Kind: frontend.RowImpression})
						if err != nil && i.host.Log != nil {
							i.host.Log.Warn("row impression failed", "error", err)
						}
					}
				}
			}
			if asArray {
				jsonReply(w, items)
			} else {
				jsonReply(w, i.queryResult(items, len(titles), offset))
			}
		}
	}
	detail := func(w http.ResponseWriter, r *http.Request) {
		v, token, err := i.viewer(r)
		if err != nil || v == nil {
			http.Error(w, "Unauthorized", 401)
			return
		}
		key, err := ids.ParseRatingKeyGUID(r.PathValue("id"))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		if i.host.Experience == nil {
			http.Error(w, "Unavailable", 503)
			return
		}
		d, err := i.host.Experience.Item(r.Context(), frontend.ItemRequest{Viewer: v, Device: device(r), Key: key})
		if err != nil {
			http.NotFound(w, r)
			return
		}
		dto := i.itemDTO(d.Title, d.UserData)
		if len(d.Versions) > 0 {
			sources := make([]any, 0, len(d.Versions))
			for _, version := range d.Versions {
				sources = append(sources, map[string]any{"Id": version.ID, "Name": version.Name, "Container": version.Media.Container, "RunTimeTicks": version.Media.Duration.Nanoseconds() / 100, "Bitrate": version.Media.BitrateKbps * 1000, "Size": version.Media.Size, "MediaStreams": mediaStreams(version.Media.Streams)})
			}
			dto["MediaSources"] = sources
		}
		if tags := i.imageTags(r, d, token); len(tags) > 0 {
			dto["ImageTags"] = tags
		}
		jsonReply(w, dto)
	}
	endpoints := []struct {
		path    string
		handler http.HandlerFunc
	}{{"/UserViews", views}, {"/Users/{userId}/Views", views}, {"/Items/Latest", rowHandler(frontend.RowRecentlyReleased, true)}, {"/Users/{userId}/Items/Latest", rowHandler(frontend.RowRecentlyReleased, true)}, {"/UserItems/Resume", rowHandler(frontend.RowContinue, false)}, {"/Users/{userId}/Items/Resume", rowHandler(frontend.RowContinue, false)}, {"/Shows/NextUp", rowHandler(frontend.RowNextUp, false)}, {"/Items", i.items}, {"/Users/{userId}/Items", i.items}, {"/Items/{id}/Similar", i.similar}, {"/Items/{id}/Ancestors", i.ancestors}, {"/Search/Hints", i.search}, {"/Genres", i.facets("genre")}, {"/Studios", i.facets("studio")}, {"/Persons", i.facets("person")}, {"/Shows/{id}/Seasons", i.children}, {"/Shows/{id}/Episodes", i.episodes}, {"/Items/{id}", detail}, {"/Users/{userId}/Items/{id}", detail}}
	for _, e := range endpoints {
		if err := add("GET", e.path, e.handler); err != nil {
			return err
		}
	}
	return nil
}

func itemQuery(r *http.Request) frontend.ItemQuery {
	q := frontend.ItemQuery{Offset: queryInt(r, "StartIndex", 0), Limit: queryInt(r, "Limit", 50)}
	if values := route.Query(r.URL.Query(), "SortBy"); len(values) == 1 {
		q.Sort, _ = kitsort.Order(strings.Split(values[0], ",")[0])
	}
	if values := route.Query(r.URL.Query(), "SortOrder"); len(values) == 1 {
		q.Desc = strings.EqualFold(values[0], "Descending")
	}

	q.Recursive = queryBool(r, "Recursive", false)
	if values := route.Query(r.URL.Query(), "IncludeItemTypes"); len(values) == 1 {
		for _, typ := range strings.Split(values[0], ",") {
			switch strings.ToLower(strings.TrimSpace(typ)) {
			case "movie":
				q.Types = append(q.Types, "movie")
			case "series":
				q.Types = append(q.Types, "show")
			case "season":
				q.Types = append(q.Types, "season")
			case "episode":
				q.Types = append(q.Types, "episode")
			}
		}
	}
	if values := route.Query(r.URL.Query(), "Genres"); len(values) == 1 && values[0] != "" {
		q.Filter.Genres = strings.Split(values[0], ",")
	}
	if values := route.Query(r.URL.Query(), "Years"); len(values) == 1 {
		if year, e := strconv.Atoi(values[0]); e == nil {
			q.Filter.YearFrom = year
			q.Filter.YearTo = year
		}
	}
	for _, value := range route.Query(r.URL.Query(), "Filters") {
		for _, f := range strings.Split(value, ",") {
			switch strings.ToLower(f) {
			case "isplayed":
				v := true
				q.Watched = &v
			case "isunplayed":
				v := false
				q.Watched = &v
			case "isfavorite":
				v := true
				q.Favorite = &v
			}
		}
	}
	return q
}
func (i *instance) page(w http.ResponseWriter, r *http.Request, v *frontend.Viewer, page frontend.Page, offset int) {
	keys := make([]frontend.RatingKey, 0, len(page.Items))
	for _, title := range page.Items {
		keys = append(keys, title.Key)
	}
	state, err := i.host.Experience.UserData(r.Context(), v, keys)
	if err != nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	items := make([]any, 0, len(page.Items))
	for _, title := range page.Items {
		items = append(items, i.itemDTO(title, state[title.Key]))
	}
	jsonReply(w, i.queryResult(items, page.Total, offset))
}
func (i *instance) children(w http.ResponseWriter, r *http.Request) {
	v, _, err := i.viewer(r)
	if err != nil || v == nil {
		http.Error(w, "Unauthorized", 401)
		return
	}
	if i.host.Experience == nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	key, err := ids.ParseRatingKeyGUID(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	q := itemQuery(r)
	page, err := i.host.Experience.Children(r.Context(), v, device(r), key, q)
	if err != nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	i.page(w, r, v, page, q.Offset)
}

func (i *instance) search(w http.ResponseWriter, r *http.Request) {
	v, _, err := i.viewer(r)
	if err != nil || v == nil {
		http.Error(w, "Unauthorized", 401)
		return
	}
	if i.host.Experience == nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	terms := route.Query(r.URL.Query(), "searchTerm")
	if len(terms) != 1 {
		http.Error(w, "Bad Request", 400)
		return
	}
	q := itemQuery(r)
	result, err := i.host.Experience.Search(r.Context(), v, device(r), terms[0], q)
	if err != nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	hints := make([]any, 0, len(result.Page.Items))
	for _, t := range result.Page.Items {
		id := ids.RatingKeyGUID(t.Key)
		kind := map[frontend.MediaType]string{"movie": "Movie", "show": "Series", "season": "Season", "episode": "Episode"}[t.Type]
		hints = append(hints, map[string]any{"Id": id, "ItemId": id, "Name": t.Title, "Type": kind, "ProductionYear": t.Year, "IsFolder": t.Type == "show" || t.Type == "season"})
	}
	jsonReply(w, map[string]any{"SearchHints": hints, "TotalRecordCount": result.Page.Total})
}

func (i *instance) items(w http.ResponseWriter, r *http.Request) {
	v, _, err := i.viewer(r)
	if err != nil || v == nil {
		http.Error(w, "Unauthorized", 401)
		return
	}
	if i.host.Experience == nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	parent := route.Query(r.URL.Query(), "ParentId")
	if len(parent) > 1 {
		http.Error(w, "Bad Request", 400)
		return
	}
	q := itemQuery(r)
	if err := i.resolveFilters(r, v, &q); err != nil {
		http.Error(w, "Invalid filter", 400)
		return
	}
	var page frontend.Page
	if len(parent) == 0 || parent[0] == "" {
		page, err = i.host.Experience.LibraryItems(r.Context(), v, device(r), "", q)
	} else if id, e := ids.ParseGUID(ids.View, parent[0]); e == nil {
		page, err = i.host.Experience.LibraryItems(r.Context(), v, device(r), ids.Decimal(id), q)
	} else if key, e := ids.ParseRatingKeyGUID(parent[0]); e == nil {
		page, err = i.host.Experience.Children(r.Context(), v, device(r), key, q)
	} else {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	i.page(w, r, v, page, q.Offset)
}
func (i *instance) similar(w http.ResponseWriter, r *http.Request) {
	v, _, err := i.viewer(r)
	if err != nil || v == nil {
		http.Error(w, "Unauthorized", 401)
		return
	}
	if i.host.Experience == nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	key, err := ids.ParseRatingKeyGUID(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	q := itemQuery(r)
	p, err := i.host.Experience.Similar(r.Context(), v, device(r), key, q)
	if err != nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	i.page(w, r, v, p.Page, q.Offset)
}
func (i *instance) ancestors(w http.ResponseWriter, r *http.Request) {
	v, _, err := i.viewer(r)
	if err != nil || v == nil {
		http.Error(w, "Unauthorized", 401)
		return
	}
	if i.host.Experience == nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	key, err := ids.ParseRatingKeyGUID(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	titles, err := i.host.Experience.Ancestors(r.Context(), v, device(r), key)
	if err != nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	keys := make([]frontend.RatingKey, 0, len(titles))
	for _, t := range titles {
		keys = append(keys, t.Key)
	}
	state, err := i.host.Experience.UserData(r.Context(), v, keys)
	if err != nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	items := make([]any, 0, len(titles))
	for _, t := range titles {
		items = append(items, i.itemDTO(t, state[t.Key]))
	}
	jsonReply(w, items)
}

func (i *instance) episodes(w http.ResponseWriter, r *http.Request) {
	v, _, err := i.viewer(r)
	if err != nil || v == nil {
		http.Error(w, "Unauthorized", 401)
		return
	}
	if i.host.Experience == nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	key, err := ids.ParseRatingKeyGUID(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	q := itemQuery(r)
	q.Types = []frontend.MediaType{"episode"}
	q.Recursive = true
	if vals := route.Query(r.URL.Query(), "Season"); len(vals) == 1 {
		season, e := strconv.Atoi(vals[0])
		if e != nil || season < 0 {
			http.Error(w, "Bad Request", 400)
			return
		}
		q.Season = &season
	}
	page, err := i.host.Experience.Children(r.Context(), v, device(r), key, q)
	if err != nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	i.page(w, r, v, page, q.Offset)
}
