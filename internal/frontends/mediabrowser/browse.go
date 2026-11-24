package mediabrowser

import (
	"errors"
	"math/rand/v2"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/contentrating"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"github.com/braedonsaunders/ether/internal/frontend/kit/paging"
	"github.com/braedonsaunders/ether/internal/frontend/kit/reading"
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
	} else if st.Reading != nil && st.Reading.Fraction > 0 {
		data["PlayedPercentage"] = min(100, st.Reading.Fraction*100)
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

// mbItemType is the MediaBrowser item type for a title: the type the owning
// plugin kind's presentation declares for its level (MusicArtist, MusicAlbum,
// Audio, AudioBook, Book, PhotoAlbum, Photo) when it declares one, else the
// video projection movie/show/season/episode titles have always carried.
func mbItemType(t frontend.Title) string {
	if t.MBItemType != "" {
		return t.MBItemType
	}
	return map[frontend.MediaType]string{"movie": "Movie", "show": "Series", "season": "Season", "episode": "Episode", "boxset": "BoxSet"}[t.Type]
}

// mbIsBookItem reports whether title is a book/ebook item that must never be
// advertised with video/audio MediaSources or opened through A/V PlaybackInfo.
func mbIsBookItem(t frontend.Title) bool {
	return reading.IsBookTitle(t)
}

// mbDeclaredNonVideo reports whether the title's declared MediaBrowser type
// sits outside the video family, so video-shaped fields (SeriesId, SeasonId,
// a Video MediaType) must not be stamped on it.
func mbDeclaredNonVideo(t frontend.Title) bool {
	switch t.MBItemType {
	case "", "Movie", "Series", "Season", "Episode", "Video":
		return false
	}
	return true
}

// mbIsFolder follows the declared type's place in the MediaBrowser model:
// artists, albums and photo albums hold children; tracks, books and photos
// do not. Undeclared titles keep the projected show/season rule.
func mbIsFolder(t frontend.Title) bool {
	switch t.MBItemType {
	case "Series", "Season", "MusicArtist", "MusicAlbum", "PhotoAlbum":
		return true
	case "Movie", "Episode", "Video", "Audio", "AudioBook", "Book", "Photo":
		return false
	}
	return t.Type == "show" || t.Type == "season" || t.Type == "boxset"
}

// mbMediaType is the MediaBrowser MediaType a leaf item carries: Audio for
// tracks and audiobooks, Book for books, Photo for photos, Video for the
// movie/episode/video leaves that carry video playback. Containers carry none.
func mbMediaType(t frontend.Title) (string, bool) {
	switch t.MBItemType {
	case "Video":
		return "Video", true
	case "Audio", "AudioBook":
		return "Audio", true
	case "Book":
		return "Book", true
	case "Photo":
		return "Photo", true
	case "Series", "Season", "MusicArtist", "MusicAlbum", "PhotoAlbum":
		return "", false
	}
	if t.Type == "movie" || t.Type == "episode" {
		return "Video", true
	}
	return "", false
}

func (i *instance) itemDTO(t frontend.Title, state frontend.WatchStatus) map[string]any {
	kind := mbItemType(t)
	m := map[string]any{"Id": ids.RatingKeyGUID(t.Key), "Name": t.Title, "Type": kind, "LocationType": "FileSystem", "IsFolder": mbIsFolder(t), "Overview": t.Summary, "ProductionYear": t.Year, "RunTimeTicks": t.Runtime.Nanoseconds() / 100, "UserData": i.userDataDTO(ids.RatingKeyGUID(t.Key), state)}
	if added := t.DateAdded(time.Now()); !added.IsZero() {
		m["DateCreated"] = wireTime(added)
	}
	m["CanDelete"] = false
	m["CanDownload"] = false
	if mbIsBookItem(t) && !mbIsFolder(t) && t.Key.IsPluginKey() {
		bookPath, container, _ := reading.VirtualPath(t)
		m["Path"] = bookPath
		m["Container"] = container
		m["CanDownload"] = i.host.KindStreams != nil
	}
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
	if mediaType, ok := mbMediaType(t); ok {
		m["MediaType"] = mediaType
	}
	attachParentIdentity(m, t)
	if kind == "MusicAlbum" || kind == "Audio" {
		attachMusicArtistCredits(m, t)
	}
	if i.protocol == "emby" {
		i.embyItemDTO(m, t, state)
		// Movies and shows belong to their configured library. Episodes and
		// seasons already name the season or series that contains them. A
		// declared non-video root (a music artist, a book) names its own
		// plugin library instead of a built-in section.
		if _, ok := m["ParentId"]; !ok {
			section := int64(0)
			if mbDeclaredNonVideo(t) && t.LibraryID != 0 {
				section = int64(t.LibraryID)
			} else {
				switch t.Type {
				case "movie":
					section = 1
				case "show":
					section = 2
				}
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
	if mbIsBookItem(title) {
		item["CanDownload"] = false
		delete(item, "Path")
		delete(item, "Container")
	}
}

// mediaBrowserCollectionType picks the Jellyfin/Emby CollectionType for a view.
// A plugin library carries its declared MediaBrowser collection type in
// View.Native (music, audiobooks, books, photos, homevideos, …); a built-in or
// host-synthesised view names the type in View.Type. The remaining values are
// the Jellyfin/Emby closed set.
func mediaBrowserCollectionType(view frontend.View) string {
	if view.Native != "" {
		return view.Native
	}
	switch view.Type {
	case "show", "tvshows":
		return "tvshows"
	case "music":
		return "music"
	case "livetv":
		return "livetv"
	case "homevideos":
		return "homevideos"
	case "books", "audiobooks", "photos", "musicvideos", "boxsets", "mixed":
		return string(view.Type)
	default:
		return "movies"
	}
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
	kind := mediaBrowserCollectionType(view)
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
	if viewNum == collectionsViewNumber {
		if folder, ok := i.collectionsFolder(r, v); ok {
			i.replyItems(w, r, folder)
		} else {
			http.NotFound(w, r)
		}
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
		if folder, ok := i.collectionsFolder(r, v); ok {
			items = append(items, folder)
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
			if series == 0 && purpose == frontend.RowNextUp && nextUpScoped(r) {
				// A series page Ether cannot scope (a plugin series): no row.
				titles = nil
			} else if series != 0 {
				titles, err = i.seriesNextUp(r, v, series)
				if err != nil {
					// Jellyfin never fails Next Up; some clients (Roku) exit on
					// an error here, so answer an empty row and log it (#421).
					i.logRowFailure(purpose, err)
					titles = nil
				}
			} else if activity, ok := i.host.Experience.(frontend.NextUpActivity); purpose == frontend.RowNextUp && ok {
				offset, limit := queryInt(r, "StartIndex", 0), queryInt(r, "Limit", 50)
				if offset > 500 || limit > 500 || offset > 500-limit {
					http.Error(w, "Unsupported page", http.StatusBadRequest)
					return
				}
				if limit != 0 {
					titles, err = activity.NextUp(r.Context(), v, device(r), offset+limit)
					if err != nil {
						i.logRowFailure(purpose, err)
						titles = nil
					}
				}
			} else {
				offset, limit := queryInt(r, "StartIndex", 0), queryInt(r, "Limit", 50)
				if offset > 500 || limit > 500 || offset > 500-limit {
					http.Error(w, "Unsupported page", http.StatusBadRequest)
					return
				}
				if limit == 0 {
					i.replyItems(w, r, i.queryResult([]any{}, 0, offset))
					return
				}
				rows, err := i.host.Experience.Rows(r.Context(), frontend.RowRequest{Viewer: v, Device: device(r), Placement: frontend.PlacementHome, Purpose: purpose, Limit: max(50, offset+limit)})
				if err != nil {
					i.logRowFailure(purpose, err)
					rows = nil
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
				i.logRowFailure(purpose, err)
				state = map[frontend.RatingKey]frontend.WatchStatus{}
			}
			items := make([]any, 0, len(page))
			images := i.gridListImages(r, page)
			ratingFacts := i.pageRatingFacts(r, v, page)
			paths := i.virtualPaths(r, v, keys)
			if fieldSet(r)["mediasources"] {
				r = i.withPageEditions(r, v, keys)
			}
			i.fillSeriesTitles(r, v, page)
			i.prefetchParentArtwork(r, page)
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
				if fieldSet(r)["mediasources"] && !mbIsBookItem(title) {
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
				previewRowArtwork(r, item)
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
			if i.host.VersionHandles != nil {
				if scope, e := i.versionScope(r, v); e == nil {
					if target, e := i.host.VersionHandles.Resolve(i.identityContext(r), scope, wireID); e == nil && target.Item != 0 {
						key, versionID, releaseID, err = target.Item, wireID, target.Release, nil
					}
				}
			}
			if err != nil {
				if i.writePersonItem(w, r, v, wireID) {
					return
				}
				http.NotFound(w, r)
				return
			}
		}
		if i.writeExtraDetail(w, r, v, key) {
			return
		}
		if i.host.Experience == nil {
			http.Error(w, "Unavailable", 503)
			return
		}
		d, err := i.host.Experience.Item(frontend.WithColdDetail(i.identityContext(r), key), frontend.ItemRequest{Viewer: v, Device: device(r), Key: key})
		if err != nil {
			http.NotFound(w, r)
			return
		}
		if d.Collection == nil && d.Playlist == nil && versionID == "" && !mbIsBookItem(d.Title) && (d.Title.Type == "movie" || d.Title.Type == "episode") {
			i.warmDetail(r, v, key)
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
		if mbIsBookItem(d.Title) && !i.bookReadable(d) {
			dto["CanDownload"] = false
			delete(dto, "Path")
			delete(dto, "Container")
		}
		if versionID != "" {
			dto["Id"] = versionID
		}
		if !mbIsBookItem(d.Title) {
			if p := i.virtualPaths(r, v, []frontend.RatingKey{d.Title.Key})[d.Title.Key]; p != "" {
				dto["Path"] = p
			}
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
		if !mbIsBookItem(d.Title) && (len(d.Versions) > 0 || d.Title.Type == "movie" || d.Title.Type == "episode") {
			// No explicit release was requested: when the ranked release
			// variants are available, list Auto then the variants. Otherwise
			// keep the edition/default shape exactly as before.
			if sources := i.detailSources(r, v, token, d, dto, key, releaseID); sources != nil {
				dto["MediaSources"] = sources
				if i.protocol == "jellyfin" {
					dto["EnableMediaSourceDisplay"] = len(sources) > 1
				}
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
			if !mbDeclaredNonVideo(d.Title) {
				i.attachTrickplay(r, v, key, dto)
			}

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
	}{{"/UserViews", views}, {"/Users/{userId}/Views", views}, {"/Items/Latest", func(w http.ResponseWriter, r *http.Request) { i.libraryActivity(w, r, true) }}, {"/Users/{userId}/Items/Latest", func(w http.ResponseWriter, r *http.Request) { i.libraryActivity(w, r, true) }}, {"/UserItems/Resume", func(w http.ResponseWriter, r *http.Request) { i.libraryActivity(w, r, false) }}, {"/Users/{userId}/Items/Resume", func(w http.ResponseWriter, r *http.Request) { i.libraryActivity(w, r, false) }}, {"/Shows/NextUp", rowHandler(frontend.RowNextUp, false)}, {"/Items", i.items}, {"/Users/{userId}/Items", i.items}, {"/Items/{id}/Similar", i.similar}, {"/Items/{id}/Collections", i.itemCollections}, {"/Items/{id}/Ancestors", i.ancestors}, {"/Items/{id}/LocalTrailers", i.localTrailers}, {"/Items/{id}/SpecialFeatures", i.specialFeatures}, {"/Users/{userId}/Items/{id}/LocalTrailers", i.localTrailers}, {"/Users/{userId}/Items/{id}/SpecialFeatures", i.specialFeatures}, {"/Items/{id}/Intros", i.intros}, {"/Users/{userId}/Items/{id}/Intros", i.intros}, {"/Search/Hints", i.search}, {"/Genres", i.facets("genre")}, {"/MusicGenres", i.facets("musicgenre")}, {"/Studios", i.facets("studio")}, {"/Persons", i.facets("person")}, {"/Shows/{id}/Seasons", i.children}, {"/Shows/{id}/Episodes", i.episodes}, {"/Shows//Episodes", i.episodes}, {"/Items/{id}", detail}, {"/Users/{userId}/Items/{id}", detail}}
	for _, e := range endpoints {
		if err := add("GET", e.path, e.handler); err != nil {
			return err
		}
	}
	if err := add("GET", "/UserViews/GroupingOptions", i.groupingOptions); err != nil {
		return err
	}
	if i.protocol == "jellyfin" {
		root := func(w http.ResponseWriter, r *http.Request) {
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
	if err := i.clientCompatibilityRoutes(); err != nil {
		return err
	}
	return i.playlistRoutes()
}

// singleAlbumID reads AlbumIds when it names exactly one album.
func singleAlbumID(r *http.Request) (frontend.RatingKey, bool) {
	var found []string
	for _, value := range route.Query(r.URL.Query(), "AlbumIds") {
		found = append(found, splitCSV(value)...)
	}
	if len(found) != 1 {
		return 0, false
	}
	key, err := ids.ParseRatingKeyGUID(found[0])
	if err != nil || key == 0 {
		return 0, false
	}
	return key, true
}

// viewForRequestedTypes names the one library that presents the item types a
// parentless request asked for, when every requested type belongs to a single
// plugin collection type (music, audiobooks, books) and the viewer has exactly
// one such library.
func (i *instance) viewForRequestedTypes(r *http.Request, viewer *frontend.Viewer, q frontend.ItemQuery) (string, bool) {
	if len(q.Types) != 0 || len(q.UnservedTypes) == 0 || q.Filter.Query != "" {
		return "", false
	}
	want := ""
	for _, typ := range q.UnservedTypes {
		collection := ""
		switch typ {
		case "musicalbum", "musicartist", "audio":
			collection = "music"
		case "audiobook":
			collection = "audiobooks"
		case "book":
			collection = "books"
		default:
			return "", false
		}
		if want != "" && want != collection {
			return "", false
		}
		want = collection
	}
	views, err := i.host.Experience.Views(r.Context(), viewer)
	if err != nil {
		return "", false
	}
	match := ""
	for _, view := range views {
		if view.PluginID == "" && view.KindKey == "" {
			continue
		}
		if mediaBrowserCollectionType(view) != want {
			continue
		}
		if match != "" {
			return "", false
		}
		match = view.ID
	}
	return match, match != ""
}

// sortByRandom reports whether the app asked for Jellyfin's Random order as
// its first sort key.
func sortByRandom(r *http.Request) bool {
	return strings.EqualFold(firstListValue(route.Query(r.URL.Query(), "SortBy")), "Random")
}

func itemQuery(r *http.Request) frontend.ItemQuery {
	q := frontend.ItemQuery{Offset: queryInt(r, "StartIndex", 0), Limit: queryInt(r, "Limit", 50)}
	// SortBy and SortOrder are lists. Clients send them comma-joined
	// (SortBy=CommunityRating,SortName) or repeated
	// (sortBy=CommunityRating&sortBy=SortName, what Jellyfin Web sends). The
	// first key Ether can order by wins, in either spelling.
sortKeys:
	for _, value := range route.Query(r.URL.Query(), "SortBy") {
		for _, part := range strings.Split(value, ",") {
			if order, ok := kitsort.Order(strings.TrimSpace(part)); ok {
				q.Sort = order
				break sortKeys
			}
		}
	}
	if order := firstListValue(route.Query(r.URL.Query(), "SortOrder")); order != "" {
		q.Desc = strings.EqualFold(order, "Descending")
	} else if q.Sort == frontend.SortPlayed {
		// DatePlayed reads as most-recently-played first unless asked
		// otherwise: the play history a client's "Played" row wants.
		q.Desc = true
	}

	q.Recursive = queryBool(r, "Recursive", false)
	for _, value := range route.Query(r.URL.Query(), "IncludeItemTypes") {
		for _, typ := range strings.Split(value, ",") {
			name := strings.ToLower(strings.TrimSpace(typ))
			switch name {
			case "":
			case "movie":
				q.Types = append(q.Types, "movie")
			case "series":
				q.Types = append(q.Types, "show")
			case "season":
				q.Types = append(q.Types, "season")
			case "episode":
				q.Types = append(q.Types, "episode")
			default:
				// A kind the catalogue does not hold. Remember it so the
				// listing can answer empty instead of dropping the filter and
				// returning every type (Moonfin's Favorites screen, #405).
				q.UnservedTypes = append(q.UnservedTypes, name)
			}
		}
	}
	for _, value := range route.Query(r.URL.Query(), "ExcludeItemTypes") {
		for _, typ := range splitCSV(value) {
			name := strings.ToLower(typ)
			if name == "series" {
				name = "show"
			}
			q.ExcludedTypes = append(q.ExcludedTypes, frontend.MediaType(name))
		}
	}

	if values := route.Query(r.URL.Query(), "Genres"); len(values) == 1 && values[0] != "" {
		q.Filter.Genres = splitCSV(values[0])
	}
	queryYears(&q, route.Query(r.URL.Query(), "Years"))
	for name, op := range map[string]string{"MinPremiereDate": ">=", "MaxPremiereDate": "<="} {
		if values := route.Query(r.URL.Query(), name); len(values) == 1 {
			if df, ok := premiereDateFilter(op, values[0]); ok {
				q.Filter.ReleasedFilters = append(q.Filter.ReleasedFilters, df)
			}
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
	// The alphabet jump: NameStartsWith=B for a letter, NameLessThan=A
	// for "#", NameStartsWithOrGreater=B to start the list there.
	for name, bound := range map[string]*string{"NameStartsWith": &q.Filter.NameStartsWith, "NameStartsWithOrGreater": &q.Filter.NameStartsWithOrGreater, "NameLessThan": &q.Filter.NameLessThan} {
		if values := route.Query(r.URL.Query(), name); len(values) == 1 {
			*bound = values[0]
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
			switch strings.ToLower(strings.TrimSpace(f)) {
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
	if values := route.Query(r.URL.Query(), "IsPlayed"); len(values) > 0 {
		for _, raw := range values {
			if played, err := strconv.ParseBool(strings.TrimSpace(raw)); err == nil {
				v := played
				q.Watched = &v
			}
		}
	}
	q.ProviderIDs, q.FilterByProviderIDs = parseAnyProviderIDEquals(route.Query(r.URL.Query(), "AnyProviderIdEquals"))
	return q
}

const maxProviderIDs = 200

func parseAnyProviderIDEquals(values []string) ([]frontend.ProviderID, bool) {
	var out []frontend.ProviderID
	var active bool
	seen := make(map[frontend.ProviderID]bool)
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			continue
		}
		active = true
		for _, raw := range strings.Split(value, ",") {
			token := strings.TrimSpace(raw)
			if token == "" {
				continue
			}
			provider, id, ok := strings.Cut(token, ".")
			provider = strings.ToLower(strings.TrimSpace(provider))
			id = strings.TrimSpace(id)
			if !ok || provider == "" || id == "" {
				continue
			}
			if provider == "imdb" {
				id = strings.ToLower(id)
			}
			entry := frontend.ProviderID{Provider: provider, ID: id}
			if seen[entry] {
				continue
			}
			seen[entry] = true
			out = append(out, entry)
			if len(out) >= maxProviderIDs {
				return out, true
			}
		}
	}
	return out, active
}

func titleMatchesProviderIDs(t frontend.Title, ids []frontend.ProviderID) bool {
	for _, p := range ids {
		id := strings.TrimSpace(p.ID)
		if id == "" {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(p.Provider)) {
		case "tmdb":
			if t.IDs.TMDB > 0 && strconv.FormatInt(t.IDs.TMDB, 10) == id {
				return true
			}
		case "imdb":
			if t.IDs.IMDB != "" && strings.EqualFold(t.IDs.IMDB, id) {
				return true
			}
		case "tvdb":
			if t.IDs.TVDB > 0 && strconv.FormatInt(t.IDs.TVDB, 10) == id {
				return true
			}
		}
	}
	return false
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
	i.pageWithCount(w, r, v, page, offset, anchor, version, nil)
}

func (i *instance) pageWithCount(w http.ResponseWriter, r *http.Request, v *frontend.Viewer, page frontend.Page, offset int, anchor frontend.RatingKey, version string, count *bool) {
	i.pageWithExtra(w, r, v, page, offset, anchor, version, count, nil)
}
func (i *instance) pageWithExtra(w http.ResponseWriter, r *http.Request, v *frontend.Viewer, page frontend.Page, offset int, anchor frontend.RatingKey, version string, count *bool, extra []any) {
	r = withJellyfinListFiles(withBrowseMemo(withListMediaEstimate(withSeriesStudioCache(r))))
	keys := make([]frontend.RatingKey, 0, len(page.Items))
	for _, title := range page.Items {
		keys = append(keys, title.Key)
	}
	state, err := i.host.Experience.UserData(r.Context(), v, keys)
	if err != nil {
		// Watch state is decoration on a browse page. A transient store
		// failure (SQLite writer contention turned whole /Items pages
		// into 503s, #614) must not fail browsing: serve the page with
		// unwatched defaults and log it, as Next Up already does.
		if i.host.Log != nil {
			i.host.Log.Warn("jellyfin page user data unavailable", "err", err)
		}
		state = map[frontend.RatingKey]frontend.WatchStatus{}
	}
	items := make([]any, 0, len(page.Items))
	images := i.gridListImages(r, page.Items)
	i.prefetchParentArtwork(r, page.Items)
	ratingFacts := i.pageRatingFacts(r, v, page.Items)
	paths := i.virtualPaths(r, v, keys)
	r = i.pageEditions(r, v, keys)
	r = i.noteLocalUnplayedCounts(r, v, page.Items)
	i.fillSeriesTitles(r, v, page.Items)
	for _, title := range page.Items {
		var editions []jellyfinEdition
		if !mbIsBookItem(title) {
			editions = i.jellyfinEditions(r, v, title)
		}
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
				previewRowArtwork(r, item)
				i.shapeJellyfinItem(r, v, item, title)
				i.jellyfinListFacts(r, item, title)
				i.jellyfinListFields(r, item, title)
				i.embyListFields(r, item)
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
		previewRowArtwork(r, item)
		i.shapeJellyfinItem(r, v, item, title)
		i.jellyfinListFacts(r, item, title)
		i.jellyfinListFields(r, item, title)
		i.embyListFields(r, item)
		i.markUnreleased(item, title)
		i.applyListedRatings(item, ratingFacts, title.Key)
		items = append(items, item)
	}
	items = append(extra, items...)
	total := page.Total + len(items) - len(page.Items)
	if total < len(items) {
		total = len(items)
	}
	if count != nil && !*count {
		total = 0
	}
	result := i.queryResult(items, total, offset)
	if count != nil {
		result["StartIndex"] = offset
	}
	i.replyItems(w, r, result)
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
	if key.IsPluginKey() && q.Level == "" && q.Role == "" && len(q.Types) == 0 && len(q.UnservedTypes) == 0 {
		if detail, derr := i.host.Experience.Item(frontend.WithNoContainerExpand(r.Context()), frontend.ItemRequest{Viewer: v, Device: device(r), Key: key}); derr == nil && detail.Title.PrimaryChildLevel != "" {
			q.Level = detail.Title.PrimaryChildLevel
		}
	}
	page, err := i.host.Experience.Children(frontend.WithColdDetail(r.Context(), key), v, device(r), key, q)
	if err != nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	i.nameChildSeries(r, v, key, &page)
	i.page(w, r, v, page, q.Offset)
}

func (i *instance) search(w http.ResponseWriter, r *http.Request) {
	r = r.WithContext(frontend.WithSearchMemo(r.Context()))
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
	if only, excluded := personTypesOnly(q); only {
		if excluded {
			i.replyItems(w, r, map[string]any{"SearchHints": []any{}, "TotalRecordCount": 0})
			return
		}
		if _, ok := i.host.Experience.(frontend.PeopleReader); ok {
			hints, total, e := i.personSearchHints(r, v, terms[0], q.Offset, q.Limit)
			if e != nil {
				i.logFacetFailure("person", e)
				i.replyItems(w, r, map[string]any{"SearchHints": []any{}, "TotalRecordCount": 0})
				return
			}
			i.replyItems(w, r, map[string]any{"SearchHints": hints, "TotalRecordCount": total})
			return
		}
	}
	_, token, _ := i.viewer(r)
	parent := route.Query(r.URL.Query(), "parentId")
	var result frontend.SearchPage
	var live []any
	var extra []any
	if len(parent) == 1 && parent[0] != "" {
		if id, pe := ids.ParseGUID(ids.View, parent[0]); pe == nil && !ids.PersonalView(id) && i.scopedSearchView(r, v, parent[0], id) {
			libQ := q
			libQ.Filter.Query = terms[0]
			page, le := i.host.Experience.LibraryItems(r.Context(), v, device(r), ids.Decimal(id), libQ)
			if le != nil {
				http.Error(w, "Unavailable", 503)
				return
			}
			result.Page = page
			goto hintsBuilt
		}
	}
	{
		var e error
		live, e = i.liveSearchItems(r, v, token, terms[0])
		if e != nil {
			http.Error(w, "Unavailable", 503)
			return
		}
		extra = paging.Slice(live, q.Offset, q.Limit)
		catalogQuery := q
		catalogQuery.Offset = max(0, q.Offset-len(live))
		catalogQuery.Limit = max(1, q.Limit-len(extra))
		result, err = i.searchPage(r, v, terms[0], catalogQuery)
		if err != nil {
			http.Error(w, "Unavailable", 503)
			return
		}
	}
hintsBuilt:
	room := max(0, q.Limit-len(extra))
	var personHints []any
	personTotal := 0
	if q.Offset == 0 && room > 0 && len(q.Types) == 0 && len(q.UnservedTypes) == 0 && !personExcluded(q) {
		if _, ok := i.host.Experience.(frontend.PeopleReader); ok {
			pLimit := min(4, max(1, room/3))
			if room == 1 && len(result.Page.Items) > 0 && result.Page.Items[0].SearchScore >= 0.96 {
				pLimit = 0
			}
			if pLimit > 0 {
				if ph, pt, pe := i.personSearchHints(r, v, terms[0], 0, pLimit); pe == nil {
					personHints, personTotal = ph, pt
				}
			}
		}
	}
	if len(result.Page.Items) > room {
		result.Page.Items = result.Page.Items[:room]
	}
	titleHints := make([]any, 0, len(result.Page.Items))
	exactPrefix := 0
	// Hints carry the poster grant as PrimaryImageTag, as Jellyfin's do;
	// without it clients draw every result as a blank card.
	images := i.gridListImages(r, result.Page.Items)
	for idx, t := range result.Page.Items {
		if idx == exactPrefix && t.SearchScore >= 0.96 {
			exactPrefix++
		}
		id := ids.RatingKeyGUID(t.Key)
		kind := mbItemType(t)
		hint := map[string]any{"Id": id, "ItemId": id, "Name": t.Title, "Type": kind, "ProductionYear": t.Year, "IsFolder": mbIsFolder(t)}
		if tags, ok := images[t.Key]["ImageTags"].(map[string]string); ok && tags["Primary"] != "" {
			hint["PrimaryImageTag"] = tags["Primary"]
		}
		if i.protocol == "jellyfin" {
			if mediaType, ok := mbMediaType(t); ok {
				hint["MediaType"] = mediaType
			}
			if t.Runtime > 0 {
				hint["RunTimeTicks"] = t.Runtime.Nanoseconds() / 100
			}
			if t.Type == "episode" && !mbDeclaredNonVideo(t) {
				hint["IndexNumber"] = t.Episode
				hint["ParentIndexNumber"] = t.Season
				if t.ShowTitle != "" {
					hint["Series"] = t.ShowTitle
				}
			}
		}
		titleHints = append(titleHints, hint)
	}
	hints := titleHints
	if len(personHints) > 0 {
		hints = make([]any, 0, len(titleHints)+len(personHints))
		hints = append(hints, titleHints[:exactPrefix]...)
		hints = append(hints, personHints...)
		hints = append(hints, titleHints[exactPrefix:]...)
		if len(hints) > room {
			hints = hints[:room]
		}
	}

	for _, raw := range extra {
		if dto, ok := raw.(map[string]any); ok {
			dto["ItemId"] = dto["Id"]
		}
	}
	hints = append(extra, hints...)
	i.replyItems(w, r, map[string]any{"SearchHints": hints, "TotalRecordCount": result.Page.Total + len(live) + personTotal})
}

func (i *instance) items(w http.ResponseWriter, r *http.Request) {
	v, _, err := i.viewer(r)
	if err != nil || v == nil {
		http.Error(w, "Unauthorized", 401)
		return
	}
	q := itemQuery(r)
	if !q.FilterByProviderIDs && i.writeLiveSearchItems(w, r, v) {
		return
	}
	if liveChannelTypesOnly(r) {
		if q.FilterByProviderIDs {
			i.replyItems(w, r, i.queryResult([]any{}, 0, q.Offset))
			return
		}
		// Swiftfin › Media › Live TV › Channels asks /Items for LiveTvChannel.
		// The catalogue library is the wrong index and was counting every season.
		i.writeLiveChannelItems(w, r, v)
		return
	}
	if i.writeLiveItemsByID(w, r, v) {
		return
	}
	if parent := route.Query(r.URL.Query(), "ParentId"); len(parent) == 1 && i.liveTVEnabled() {
		// Opening the Live TV folder (Jellyfin Web's Live TV library) browses
		// its channels; without this the folder 404'd while /LiveTv/Channels
		// worked. Answer the same channel list.
		if n, e := ids.ParseGUID(ids.View, parent[0]); e == nil && n == liveTVViewNumber {
			if q.FilterByProviderIDs {
				i.replyItems(w, r, i.queryResult([]any{}, 0, q.Offset))
				return
			}
			i.writeLiveChannelItems(w, r, v)
			return
		}
	}
	if i.host.Experience == nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	if i.writeItemsByID(w, r, v) {
		return
	}
	parent := route.Query(r.URL.Query(), "ParentId")
	if len(parent) > 1 {
		http.Error(w, "Bad Request", 400)
		return
	}
	if len(parent) == 1 {
		if n, e := ids.ParseGUID(ids.View, parent[0]); e == nil && n == collectionsViewNumber {
			if q.FilterByProviderIDs {
				i.replyItems(w, r, i.queryResult([]any{}, 0, q.Offset))
				return
			}
			i.writeBoxSets(w, r, v, q)
			return
		}
	}
	switch requestedSpecialItems(r) {
	case specialBoxSets:
		if q.FilterByProviderIDs || (len(parent) == 1 && i.pluginView(r, v, parent[0])) {
			// Catalogue collections belong to the built-in libraries; a
			// plugin library has none of them.
			i.replyItems(w, r, i.queryResult([]any{}, 0, q.Offset))
			return
		}
		i.writeBoxSets(w, r, v, q)
		return
	case specialPlaylists:
		if q.FilterByProviderIDs {
			i.replyItems(w, r, i.queryResult([]any{}, 0, q.Offset))
			return
		}
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
	if search := q.Filter.Query; search != "" && !q.FilterByProviderIDs && i.searchableParent(r, v, parent) {
		if q.Favorite == nil {
			if only, excluded := personTypesOnly(q); only {
				if excluded {
					jsonReply(w, i.queryResult([]any{}, 0, q.Offset))
					return
				}
				if _, ok := i.host.Experience.(frontend.PeopleReader); ok {
					i.searchPersons(w, r, v, search)
					return
				}
			}
		}
		// /Items?searchTerm= is how Jellyfin and Emby apps search. Answer it
		// from the title index with a bounded window, not a library walk
		// filtered by name.
		q.Filter.Query = ""
		_, token, _ := i.viewer(r)
		live, e := i.liveSearchItems(r, v, token, search)
		if e != nil {
			http.Error(w, "Unavailable", 503)
			return
		}
		extra := paging.Slice(live, q.Offset, q.Limit)
		catalogQuery := q
		catalogQuery.Offset = max(0, q.Offset-len(live))
		catalogQuery.Limit = max(1, q.Limit-len(extra))
		result, e := i.searchPage(r, v, search, catalogQuery)
		if e != nil {
			http.Error(w, "Unavailable", 503)
			return
		}
		room := max(0, q.Limit-len(extra))
		if len(result.Page.Items) > room {
			result.Page.Items = result.Page.Items[:room]
		}
		result.Page.Total += len(live) - len(extra)
		i.pageWithExtra(w, r, v, result.Page, q.Offset, 0, "", nil, extra)
		return
	}
	if hasEmptyArtistFilter(r) {
		i.page(w, r, v, frontend.Page{Items: []frontend.Title{}}, q.Offset)
		return
	}
	if artistKey, ok := artistParentKey(r); ok && (len(parent) == 0 || parent[0] == "" || func() bool { _, e := ids.ParseGUID(ids.View, parent[0]); return e == nil }()) {
		artistQuery := q
		artistQuery.Recursive = true
		page, err = i.host.Experience.Children(r.Context(), v, device(r), artistKey, artistQuery)
	} else if albumKey, ok := singleAlbumID(r); ok && (len(parent) == 0 || parent[0] == "" || func() bool { _, e := ids.ParseGUID(ids.View, parent[0]); return e == nil }()) {
		// AlbumIds=<album> lists that album's tracks whether or not a view
		// ParentId was included.
		page, err = i.host.Experience.Children(r.Context(), v, device(r), albumKey, q)
	} else if viewID, ok := i.viewForRequestedTypes(r, v, q); ok && (len(parent) == 0 || parent[0] == "") {
		// IncludeItemTypes names only kinds one plugin library presents
		// (MusicAlbum, AudioBook, Book…): list that library, not the server.
		page, err = i.host.Experience.LibraryItems(r.Context(), v, device(r), viewID, q)
	} else if len(parent) == 0 || parent[0] == "" {
		page, err = i.host.Experience.LibraryItems(r.Context(), v, device(r), "", q)
	} else if id, e := ids.ParseGUID(ids.View, parent[0]); e == nil && ids.PersonalView(id) {
		i.writePersonalItems(w, r, v, id)
		return
	} else if id, e := ids.ParseGUID(ids.View, parent[0]); e == nil {
		// SortBy=Random: a bounded random window of the library, shuffled so
		// the request stays O(1) independent of library size. The reply still
		// reports the offset the app asked for.
		pq := q
		random := sortByRandom(r) && q.Limit > 0 && q.Offset == 0
		if random {
			probe := q
			probe.Sort, probe.Desc, probe.Limit = "", false, 1
			if first, e := i.host.Experience.LibraryItems(r.Context(), v, device(r), ids.Decimal(id), probe); e == nil {
				pool := min(first.Total, maxRandomBrowseWindow)
				if pool > q.Limit {
					pq.Sort, pq.Desc = "", false
					pq.Offset = rand.IntN(pool - q.Limit + 1)
				}
			}
		}
		page, err = i.host.Experience.LibraryItems(r.Context(), v, device(r), ids.Decimal(id), pq)
		if random && err == nil && len(page.Items) > 1 {
			shuffled := append([]frontend.Title(nil), page.Items...)
			rand.Shuffle(len(shuffled), func(a, b int) { shuffled[a], shuffled[b] = shuffled[b], shuffled[a] })
			page.Items = shuffled
		}
	} else if key, e := ids.ParseRatingKeyGUID(parent[0]); e == nil {
		page, err = i.host.Experience.Children(r.Context(), v, device(r), key, q)
		if key.Kind() == frontend.KindCollection && err == nil {
			detail, e := i.host.Experience.Item(r.Context(), frontend.ItemRequest{Viewer: v, Device: device(r), Key: key})
			if e == nil && detail.Collection != nil && detail.Collection.RankedTop10 {
				if q.Offset >= 10 {
					page.Items = nil
				} else {
					page.Items = page.Items[:min(len(page.Items), 10-q.Offset)]
				}
				page.Total = min(page.Total, 10)
				r = withRankedRow(r, true, page.Items, q.Offset)
			}
		}
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

// itemCollections answers /Items/{id}/Collections, which Jellyfin Web asks on
// every detail page. Ether's collections are not offered here yet, so the
// answer is the empty result real Jellyfin gives an item in no collection.
func (i *instance) itemCollections(w http.ResponseWriter, r *http.Request) {
	v, _, err := i.viewer(r)
	if err != nil || v == nil {
		http.Error(w, "Unauthorized", 401)
		return
	}
	i.replyItems(w, r, i.queryResult([]any{}, 0, queryInt(r, "StartIndex", 0)))
}

// maxItemsByID bounds one /Items?Ids= lookup; apps send a screen's worth.
const maxItemsByID = 200

// writeItemsByID answers /Items?Ids=a,b,c with exactly those titles, in the
// order asked, each through the viewer's own item authorization. Apps fetch
// an item page, a queue or a SyncPlay playlist this way (Streamyfin, Findroid,
// Jellyfin Web); without it the filter was dropped and the whole library came
// back, so the app opened the first title of the library instead. Ids that
// are not titles, or that this viewer may not see, are left out.
func (i *instance) writeItemsByID(w http.ResponseWriter, r *http.Request, v *frontend.Viewer) bool {
	values := route.Query(r.URL.Query(), "Ids")
	if len(values) == 0 {
		return false
	}
	var keys []frontend.RatingKey
	seen := map[frontend.RatingKey]bool{}
	for _, value := range values {
		for _, id := range strings.Split(value, ",") {
			key, err := ids.ParseRatingKeyGUID(strings.TrimSpace(id))
			if err != nil || seen[key] || len(keys) >= maxItemsByID {
				continue
			}
			seen[key] = true
			keys = append(keys, key)
		}
	}
	r = withBrowseMemo(r)
	page := frontend.Page{Items: make([]frontend.Title, 0, len(keys))}
	if batch, ok := i.host.Experience.(frontend.TitleBatchReader); ok {
		if titles, err := batch.BatchTitles(r.Context(), v, keys); err == nil {
			page.Items = titles
		}
	} else {
		for _, key := range keys {
			detail, err := i.host.Experience.Item(r.Context(), frontend.ItemRequest{Viewer: v, Device: device(r), Key: key})
			if err != nil {
				continue
			}
			page.Items = append(page.Items, detail.Title)
		}
	}
	page.Total = len(page.Items)
	i.page(w, r, v, page, 0)
	return true
}

// maxRandomBrowseWindow bounds SortBy=Random offsets so library browse stays
// O(1) independent of total catalog size.
const maxRandomBrowseWindow = 500

// scopedSearchView reports whether a view GUID must search within itself
// (plugin libraries and scoped built-in/custom libraries) instead of the global
// title index.
func (i *instance) scopedSearchView(r *http.Request, v *frontend.Viewer, parentGUID string, id int64) bool {
	if i.pluginView(r, v, parentGUID) {
		return true
	}
	if sr, ok := i.host.Experience.(frontend.LibraryScopeReader); ok {
		if _, scoped, err := sr.LibraryBrowseFilter(r.Context(), v, ids.Decimal(id), frontend.Filter{}); err == nil && scoped {
			return true
		}
	}
	return false
}

// searchableParent reports whether a search may use the global title index:
// no parent, or an unscoped library view. Searches inside a show, season,
// personal library, plugin library, custom library or library-scoped built-in
// library filter that view directly.
func (i *instance) searchableParent(r *http.Request, v *frontend.Viewer, parent []string) bool {
	if len(parent) == 0 || parent[0] == "" {
		return true
	}
	id, err := ids.ParseGUID(ids.View, parent[0])
	if err != nil || ids.PersonalView(id) {
		return false
	}
	return !i.scopedSearchView(r, v, parent[0], id)
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
	if _, err := ids.ParseGUID(ids.Channel, r.PathValue("id")); err == nil && i.liveTVEnabled() {
		if _, err := i.host.LiveTV.ResolveChannelGUID(r.Context(), v, r.PathValue("id")); err != nil {
			http.NotFound(w, r)
			return
		}
		jsonReply(w, []any{})
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
		if i.host.Log != nil {
			i.host.Log.Warn("jellyfin ancestors user data unavailable", "err", err)
		}
		state = map[frontend.RatingKey]frontend.WatchStatus{}
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

// pluginSeason resolves the season container a client asked for under a plugin
// show. asked is false when the request names no season. A zero key with
// asked true means the season does not exist under this show.
func (i *instance) pluginSeason(r *http.Request, v *frontend.Viewer, show frontend.RatingKey, number *int) (season frontend.RatingKey, asked bool) {
	if values := route.Query(r.URL.Query(), "SeasonId"); len(values) > 0 && !(len(values) == 1 && values[0] == "") {
		if len(values) != 1 {
			return 0, true
		}
		key, err := ids.ParseRatingKeyGUID(values[0])
		if err != nil || !key.IsPluginKey() || key == show {
			return 0, true
		}
		detail, err := i.host.Experience.Item(r.Context(), frontend.ItemRequest{Viewer: v, Device: device(r), Key: key})
		if err != nil || detail.Title.Type != "season" || (detail.Title.ShowKey != show && detail.Title.ParentKey != show) {
			return 0, true
		}
		return key, true
	}
	if number == nil {
		return 0, false
	}
	seasons, err := i.host.Experience.Children(r.Context(), v, device(r), show, frontend.ItemQuery{Limit: showChildPageMax})
	if err != nil {
		return 0, true
	}
	for _, s := range seasons.Items {
		if s.Type == "season" && s.Season == *number {
			return s.Key, true
		}
	}
	return 0, true
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
	// Roku's typed screen node can supply an empty showID. Use only its
	// explicit episode anchor, never a library search, to recover the parent.
	showID := r.PathValue("id")
	if showID == "" {
		anchors := route.Query(r.URL.Query(), "StartItemId")
		if len(anchors) == 1 {
			if anchor, e := ids.ParseRatingKeyGUID(anchors[0]); e == nil && anchor.Kind() == frontend.KindEpisode {
				showID = ids.RatingKeyGUID(frontend.ShowKey(anchor.TMDB()))
			}
		}
		if showID == "" {
			i.page(w, r, v, frontend.Page{}, 0)
			return
		}
	}
	key, err := ids.ParseRatingKeyGUID(showID)
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
	// A plugin library published as TV shows keeps its own item tree: a season
	// is a container item, not a TMDB season number. Answer SeasonId (and
	// Season=<index>) with that container's children.
	if key.IsPluginKey() {
		if seasonKey, asked := i.pluginSeason(r, v, key, q.Season); asked {
			if seasonKey == 0 {
				i.page(w, r, v, frontend.Page{}, q.Offset)
				return
			}
			sq := q
			sq.Types, sq.Recursive, sq.Season = nil, false, nil
			page, cerr := i.host.Experience.Children(frontend.WithColdDetail(r.Context(), seasonKey), v, device(r), seasonKey, sq)
			if cerr != nil {
				if errors.Is(cerr, frontend.ErrNotFound) {
					i.page(w, r, v, frontend.Page{}, q.Offset)
					return
				}
				http.Error(w, "Unavailable", 503)
				return
			}
			i.pageWithVersion(w, r, v, page, q.Offset, q.StartAt, version)
			return
		}
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
	requested := key
	if q.Season != nil {
		requested = frontend.SeasonKey(key.TMDB(), *q.Season)
	}
	page, err := i.host.Experience.Children(frontend.WithColdDetail(r.Context(), requested), v, device(r), key, q)
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

// queryYears reads Years, which Jellyfin and Emby send as a comma list
// (a decade is Years=1990,1991,...,1999). A consecutive run is a year
// range; any other set of years is a list.
func queryYears(q *frontend.ItemQuery, values []string) {
	var years []int
	for _, value := range values {
		for _, part := range splitCSV(value) {
			if year, err := strconv.Atoi(part); err == nil && year > 0 {
				years = append(years, year)
			}
		}
	}
	if len(years) == 0 {
		return
	}
	slices.Sort(years)
	years = slices.Compact(years)
	if years[len(years)-1]-years[0] == len(years)-1 {
		q.Filter.YearFrom, q.Filter.YearTo = years[0], years[len(years)-1]
		return
	}
	q.Filter.Years = years
}

// premiereDateFilter is MinPremiereDate (op ">=") or MaxPremiereDate
// ("<="): an ISO 8601 instant, or a date, which MaxPremiereDate takes
// through the end of the day.
func premiereDateFilter(op, raw string) (frontend.DateFilter, bool) {
	raw = strings.TrimSpace(raw)
	if tm, err := time.Parse("2006-01-02", raw); err == nil {
		return frontend.DateFilter{Op: op, Unix: tm.Unix(), SpanDay: true}, true
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.9999999", "2006-01-02T15:04:05"} {
		if tm, err := time.Parse(layout, raw); err == nil {
			return frontend.DateFilter{Op: op, Unix: tm.UTC().Unix()}, true
		}
	}
	return frontend.DateFilter{}, false
}

// logRowFailure records a home row that could not be built; the client is
// answered an empty row instead of an error (#421).
func (i *instance) logRowFailure(purpose frontend.RowPurpose, err error) {
	if i.host.Log != nil {
		i.host.Log.Warn("jellyfin row unavailable", "purpose", purpose, "err", err)
	}
}

// pluginView reports whether a ParentId names a plugin-provided library view.
func (i *instance) pluginView(r *http.Request, viewer *frontend.Viewer, parent string) bool {
	id, err := ids.ParseGUID(ids.View, parent)
	if err != nil || ids.PersonalView(id) {
		return false
	}
	views, err := i.host.Experience.Views(r.Context(), viewer)
	if err != nil {
		return false
	}
	want := ids.Decimal(id)
	for _, view := range views {
		if view.ID == want {
			return view.PluginID != "" || view.KindKey != ""
		}
	}
	return false
}
