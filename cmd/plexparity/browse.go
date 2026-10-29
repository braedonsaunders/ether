package main

// These requests use discovered fixture IDs and exercise populated read paths.
func browsingPaths() []string {
	return []string{
		"/library", "/library/", "/library/all", "/library/sections/all", "/library/sections/{{section}}", "/library/recentlyAdded",
		"/library/metadata/{{showKey}}/allLeaves", "/library/metadata/{{showKey}}/grandchildren", "/library/metadata/{{episodeKey}}/parent", "/library/metadata/{{episodeKey}}/grandparent",
		"/library/metadata/{{movieKey}}/related", "/library/metadata/{{movieKey}}/similar", "/library/metadata/{{showKey}}/onDeck", "/library/metadata/{{movieKey}}/extras",
		"/library/collections", "/library/collections/{{collectionKey}}/children", "/library/collections/{{collectionKey}}/items",
		"/hubs/continueWatching/items", "/hubs/home/continueWatching", "/hubs/home/onDeck", "/hubs/home/recentlyAdded", "/hubs/sections/{{section}}/continueWatching/items",
		"/hubs/metadata/{{movieKey}}", "/hubs/metadata/{{movieKey}}/related", "/hubs/metadata/{{movieKey}}/postplay", "/hubs/search/voice?query=fixture", "/search?query=fixture",
		"/playlists", "/playlists/all", "/playlists/{{playlistKey}}/items", "/status/sessions/history/all", "/servers", "/clients", "/devices", "/activities", "/security/resources", "/system/agents", "/transcode/sessions",
	}
}
