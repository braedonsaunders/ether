package schema

import (
	"fmt"

	"github.com/braedonsaunders/ether/internal/extend"
)

// Builtins are Ether's own settings forms, served as settingsSchema
// descriptors. They moved here from web/src/pages/settings/schema.ts with
// the same copy, order and grouping; testdata/builtins.json is the golden
// the mock console backend is held to.
func Builtins() []Builtin {
	return []Builtin{
		// SERVER
		{ID: "server", ConfigKey: "server", Label: extend.T("What viewers see"), Schema: Schema{Groups: []Group{
			{ID: "identity", Title: extend.T("What viewers see"), Description: extend.T("The name apps show for this server."), Fields: []Field{
				{Key: "friendlyName", Label: extend.T("Server name"), Kind: KindText, Help: extend.T("How the server appears in apps.")},
			}},
			{ID: "connect", Title: extend.T("How apps connect"), Description: extend.T("The address apps use to reach this computer."), Fields: []Field{
				{Key: "port", Label: extend.T("Port"), Kind: KindNumber, Help: extend.T("Plex apps expect 32400. A restart is needed after changing it."), Min: F(1), Max: F(65535), Step: F(1)},
			}, More: []Field{
				{Key: "bindAddress", Label: extend.T("Listen address"), Kind: KindText, Help: extend.T("0.0.0.0 listens on every network interface.")},
				{Key: "machineIdentifier", Label: extend.T("Machine identifier"), Kind: KindReadonly, Help: extend.T("Generated on first run; never changes.")},
				{Key: "webFrameOrigins", Label: extend.T("Sites that may embed this page"), Kind: KindList, Help: extend.T("Exact addresses allowed to frame the owner page. Empty means nowhere."), Placeholder: "https://home.example.com"},
				{Key: "trustedProxyCIDRs", Label: extend.T("Trusted proxy networks"), Kind: KindList, Help: extend.T("Networks whose forwarding headers Ether trusts. Empty trusts none."), Placeholder: "10.0.0.0/8"},
				{Key: "logLevel", Label: extend.T("Log level"), Kind: KindSelect, Options: []Option{{Value: "debug", Label: extend.T("Debug")}, {Value: "info", Label: extend.T("Info")}, {Value: "warn", Label: extend.T("Warnings")}, {Value: "error", Label: extend.T("Errors only")}}},
			}},
			{ID: "streams", Title: extend.T("Who can watch at once"), Description: extend.T("Leave these at the defaults unless too many people are watching at the same time."), Fields: []Field{}, More: []Field{
				{Key: "defaultStreamLimit", Label: extend.T("Streams per viewer"), Kind: KindNumber, Help: extend.T("Default simultaneous streams for each viewer. 0 = unlimited."), Min: F(0), Step: F(1)},
				{Key: "maxStreams", Label: extend.T("Server-wide limit"), Kind: KindNumber, Help: extend.T("0 derives the limit from your Usenet connections."), Min: F(0), Step: F(1)},
			}},
		}}},
		// PLEX
		{ID: "plex", ConfigKey: "plex", Label: extend.T("Remote access"), Schema: Schema{Groups: []Group{
			{ID: "remote", Title: extend.T("Remote access"), Description: extend.T("Let Plex apps reach Ether from outside your network."), Fields: []Field{
				{Key: "remoteAccess", Label: extend.T("Allow remote access"), Kind: KindBool, Help: extend.T("Publishes your public address to plex.tv.")},
			}, More: []Field{
				{Key: "useUpnp", Label: extend.T("Map the port automatically (UPnP)"), Kind: KindBool, Help: extend.T("Asks your router to forward the port.")},
				{Key: "publicPort", Label: extend.T("Public port"), Kind: KindNumber, Help: extend.T("The port forwarded on your router. 0 uses the server port."), Min: F(0), Max: F(65535), Step: F(1)},
				{Key: "publicHost", Label: extend.T("Public host name"), Kind: KindText, Help: extend.T("Optional. Enter a name like plex.example.com, or the full https:// address of your reverse proxy. Leave empty to detect your public address.")},
				{Key: "customUrls", Label: extend.T("Custom server URLs"), Kind: KindList, Help: extend.T("Extra addresses Plex apps should try, comma-separated."), Placeholder: "https://ether.example.com:443"},
			}},
			{ID: "secure", Title: extend.T("Secure connections"), Description: extend.T("How Plex apps encrypt the connection to this server."), Fields: []Field{
				{Key: "secureConnections", Label: extend.T("Secure connections"), Kind: KindSelect, Options: []Option{{Value: "required", Label: extend.T("Required")}, {Value: "preferred", Label: extend.T("Preferred")}, {Value: "disabled", Label: extend.T("Disabled")}}},
			}, More: []Field{
				{Key: "certFile", Label: extend.T("Certificate file"), Kind: KindText, Help: extend.T("Leave empty to use the plex.direct certificate Ether manages."), Wide: true},
				{Key: "keyFile", Label: extend.T("Key file"), Kind: KindText, Help: extend.T("The private key that matches the certificate file."), Wide: true},
			}},
			{ID: "compatibility", Title: extend.T("Compatibility"), Fields: []Field{
				{Key: "trackLatestVersion", Label: extend.T("Report the latest public PMS version"), Kind: KindBool, Help: extend.T("Off reports the version Ether is verified against. Plex enables features by server version.")},
				{Key: "latestCompatibilityVersion", Label: extend.T("Latest public PMS version"), Kind: KindReadonly},
			}, More: []Field{
				{Key: "compatibilityVersion", Label: extend.T("Compatibility version override"), Kind: KindText, Help: extend.T("Leave empty to use the verified version, or the latest public version when tracking is on.")},
			}},
		}}},
		// CACHE
		{ID: "cache", ConfigKey: "cache", Label: extend.T("Disk cache"), Schema: Schema{Groups: []Group{
			{Title: extend.T("Disk cache"), Description: extend.T("Segments from Usenet are cached on disk so seeks and rewatches are instant."), Fields: []Field{
				{Key: "dir", Label: extend.T("Cache folder"), Kind: KindText, Wide: true},
				{Key: "budgetBytes", Label: extend.T("Disk budget"), Kind: KindBytes, Help: extend.T("The oldest segments are evicted when the cache reaches this size."), Unit: extend.Text{Source: "GiB"}},
			}},
			{Title: extend.T("Prefetch"), Fields: []Field{
				{Key: "prefetchNextEpisode", Label: extend.T("Prefetch the next episode"), Kind: KindBool, Help: extend.T("Warms the opening of the next episode so it starts instantly.")},
				{Key: "nextEpisodeAt", Label: extend.T("Start prefetching at"), Kind: KindNumber, Help: extend.T("Fraction of the current episode watched (0–1)."), Min: F(0), Max: F(1), Step: F(0.05)},
				{Key: "prefetchBytes", Label: extend.T("Prefetch size"), Kind: KindBytes, Help: extend.T("How much of the next episode to pull ahead."), Unit: extend.Text{Source: "MiB"}},
				{Key: "keepInProgressWarm", Label: extend.T("Keep in-progress titles warm"), Kind: KindBool, Help: extend.T("Keeps the resume point of unfinished titles cached.")},
				{Key: "inProgressPinMaxAge", Label: extend.T("Stop keeping a title warm after"), Kind: KindDuration, Help: extend.T("An unfinished title nobody has watched for this long loses its cache pin. Empty uses 30 days."), Placeholder: "720h"},
				{Key: "inProgressPinPercent", Label: extend.T("Share of the cache for in-progress titles"), Kind: KindNumber, Help: extend.T("Percent of the disk budget unfinished titles may keep pinned; the least recently watched lose their pin first. 0 uses 50."), Min: F(0), Max: F(100), Step: F(5)},
			}},
		}}},
		// CATALOG
		{ID: "catalog", ConfigKey: "catalog", Label: extend.T("Names and dates"), Schema: Schema{Groups: []Group{
			{Title: extend.T("Names and dates"), Description: extend.T("Language and region choose title names, descriptions, release dates and which titles count as available."), Fields: []Field{
				{Key: "language", Label: extend.T("Language"), Kind: KindText, Help: extend.T("Metadata language, e.g. en-US or fr-FR.")},
				{Key: "region", Label: extend.T("Region"), Kind: KindText, Help: extend.T("Two-letter region for release dates and streaming availability.")},
				{Key: "refreshInterval", Label: extend.T("Refresh every"), Kind: KindDuration},
			}},
			{Title: extend.T("What's browsable"), Description: extend.T("Hide obscure titles from libraries. They stay searchable."), Fields: []Field{
				{Key: "minPopularity", Label: extend.T("Minimum popularity"), Kind: KindNumber, Min: F(0), Step: F(1)},
				{Key: "minVotes", Label: extend.T("Minimum votes"), Kind: KindNumber, Min: F(0), Step: F(10)},
				{Key: "includeAdult", Label: extend.T("Include adult titles"), Kind: KindBool},
				{Key: "hideFranchiseCollections", Label: extend.T("Hide automatic franchise collections"), Kind: KindBool, Help: extend.T("Hide automatic franchise groups, such as Zootopia.")},
			}},
		}}},
		// PLAY_VERSIONS
		{ID: "playVersions", ConfigKey: "resolver", Label: extend.T("Versions in the app"), Schema: Schema{Groups: []Group{
			{Title: extend.T("Versions in the app"), Description: extend.T("Extra copies of the same picture, after Auto."), Fields: []Field{
				{Key: "sameQualityAlternatives", Label: extend.T("Same-quality versions to offer"), Kind: KindNumber, Help: extend.T("Listed after Auto in the app's version menu."), Min: F(0), Max: F(3), Step: F(1)},
				{Key: "alternativesPerResolution", Label: extend.T("Offer alternatives at every resolution"), Kind: KindBool, Help: extend.T("Apply the alternatives count to each available resolution, up to seven versions total. Auto stays first; profile and device limits still apply.")},
				{Key: "offerHigherVersions", Label: extend.T("Offer higher versions"), Kind: KindBool, Help: extend.T("Version menus can list releases taller than the profile. Off keeps the ceiling you set. HDR and file-size limits still apply.")},
			}},
		}}},
		// RELEASE_SEARCH
		{ID: "releaseSearch", ConfigKey: "resolver", Label: extend.T("How Ether searches"), Schema: Schema{Groups: []Group{
			{Title: extend.T("How Ether searches"), Description: extend.T("How many releases Ether keeps ready, and how long it remembers a search. The defaults are enough for a household."), Fields: []Field{
				{Key: "maxCandidates", Label: extend.T("Releases to keep ready"), Kind: KindNumber, Help: extend.T("Fallback releases kept for instant failover."), Min: F(0), Step: F(1)},
				{Key: "healthSamples", Label: extend.T("Health check samples"), Kind: KindNumber, Help: extend.T("Articles checked before a release is trusted."), Min: F(0), Step: F(1)},
				{Key: "searchCacheTtl", Label: extend.T("Remember searches for"), Kind: KindDuration},
				{Key: "unavailableCooloff", Label: extend.T("Retry unavailable titles after"), Kind: KindDuration},
			}},
		}}},
		// RELEASE_BLOCKS
		{ID: "releaseBlocks", ConfigKey: "resolver", Label: extend.T("Words and groups"), Schema: Schema{Groups: []Group{
			{Title: extend.T("Words and groups"), Description: extend.T("Blocked terms match whole tokens. sub matches SUB, not Subtitles or SUBPACK. Subtitle language tags alone do not prove burned-in subtitles."), Fields: []Field{
				{Key: "hardSubtitles", Label: extend.T("Burned-in subtitles"), Kind: KindSelect, Help: extend.T("Video checks run after playback starts. A newly discovered verdict applies to later selections."), Options: []Option{{Value: "", Label: extend.T("Prefer clean releases")}, {Value: "exclude", Label: extend.T("Exclude known hardsubs")}, {Value: "allow", Label: extend.T("Allow")}}},
				{Key: "blockedGroups", Label: extend.T("Blocked release groups"), Kind: KindList, Placeholder: "GROUP1, GROUP2"},
				{Key: "blockedTerms", Label: extend.T("Blocked terms"), Kind: KindList, Placeholder: "hardcoded, korsub"},
			}},
		}}},
		// TRANSCODE
		{ID: "transcode", ConfigKey: "transcode", Label: extend.T("Converting for the app"), Schema: Schema{Groups: []Group{
			{ID: "convert", Title: extend.T("Converting for the app"), Description: extend.T("Ether plays the file directly when the app can. It converts the video on this computer only when it has to."), Fields: []Field{
				{Key: "hwAccel", Label: extend.T("Hardware acceleration"), Kind: KindSelect, Help: extend.T("Automatic uses this computer's graphics chip when an app can't play the file as-is. Choose Off only if converted video looks wrong."), Options: []Option{{Value: "auto", Label: extend.T("Automatic")}, {Value: "none", Label: extend.T("Off (software)")}, {Value: "videotoolbox", Label: extend.Text{Source: "VideoToolbox (Apple)"}}, {Value: "nvenc", Label: extend.Text{Source: "NVENC (NVIDIA)"}}, {Value: "qsv", Label: extend.Text{Source: "Quick Sync (Intel)"}}, {Value: "vaapi", Label: extend.Text{Source: "VA-API (Linux)"}}}},
			}, More: []Field{
				{Key: "vaapiDevice", Label: extend.T("VA-API device"), Kind: KindText, Help: extend.T("Linux graphics device, for example /dev/dri/renderD128. Leave empty unless converted video fails to start.")},
				{Key: "maxTranscodes", Label: extend.T("Simultaneous transcodes"), Kind: KindNumber, Help: extend.T("How many videos can be converted at once. Leave the default unless several people watch together and the computer struggles."), Min: F(0), Step: F(1)},
				{Key: "remoteMaxBitrateKbps", Label: extend.T("Remote bitrate cap"), Kind: KindNumber, Help: extend.T("Limits picture quality for people watching away from home, so the stream fits their connection. 0 means no limit."), Min: F(0), Step: F(500), Unit: extend.Text{Source: "kbps"}},
			}},
			{ID: "enhance", Title: extend.T("Sharper picture"), Description: extend.T("On a supported NVIDIA graphics card, Ether can clean up a soft video before sending it. This uses extra graphics power, and Ether installs what it needs on a supported Windows computer."), Fields: []Field{
				{Key: "rtxVideo", Label: extend.Text{Source: "RTX Video Super Resolution"}, Kind: KindGroup, Fields: []Field{
					{Key: "mode", Label: extend.Text{Source: "RTX Video Super Resolution"}, Kind: KindSelect, Help: extend.T("Cleans up a soft picture before it is sent. The viewer's resolution and bitrate limits still apply. Unsupported videos play normally."), Options: []Option{{Value: "off", Label: extend.T("Off")}, {Value: "auto", Label: extend.T("Automatic · local streams")}, {Value: "on", Label: extend.T("On · eligible streams")}}},
				}},
			}, More: []Field{
				{Key: "rtxVideo", Label: extend.Text{Source: "RTX Video Super Resolution"}, Kind: KindGroup, Fields: []Field{
					{Key: "sourceMaxHeight", Label: extend.T("Enhance sources up to"), Kind: KindNumber, Help: extend.T("Videos taller than this play normally, without enhancement."), Min: F(144), Max: F(2160), Step: F(1), Unit: extend.Text{Source: "p"}},
					{Key: "targetMaxHeight", Label: extend.T("Enhanced resolution limit"), Kind: KindNumber, Help: extend.T("The tallest picture enhancement is allowed to produce."), Min: F(144), Max: F(2160), Step: F(1), Unit: extend.Text{Source: "p"}},
					{Key: "quality", Label: extend.T("Enhancement quality"), Kind: KindSelect, Help: extend.T("Higher quality uses more of the graphics card. Streaming choices need a recent NVIDIA card. Medium is a good start."), Numeric: true, Options: []Option{{Value: "1", Label: extend.T("Low")}, {Value: "2", Label: extend.T("Medium")}, {Value: "3", Label: extend.T("High")}, {Value: "4", Label: extend.T("Ultra")}, {Value: "21", Label: extend.T("Streaming medium")}, {Value: "23", Label: extend.T("Streaming ultra")}}},
					{Key: "maxConcurrent", Label: extend.T("Simultaneous enhancements"), Kind: KindNumber, Help: extend.T("Additional streams play normally once this many are already being enhanced."), Min: F(1), Max: F(8), Step: F(1)},
					{Key: "executablePath", Label: extend.T("NVEnc executable path"), Kind: KindText, Help: extend.T("Leave empty and Ether sets this up itself. A path you fill in is never replaced.")},
				}},
			}},
			{ID: "programs", Title: extend.T("Converter programs"), Description: extend.T("Ether includes the programs it needs. Change a path only if you installed your own copy."), Fields: []Field{}, More: []Field{
				{Key: "ffmpegPath", Label: extend.T("ffmpeg path"), Kind: KindText, Help: extend.T("Leave empty to use the bundled ffmpeg."), Wide: true},
				{Key: "ffprobePath", Label: extend.T("ffprobe path"), Kind: KindText, Help: extend.T("Leave empty to use the program Ether includes."), Wide: true},
				{Key: "tempDir", Label: extend.T("Temporary folder"), Kind: KindText, Help: extend.T("Where converted video is written while it plays. Empty uses the normal temporary folder."), Wide: true},
			}},
		}}},
		// TRAKT
		{ID: "trakt", ConfigKey: "trakt", Label: extend.Text{Source: "Trakt"}, Schema: Schema{Groups: []Group{
			{Title: extend.Text{Source: "Trakt"}, Description: extend.T("Sync watch history and lists with Trakt."), Fields: []Field{
				{Key: "enabled", Label: extend.T("Enabled"), Kind: KindBool},
				{Key: "clientId", Label: extend.T("Client ID"), Kind: KindText, Wide: true},
				{Key: "clientSecret", Label: extend.T("Client secret"), Kind: KindSecret},
			}},
		}}},
		// OPENSUBTITLES
		{ID: "openSubtitles", ConfigKey: "openSubtitles", Label: extend.Text{Source: "OpenSubtitles"}, Schema: Schema{Groups: []Group{
			{Title: extend.Text{Source: "OpenSubtitles"}, Description: extend.T("Fetch subtitles when a release doesn't include them."), Fields: []Field{
				{Key: "enabled", Label: extend.T("Enabled"), Kind: KindBool},
				{Key: "apiKey", Label: extend.T("API key"), Kind: KindSecret},
				{Key: "username", Label: extend.T("Username"), Kind: KindText},
				{Key: "password", Label: extend.T("Password"), Kind: KindSecret},
				{Key: "automaticPreferred", Label: extend.T("Download a subtitle automatically when playback starts"), Kind: KindBool, Help: extend.T("When a viewer's subtitle setting asks for subtitles and none in their language is available, Ether downloads one once the video starts playing. Uses one OpenSubtitles download per title per viewer, so standard accounts may run out sooner.")},
			}},
		}}},
		// UPDATES
		{ID: "updates", ConfigKey: "updates", Label: extend.T("Release channel"), Schema: Schema{Groups: []Group{
			{Title: extend.T("Release channel"), Fields: []Field{
				{Key: "enabled", Label: extend.T("Check for updates"), Kind: KindBool, Help: extend.T("Checks the signed release feed every few hours.")},
				{Key: "channel", Label: extend.T("Channel"), Kind: KindSelect, Options: []Option{{Value: "stable", Label: extend.T("Stable")}, {Value: "beta", Label: extend.T("Beta")}}},
				{Key: "autoUpdate", Label: extend.T("Install updates automatically"), Kind: KindBool, Help: extend.T("Installs during the maintenance window, once nobody is watching.")},
			}, More: []Field{
				{Key: "maintenanceStart", Label: extend.T("Maintenance window starts"), Kind: KindText, Help: extend.T("24-hour local time."), Placeholder: "03:00"},
				{Key: "maintenanceEnd", Label: extend.T("Maintenance window ends"), Kind: KindText, Help: extend.T("May wrap past midnight."), Placeholder: "06:00"},
				{Key: "feedUrl", Label: extend.T("Update feed"), Kind: KindText, Help: extend.T("Leave empty for the official feed."), Wide: true},
				{Key: "maxDrain", Label: extend.T("Wait for playback to finish"), Kind: KindDuration, Help: extend.T("How long a clustered node waits for current playback to end before installing. Leave empty to wait 30 minutes. A server on its own waits until nobody is watching.")},
			}},
		}}},
		// SPIKE
		{ID: "spike", ConfigKey: "spike", Label: extend.T("Test playback with a local file"), Schema: Schema{Groups: []Group{
			{Title: extend.T("Test playback with a local file"), Description: extend.T("Play a local file in Plex to see if the problem is the app or the stream."), Fields: []Field{
				{Key: "enabled", Label: extend.T("Serve test files"), Kind: KindBool},
				{Key: "localFiles", Label: extend.T("Test files"), Kind: KindList, Help: extend.T("Full paths to video files on this computer."), Placeholder: "/path/to/movie.mkv"},
			}},
		}}},
		// MARKERS
		{ID: "markers", ConfigKey: "markers", Label: extend.T("Chapters and skip markers"), Schema: Schema{Groups: []Group{
			{Title: extend.T("Chapters and skip markers"), Fields: []Field{
				{Key: "theIntroDb", Label: extend.T("Look up intro and credits markers"), Kind: KindBool, Help: extend.T("Use the public TheIntroDB lookup, cached per episode and runtime.")},
				{Key: "apiKey", Label: extend.T("Optional TheIntroDB key"), Kind: KindSecret},
			}},
		}}},
		// DEBRID
		{ID: "debrid", ConfigKey: "debrid", Label: extend.T("Torrent handling"), Schema: Schema{Groups: []Group{
			{Title: extend.T("Torrent handling"), Fields: []Field{
				{Key: "cachedOnly", Label: extend.T("Cached torrents only"), Kind: KindBool, Help: extend.T("Only use torrents your debrid service already holds. Nothing is downloaded and playback never waits. Turn off to have Ether send the best uncached release of a title to your first debrid service when someone presses Play; it plays once the download finishes (the app says so meanwhile).")},
				{Key: "removeUncached", Label: extend.T("Remove uncached torrents"), Kind: KindBool, Help: extend.T("Remove torrents Ether added that turned out not to be cached. Cached torrents stay on your account so the next play starts instantly and they are reused instead of re-added.")},
			}},
			{Title: extend.T("Advanced"), Description: extend.T("Leave at 0 for the built-in behaviour."), Fields: []Field{
				{Key: "chunkBytes", Label: extend.T("Largest range request"), Kind: KindBytes, Help: extend.T("Requests start at 512 KiB for a fast first byte and grow up to this size. 0 = 8 MiB; at most 64 MiB."), Unit: extend.Text{Source: "MiB"}},
				{Key: "readAheadChunks", Label: extend.T("Fixed read-ahead"), Kind: KindNumber, Help: extend.T("0 adapts read-ahead to the measured download speed. Set up to 32 chunks to fix the window; -1 turns read-ahead off."), Min: F(-1), Max: F(32), Step: F(1), Unit: extend.T("chunks")},
				{Key: "maxConnections", Label: extend.T("Connections per file"), Kind: KindNumber, Help: extend.T("Concurrent range requests per open file. 0 = 4; at most 16."), Min: F(0), Max: F(16), Step: F(1)},
			}},
		}}},
		// DOWNLOADS
		{ID: "downloads", ConfigKey: "downloads", Label: extend.T("Downloads"), Schema: Schema{Groups: []Group{
			{Title: extend.T("Downloads"), Description: extend.T("Lets Plex apps save titles for offline viewing. Ether remuxes each one on demand."), Fields: []Field{
				{Key: "enabled", Label: extend.T("Allow downloads"), Kind: KindBool},
				{Key: "allowFriends", Label: extend.T("Friends can download"), Kind: KindBool, Help: extend.T("Off lets only you download. A viewer's access policy can still allow or deny it.")},
			}, More: []Field{
				{Key: "maxHeight", Label: extend.T("Highest resolution"), Kind: KindNumber, Help: extend.T("Downloads are capped at this resolution. 0 uses 1080p."), Min: F(0), Step: F(1), Unit: extend.Text{Source: "p"}},
				{Key: "maxConcurrent", Label: extend.T("Downloads at once"), Kind: KindNumber, Help: extend.T("Across the whole server. 0 uses 2."), Min: F(0), Step: F(1)},
				{Key: "maxConcurrentPerUser", Label: extend.T("Downloads at once per viewer"), Kind: KindNumber, Help: extend.T("0 uses 1."), Min: F(0), Step: F(1)},
				{Key: "diskBudgetBytes", Label: extend.T("Temporary disk space"), Kind: KindBytes, Help: extend.T("Maximum space for prepared downloads. Set to 0 to use the default limit of 20 GiB."), Unit: extend.Text{Source: "GiB"}},
				{Key: "expiry", Label: extend.T("Keep prepared files for"), Kind: KindDuration, Help: extend.T("A finished download is deleted after this long if the app hasn't fetched it.")},
			}},
		}}},
		// PERSONALIZE
		{ID: "personalize", ConfigKey: "personalize", Label: extend.T("Personalization"), Schema: Schema{Groups: []Group{
			{Title: extend.T("Personalization"), Description: extend.T("Extras for each viewer. Turn any off for the plain layout."), Fields: []Field{
				{Key: "postPlay", Label: extend.T("Personal “up next” picks"), Kind: KindBool, Help: extend.T("After a movie or finale, suggest what this viewer is most likely to watch next.")},
				{Key: "postPlayPrebuffer", Label: extend.T("Pre-buffer the top pick"), Kind: KindBool, Help: extend.T("Warm the first suggestion in the last minutes so it starts instantly.")},
				{Key: "microReasons", Label: extend.T("Match and finish time on details"), Kind: KindBool, Help: extend.T("Show “96% Match · Ends at 10:45 PM” on a title's detail page.")},
				{Key: "sessionIntent", Label: extend.T("Adapt Home to tonight's browsing"), Kind: KindBool, Help: extend.T("Titles a viewer opened but didn't play steer the rows they see next.")},
			}, More: []Field{
				{Key: "clockFormat", Label: extend.T("Finish time format"), Kind: KindSelect, Options: []Option{{Value: "", Label: extend.T("From the language")}, {Value: "12h", Label: extend.T("12-hour")}, {Value: "24h", Label: extend.T("24-hour")}}},
			}},
		}}},
		// EXTRAS
		{ID: "extras", ConfigKey: "extras", Label: extend.T("Trailers & extras"), Schema: Schema{Groups: []Group{
			{Title: extend.T("Trailers & extras"), Description: extend.T("Play a trailer before a movie or episode."), Fields: []Field{
				{Key: "enabled", Label: extend.T("Trailers and Cinema Trailers"), Kind: KindBool, Help: extend.T("Offer trailers in apps.")},
			}, More: []Field{
				{Key: "resolverCommand", Label: extend.T("Resolver command"), Kind: KindText, Help: extend.T("Command that turns a trailer page into a stream."), Placeholder: "yt-dlp", Wide: true},
				{Key: "resolverArgs", Label: extend.T("Arguments"), Kind: KindList, Help: extend.T("Leave empty for the yt-dlp default. {url}, {site}, {key} and {id} are filled in; no shell is used."), Placeholder: "-g, -f, b[ext=mp4]/b, --no-playlist, {url}"},
				{Key: "resolverTimeout", Label: extend.T("Give up after"), Kind: KindDuration},
				{Key: "prewarmBytes", Label: extend.T("Pre-buffer the feature"), Kind: KindBytes, Help: extend.T("Buffered while the trailer plays, 32–64 MiB. 0 uses 48 MiB."), Unit: extend.Text{Source: "MiB"}},
			}},
		}}},
		// clusterServerGroups()
		{ID: "serverCluster", ConfigKey: "server", Label: extend.T("What viewers see"), Schema: Schema{Groups: []Group{
			{ID: "identity", Title: extend.T("What viewers see"), Description: extend.T("The name apps show for this server."), Fields: []Field{
				{Key: "friendlyName", Label: extend.T("Server name"), Kind: KindText, Help: extend.T("How the server appears in apps.")},
			}},
			{ID: "connect", Title: extend.T("How apps connect"), Description: extend.T("The address apps use to reach this computer."), Fields: []Field{}, More: []Field{
				{Key: "webFrameOrigins", Label: extend.T("Sites that may embed this page"), Kind: KindList, Help: extend.T("Exact addresses allowed to frame the owner page. Empty means nowhere."), Placeholder: "https://home.example.com"},
			}},
			{ID: "streams", Title: extend.T("Who can watch at once"), Description: extend.T("Leave these at the defaults unless too many people are watching at the same time."), Fields: []Field{}, More: []Field{
				{Key: "defaultStreamLimit", Label: extend.T("Streams per viewer"), Kind: KindNumber, Help: extend.T("Default simultaneous streams for each viewer. 0 = unlimited."), Min: F(0), Step: F(1)},
				{Key: "maxStreams", Label: extend.T("Server-wide limit"), Kind: KindNumber, Help: extend.T("0 derives the limit from your Usenet connections."), Min: F(0), Step: F(1)},
			}},
			{ID: "node", Title: extend.T("This computer"), Description: extend.T("These apply only to this computer. Every other setting is shared by the cluster."), Fields: []Field{
				{Key: "port", Label: extend.T("Port"), Kind: KindNumber, Help: extend.T("Plex apps expect 32400. A restart is needed after changing it."), Min: F(1), Max: F(65535), Step: F(1)},
			}, More: []Field{
				{Key: "bindAddress", Label: extend.T("Listen address"), Kind: KindText, Help: extend.T("0.0.0.0 listens on every network interface.")},
				{Key: "machineIdentifier", Label: extend.T("Machine identifier"), Kind: KindReadonly, Help: extend.T("Generated on first run; never changes.")},
				{Key: "trustedProxyCIDRs", Label: extend.T("Trusted proxy networks"), Kind: KindList, Help: extend.T("Networks whose forwarding headers Ether trusts. Empty trusts none."), Placeholder: "10.0.0.0/8"},
				{Key: "logLevel", Label: extend.T("Log level"), Kind: KindSelect, Options: []Option{{Value: "debug", Label: extend.T("Debug")}, {Value: "info", Label: extend.T("Info")}, {Value: "warn", Label: extend.T("Warnings")}, {Value: "error", Label: extend.T("Errors only")}}},
				{Key: "clusterWeight", Label: extend.T("Node weight"), Kind: KindNumber, Help: extend.T("This computer's share of each provider's connections and request limits, relative to the others. 2 gets twice the share of a computer at 1; 0 means 1."), Min: F(0), Max: F(100), Step: F(1)},
			}},
		}}},
		// QUALITY_*
	}
}

// BuiltinDescriptors wraps Builtins as settingsSchema descriptors ready for
// extend.Registry.Register.
func BuiltinDescriptors() ([]extend.Descriptor, error) {
	builtins := Builtins()
	ds := make([]extend.Descriptor, 0, len(builtins))
	for i, b := range builtins {
		if err := ValidateSchema(b.Schema, false); err != nil {
			return nil, fmt.Errorf("settings schema %s: %w", b.ID, err)
		}
		ds = append(ds, extend.Descriptor{
			Kind:  extend.KindSettingsSchema,
			ID:    b.ID,
			Owner: extend.Builtin,
			Label: b.Label,
			Order: i,
		}.WithMeta(b.Schema.Meta(b.ConfigKey)))
	}
	return ds, nil
}
