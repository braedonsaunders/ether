package plex

import (
	"github.com/braedonsaunders/ether/internal/plex/model"
	"net/http"

	"github.com/go-chi/chi/v5"
)

func (s *Server) routes() chi.Router {
	r := chi.NewRouter()
	r.Use(s.recoverer, s.commonHeaders, s.parse, s.requestLog, s.authenticate)
	r.NotFound(s.handleUnknown)
	r.MethodNotAllowed(s.handleUnknown)

	// get registers GET and HEAD.
	get := func(pattern string, h http.HandlerFunc) {
		handler := func(w http.ResponseWriter, r *http.Request) {
			if !s.serveShelfGeneration(w, r) {
				h(w, r)
			}
		}
		r.Get(pattern, handler)
		r.Head(pattern, handler)
	}
	any := func(pattern string, h http.HandlerFunc) { r.HandleFunc(pattern, h) }

	// Server
	get("/", s.handleRoot)
	get("/identity", s.handleIdentity)
	get("/media/providers", s.handleProviders)
	get("/myplex/account", s.handleMyPlexAccount)
	get("/servers", s.handleServers)
	get("/clients/profiles", s.handleClientProfiles)
	get("/clients/profiles/", s.handleClientProfiles)
	any("/sync", func(w http.ResponseWriter, r *http.Request) {
		s.write(w, r, &struct {
			Size    int    `plex:"size"`
			Content string `plex:"content"`
		}{0, "plugins"})
	})
	get("/clients", func(w http.ResponseWriter, r *http.Request) { s.writeEmpty(w, r) })
	get("/devices", s.ownerOnly(s.handleDevices))
	get("/devices/{id}", s.ownerOnly(s.handleDevice))
	get("/accounts", s.ownerOnly(s.handleAccounts))
	get("/:/prefs", s.ownerOnly(s.handlePrefs))
	r.Put("/:/prefs", s.ownerOnly(s.handlePrefsSet))
	get("/system/agents", s.handleAgents)
	get("/activities", s.handleActivities)
	r.Delete("/activities/{id}", s.ownerOnly(s.handleActivityDelete))
	r.Put("/activities/{id}", s.ownerOnly(func(w http.ResponseWriter, r *http.Request) { writeHTTPError(w, http.StatusBadRequest) }))
	get("/butler", s.ownerOnly(func(w http.ResponseWriter, r *http.Request) { s.writeEmpty(w, r) }))
	any("/butler/{task}", s.ownerOnly(s.handleUnsupportedManagement))
	r.Post("/butler", s.ownerOnly(s.handleUnsupportedManagement))
	r.Delete("/butler", s.ownerOnly(s.handleUnsupportedManagement))
	get("/security/resources", func(w http.ResponseWriter, r *http.Request) { s.writeEmpty(w, r) })
	get("/security/token", s.handleSecurityToken)
	get("/updater/status", s.handleUpdaterStatus)
	r.Put("/updater/check", s.ownerOnly(s.handleUpdaterCheck))
	r.Put("/updater/apply", s.ownerOnly(func(w http.ResponseWriter, r *http.Request) { writeHTTPError(w, http.StatusBadRequest) }))
	get("/:/resources/*", func(w http.ResponseWriter, r *http.Request) { writeHTTPError(w, http.StatusNotFound) })
	get("/:/eventsource/notifications", s.handleEventSource)
	get("/:/websockets/notifications", s.handleWebsocket)
	get("/:/websocket/notifications", s.handleWebsocket)

	// Library
	get("/library", s.handleLibraryRoot)
	get("/library/", s.handleLibraryRoot)
	get("/library/sections", s.handleSections)
	get("/library/sections/all", s.handleSections)
	get("/library/sections/{section}", s.handleSection)
	get("/library/sections/{section}/{sub}", s.handleSectionSub)
	get("/library/sections/{section}/{sub}/{key}", s.handleSectionSubKey)
	any("/library/sections/{section}/refresh", s.ownerOnly(s.handleUnsupportedManagement))
	get("/library/onDeck", s.handleOnDeck)
	get("/library/recentlyAdded", s.handleRecentlyAdded)
	get("/library/all", s.handleLibraryAll)
	get("/library/search", s.handleLibrarySearch)
	get("/search", s.handleSearch)
	get("/library/metadata/{key}", s.handleMetadata)
	r.Delete("/library/metadata/{key}", s.handlePlaylistMetadataRefusal)
	r.Put("/library/metadata/{key}/prefs", s.handlePlaylistMetadataRefusal)
	r.Put("/library/metadata/{key}/analyze", s.handlePlaylistMetadataRefusal)
	r.Put("/library/metadata/{key}/match", s.handlePlaylistMetadataRefusal)
	get("/library/metadata/{key}/match", s.handlePlaylistMetadataRefusal)
	get("/library/metadata/{key}/tree", s.handleMetadataTree)
	get("/library/metadata/{key}/children", s.handleChildren)
	get("/library/metadata/{key}/allLeaves", s.handleAllLeaves)
	get("/library/metadata/{key}/grandchildren", s.handleAllLeaves)
	get("/library/metadata/{key}/related", s.handleMetadataHubs)
	get("/library/metadata/{key}/similar", s.handleSimilar)
	get("/library/metadata/{key}/onDeck", s.handleItemOnDeck)
	get("/library/metadata/{key}/parent", s.handleParent)
	get("/library/metadata/{key}/grandparent", s.handleParent)
	get("/library/metadata/{key}/extras", s.handlePlaylistExtras)
	r.Post("/library/metadata/{key}/extras", func(w http.ResponseWriter, r *http.Request) { writeHTTPError(w, http.StatusBadRequest) })
	get("/library/metadata/{key}/extras/all", s.handlePlaylistExtras)
	get("/library/metadata/{key}/subtitles", s.handleSubtitleSearch)
	r.Put("/library/metadata/{key}/subtitles", s.handleSubtitlePick)
	r.Post("/library/metadata/{key}/subtitles", s.handleSubtitleUpload)
	for _, leaf := range []string{"posters", "arts", "clearLogos", "themes"} {
		get("/library/metadata/{key}/"+leaf, s.handlePlaylistArtworkCandidates)
		r.Post("/library/metadata/{key}/"+leaf, s.handlePlaylistMetadataRefusal)
		r.Put("/library/metadata/{key}/"+leaf, s.handlePlaylistMetadataRefusal)
	}
	get("/library/metadata/{key}/person/{kind}/{identity}", s.handlePersonImage)
	get("/library/metadata/{key}/{element}", s.handleImage)
	get("/library/metadata/{key}/{element}/{ts}", s.handleImage)
	get("/library/people/{id}", s.handleLibraryPerson)
	get("/library/people/{id}/media", s.handleLibraryPersonMedia)
	get("/library/collections", s.handleCollections)
	get("/library/collections/{key}", s.handleCollection)
	get("/library/collections/{key}/children", s.handleCollectionItems)
	get("/library/collections/{key}/items", s.handleCollectionItems)
	get("/library/collections/{key}/{element}/{ts}", s.handleImage)
	get("/photo/:/transcode", s.handlePhotoTranscode)

	// Hubs and search
	any("/player/*", func(w http.ResponseWriter, r *http.Request) { writeHTTPError(w, http.StatusBadRequest) })
	get("/hubs", s.handleHubs)
	get("/hubs/promoted", s.handlePromotedHubs)
	get("/hubs/continueWatching", s.handleContinueWatching)
	get("/hubs/continueWatching/items", s.handleContinueWatchingItems)
	get("/hubs/home/continueWatching", s.handleContinueWatchingItems)
	get("/hubs/home/onDeck", s.handleOnDeck)
	get("/hubs/home/recentlyAdded", s.handleRecentlyAdded)
	get("/hubs/items", s.handleHubItems)
	get("/hubs/items/{identifier}", s.handleHubItems)
	get("/hubs/search", s.handleHubSearch)
	get("/hubs/search/voice", s.handleHubSearch)
	get("/hubs/sections/{section}", s.handleSectionHubs)
	get("/hubs/sections/{section}/continueWatching/items", s.handleContinueWatchingItems)
	get("/hubs/metadata/{key}", s.handleMetadataHubs)
	get("/hubs/metadata/{key}/related", s.handleMetadataHubs)
	get("/hubs/metadata/{key}/postplay", s.handlePostplay)

	// Playlists
	get("/playlists", s.handlePlaylists)
	get("/playlists/all", s.handlePlaylists)
	get("/playlists/sorts", s.handlePlaylistSorts)
	r.Post("/playlists", s.handlePlaylistCreate)
	r.Post("/playlists/upload", s.handlePlaylistUpload)
	get("/playlists/{key}", s.handlePlaylist)
	r.Put("/playlists/{key}", s.handlePlaylistEdit)
	r.Delete("/playlists/{key}", s.handlePlaylistDelete)
	get("/playlists/{key}/items", s.handlePlaylistItems)
	r.Put("/playlists/{key}/items", s.handlePlaylistAdd)
	r.Delete("/playlists/{key}/items", s.handlePlaylistClear)
	r.Delete("/playlists/{key}/items/{item}", s.handlePlaylistRemove)
	r.Put("/playlists/{key}/items/{item}", s.handlePlaylistItemRefusal)
	r.Put("/playlists/{key}/items/{item}/move", s.handlePlaylistMove)
	get("/playlists/{key}/{element}/{ts}", s.handleImage)

	// Play queues
	get("/playQueues", func(w http.ResponseWriter, r *http.Request) {
		if _, present := r.URL.Query()["type"]; present {
			writeHTTPError(w, http.StatusBadRequest)
			return
		}
		s.writeEmpty(w, r)
	})
	r.Post("/playQueues", s.handlePlayQueueCreate)
	get("/playQueues/{pq}", s.handlePlayQueue)
	r.Put("/playQueues/{pq}", s.handlePlayQueueMutate("add"))
	r.Put("/playQueues/{pq}/shuffle", s.handlePlayQueueMutate("shuffle"))
	r.Put("/playQueues/{pq}/unshuffle", s.handlePlayQueueMutate("unshuffle"))
	r.Put("/playQueues/{pq}/reset", s.handlePlayQueueMutate("reset"))
	r.Delete("/playQueues/{pq}/items", s.handlePlayQueueMutate("clear"))
	r.Delete("/playQueues/{pq}/items/{item}", s.handlePlayQueueMutate("remove"))
	r.Put("/playQueues/{pq}/items/{item}/move", s.handlePlayQueueMutate("move"))

	// Watch state
	any("/:/timeline", s.handleTimeline)
	any("/:/progress", s.handleProgress)
	any("/:/scrobble", s.handleScrobble)
	any("/:/unscrobble", s.handleUnscrobble)
	any("/:/rate", s.handleRate)
	any("/actions/removeFromContinueWatching", s.handleRemoveFromContinueWatching)
	get("/status/sessions", s.handleSessions)
	get("/history", func(w http.ResponseWriter, r *http.Request) { s.write(w, r, &model.MediaContainer{Content: "plugins"}) })
	get("/hubs/sections/{section}/popular", s.handleHistoryPopular)
	get("/library/all/top", s.handleHistoryTop)
	get("/library/metadata/{key}/users/top", s.handleHistoryTopUsers)
	get("/status/sessions/history/all", s.handleHistory)
	get("/status/sessions/history/{history}", s.handleHistoryItem)
	r.Delete("/status/sessions/history/{history}", s.handleHistoryItem)
	get("/statistics/bandwidth", s.ownerOnly(s.handleStatisticsBandwidth))
	get("/statistics/resources", s.ownerOnly(s.handleStatisticsResources))
	get("/statistics/media", s.ownerOnly(s.handleStatisticsMedia))
	any("/status/sessions/terminate", s.ownerOnly(s.handleTerminate))
	any("/status/sessions/terminate/player", s.ownerOnly(s.handleTerminate))

	// Playback
	get("/{ttype:video|music|audio|subtitles}/:/transcode/universal/decision", s.handleDecision)
	get("/{ttype:video|music|audio|subtitles}/:/transcode/universal/start.m3u8", s.handleStartHLS)
	get("/{ttype:video|music|audio|subtitles}/:/transcode/universal/start.mpd", s.handleStartDASH)
	get("/{ttype:video|music|audio|subtitles}/:/transcode/universal/start", s.handleStartHTTP)
	get("/{ttype:video|music|audio|subtitles}/:/transcode/universal/start.mkv", s.handleStartHTTP)
	get("/{ttype:video|music|audio|subtitles}/:/transcode/universal/session/{session}/{stream:[0-9]+}/header", s.handleDASHInit)
	get("/{ttype:video|music|audio|subtitles}/:/transcode/universal/session/{session}/{stream:[0-9]+}/{segment}", s.handleDASHSegment)
	get("/{ttype:video|music|audio|subtitles}/:/transcode/universal/session/{session}/*", s.handleTranscodeSession)
	any("/{ttype:video|music|audio|subtitles}/:/transcode/universal/ping", s.handleTranscodePing)
	any("/{ttype:video|music|audio|subtitles}/:/transcode/universal/stop", s.handleTranscodeStop)
	get("/{ttype:video|music|audio|subtitles}/:/transcode/universal/subtitles", s.handleTranscodeSubtitles)
	get("/subtitles/:/transcode/universal/start", s.handleTranscodeSubtitles)
	get("/transcode/sessions", s.handleTranscodeSessions)
	get("/library/parts/{part}/indexes/{index}/{offset}", s.handleIndexFrame)
	get("/library/parts/{part}/indexes/{index}", s.handleIndexBIF)
	get("/library/parts/{part}/{changestamp:[0-9]+}/{file}", s.handlePart)
	// PM4K (Plex for Kodi) drops the "file.<ext>" name before handing the URL
	// to Kodi, so Kodi doesn't scan the "directory" for subtitles. PMS serves
	// the part at the bare changestamp path, so Ether does too.
	get("/library/parts/{part}/{changestamp}/", s.handlePart)
	get("/library/parts/{part}/{file}", s.handlePart)
	get("/library/parts/{part}", s.handlePart)
	r.Put("/library/parts/{part}", s.handlePartSelect)
	r.Post("/library/parts/{part}", s.handlePartSelect)
	get("/library/streams/{stream}", s.handleStream)
	r.Put("/library/streams/{stream}", s.handleStream)
	r.Delete("/library/streams/{stream}", s.handleStream)

	// Downloads (Download Queue API)
	r.Post("/downloadQueue", s.handleDownloadQueueCreate)
	get("/downloadQueue/{queue}", s.handleDownloadQueue)
	r.Post("/downloadQueue/{queue}/add", s.handleDownloadQueueAdd)
	get("/downloadQueue/{queue}/items", s.handleDownloadQueueItems)
	get("/downloadQueue/{queue}/items/{item}", s.handleDownloadQueueItem)
	r.Delete("/downloadQueue/{queue}/items/{item}", s.handleDownloadQueueItemDelete)
	r.Post("/downloadQueue/{queue}/items/{item}/restart", s.handleDownloadQueueItemRestart)
	get("/downloadQueue/{queue}/item/{item}/decision", s.handleDownloadItemDecision)
	get("/downloadQueue/{queue}/item/{item}/media", s.handleDownloadItemMedia)

	get("/library/metadata/{id}/discoveryPoster/{version}", s.handleDiscoveryPoster)
	s.registerLiveTVRoutes(r)
	return r
}
