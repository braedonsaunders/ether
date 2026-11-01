package mediabrowser

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/contentrating"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"github.com/braedonsaunders/ether/internal/frontend/kit/paging"
	"github.com/braedonsaunders/ether/internal/frontend/kit/route"
	kitsort "github.com/braedonsaunders/ether/internal/frontend/kit/sort"
)

// showChildPageMax is the indexed window used when a show's season or
// episode list omits Limit. Jellyfin returns the whole list; this ceiling
// covers a long-running soap's seasons and one season's episodes without
// the silent page of 50 those routes used to apply.
const showChildPageMax = 1000

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
		// Jellyfin and Emby write other countries' ratings "DE-6".
		m["OfficialRating"] = contentrating.Prefixed(t.ContentRating)
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
	i.markUnreleased(m, t)
	return m
}

// markUnreleased shows a future premiere as a placeholder. Media stays off
// the item so a client does not start playback. The broker also refuses to
// resolve a title whose premiere is still ahead.
func (i *instance) markUnreleased(item map[string]any, title frontend.Title) {
	if item == nil || title.Released.IsZero() || !title.Released.After(time.Now()) {
		return
	}
	item["IsPlaceHolder"] = true
	if i.protocol != "emby" {
		item["LocationType"] = "Virtual"
	}
	delete(item, "MediaSources")
	delete(item, "MediaStreams")
}

// collectionFolder is one library view. The id is the view GUID, not a title.
func (i *instance) collectionFolder(r *http.Request, v *frontend.Viewer, view frontend.View) (map[string]any, bool) {
	viewNum, e := ids.ParseDecimal(view.ID)
	if e != nil {
		return nil, false
	}
	viewID, e := ids.GUID(ids.View, viewNum)
	if e != nil {
		return nil, false
	}
	kind := "movies"
	switch view.Type {
	case "show":
		kind = "tvshows"
	case "livetv":
		kind = "livetv"
	}
	item := map[string]any{"Id": viewID, "Name": view.Name, "Type": "CollectionFolder", "CollectionType": kind, "IsFolder": true, "ServerId": i.info.ID, "SortName": view.Name, "DisplayPreferencesId": viewID, "CanDelete": false, "CanDownload": false, "ProviderIds": map[string]string{}}
	if i.protocol == "emby" {
		i.embyViewDTO(item)
	}
	if i.protocol == "jellyfin" {
		i.jellyfinCollectionFolder(item, viewID)
		// Every library sits under the same user root. The id is stable so
		// ParentId does not change between responses.
		item["ParentId"] = jellyfinRootID()
		// ChildCount is the number of items this viewer can list in the view.
		// A failed count stays absent rather than a fabricated zero. Counts and
		// the two library dates are cached for the catalogue generation so a
		// warm view list does not walk the library.
		i.fillJellyfinViewFacts(r, v, view.ID, item)
		i.attachLibraryCollage(r, v, viewNum, viewID, item)
	}
	return item, true
}

func (i *instance) writeCollectionFolder(w http.ResponseWriter, r *http.Request, v *frontend.Viewer, viewNum int64) {
	if i.host.Experience == nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	if i.protocol == "jellyfin" && viewNum == 0 {
		i.writeUserRoot(w, r, v)
		return
	}
	if ids.PersonalView(viewNum) {
		i.writePersonalFolder(w, r, v, viewNum)
		return
	}
	if viewNum == liveTVViewNumber && i.liveTVEnabled() {
		item, ok := i.collectionFolder(r, v, frontend.View{ID: ids.Decimal(liveTVViewNumber), Name: "Live TV", Type: "livetv"})
		if !ok {
			http.NotFound(w, r)
			return
		}
		i.replyItems(w, r, item)
		return
	}
	vs, err := i.host.Experience.Views(r.Context(), v)
	if err != nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	want := ids.Decimal(viewNum)
	for _, view := range vs {
		if view.ID != want {
			continue
		}
		item, ok := i.collectionFolder(r, v, view)
		if !ok {
			break
		}
		i.replyItems(w, r, item)
		return
	}
	http.NotFound(w, r)
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
		items := make([]any, 0, len(vs)+1)
		for _, view := range vs {
			item, ok := i.collectionFolder(r, v, view)
			if !ok {
				continue
			}
			items = append(items, item)
		}
		if i.liveTVEnabled() {
			if item, ok := i.collectionFolder(r, v, frontend.View{ID: ids.Decimal(liveTVViewNumber), Name: "Live TV", Type: "livetv"}); ok {
				items = append(items, item)
			}
		}
		items = i.prependPersonalViews(r, v, items)
		i.replyItems(w, r, i.queryResult(items, len(items), 0))
	}
	rowHandler := func(purpose frontend.RowPurpose, asArray bool) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			r = withJellyfinListFiles(withBrowseMemo(withListMediaEstimate(withSeriesStudioCache(r))))
			v, _, err := i.viewer(r)
			if err != nil || v == nil {
				http.Error(w, "Unauthorized", 401)
				return
			}
			if i.host.Experience == nil {
				http.Error(w, "Unavailable", 503)
				return
			}
			type rowSpan struct {
				start, end int
				handle     string
			}
			spans := []rowSpan{}
			titles := []frontend.Title{}
			var series frontend.RatingKey
			if purpose == frontend.RowNextUp {
				series = nextUpSeries(r)
			}
			if series != 0 {
				titles, err = i.seriesNextUp(r, v, series)
				if err != nil {
					http.Error(w, "Unavailable", 503)
					return
				}
			} else {
				rows, err := i.host.Experience.Rows(r.Context(), frontend.RowRequest{Viewer: v, Device: device(r), Placement: frontend.PlacementHome, Purpose: purpose, Limit: queryInt(r, "Limit", 0)})
				if err != nil {
					http.Error(w, "Unavailable", 503)
					return
				}
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
				// Jellyfin Next Up is the next episode after something the viewer
				// watched. That episode is carried on the continue row when no
				// home row is labeled next-up.
				if purpose == frontend.RowNextUp && len(titles) == 0 {
					for _, row := range rows {
						if row.Purpose != frontend.RowContinue {
							continue
						}
						start := len(titles)
						if len(row.Entries) > 0 {
							for _, entry := range row.Entries {
								if entry.Kind == frontend.RowEntryTitle && entry.Title != nil && entry.Title.Type == "episode" {
									titles = append(titles, *entry.Title)
								}
							}
						} else {
							for _, title := range row.Items {
								if title.Type == "episode" {
									titles = append(titles, title)
								}
							}
						}
						if row.Handle != "" && len(titles) > start {
							spans = append(spans, rowSpan{start, len(titles), row.Handle})
						}
					}
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
			images := i.gridListImages(r, page)
			ratingFacts := i.pageRatingFacts(r, v, page)
			paths := i.virtualPaths(r, v, keys)
			if fieldSet(r)["mediasources"] {
				r = i.withPageEditions(r, v, keys)
			}
			i.fillSeriesTitles(r, v, page)
			for _, title := range page {
				if purpose == frontend.RowNextUp && title.Type == "episode" && i.host.Experience != nil {
					if stored, ok := i.storedListTitle(r, v, title.Key); ok && stored.Type == "episode" {
						title = stored
					}
				}
				if title.ShowTitle == "" && (title.Type == "episode" || title.Type == "season") {
					one := []frontend.Title{title}
					i.fillSeriesTitles(r, v, one)
					title = one[0]
				}
				item := i.itemDTO(title, state[title.Key])
				if p := paths[title.Key]; p != "" {
					item["Path"] = p
				}
				if fieldSet(r)["mediasources"] {
					editions := i.jellyfinEditions(r, v, title)
					if len(editions) == 1 {
						setSourcePath(editions[0].source, paths[title.Key])
						item["MediaSources"] = []any{editions[0].source}
						applyJellyfinEditionFacts(item, editions[0].media)
					}
				}
				for key, value := range images[title.Key] {
					item[key] = value
				}
				i.shapeJellyfinItem(r, v, item, title)
				i.jellyfinSeasonListFields(r, item, title)
				i.applyListedRatings(item, ratingFacts, title.Key)
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
				i.replyItems(w, r, items)
			} else {
				i.replyItems(w, r, i.queryResult(items, len(titles), offset))
			}
		}
	}
	detail := func(w http.ResponseWriter, r *http.Request) {
		r = withSeriesStudioCache(r)
		v, token, err := i.viewer(r)
		if err != nil || v == nil {
			http.Error(w, "Unauthorized", 401)
			return
		}
		wireID := r.PathValue("id")
		if viewNum, e := ids.ParseGUID(ids.View, wireID); e == nil {
			i.writeCollectionFolder(w, r, v, viewNum)
			return
		}
		if i.writeLiveGuideItem(w, r, v, token, wireID) {
			return
		}
		key, err := ids.ParseRatingKeyGUID(wireID)
		versionID := ""
		releaseID := ""
		if err != nil {
			scope, e := i.versionScope(r, v)
			if e != nil || i.host.VersionHandles == nil {
				http.NotFound(w, r)
				return
			}
			target, e := i.host.VersionHandles.Resolve(i.identityContext(r), scope, wireID)
			if e != nil || target.Item == 0 {
				http.NotFound(w, r)
				return
			}
			key, versionID, releaseID = target.Item, wireID, target.Release
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
			i.replyItems(w, r, dto)
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
				if releaseID != "" && version.ID != releaseID {
					continue
				}
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
		shown := d.Versions
		if releaseID != "" {
			shown = nil
			for _, version := range d.Versions {
				if version.ID == releaseID {
					shown = append(shown, version)
				}
			}
		}
		// A single item page is still browsing. Compatibility comes from the
		// estimate plan above; DetailPlan runs only when playback starts.
		i.versionItemFacts(dto, shown)
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
			i.jellyfinKnownVersionFacts(dto, shown)
			jellyfinStudioFacts(dto, d.Title)
			i.jellyfinDetailShell(dto)
			if trailers := jellyfinRemoteTrailers(d); len(trailers) > 0 {
				dto["RemoteTrailers"] = trailers
			} else if d.Title.RemoteTrailerURL != "" {
				name := d.Title.RemoteTrailerName
				if name == "" {
					name = "Trailer"
				}
				dto["RemoteTrailers"] = []any{map[string]string{"Name": name, "Url": d.Title.RemoteTrailerURL}}
			}
			if d.Title.Type == "show" {
				i.attachUnplayedCount(r, v, dto, d.Title)
				i.attachCumulativeRuntime(r, v, dto, d.Title)
			}
			i.attachTrickplay(r, v, key, dto)

		}
		if i.protocol == "emby" {
			i.embyItemDetailDTO(dto, d)
		}
		i.markUnreleased(dto, d.Title)
		i.applyOpenedDetail(r, v, dto, d.Title)
		i.replyItems(w, r, dto)
	}
	endpoints := []struct {
		path    string
		handler http.HandlerFunc
	}{{"/UserViews", views}, {"/Users/{userId}/Views", views}, {"/Items/Latest", func(w http.ResponseWriter, r *http.Request) { i.libraryActivity(w, r, true) }}, {"/Users/{userId}/Items/Latest", func(w http.ResponseWriter, r *http.Request) { i.libraryActivity(w, r, true) }}, {"/UserItems/Resume", func(w http.ResponseWriter, r *http.Request) { i.libraryActivity(w, r, false) }}, {"/Users/{userId}/Items/Resume", func(w http.ResponseWriter, r *http.Request) { i.libraryActivity(w, r, false) }}, {"/Shows/NextUp", rowHandler(frontend.RowNextUp, false)}, {"/Items", i.items}, {"/Users/{userId}/Items", i.items}, {"/Items/{id}/Similar", i.similar}, {"/Items/{id}/Ancestors", i.ancestors}, {"/Items/{id}/LocalTrailers", i.localTrailers}, {"/Items/{id}/SpecialFeatures", i.specialFeatures}, {"/Users/{userId}/Items/{id}/LocalTrailers", i.localTrailers}, {"/Users/{userId}/Items/{id}/SpecialFeatures", i.specialFeatures}, {"/Items/{id}/Intros", i.intros}, {"/Users/{userId}/Items/{id}/Intros", i.intros}, {"/Search/Hints", i.search}, {"/Genres", i.facets("genre")}, {"/Studios", i.facets("studio")}, {"/Persons", i.facets("person")}, {"/Shows/{id}/Seasons", i.children}, {"/Shows/{id}/Episodes", i.episodes}, {"/Items/{id}", detail}, {"/Users/{userId}/Items/{id}", detail}}
	for _, e := range endpoints {
		if err := add("GET", e.path, e.handler); err != nil {
			return err
		}
	}
	if err := add("GET", "/UserViews/GroupingOptions", i.groupingOptions); err != nil {
		return err
	}
	root := func(w http.ResponseWriter, r *http.Request) {
		if i.protocol != "jellyfin" {
			http.NotFound(w, r)
			return
		}
		v, _, err := i.viewer(r)
		if err != nil || v == nil {
			http.Error(w, "Unauthorized", 401)
			return
		}
		i.writeUserRoot(w, r, v)
	}
	if err := add("GET", "/Items/Root", root); err != nil {
		return err
	}
	if err := add("GET", "/Users/{userId}/Items/Root", root); err != nil {
		return err
	}
	if err := add("GET", "/Movies/Recommendations", func(w http.ResponseWriter, r *http.Request) { i.recommendations(w, r, "movie") }); err != nil {
		return err
	}
	if err := add("GET", "/Shows/Recommendations", func(w http.ResponseWriter, r *http.Request) { i.recommendations(w, r, "show") }); err != nil {
		return err
	}
	if err := add("GET", "/Library/VirtualFolders", i.virtualFolders); err != nil {
		return err
	}
	return i.playlistRoutes()
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
		for _, v := range splitCSV(values[0]) {
			q.Filter.ContentRatings = append(q.Filter.ContentRatings, contentrating.FromPrefixed(v))
		}
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

func withBrowseMemo(r *http.Request) *http.Request {
	if r == nil {
		return r
	}
	return r.WithContext(frontend.WithRequestMemo(r.Context()))
}

func (i *instance) pageWithVersion(w http.ResponseWriter, r *http.Request, v *frontend.Viewer, page frontend.Page, offset int, anchor frontend.RatingKey, version string) {
	r = withJellyfinListFiles(withBrowseMemo(withListMediaEstimate(withSeriesStudioCache(r))))
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
	images := i.gridListImages(r, page.Items)
	ratingFacts := i.pageRatingFacts(r, v, page.Items)
	paths := i.virtualPaths(r, v, keys)
	r = i.pageEditions(r, v, keys)
	i.fillSeriesTitles(r, v, page.Items)
	for _, title := range page.Items {
		editions := i.jellyfinEditions(r, v, title)
		if len(editions) > 1 {
			for _, edition := range editions {
				item := i.itemDTO(title, state[title.Key])
				item["Id"] = edition.handle
				item["MediaSources"] = []any{edition.source}
				applyJellyfinEditionFacts(item, edition.media)
				if p := paths[title.Key]; p != "" {
					item["Path"] = p
				}
				for key, value := range images[title.Key] {
					item[key] = value
				}
				i.shapeJellyfinItem(r, v, item, title)
				i.jellyfinListFacts(r, item, title)
				i.jellyfinListFields(r, item, title)
				i.markUnreleased(item, title)
				i.applyListedRatings(item, ratingFacts, title.Key)
				items = append(items, item)
			}
			continue
		}
		item := i.itemDTO(title, state[title.Key])
		if version != "" && title.Key == anchor {
			item["Id"] = version
		}
		if len(editions) == 1 {
			item["MediaSources"] = []any{editions[0].source}
			applyJellyfinEditionFacts(item, editions[0].media)
		}
		if p := paths[title.Key]; p != "" {
			item["Path"] = p
			if len(editions) == 1 {
				setSourcePath(editions[0].source, p)
			}
		}
		for key, value := range images[title.Key] {
			item[key] = value
		}
		i.shapeJellyfinItem(r, v, item, title)
		i.jellyfinListFacts(r, item, title)
		i.jellyfinListFields(r, item, title)
		i.markUnreleased(item, title)
		i.applyListedRatings(item, ratingFacts, title.Key)
		items = append(items, item)
	}
	total := page.Total + len(items) - len(page.Items)
	if total < len(items) {
		total = len(items)
	}
	i.replyItems(w, r, i.queryResult(items, total, offset))
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
	if len(route.Query(r.URL.Query(), "Limit")) == 0 {
		q.Limit = showChildPageMax
	}
	page, err := i.host.Experience.Children(r.Context(), v, device(r), key, q)
	if err != nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	i.nameChildSeries(r, v, key, &page)
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
	i.replyItems(w, r, map[string]any{"SearchHints": hints, "TotalRecordCount": result.Page.Total})
}

func (i *instance) items(w http.ResponseWriter, r *http.Request) {
	v, _, err := i.viewer(r)
	if err != nil || v == nil {
		http.Error(w, "Unauthorized", 401)
		return
	}
	if liveChannelTypesOnly(r) {
		// Swiftfin › Media › Live TV › Channels asks /Items for LiveTvChannel.
		// The catalogue library is the wrong index and was counting every season.
		i.writeLiveChannelItems(w, r, v)
		return
	}
	if i.writeLiveItemsByID(w, r, v) {
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
		i.writePlaylists(w, r, v, q)
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
	if search := q.Filter.Query; search != "" && i.searchableParent(parent) {
		// /Items?searchTerm= is how Jellyfin and Emby apps search. Answer it
		// from the title index with a bounded window, not a library walk
		// filtered by name.
		q.Filter.Query = ""
		result, e := i.host.Experience.Search(r.Context(), v, device(r), search, q)
		if e != nil {
			http.Error(w, "Unavailable", 503)
			return
		}
		i.page(w, r, v, result.Page, q.Offset)
		return
	}
	if len(parent) == 0 || parent[0] == "" {
		page, err = i.host.Experience.LibraryItems(r.Context(), v, device(r), "", q)
	} else if id, e := ids.ParseGUID(ids.View, parent[0]); e == nil && ids.PersonalView(id) {
		i.writePersonalItems(w, r, v, id)
		return
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

// searchableParent reports whether a search may use the title index: no
// parent, or a library view. Searches inside a show, season or personal
// library keep filtering that listing.
func (i *instance) searchableParent(parent []string) bool {
	if len(parent) == 0 || parent[0] == "" {
		return true
	}
	id, err := ids.ParseGUID(ids.View, parent[0])
	return err == nil && !ids.PersonalView(id)
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
	ratingFacts := i.pageRatingFacts(r, v, titles)
	for _, t := range titles {
		item := i.itemDTO(t, state[t.Key])
		if p := paths[t.Key]; p != "" {
			item["Path"] = p
		}
		i.shapeJellyfinItem(r, v, item, t)
		i.jellyfinSeasonListFields(r, item, t)
		i.applyListedRatings(item, ratingFacts, t.Key)
		items = append(items, item)
	}
	i.replyItems(w, r, items)
}

func applySeasonID(show frontend.RatingKey, r *http.Request, q *frontend.ItemQuery) error {
	values := route.Query(r.URL.Query(), "SeasonId")
	if len(values) == 0 || (len(values) == 1 && values[0] == "") {
		return nil
	}
	if len(values) != 1 {
		return errors.New("ambiguous season")
	}
	key, err := ids.ParseRatingKeyGUID(values[0])
	if err != nil || key.Kind() != frontend.KindSeason {
		return frontend.ErrNotFound
	}
	if show.Kind() == frontend.KindShow && key.TMDB() != show.TMDB() {
		return frontend.ErrNotFound
	}
	season := key.Season()
	q.Season = &season
	return nil
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
	if len(route.Query(r.URL.Query(), "Limit")) == 0 {
		q.Limit = showChildPageMax
	}
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
	// SeasonId is the season item Swiftfin asks for. It wins over Season so a
	// season folder cannot be filled with the rest of the series.
	if err = applySeasonID(key, r, &q); err != nil {
		if errors.Is(err, frontend.ErrNotFound) {
			i.page(w, r, v, frontend.Page{}, q.Offset)
			return
		}
		http.Error(w, "Bad Request", 400)
		return
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
