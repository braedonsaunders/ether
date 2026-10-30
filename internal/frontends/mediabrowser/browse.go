package mediabrowser

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

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
		data["LastPlayedDate"] = wireTime(st.LastViewedAt)
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
		data["Key"] = dashedGUID(id)
		data["ItemId"] = id
	}
	return data
}
func (i *instance) itemDTO(t frontend.Title, state frontend.WatchStatus) map[string]any {
	kind := map[frontend.MediaType]string{"movie": "Movie", "show": "Series", "season": "Season", "episode": "Episode"}[t.Type]
	m := map[string]any{"Id": ids.RatingKeyGUID(t.Key), "Name": t.Title, "Type": kind, "LocationType": "FileSystem", "IsFolder": t.Type == "show" || t.Type == "season", "Overview": t.Summary, "ProductionYear": t.Year, "RunTimeTicks": t.Runtime.Nanoseconds() / 100, "UserData": i.userDataDTO(ids.RatingKeyGUID(t.Key), state)}
	if added := t.DateAdded(time.Now()); !added.IsZero() {
		m["DateCreated"] = wireTime(added)
	}
	m["CanDelete"] = false
	m["CanDownload"] = false
	if i.protocol == "emby" {
		delete(m, "LocationType")
	}
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
	// The pinned Emby 4.10 schema does not describe OriginalLanguage.
	if t.Language != "" && i.protocol != "emby" {
		m["OriginalLanguage"] = t.Language
	}
	if t.ContentRating != "" {
		m["OfficialRating"] = t.ContentRating
	}
	if t.Rating > 0 {
		m["CommunityRating"] = t.Rating
	}
	if !t.Released.IsZero() {
		m["PremiereDate"] = wireTime(t.Released)
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
	attachParentIdentity(m, t)
	if i.protocol == "emby" {
		i.embyItemDTO(m, t, state)
		// Movies and shows belong to their configured library. Episodes and
		// seasons already name the season or series that contains them.
		if _, ok := m["ParentId"]; !ok {
			section := int64(0)
			switch t.Type {
			case "movie":
				section = 1
			case "show":
				section = 2
			}
			if section != 0 {
				if id, err := ids.GUID(ids.View, section); err == nil {
					m["ParentId"] = id
				}
			}
		}
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
			item := map[string]any{"Id": viewID, "Name": view.Name, "Type": "CollectionFolder", "CollectionType": kind, "IsFolder": true, "ServerId": i.info.ID, "SortName": view.Name, "DisplayPreferencesId": viewID, "CanDelete": false, "CanDownload": false, "ProviderIds": map[string]string{}}
			if i.protocol == "emby" {
				i.embyViewDTO(item)
			}
			if i.protocol == "jellyfin" {
				i.jellyfinCollectionFolder(item, viewID)
				// ChildCount is the number of items this viewer can list in the view.
				// A failed count stays absent rather than a fabricated zero.
				page, err := i.host.Experience.LibraryItems(r.Context(), v, device(r), view.ID, frontend.ItemQuery{Limit: 1})
				if err == nil {
					item["ChildCount"] = page.Total
				}
			}
			items = append(items, item)
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
			images := i.gridImages(r, keys)
			paths := i.virtualPaths(r, v, keys)
			for _, title := range page {
				item := i.itemDTO(title, state[title.Key])
				if p := paths[title.Key]; p != "" {
					item["Path"] = p
				}
				for key, value := range images[title.Key] {
					item[key] = value
				}
				i.shapeJellyfinItem(r, v, item, title)
				i.jellyfinSeasonListFields(r, item, title)
				items = append(items, item)
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
		wireID := r.PathValue("id")
		key, err := ids.ParseRatingKeyGUID(wireID)
		versionID := ""
		if err != nil {
			scope, e := i.versionScope(r, v)
			if e != nil {
				http.NotFound(w, r)
				return
			}
			target, _, _, e := i.resolveSource(r, scope, wireID)
			if e != nil || target == 0 {
				http.NotFound(w, r)
				return
			}
			key, versionID = target, wireID
		}
		if i.host.Experience == nil {
			http.Error(w, "Unavailable", 503)
			return
		}
		d, err := i.host.Experience.Item(i.identityContext(r), frontend.ItemRequest{Viewer: v, Device: device(r), Key: key})
		if err != nil {
			http.NotFound(w, r)
			return
		}
		if d.Collection != nil || d.Playlist != nil {
			dto, ok := i.containerDTO(d, key)
			if !ok {
				http.NotFound(w, r)
				return
			}
			d.Title.Key = key // Bind image capabilities to the authorized container.
			tags, backdrops, imageMetadata := i.imageTags(r, d, token)
			for name, value := range imageMetadata {
				dto[name] = value
			}
			if len(tags) > 0 {
				dto["ImageTags"] = tags
			}
			if len(backdrops) > 0 {
				dto["BackdropImageTags"] = backdrops
			}
			jsonReply(w, dto)
			return
		}
		dto := i.itemDTO(d.Title, d.UserData)
		if versionID != "" {
			dto["Id"] = versionID
		}
		if p := i.virtualPaths(r, v, []frontend.RatingKey{d.Title.Key})[d.Title.Key]; p != "" {
			dto["Path"] = p
		}
		if people := i.peopleDTO(r, v, token, d); len(people) > 0 {
			dto["People"] = people
		}
		if i.protocol == "jellyfin" {
			if urls := jellyfinExternalURLs(d.Title); len(urls) > 0 {
				dto["ExternalUrls"] = urls
			}
		}
		if len(d.Tracks) > 0 && !d.TracksEstimated {
			streams := i.mediaStreams(d.Tracks)
			dto["MediaStreams"] = streams
		}
		if i.protocol == "jellyfin" {
			dto["EnableMediaSourceDisplay"] = false
		}
		if len(d.Versions) > 0 || d.Title.Type == "movie" || d.Title.Type == "episode" {
			sources := make([]any, 0, len(d.Versions))
			scope, scopeErr := i.versionScope(r, v)
			needDefault := scopeErr != nil || len(d.Versions) == 0
			for _, version := range d.Versions {
				if scopeErr != nil {
					break
				}
				handle, e := i.host.VersionHandles.Issue(i.identityContext(r), scope, key, version.ID)
				if e != nil || handle == "" {
					needDefault = true
					continue
				}
				source := i.detailMediaSourceDTO(handle, version.Media)
				source["Name"] = version.Name
				i.versionSourceFacts(source, version)
				plan := frontend.PlanRequest{Viewer: v, Device: scope.Device, Protocol: i.protocol, Item: key, Version: handle, Caps: scope.Caps, Intent: frontend.PlayIntent{Estimate: true, ExactVersion: true, Options: frontend.DecideOptions{DirectPlay: true}}}
				if path := i.lazySourceURL(r, token, plan); path != "" {
					source["Path"] = path
				}
				i.mediaSourceDialect(source)
				if i.protocol == "emby" {
					i.embyMediaSourceDTO(source, ids.RatingKeyGUID(d.Title.Key), version.Media)
				}
				if i.protocol == "jellyfin" {
					i.jellyfinMediaSourceDTO(source, version.Media, false)
					if name, _ := source["Name"].(string); name == "" {
						if itemName, ok := dto["Name"].(string); ok && itemName != "" {
							source["Name"] = itemName
						}
					}
				}
				sources = append(sources, source)
			}
			if needDefault {
				sources = append(sources, i.defaultMediaSource(r, v, token, key))
			}
			if len(sources) > 0 {
				dto["MediaSources"] = sources
			}
			if i.protocol == "jellyfin" {
				dto["EnableMediaSourceDisplay"] = len(sources) > 1
			}
		}
		i.versionItemFacts(dto, d.Versions)
		if d.Title.Type == "movie" || d.Title.Type == "episode" {
			i.applyResolvedDetail(r, v, token, key, versionID, dto)
		}
		tags, backdrops, imageMetadata := i.imageTags(r, d, token)
		for key, value := range imageMetadata {
			dto[key] = value
		}
		if len(tags) > 0 {
			dto["ImageTags"] = tags
		}
		if len(backdrops) > 0 {
			dto["BackdropImageTags"] = backdrops
		}
		i.addNamedFacets(r, v, d, dto)
		if i.protocol == "jellyfin" {
			i.jellyfinItemDetailDTO(d, dto)
			i.shapeJellyfinItem(r, v, dto, d.Title)
			i.jellyfinKnownVersionFacts(dto, d.Versions)
			jellyfinStudioFacts(dto, d.Title)
			i.jellyfinDetailShell(dto)
			if d.Title.Type == "show" {
				i.attachUnplayedCount(r, v, dto, d.Title)
				i.attachCumulativeRuntime(r, v, dto, d.Title)
			}
			i.attachTrickplay(r, v, key, dto)

		}
		if i.protocol == "emby" {
			i.embyItemDetailDTO(dto, d)
		}
		jsonReply(w, dto)
	}
	endpoints := []struct {
		path    string
		handler http.HandlerFunc
	}{{"/UserViews", views}, {"/Users/{userId}/Views", views}, {"/Items/Latest", func(w http.ResponseWriter, r *http.Request) { i.libraryActivity(w, r, true) }}, {"/Users/{userId}/Items/Latest", func(w http.ResponseWriter, r *http.Request) { i.libraryActivity(w, r, true) }}, {"/UserItems/Resume", func(w http.ResponseWriter, r *http.Request) { i.libraryActivity(w, r, false) }}, {"/Users/{userId}/Items/Resume", func(w http.ResponseWriter, r *http.Request) { i.libraryActivity(w, r, false) }}, {"/Shows/NextUp", rowHandler(frontend.RowNextUp, false)}, {"/Items", i.items}, {"/Users/{userId}/Items", i.items}, {"/Items/{id}/Similar", i.similar}, {"/Items/{id}/Ancestors", i.ancestors}, {"/Items/{id}/LocalTrailers", i.unsupportedExtras}, {"/Items/{id}/SpecialFeatures", i.unsupportedExtras}, {"/Users/{userId}/Items/{id}/LocalTrailers", i.unsupportedExtras}, {"/Users/{userId}/Items/{id}/SpecialFeatures", i.unsupportedExtras}, {"/Search/Hints", i.search}, {"/Genres", i.facets("genre")}, {"/Studios", i.facets("studio")}, {"/Persons", i.facets("person")}, {"/Shows/{id}/Seasons", i.children}, {"/Shows/{id}/Episodes", i.episodes}, {"/Items/{id}", detail}, {"/Users/{userId}/Items/{id}", detail}}
	for _, e := range endpoints {
		if err := add("GET", e.path, e.handler); err != nil {
			return err
		}
	}
	if err := add("GET", "/UserViews/GroupingOptions", i.groupingOptions); err != nil {
		return err
	}
	if err := add("GET", "/Library/VirtualFolders", i.virtualFolders); err != nil {
		return err
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
	for _, value := range route.Query(r.URL.Query(), "IncludeItemTypes") {
		for _, typ := range strings.Split(value, ",") {
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
		q.Filter.Genres = splitCSV(values[0])
	}
	if values := route.Query(r.URL.Query(), "Years"); len(values) == 1 {
		if year, e := strconv.Atoi(values[0]); e == nil {
			q.Filter.YearFrom = year
			q.Filter.YearTo = year
		}
	}
	if values := route.Query(r.URL.Query(), "MinCommunityRating"); len(values) == 1 {
		if n, e := strconv.ParseFloat(values[0], 64); e == nil && n > 0 {
			q.Filter.MinRating = n
		}
	}
	if values := route.Query(r.URL.Query(), "OfficialRatings"); len(values) == 1 && values[0] != "" {
		q.Filter.ContentRatings = splitCSV(values[0])
	}
	if values := route.Query(r.URL.Query(), "Studios"); len(values) == 1 {
		names := splitCSV(values[0])
		if len(names) == 1 {
			q.Filter.Studio = names[0]
		}
	}
	if values := route.Query(r.URL.Query(), "SearchTerm"); len(values) == 1 {
		q.Filter.Query = strings.TrimSpace(values[0])
	}
	if values := route.Query(r.URL.Query(), "MinRuntimeTicks"); len(values) == 1 {
		if n, e := strconv.ParseInt(values[0], 10, 64); e == nil && n > 0 {
			q.Filter.MinRuntime = time.Duration(n) * 100
		}
	}
	if values := route.Query(r.URL.Query(), "MaxRuntimeTicks"); len(values) == 1 {
		if n, e := strconv.ParseInt(values[0], 10, 64); e == nil && n > 0 {
			q.Filter.MaxRuntime = time.Duration(n) * 100
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
	i.pageWithVersion(w, r, v, page, offset, 0, "")
}

func (i *instance) pageWithVersion(w http.ResponseWriter, r *http.Request, v *frontend.Viewer, page frontend.Page, offset int, anchor frontend.RatingKey, version string) {
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
	images := i.gridImages(r, keys)
	paths := i.virtualPaths(r, v, keys)
	for _, title := range page.Items {
		item := i.itemDTO(title, state[title.Key])
		if version != "" && title.Key == anchor {
			item["Id"] = version
		}
		if p := paths[title.Key]; p != "" {
			item["Path"] = p
		}
		for key, value := range images[title.Key] {
			item[key] = value
		}
		i.shapeJellyfinItem(r, v, item, title)
		i.jellyfinListFields(r, item, title)
		items = append(items, item)
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
		hint := map[string]any{"Id": id, "ItemId": id, "Name": t.Title, "Type": kind, "ProductionYear": t.Year, "IsFolder": t.Type == "show" || t.Type == "season"}
		if i.protocol == "jellyfin" {
			if t.Type == "movie" || t.Type == "episode" {
				hint["MediaType"] = "Video"
			}
			if t.Runtime > 0 {
				hint["RunTimeTicks"] = t.Runtime.Nanoseconds() / 100
			}
			if t.Type == "episode" {
				hint["IndexNumber"] = t.Episode
				hint["ParentIndexNumber"] = t.Season
				if t.ShowTitle != "" {
					hint["Series"] = t.ShowTitle
				}
			}
		}
		hints = append(hints, hint)
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
	switch requestedSpecialItems(r) {
	case specialBoxSets:
		i.writeBoxSets(w, r, v, q)
		return
	case specialPlaylists:
		i.writePlaylists(w, v, q)
		return
	}
	if err := navigationQuery(r, &q, false); err != nil {
		http.Error(w, "Invalid navigation anchor", http.StatusBadRequest)
		return
	}
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
		if errors.Is(err, frontend.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
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
	paths := i.virtualPaths(r, v, keys)
	for _, t := range titles {
		item := i.itemDTO(t, state[t.Key])
		if p := paths[t.Key]; p != "" {
			item["Path"] = p
		}
		i.shapeJellyfinItem(r, v, item, t)
		i.jellyfinSeasonListFields(r, item, t)
		items = append(items, item)
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
	version, err := i.navigationWithVersion(r, v, &q, true)
	if err != nil {
		if errors.Is(err, frontend.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "Invalid navigation anchor", http.StatusBadRequest)
		return
	}
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
		if errors.Is(err, frontend.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "Unavailable", 503)
		return
	}
	i.pageWithVersion(w, r, v, page, q.Offset, q.StartAt, version)
}
