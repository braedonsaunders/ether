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
		"/tv.plex.providers.epg.xmltv:1", "/tv.plex.providers.epg.xmltv:1/hubs", "/tv.plex.providers.epg.xmltv:1/watchnow", "/tv.plex.providers.epg.xmltv:1/watchnow/all", "/tv.plex.providers.epg.xmltv:1/sections/1/all", "/tv.plex.providers.epg.xmltv:1/lineups/dvr/channels", "/livetv/dvrs", "/livetv/dvrs/1", "/livetv/epg/channelmap", "/livetv/epg/lineupchannels", "/livetv/channels/fixture-channel/logo.svg", "/livetv/sessions", "/media/grabbers", "/media/grabbers/devices", "/media/grabbers/devices/1", "/media/grabbers/devices/1/channels", "/media/grabbers/devices/1/channelmap", "/media/subscriptions", "/accounts", "/updater/status", "/tv.plex.providers.epg.xmltv:1/grid?beginsAt=1792324800&endsAt=1792328400", "/tv.plex.providers.epg.xmltv:1/channels/fixture-channel", "/livetv/epg/channels", "/livetv/guide.xml",
		"/playlists", "/playlists/all", "/playlists/{{playlistKey}}/items", "/status/sessions/history/all", "/servers", "/clients", "/devices", "/activities", "/security/resources", "/system/agents", "/transcode/sessions",
	}
}
