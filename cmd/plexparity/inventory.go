package main

import (
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"

	protocol "github.com/braedonsaunders/ether/internal/conform/plex"
	server "github.com/braedonsaunders/ether/internal/plex"
	"github.com/go-chi/chi/v5"
)

// missingRoutes audits coverage; sample substitutions are used solely for
// route matching, never as request targets or fixture discovery fallbacks.
func missingRoutes() []string {
	paths := []string{}
	template := regexp.MustCompile(`\{\{[^}]+\}\}`)
	for _, p := range protocol.Corpus() {
		path := template.ReplaceAllString(p.Path, "1")
		if u, err := url.Parse(path); err == nil {
			paths = append(paths, u.Path)
		}
	}
	for _, path := range imagePaths() {
		path = template.ReplaceAllString(path, "1")
		if u, err := url.Parse(path); err == nil {
			paths = append(paths, u.Path)
		}
	}
	for _, path := range browsingPaths() {
		path = template.ReplaceAllString(path, "1")
		if u, err := url.Parse(path); err == nil {
			paths = append(paths, u.Path)
		}
	}
	paths = append(paths, "/:/eventsource/notifications", "/:/websocket/notifications", "/:/websockets/notifications", "/:/resources/fixture", "/playQueues/1", "/tv.plex.providers.epg.xmltv:1/metadata/1", "/status/sessions/terminate", "/status/sessions/terminate/player", "/library/parts/1/1/file.mp4", "/library/parts/1/file.mp4", "/video/:/transcode/universal/start.m3u8", "/video/:/transcode/universal/start.mpd", "/video/:/transcode/universal/session/fixture/0/header", "/video/:/transcode/universal/session/fixture/0/0.m4s", "/video/:/transcode/universal/session/fixture/base/index.m3u8", "/video/:/transcode/universal/ping", "/video/:/transcode/universal/stop", "/:/timeline", "/:/scrobble", "/:/progress", "/:/rate", "/:/unscrobble", "/actions/removeFromContinueWatching", "/library/onDeck", "/hubs/continueWatching", "/hubs")
	paths = append(paths, "/livetv/sessions/1", "/livetv/sessions/1/0/index.m3u8", "/livetv/sessions/1/0/program/1/0.ts", "/media/subscriptions/1", "/media/grabbers/operations/1")
	paths = append(paths, "/downloadQueue/1", "/downloadQueue/1/items", "/downloadQueue/1/items/1", "/downloadQueue/1/item/1/decision", "/downloadQueue/1/item/1/media")
	paths = append(paths, "/library/parts/1/indexes/sd", "/library/parts/1/indexes/sd/0", "/library/metadata/1/subtitles", "/library/streams/1", "/video/:/transcode/universal/subtitles")
	var missing []string
	for _, route := range server.RouteInventory().Routes {
		if route.Method != http.MethodGet {
			continue
		}
		mux := chi.NewRouter()
		mux.Get(route.Pattern, func(http.ResponseWriter, *http.Request) {})
		covered := false
		for _, path := range paths {
			if mux.Match(chi.NewRouteContext(), http.MethodGet, path) {
				covered = true
				break
			}
		}
		if !covered {
			missing = append(missing, route.Pattern)
		}
	}
	sort.Strings(missing)
	return missing
}
func inventorySummary() string { m := missingRoutes(); return strings.Join(m, ", ") }
