# Using Ether

Open <http://localhost:32400/ether/> on the server to manage Ether. Use your
chosen media apps to browse and watch, subject to the support notes below.
[Ether's multi-app support](apps.md) is the shared server containing
the Plex, Jellyfin, Emby and Silo frontends.
If Ether uses another port, such as
32450 alongside an existing Plex server, use that port instead.

Start here: [libraries and the catalogue](#libraries-catalogue-and-home),
[put your watchlist on Home](#put-your-watchlist-on-home), or
[choose Home rows](#choose-home-rows).

## Jellyfin and Emby apps

Jellyfin, Emby and Silo support ships in 0.3.0 without per-app certification.
Every app below is marked `waived` for 0.3.0: Swiftfin, Infuse and Silo passed
Apple TV checks for browsing, direct play, resume and sign-in, and the others
have not been device-tested yet. The reason for each waiver is in
`docs/certification/matrix.json`. Direct, remux, and transcode are separate
results. A release tag does not publish until this matrix is reviewed for that
minor (`scripts/certification-gate`). `unsupported` means the review is finished
and the app cannot work with Ether.

Downloads: not supported.

<!-- certification-matrix:jellyfin -->
| Client | Version | Browse | Direct | Remux | Transcode | Resume | Watched | Skip intro | Subtitles | Live TV | Quick Connect |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| Jellyfin for Android TV | Not tested | waived | waived | waived | waived | waived | waived | waived | waived | waived | waived |
| Swiftfin | tvOS, Nov 2026 | waived | waived | waived | waived | waived | waived | waived | waived | waived | waived |
| Findroid | Not tested | waived | waived | waived | waived | waived | waived | waived | waived | waived | waived |
| Streamyfin | Not tested | waived | waived | waived | waived | waived | waived | waived | waived | waived | waived |
| Infuse | 8.5.9 | waived | waived | waived | waived | waived | waived | waived | waived | waived | waived |
| JellyCon | Not tested | waived | waived | waived | waived | waived | waived | waived | waived | waived | waived |
| Jellyfin for Roku | Not tested | waived | waived | waived | waived | waived | waived | waived | waived | waived | waived |
<!-- /certification-matrix:jellyfin -->

<!-- certification-matrix:emby -->
| Client | Version | Browse | Direct | Remux | Transcode | Resume | Watched | Skip intro | Subtitles | Live TV | Quick Connect |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| Emby Web (externally hosted) | Not tested | waived | waived | waived | waived | waived | waived | waived | waived | waived | waived |
| Emby for Android TV | Not tested | waived | waived | waived | waived | waived | waived | waived | waived | waived | waived |
| Emby for TV (Apple TV, 2.x web-shell) | 2.0.9 | unsupported | unsupported | unsupported | unsupported | unsupported | unsupported | unsupported | unsupported | unsupported | unsupported |
<!-- /certification-matrix:emby -->

Jellyfin for Android phones and Jellyfin Desktop are unsupported: they wrap
Jellyfin Web, which Ether does not ship. Ether's `/web` page gives connection
instructions; it is not a browser media player.
Ether does not bundle Emby Web; the externally hosted client has not yet been
certified against Ether. Emby for Android TV is in the matrix and is not
certified yet. Emby for TV 2.0.9 on Apple TV is unsupported: it
requests server-hosted Emby Web assets, which Ether does not ship, and reports
that the server needs updating. For Apple TV, try Infuse or Swiftfin with
Ether; their feature support remains subject to the test matrix above.

Emby's own app restrictions still apply. Its [Premiere feature matrix](https://support.emby.media/support/articles/Premiere-Feature-Matrix.html)
lists full playback in the iOS and Android mobile apps as requiring an App
Unlock or Premiere; Windows, Linux, macOS and game-console apps require
Premiere for full playback. It lists free full playback for Roku, Apple TV and
LG/Samsung TVs, and up to five TV devices for Android TV or Fire TV; Live TV
requires Premiere. Availability and device limits can change, so consult Emby's
matrix. Ether does not provide or emulate an Emby Premiere entitlement.

## Silo apps

Silo support is in development. Ether's Silo surface uses port 8090 by default;
if that port is occupied, check **Server › General › How apps connect** for the
actual listening address.
Add that address in the Silo app and sign in with an Ether viewer account, or
approve the device's code in the viewer portal. A dash means the feature has
not yet been verified on the named client. Direct, remux, and transcode are
separate results. The release check includes this table.
Settings follow the pinned contract through revision 24.
A PQ video variant with unknown HDR10+ metadata stays available as HDR10 and
does not claim HDR10+. SDR and HLG variants remain available. For media with
an unknown transfer function, an omitted HDR10+ boolean means HDR10+ has not
been positively identified; the media remains playable.
Watch details include marker ranges only when the response has one probed file
those ranges fit. Every other file reports an empty marker set.
For an unresolved version, Silo's required size, duration and bitrate use zero
and required codec, container and resolution strings are empty. Playback resolves
the source and reports the known values when it starts.
Last-played codec, resolution and HDR describe the last recorded playback
sample and can briefly lag if storing a newer sample fails. The last-played
file identifier is omitted until it can be matched to an authorized version.
Native certification also requires verifying that Ether's stream request
headers reach direct playback and HLS playlists and segments, including after a
seek; this has not yet been verified on the physical devices.

<!-- certification-matrix:silo -->
| Client | Version | Browse | Direct | Remux | Transcode | Resume | Watched | Subtitles | Device pairing |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| Silo for Apple TV, iOS and macOS | tvOS, Nov 2026 | waived | waived | waived | waived | waived | waived | waived | waived |
| Silo for Android and Android TV | Not tested | waived | waived | waived | waived | waived | waived | waived | waived |
<!-- /certification-matrix:silo -->

## Apps and client support

Enable an app on **Apps**, then edit its configuration in its enabled app tab.
Ports are set only under **Server › General › How apps connect**, which lists
each enabled app's port, local address and remote address. Use those addresses
rather than assuming a default port.
Press ⌘K to jump to any page or individual setting; the console opens **More
options** if the setting is inside one and highlights the row.
Client names below identify intended connection methods; native-device
certification and exact-build conformance remain pending.

| Surface | Intended clients and connection | Limits |
| --- | --- | --- |
| Jellyfin | Android TV, Swiftfin, Findroid, Streamyfin, Infuse, JellyCon, Roku; server URL and viewer sign-in | Native feature matrix above is pending; Android phone and Desktop web shells are unsupported |
| Emby | Native clients using the server URL and Ether viewer account | No bundled Web; Apple TV Emby for TV 2.x web shell is unsupported; app licensing still applies |
| Silo | Apple and Android native apps; server URL and viewer sign-in or pairing | Versioned device acceptance pending; see Silo limits above |
| Stremio | Install the viewer-specific addon from the viewer portal in Stremio | Addon API only; no hosted Stremio web player |
| Files (WebDAV) | Infuse, VLC or Kodi with the viewer’s WebDAV credentials | Read-only virtual files; client playback and resume behavior require verification |
| M3U/XMLTV | Channels DVR or TiviMate with viewer-specific playlist and guide links | Live TV export; treat connection links as secrets |
| DLNA | LAN televisions and players with a ContentDirectory browser | Uses the owner-selected guest viewer’s policy; no per-device login; device compatibility pending |

## Set up

The setup wizard starts with app selection and owner-account setup. If you
select Plex, sign in with Plex and claim the server; otherwise, create a local
Ether owner account. Jellyfin, Emby and Silo do not require a Plex account;
use Ether viewer accounts and the connection instructions on the Apps page.
Then choose where Ether streams from: a
debrid account (Real-Debrid, TorBox, AllDebrid, Premiumize, Debrid-Link or
Offcloud), a Usenet provider, or both, each tested before you continue. Next add at least one
indexer: a Stremio stream addon's install link or a Torznab URL for debrid,
and a Newznab URL for Usenet. Finally choose a cache folder and budget.
Out of the box there is no server quality cap, at home or away: each device
plays the best release it can show (phones up to 1080p, TVs and streaming boxes
up to 4K), and the app's own quality setting still applies. To limit quality,
open **Quality › Profiles**. Edit a profile and select **Away** under **Default for** to cap people watching away
from home (for example with the 1080p profile), or set the shared **Remote bitrate
limit** in Mbps (0 means no limit). To limit one device, app or viewer,
add a rule under **Quality › Rules**. A rule wins over the Away profile.
**Check quality**, its own tab under **Quality**, shows the quality a viewer gets
in an app, at home or away, and which rule decided.
Ether supplies no media, provider accounts or indexer access.

## Stream torrents through debrid

1. Under **Sources › Indexers**, add a torrent source: a torrent site (see
   [Sites](#sites)),
   or a Torznab URL with its protocol set to **Torrent (Torznab)**. If you
   already run Prowlarr you can import its indexers instead, but you don't
   need it.
2. Under **Sources › Debrid**, add your debrid account with its API key and use
   **Test** to check it. Lower priority numbers are tried first.

Without an enabled debrid account, torrent results are ignored. Ether streams
the file over HTTPS from your debrid service; it never joins a torrent swarm and
never downloads a whole file.

- **Cached torrents only** (on by default): a torrent is used only if your
  service can serve it now, so playback never waits for a download. When no
  release of a title is cached, the player says so and suggests turning this
  off. Turned off, pressing Play on such a title sends its best release to your
  first debrid service that takes it (one download per title). The player says
  it is downloading, and a later Play starts it once the download finishes. If
  the service refuses the download or the account is at its active-download
  limit, the player says that instead. The change applies at once, without a
  restart.
- **Remove uncached torrents** (on by default): torrents Ether added that turn
  out not to be cached are removed again. Cached torrents stay on your account,
  so the next play of that title starts straight away.
- **Season packs:** for an episode, Ether streams that episode's file from a
  torrent season pack. Pins, blocks and health checks apply to the episode.

Torrents whose video is inside RAR archives are skipped. A result that only
offers a `.torrent` link costs one indexer download to read its hash.

### Stremio addons as sources

Under **Sources › Indexers**, choose **Add indexer › Stremio addon URL…**
and supply the configured install link (`https://…/manifest.json` or
`stremio://…`). Choosing a torrent protocol on a Torznab indexer does not
make it a Stremio addon.

Ether asks an addon for a title by IMDb ID when the addon accepts them, else
by TMDB ID. Addons that look anime up only by Kitsu, MyAnimeList, AniList or
AniDB ID are asked by that ID for anime: the first time such an addon needs it,
Ether downloads the community [anime-lists](https://github.com/Fribb/anime-lists)
mapping (about 6 MB, refreshed daily) and uses an ID only when the mapping
names exactly one entry for the season and episode. Other titles, unmapped
seasons, whole-season requests and season-zero specials are not asked. **Test**
checks the addon's manifest and advertised movie/series stream support; a
successful test does not prove that a particular title returns usable releases.

A result needs a release name and one of these, tried in order:

- **A torrent hash** (explicit hashes, magnets and supported hash-bearing
  resolver URLs): Ether checks and streams it through your enabled debrid
  accounts. Without a debrid account, a torrent the addon also linked plays
  through that link instead.
- **An NZB link** (`nzbUrl`): a Usenet release, downloaded like any NZB and
  streamed from your Usenet providers. It is ignored until a Usenet provider is
  enabled.
- **An HTTP stream link**, including an addon's own resolver or playback URL:
  Ether reads it directly with range requests, without needing a debrid account
  of its own. Before playing, one small request checks that the link serves the
  whole file. An error page, a short "downloading" clip or a server without
  range requests is skipped for now and tried again later. With **Cached torrents
  only** on, links the addon labels only as uncached are left out.

  Opening an addon's link usually makes the addon fetch that release (resolve
  it on its debrid account, or download and import an NZB), so Ether opens one
  link per Play: the next one only after the first fails, and none while it is
  still being prepared. **Concurrent checks** does not apply to these links.
  Detail pages, warm-ups, next-episode prefetch and availability checks never
  open them, so an addon-only title starts without a prepared opening.

External-player links and YouTube IDs have nothing Ether can stream.

HTTP and NZB links may point at the addon's own address, even on your network.
Any other address they lead to, including redirects, must be on the internet.
Request headers the addon asks for are sent only to the addon's own link. Links
are never shown in the owner UI or logs, because they can carry the addon's
credentials.

Use the configured install URL supplied by the addon. Addon settings and
credentials encoded in its URL path are retained; query-string configuration
is not. Ether's general indexer API-key field does not configure a Stremio
addon. Some addons require their own login or key, separate from the debrid
account configured under **Sources › Debrid**. Keep configured links private.

Ether checks torrent availability through your enabled debrid accounts. An
addon's cached labels are hints, not proof that your account can stream a file.
Some public instances also refuse server clients; use an instance that permits
them or a self-hosted instance.

Language flags in an addon's results (🇫🇷, 🇲🇽 for Latino Spanish, 🇧🇷 for
Brazilian Portuguese) and "Multi Audio" count as the release's audio
languages.

Self-hosted DMM hashlist indexers that speak Torznab, such as Zilean
(`http://HOST:8182/torznab`) or StremThru (`https://HOST/v0/torznab`), can be
added as Torznab URLs with protocol **Torrent (Torznab)**; Ether adds `/api`.

### Which source plays

Ether never trades quality for speed. Among releases of the same quality, it
puts the source that starts faster first. Set this under **Quality › Profiles ›
Which source goes first**. It applies to every profile:

- **Start fastest:** once both sources have enough plays, the one with
  the lower measured start time goes first. Until then, popular titles use
  cached debrid first. The measured times are shown beside the setting.
- **Usenet first** or **Debrid first:** that source goes first when releases
  are equal in quality.
- **Always Usenet** or **Always cached debrid:** that source goes first even
  when the other has a better release. Ether falls back to the other source
  when the first has nothing that plays.

The best release from the other source is checked at the same time, and both
remain available for failover. **Quality › Pinned** (when you find a title) and **History** show when
routing moved a release, and why.

## Sites

Ether can search sites itself, without a separate indexer manager. Each
site runs from a *definition*: a YAML file in the Cardigann format
that Prowlarr and Jackett also use, describing the site's search pages,
sign-in, settings and categories. **Ether ships no definitions.** Install only
definitions for sites you have the right to use, and follow those sites'
rules.

A definition is not searched by itself: it describes *how* to search a site.
The site is searched once you add it as an indexer, and it then appears in the
indexer list like any other indexer. One definition can back more than one
indexer, for example two accounts or two mirror addresses.

### Install definitions

Under **Sources › Indexers › Site definitions**, below the indexer list (the
source for definitions is under **More options** there):

- **Installed** counts your definitions and how many your indexers use.
  Definitions no indexer uses yet are listed under **Not added as an
  indexer**, each with **Add as indexer** and **Remove**. A definition an
  indexer uses is shown in that indexer's drawer instead, with where it came
  from and any warnings, so each site appears once on the page.
- **Upload definition** takes a `.yml` file or pasted YAML. Ether checks it
  before installing and lists anything it does not support (for example a
  captcha sign-in or an unknown filter) as a warning beside the definition.
  You can also upload from **Add indexer › Site…**, which continues straight
  to the new indexer's settings.
- **Definitions source** (optional) is a folder of definitions you trust: a
  GitHub folder URL (`https://github.com/<owner>/<repo>/tree/<branch>/<folder>`),
  the matching raw folder or GitHub contents API URL, or a URL that returns a
  plain list of definition files. Ether reads the list only when you add a
  site, and downloads a definition when you pick it.
- **Update definitions** re-downloads every definition that came from the
  source and installs the ones that changed. Uploaded definitions are left
  as they are. A definition that an indexer uses cannot be removed.

### Add a site

1. Choose **Add indexer › Site…**, search, and pick an installed definition,
   install one from your definitions source, or upload one. Sites you have
   already added are marked **Added**.
2. The drawer shows the settings the definition asks for, such as a username
   and password, a cookie, a passkey or a sort order. Passwords are stored
   encrypted and shown as `••••` afterwards; leave them to keep them.
3. **Site address** defaults to the definition's first address; alternative
   addresses are listed under it.
4. Set daily search and download budgets if the site limits them.
5. **Test** signs in and runs a sample search, then shows how many results it
   found and a few titles, or the site's error.

Sites appear in the same list as Newznab and Torznab indexers, with a
**Torrent site** or **Usenet site** badge, their address and their last test
result. They share
the same search order, daily budgets, result cache and failover. Torrent sites
stream through your debrid account; a definition that serves NZB files
streams from your Usenet providers.

### Sites behind a browser challenge

Some sites put a Cloudflare challenge in front of their pages. Ether cannot
pass it alone and reports that the site needs FlareSolverr. If you run
[FlareSolverr](https://github.com/FlareSolverr/FlareSolverr), enter its
address (for example `http://localhost:8191`) under **More options** on
**Sources › Indexers**. Ether
asks it to solve the challenge only when a site presents one, then reuses
the clearance it returns.

Sign-ins that need a captcha are not supported; if the definition offers
cookie sign-in, paste the cookie from a signed-in browser instead.

## Share with viewers

Use Plex library sharing to invite people or remove their access. They appear
in Ether's **Viewers** page after signing in. A viewer's page has the tabs
**Overview**, **Home**, **Taste**, **Recent activity**, **Audio & subtitles**,
**Limits**, **Sign-in & devices** and **Lists & alerts**. Use **Limits** for
content and playback limits. To cap one viewer's quality, add a rule under
**Quality › Rules**. Each viewer keeps their own history and recommendations.

## Libraries, catalogue and Home

Ether appears as its own server in Plex, with Movies and TV Shows libraries.
Rename those libraries under **Server › General** (Movies library name and TV
Shows library name); Plex's own library rename does not stick because Ether
generates the libraries. These libraries contain the titles exposed by Ether's
catalogue and visibility rules; they are not folders of downloaded media. A
catalogue entry does not guarantee that your configured sources have a playable
release.

Your existing Plex Media Server keeps its own libraries and files. Ether does
not merge or replace them. Plex Home can show rows from multiple pinned
libraries, so check the server name beneath a row and pin the Ether libraries
you want to browse in your Plex app.

Use **Catalog › Settings › Rules** to control what Ether exposes, and
**Catalog › Settings › General** to configure the catalogue. **Collections**
groups titles within those libraries. **Home** arranges recommendation rows;
changing a Home row does not delete a library or remove a title from the
catalogue. Plex's own Watchlist and other Plex-provided navigation remain
client features.

In **Library › Catalog**, use **Add to Watchlist** on a title card or in its
details drawer. The watchlist viewer picker starts with your owner profile;
select up to ten household viewers to update them together. A card shows the
number of selected watchlists containing the title. When only some contain it,
the add action adds it for all selected viewers; when all contain it, the
remove action removes it for all. These titles use Ether's watchlist monitoring,
including the Watchlist and Now available rows and availability alerts when
those features are enabled in Catalog settings.

Ether also sends the change to an enabled, linked Trakt account. If that write
fails, the console reports it and the change remains in Ether; update Trakt
separately. Plex watchlist changes stay in Ether: Ether does not write to
plex.tv, and a subsequent Plex watchlist sync can restore an item removed in
Ether. Remove it in Plex as well if you do not want it restored.

## Organise your Home

Movies, TV and Home put populated **Continue Watching** first, followed by
**Top 10 for You**, **New & Noteworthy**,
**Trending This Week**, and a **Because You Watched** or **Fans Also Loved**
row when enough eligible titles are available. Home mixes movies and shows in its leading
rows; themed and type-specific rows follow. Movies and TV pages keep their own
types. New viewers get today's Top 10 until there is enough viewing history.

These are the defaults for every install. Explicit saved layouts take precedence.
Use **Home** in Ether's sidebar to choose the server's default rows and ordering. Select a
viewer to preview their Home and make personal exceptions. **Collections** lets
you build and edit themed lists. Recommendations improve as viewers watch and
rate titles.

A collection can mix movies and TV shows in one Home row. In its editor, choose
**Contains → Both**, add sources or individual titles, and enable its Home row
or place it in Home Designer. Each item opens its own movie or show page.
Viewer access and age limits still apply; Movies and TV library pages keep their
respective types.

To follow a public TMDB user list, add the **TMDB user list** source and paste
its list URL or numeric ID. Choose **Source** sort to keep the list order. Ether
refreshes it on the collection’s chosen schedule and keeps up to 5,000 items.
Private or missing lists cannot be read. Normal release-date and visibility
rules still apply to titles in the list.

Movies appear from their theatrical release date. Shows, seasons and episodes
appear from their air dates. Items with no known release date stay hidden.
Under **Upcoming content** in Catalog settings, **Show upcoming movies** and
**Show upcoming episodes** list unreleased titles early; both are off by default.

### Choose Home rows

1. Open **Home** in Ether. Choose **Everyone** for the default layout, or the
   same viewer you use in Plex for personal overrides.
2. Select the page and preview device you want to configure. Device and time
   conditions can make phone and TV previews differ.
3. In **Rows**, find the row and choose **Always**, **Eligible** or **Off**.
   **Always** includes an available row when it has eligible titles;
   **Eligible** lets personalization choose it; **Off** hides it. Always
   does not create missing titles or override viewer access restrictions.
4. To keep a row in a specific position, expand its controls and set
   **Anchor to slot**. Use **Layout** for the overall page arrangement.
5. Select **Save layout**, then reload Home in your Plex app. Compare it with
   Ether's preview for the same viewer, page and device.

A personalized row can change as its eligible titles, viewing history or
scheduled conditions change. If it repeatedly disappears and returns without
those changes, record the row name, Ether version, Plex app/device and whether
the preview does the same. A screenshot of a gap alone cannot identify the
cause.

### Put your watchlist on Home

1. Add wanted titles to your Plex Watchlist.
2. In Ether, open **Catalog › Settings › General › Watchlists**, enable
   **Watchlist monitoring** and **Plex watchlists**, then save. Opening-minute
   warm-up for a title that just became playable is **Cache › Preloading ›
   Warm up new arrivals**.
3. Open **Viewers › your viewer › Watchlist**. **Sync viewer** imports the
   list; **Check availability** checks for playable releases. These actions
   are separate: importing a title does not prove it can stream.
4. Return to **Home** and inspect the same viewer's preview. The watchlist
   rows are **Waiting for a stream** and **Now available**. Check that your
   saved row policies do not hide them, and reload Plex Home.

A watchlisted show becomes available through its first episode. After that,
Ether keeps looking for each new regular episode as it airs (hourly for the
first two days, then on the normal check interval). When a playable release
turns up it sends one **Watchlist title available** alert for that episode and
warms its opening, however many viewers follow the show. Specials and
unaired episodes never trigger an alert.

Plex watchlist access covers the owner and supported Plex Home members.
Sharing a server with a friend does not grant access to that friend's Plex
Watchlist. Trakt watchlist monitoring is a separate option and requires that
viewer's linked account.

For a hand-picked list independent of watchlist monitoring, create a
**Collection**, add titles and enable its **Home row**. It becomes a candidate
row; use Home's **Rows** controls if you want to keep it visible whenever its
eligible contents are available. The collection's **Collections tab** setting
is separate from its Home row.

## Edit settings

Pages use subtabs to group related settings. Use an **Edit** or **Create** action
to open a drawer, make changes and save. Start with the settings relevant to
what you want to change:

| Task | Where to look |
| --- | --- |
| Connect providers and indexers | **Sources** |
| Inspect a failed source search | **Quality › History**, then open the attempt |
| Inspect observed playback | **Activity › History** |
| Change one viewer's track preferences | **Viewers › viewer › Audio & subtitles** |
| Change household audio and subtitles | **Playback › Audio & subtitles** |
| Arrange rows in Plex | **Home** |
| Set up poster providers | **Integrations** |
| Pick the poster source | **Metadata › Artwork** |

Activity history has one row per viewing. Opens of the same title by the
same viewer and device within five minutes of each other (probes, a decision
and start, a reopen after a seek or a stop and replay) continue one row. A row
appears once media is delivered; an attempt that fails first is listed with
its error and does not count as a play.

A playback attempt can fail before Activity history begins. Quality history
records source-selection results, including failures; open its details for
candidate rejection reasons and effective playback limits. Neither a
catalogue poster nor an indexer's “cached” label guarantees a playable source.

## Versions, trailers, downloads and previews

Detail pages can list several versions of a title, such as 4K, a
high-compatibility 1080p release or an extended cut. Right after the automatic
choice, other releases of the same cut, resolution and HDR type are listed,
named by quality, audio, size and release group, so you can switch if one
fails. **Quality › Profiles › Version menu › Same-quality versions to offer** sets
how many (0 to 3; 1 by default, 0 turns the extra versions off). Choose one with Plex's **Play Version**; otherwise Ether picks the
best version for the device. After a restart or candidate-cache expiry, an
admitted detail-page warm-up that reuses your remembered release also requests a
background search for versions. Your remembered source does not wait or change;
a later page load can show alternatives after that search succeeds. Refills
share an episode-specific search key across viewers and clients, allow at most
two searches at once with a 45-second ceiling, and use the existing indexer
limits and cache lifetimes. Full capacity skips the refill rather than queuing
it; there is no polling or automatic retry. Disabled or refused warming does
not request one, and empty or failed searches retain the current single-version
fallback. Version lookup itself remains cache-only.

Search also accepts descriptions such as "90s thrillers under 100 mins" and offers a
matching collection; rate it to keep it as a personal collection.

These settings have no dedicated pages yet. The owner UI lists them alongside
the server's general settings; they are stored in the server's configuration
file under these names:

- `downloads`: offline downloads, on for the owner. Set `allowFriends` to let
  viewers download.
- `extras`: trailers and Cinema Trailers, off until `enabled` is set. Configure
  `resolverCommand`, a yt-dlp-compatible tool you install.
- `personalize`: post-play ranking, detail-page headers and session-based Home
  changes.
- `transcode`: `fastStart`, `abrLadder`, `abrLadderUnderCopy`, `vbvReservoir`
  and `spikeGovernor` control adaptive streaming. `abrLadderUnderCopy`
  (default on) keeps soft encode rungs under Direct Stream / video-copy when
  `abrLadder` is on; set it false to advertise only the copy primary.
- `usenet.minIdle`: warm provider connections kept ready for playback.

Preview thumbnails appear while a title plays and fill in as playback
progresses.

## Pre-rolls

A pre-roll is a short video of your own, such as a cinema sting or a seasonal
greeting, that plays before a movie or an episode. Add them in the owner
console's **Pre-rolls** page: upload a video, or give the full path of a video
file on the server (Ether only reads it). Ether converts each clip in the
background to an MP4 that every app can play directly (H.264 and stereo AAC, at
most 1080p) and makes a thumbnail. Clips longer than 3 minutes are refused.
One file can be up to 500 MB, and converted pre-rolls share a 5 GB folder,
`prerolls` in the data folder.

At most one pre-roll plays before a title, and never when a viewer resumes
part-way through. Each pre-roll has rules, and all of them must match:

- **Movies and episodes**: which kinds it plays before.
- **First episode only**: no pre-roll when the next episode plays by itself.
  Its **Binge gap** also skips the pre-roll when the same viewer finished or
  stopped something less than that many minutes ago. With **First episode
  only** off, the pre-roll plays before every episode.
- **Genres**, **Excluded genres** and **Collections**: checked against the
  title (an episode uses its show).
- **Highest rating**: plays only before titles rated at or below it.
- **Dates**: a yearly window such as 10-01 to 10-31; 12-15 to 01-05 spans the
  new year.
- **Viewers**: everyone, only some viewers, or everyone except some.
- **At home or away** and **Devices** (TV, mobile, desktop, web).
- **How often**: every time, once a day, once every few hours, or once per
  title for each viewer.

When several pre-rolls match, the highest **Priority** wins (a tie is chosen at
random), or choose **Shuffle** to pick at random among all matches. With Plex
Cinema Trailers on, **Order** puts the pre-roll before or after the trailers.
The **Test** panel shows, for a viewer, a title and a device, which pre-roll
would play and why each other one would not. Each pre-roll lists its recent
plays; a play counts when the video actually starts.

To bring pre-rolls over from Plex Media Server, paste its
`CinemaTrailersPrerollID` value (Settings › Extras › Cinema Trailers pre-roll
video) into **Import from Plex**. Paths separated by commas share a priority,
so one of them plays at random, as in Plex. Paths separated by semicolons play
in order in Plex; Ether plays one pre-roll per title, so the first group gets
the highest priority.

Pre-rolls play in every Plex app, in Jellyfin Web and Jellyfin for Android TV,
and in the Emby apps. Infuse, Swiftfin, Silo, Stremio and DLNA players do not
ask the server for intros, so they start the title without a pre-roll. In a
cluster, a pre-roll plays only on the node that converted it.

## Subtitles from OpenSubtitles

With OpenSubtitles enabled, Ether adds subtitles in each viewer's preferred
languages (**Audio & subtitles**) when a release doesn't include them. They
appear in the Plex app's subtitle menu as external tracks, such as "English (SRT
External)", and subtitle mode **Always** selects them.

- Ether downloads them while a title's page is open and when playback starts.
  The first time a title is opened they can take a few seconds to appear; reopen
  the page or start playback to see them.
- **Search** in the Plex subtitle menu lists OpenSubtitles results for a
  language. Choosing one downloads it, adds it to the title for every viewer and
  selects it for you.
- Every download counts against the OpenSubtitles daily quota. Signing in with
  an OpenSubtitles account raises it. Ether limits background downloads so
  browsing doesn't use it up.
- When a stream is converted, text subtitles are sent alongside the video
  rather than burned into it.

## Release languages and anime numbering

A quality profile's **Languages** accept preferred ISO language codes, such as
`fr`, `es-419` or `pt-BR`. Ether reads audio languages from release names and
addon flags:

- **Regional variants:** `LATINO`, `ES-MX` and `es-419` are Latin American
  Spanish; `CASTELLANO` and `ES-ES` identify Spanish from Spain. `PT-BR`,
  `DUBLADO` and `BRAZILIAN` identify Brazilian Portuguese; `PT-PT` identifies
  Portuguese from Portugal. Plain `SPANISH` and `PORTUGUESE` leave the region
  unspecified. Regional preferences (`es-ES`, `es-419`, `pt-PT`, `pt-BR`) rank
  their own variant above other or unspecified variants. Bare `es` and `pt`
  accept all regions equally. Imported custom formats retain Starr's separate
  Latino/Brazilian language categories.
- **Subtitle tags:** `VOSTFR`, `VOSE`, `SUBFRENCH` and `LEGENDADO` mean the
  original audio with subtitles in that language. They never count as that
  language's audio, so a French dub still ranks above a VOSTFR release for a
  French preference. They add a small bonus when the subtitle language is
  preferred.
- `MULTi`, `Dual-Audio` and "Multi Audio" mean several audio tracks; `Multi-Subs`
  does not.

Anime releases often number episodes from the start of the series
(`[Group] Show - 1071 (1080p)`). Ether counts each episode's number from the
seasons in your catalogue and accepts either numbering. It also searches
indexers by that number for anime, meaning TMDB's "anime" keyword or
Japanese-language animation. When an earlier season's episode count is
unknown, only the season-and-episode numbering is used.

## Quality profiles for movies and shows

Every quality profile ranks releases for one kind of title, **Movies** or
**Shows**, as in Radarr and Sonarr. The **Movies | Shows** picker on
**Quality › Profiles** shows that kind's profiles with their **At home** and
**Away** defaults, and **Add profile** creates one of the kind shown. A movie
profile and a show profile can share a name, such as *1080p*, and still have
different settings. A profile's **Applies to** in its editor offers **Movies**
or **Shows**; moving a profile to the other kind takes the rules that used it
along, and a default it can no longer serve moves to another profile.

A rule under **Quality › Rules** applies to movies, to shows, or to both. A
rule for both names a profile that exists for movies and for shows; movies
then use the movie profile of that name and shows the show one.

A new install starts with 4K, 1080p and 720p profiles for movies and the same
three for shows, with 4K as the At home default for each. Upgrading from
v0.3.4 or earlier: each profile there served movies and shows alike. Ether
splits it once, when the server starts, into a movie profile and a show
profile with the same name and settings. Rules and defaults keep the names
they had, so every viewer, app and title gets exactly the quality it got
before, and remembered releases and resume points stay valid. Renaming or
removing one of the pair changes only that kind: a rule for both kinds that
named it becomes one rule for each kind, so the other kind keeps its
profile.

## Import a quality profile

Coming from Radarr, Sonarr, or another guide? Open **Quality › Profiles** and
select **Movies** or **Shows**, choose **Add profile**, then **Import a
preset quality profile**. Movies import from Radarr's guide and shows from
Sonarr's. Choose a profile for that media, such as
*HD Bluray + WEB* or *[SQP] SQP-1 (2160p)*. Ether supports **TRaSH Guides**,
**Profilarr (Dictionarry)**, and pasted profile JSON. It shows what it will
add before saving:

- the qualities the profile allows, its resolution range, source order and
  minimum custom format score;
- every custom format the profile scores, with its score from the profile's
  TRaSH score set: **Added** formats join **Custom scoring**, **Already in
  Custom scoring** ones are reused unchanged, and any Ether can't evaluate
  (indexer flags, for example) are listed but left out;
- what doesn't carry over. Upgrade and cutoff settings don't apply, because
  Ether picks the best release each time something plays. Qualities with no
  streaming equivalent, such as Raw-HD or SDTV, are skipped. Radarr's
  "Original" language rule isn't a hard filter in Ether.

Pasted JSON whose qualities name the other app is refused with a note to
switch kinds first: a Sonarr profile pasted under **Movies** is imported under
**Shows**. Rename the profile if you like, then choose **Add profile**; to
make it a default, edit it and choose **At home** or **Away** under **Default
for**. Your other profiles keep ranking releases exactly as before: they score
the newly added formats 0, as Radarr and Sonarr do for formats a profile
doesn't list.

Ether reads the profiles from the guide and keeps them for an hour. If the
guide is unreachable or its request limit is used up, switch to **Paste JSON**
and paste or upload a profile file. Custom formats Ether can't fetch are
marked **Not found**; paste them under **Custom scoring** and import the
profile again. The preview refuses a profile whose minimum format score is
higher than the formats it could import, because no release could ever satisfy
it.

### Keep imported profiles in sync

**Quality › Profiles › Keep imported profiles in sync** updates profiles
imported from TRaSH Guides or Profilarr. Turn it on with the switch in the
card's header, then choose how often to **Check for changes** (6 hours to 7
days). The card shows when the last sync ran. Until you import a profile, it
only says that it applies once you do. A sync re-fetches the
source profile and its default format groups and overwrites that profile's
quality definition and custom format scores exactly, so formats the guide adds
are added and formats it removes are removed. The shared custom format
definitions for that profile are updated too; definitions the sync owns and no
profile uses are removed, while formats you created by hand are left alone.
Profiles that are not imported, or that could not be fetched completely, are
not changed. **Sync now** runs the same update immediately, even while the
automatic sync is off, for owners who prefer to take guide changes when they
choose.

## Rank releases by custom score

A quality profile's **What matters most** list is checked from top to bottom;
the first step where two releases differ decides which plays first. **Custom
score** is one of those steps. It ranks the release with the higher total
score first: Ether's own quality points plus the profile's custom format
scores (imported from TRaSH Guides, Radarr or Sonarr, or entered under
**Custom scoring**) plus how well the release suits the app.

By default **Custom score** sits just above **Played fine recently**, so your
language, resolution, release type and HDR preferences come first, and a
release that already played well only wins among equally scored ones. Drag it
to change that. For example, put **Resolution** first and **Custom score**
second to play 4K whenever a 4K release exists and let your scores rank
everything else. Profiles saved before this step existed rank exactly as they
did.

**Edit scores** on the step opens the profile's custom scoring editor.
**Only play** and **Never play** filters still decide which releases are
allowed at all; the step only orders the ones that pass.

## Choose and fix releases

**Quality** explains and overrides which release plays for each title.

- **Pinned** starts with **Find a release**: find a movie, or a show then a
  season and episode, to list every release the indexers return. Each one
  shows its score, custom formats, size, age, indexer and Ether's verdict for
  the chosen quality profile, ranked exactly as playback ranks them, with the
  release that would play first marked. Choose **No profile limits** to see
  releases a profile would reject, or **Offline download** for the download
  profile. **Search again** forgets the cached results and any "unavailable"
  cool-off for the title. Each release's menu can pin it, block it with a
  reason, or check its health now.

  Below the search are the releases you pinned. A pin plays first for its title on
  every app, overriding quality-profile limits and title matching. Ether still
  skips a pin that is blocklisted, passworded, already tried during the current
  playback, a season pack for an episode, in a video codec the app cannot
  decode, or Dolby Vision without an HDR10 base on an app without Dolby Vision.
  A viewer's own **Play Version** choice wins over a pin, and offline downloads
  keep their own profile. When a pin fails its health check, Ether plays the
  next best release and the pin stays, showing why it failed.
- **Blocklist** lists releases Ether will not pick: ones that failed health
  checks or broke during playback, and ones you blocked. Filter by title or
  source; unblocking a release has it checked again the next time it is a
  candidate. Automatic entries expire 90 days after they last failed (a
  release still broken then fails one more check); releases you blocked stay
  blocked until you unblock them.
- **History** records each release decision: the winner, failovers, pin use
  and, for every candidate, why it won or lost. It keeps the newest 5,000
  decisions for up to 60 days.

Pins, blocks and **Search again** take effect on the next play, including for
titles a detail page has already prepared.

## Bring your library and history

Use **Maintenance › Backups & import › Import & export** to preview a Plex import. Map
each source viewer to the correct Ether viewer and review unmatched titles
before importing.

To download a snapshot, sign in as the owner and open **Maintenance › Backups & import ›
Backups** after setup is complete. Enter a backup password of at least 12
characters, then choose **Download**. The downloaded `.etherbackup` file is
encrypted. Ether does not keep this password; save it somewhere safe because
it is needed to restore the file.

To restore, choose **Restore** next to a snapshot, or **Choose backup file**
and enter the password a download was made with. Ether checks the backup,
shows what will change and who is watching, then restores it and restarts by
itself; the page follows along until your server is back. Your current data
is kept, so the report on the Backups page offers **Undo restore**. An older,
unencrypted `.tar.gz` backup requires an owner sign-in and explicit
confirmation. See [installation and restoration](install.md#backups) for
clusters and how restarts work.

Keep server-side snapshots and shared backup folders private: scheduled and
internal snapshots remain unencrypted on disk and include the credentials and
key needed to restore your server. Password protection applies to downloaded
`.etherbackup` files, not existing snapshots or older downloads.

## Volume when a source changes

For native Plex playback using segmented Matroska, Ether can match the level
of a replacement release to the one already playing. It compares a short,
corresponding audio passage only when there is enough published media ahead.
The passage must match both in waveform and in its loud/quiet pattern. Silence,
different languages or channel layouts, changing mixes, and unavailable samples
leave the audio unchanged. Analysis is optional and bounded; it never rejects a
playable replacement or runs on an unbuffered rescue.

A difference below 1.5 dB keeps audio copying. Larger differences use a fixed
adjustment, capped at 6 dB quieter or 3 dB louder, with peak limiting for boosts.
Only the audio is encoded for this adjustment; copied video stays copied. The
level does not continuously rise and fall with dialogue. Corrections are cached
for the session and do not stack across repeated replacements.

Viewer loudness normalization, night mode and dialogue processing take
precedence. This matching does not change direct play, Live TV, adaptive ladders,
or RTX enhancement sessions. It preserves passthrough for lossless audio, copied
E-AC-3 (which may carry Atmos), and HE-AAC rather than converting those formats
solely to change their volume. AAC-LC and AC-3 copy, and compatible audio that is
already being encoded, can be matched. It is a best-effort reduction in volume
jumps, not a guarantee that differently mastered releases sound identical.

## Live TV

Open **Live TV & IPTV** to create a virtual channel or import an M3U source.
Supply a matching XMLTV guide for programme schedules. Search the channel list
or filter to favourites; select a channel to change its settings.
Ether includes no channel sources by default.

A channel can have several stream sources in priority order. Enable the inputs
you want and put the preferred one first. If it fails during tuning, Ether tries
a backup within the same startup budget. Failed inputs cool down briefly before
being retried. The channel keeps its number, favourites, sharing and programme
guide when its input changes. A running stream stays on its selected input;
retuning after an interruption can select a healthy backup.

Who can watch a channel is set on the channel's **Viewers** tab. A viewer
allowed on all channels also gets channels you add later. Switching them off for
one channel keeps them on the current other channels only, so new channels stay
hidden until you allow them. Turn on **Private** on that tab to keep the channel
from everyone you have not given it to, including people you add later;
switching a viewer on there gives it to them. A viewer's page only has
**Disable Live TV for this viewer**.

Programme guides can come from separate XMLTV sources, including compressed
`.xml.gz` feeds. Map each channel to its matching guide. URL sources refresh
automatically and retain existing listings if a refresh fails; uploaded files
remain snapshots. Guide coverage shows channels that need a mapping or new data.

## Cluster

**Server › Cluster** shows how this server stores its data. A standalone
server uses its own SQLite database; choose **Create cluster** to test a
PostgreSQL connection, review the migration and let Ether back up, migrate and
restart automatically. Choose **Join cluster** to add this machine with a
one-time token from another node (see [Running a cluster](install.md#running-a-cluster)).

On a cluster node the page lists every node with its version, whether it leads
the cluster, whether it is online (or when it was last seen), its active
streams, hardware transcoding, node weight and public IP. A banner warns about
conditions to fix, such as nodes reaching the internet from different public
addresses, or this node losing touch with the database. The page also shows
the shared settings version and the credential key fingerprint, which match on
every node, and refreshes every 10 seconds.

To add a node, choose **Create join token**. The drawer shows the token
once. On the new machine, open **Cluster › Join cluster**, paste it and follow
the wizard. Fresh installs also offer **Join a cluster instead** during setup.
No terminal commands are required. Keep the token secret and revoke unused
tokens you no longer need. Expiration, use and revocation only prevent Ether
enrollment; the embedded database password and credential key remain
sensitive. If a token leaks, rotate the database password on every node and
any stored provider or Plex credentials the recipient could have read.
Revocation alone does not contain a leak.

**Manage this node** adjusts its provider capacity weight or pauses/resumes new
streams while existing streams keep playing. A pause ends after a restart.
Pausing the only available node prevents new playback until you resume it.
**Open manager** opens a peer node’s own settings; **Updates** handles cluster
software updates. The first upgrade onto multi-app support requires stopping and fencing all old
writers, upgrading all nodes, then serving traffic. Follow the
[mandatory upgrade runbook](acceptance/upgrade.md).

Elsewhere, **Backups & import** shows which node holds each backup and offers
a shared backup folder, the Usenet and Debrid pages show this node's share of
each account's connections, and **General** holds this node's own settings,
including its node weight.

## Monitor and troubleshoot

Use **Activity & notifications** for playback activity and alerts. **Logs** shows
newest entries first. If playback fails, check provider and indexer connectivity,
quality limits and the client's supported formats.

To report an issue, use **Logs → Download logs**. It saves this server's log
files (oldest first) as one text file with its version and platform. Ether
removes tokens, passwords, API keys, URL credentials, `Authorization` and cookie
headers from every log line as it is written, and again in the download.
Settings, the credential key and the database are never included. Skim the file
before sharing it: the scrubbing recognises credentials by name and position,
not every possible secret.

## Owner sign-in and playback access

After setup, sign in with the owner account configured during setup, including
when you open the owner UI on localhost: the Plex owner account for Plex setup,
or the local Ether owner account for setup without Plex. Jellyfin, Emby and
Silo viewers use Ether viewer accounts through their frontend’s sign-in or
device-approval flow.

Plex viewers access shared libraries through their own Plex accounts. Refreshing
Plex sharing removes revoked viewers' access. Stream links stop working when
the viewer is inactive or the session ends.

## Plex account watch sync

Ether can keep a viewer's watched marks and ratings in step with that
viewer's own Plex account, the same state Plex Web shows on Discover. It uses
the same requests Plex Web makes for **Mark as watched** and ratings, with the
viewer's own Plex sign-in. The owner's account is never used for a viewer.

1. Open **Viewers › viewer › Limits** and find **Plex account watch sync**.
   The viewer needs a Plex account (Plex viewers have one).
2. Choose **Connect**, then **Open Plex approval**. Sign in to Plex **as the
   viewer** and approve, then choose **Check connection**. Ether refuses an
   approval from any other Plex account.
3. Pick **Sync**: **Ether → Plex**, **Plex → Ether** or **Both**, and for
   reading from Plex how often to check (5, 10, 30 or 60 minutes).

What syncs:

- Movies and episodes, watched and unwatched, and ratings. Ether's ratings map
  to Plex's stars (half stars included); clearing a rating clears it in Plex.
  Resume positions are not synced.
- **Ether → Plex:** each change is sent within seconds. Turning sending on
  starts from that moment; **Send watched titles to Plex** queues the viewer's
  existing watched and rated titles once.
- **Plex → Ether:** each check reads part of the library in the background
  and carries on from where the last check stopped, so a very large library is
  read over several checks. Shows are read episode by episode only when Plex's
  watched count for the show changed. **Check Plex now** runs a check
  immediately.
- The newest change wins. A change that came from Plex is never sent back, and
  Ether's own changes are not read back as Plex changes.
- Only titles Ether has matched to a verified Plex identity sync; nothing is
  matched by name.

If Plex no longer accepts the connection (for example the viewer signed out of
all devices or changed their password), the section shows **Reconnect
required** and sync stops until the viewer approves again with **Reconnect**;
the chosen direction is kept. If Plex asks Ether to slow down or cannot be
reached, sync pauses and resumes on its own. **Disconnect** removes the stored
sign-in and stops everything at once.

Watch state is unaffected inside Ether: watched marks, ratings and resume
positions still sync between every app a viewer uses with Ether.

## Sync watched state with your Plex Media Server

If you run Ether alongside your own Plex Media Server, Ether can keep watched
marks in step between them: watch something on either and it shows as watched
on the other. This talks to your server directly, with your server's own
token. It is separate from the Plex account sync above and does not use it.

1. Open **Integrations › Plex Media Servers** and choose **Add a Plex Media
   Server**. Enter a name, the server's address with its port (for example
   `http://192.168.1.10:32400`) and the **server owner's Plex token**. To find
   the token, open any title's **⋯** menu in Plex Web, choose **Get Info**,
   then **View XML**; it is the `X-Plex-Token` at the end of the address. Ether
   checks the address and that the token belongs to the server's owner, stores
   the token encrypted and never shows it again.
2. The server's people are listed. For each person you want synced, choose
   **Match to a viewer**, pick their Ether viewer, a **Sync direction**
   (**Ether → Plex**, **Plex → Ether** or **Both**) and how often to check.
   People you don't match are never synced, and nothing is matched by name.
3. To send to Plex for anyone other than the server owner, paste that person's
   own Plex token for the server too: Plex marks titles watched for whoever's
   token is used. Reading what they watched works with the owner's token.

What syncs:

- **Plex → Ether:** each new view in the server's watch history marks that
  title watched in Ether, unless the viewer changed it in Ether more recently.
- **Ether → Plex:** watching or marking a title watched in Ether marks it
  watched on the server; marking it unwatched in Ether marks it unwatched on
  the server, unless the server saw a newer view. Resume points for titles in
  progress in Ether are sent too.
- Turning a direction on starts from that moment; past history is not copied.
  To bring older history across once, use **Bring your library and history**.
- Ratings are not synced, and unwatching a title on the Plex side is not
  picked up, because Plex's watch history only records views.

With two or more servers, a title watched on one reaches Ether and every other
server once. A change never bounces back to the server it came from, and
Ether ignores its own marks when they appear in a server's history.

Titles are matched only by their TMDB, IMDb or TVDB identity (and for
episodes, the show's identity plus season and episode number). Titles that
don't match are listed under **unmatched titles** for that person with the
reason, and are checked again later. Sending to the server also needs the
title's Plex identity, which Ether looks up through its Plex app connection.

Sync runs in the background on one node of a cluster, reads only what changed
since the last check, and backs off if the server is busy or unreachable. If a
server asks Ether to slow down, every person on that server waits as asked.
Each person's row shows when Ether last read from and sent to the server, the
next check, and any problem in plain words; **Sync now** runs a check
immediately.

## Source interruptions

Usenet providers are tried in priority order. When every connection of a
higher-priority provider is busy, a viewer's request goes to the next provider
that has a free connection instead of waiting. Background work does this only
when that provider is otherwise idle. Backfill (block) accounts are used only
for articles the other providers lack, so a busy primary never spends block
data. **Sources › Stats** shows how many requests each provider took over from
a busy one this month.

If a server refuses connections because the account is at its limit, Ether
learns that limit by itself. The provider list then shows, for example,
"using 47 of 50 connections (server limit learned)". When the server states its
limit, Ether stays just below it. Otherwise Ether adds connections back one at a
time while requests succeed. A refusal right after Ether closed a connection is
not treated as a limit: the server is still releasing the old connection. There
is nothing to configure.

Ether tries other providers before changing a release. Copy-only HLS or DASH
can restart on another release at the current media time. Direct-play MKV and MP4
have stricter replacement limits because the player retains the original file's
headers. Provider outages and timeouts are retriable and do not permanently mark
a release bad. Different cuts of a title can still skip or repeat a short section
when Ether maps the playback timestamp.

## Cache management

**Streaming › Cache** controls media retention, opening and resume warming, and an
optional managed shelf of preloaded movies and TV episodes. Selecting a preset
only updates the draft until you review and save.

- **Presets:** **Pass-through** stores no persistent media segments on disk.
  **Feather / Everyday** (256 MiB / 32 GiB) keep small opening windows and
  short retention. **Encore / Deep shelf** (250 / 750 GiB) retain fetched
  segments and favour repeat watches without downloading whole titles ahead of
  time. **Cinema node / Edge library** (2 / 20 TiB) dedicate space inside the
  cache budget to proactive full-title preloading.
- **Retention & warming:** Demand mode evicts the oldest unprotected segments;
  adaptive mode gives repeat watches extra retention credit. Detail-page opening
  windows, resume windows and next-episode warming are independently
  configurable and always yield priority to active streams.
- **Free-space floor:** Whatever the budget, the cache keeps at least 10 GiB or
  5% of its volume free (whichever is smaller), evicting the oldest unprotected
  segments when other files fill the disk. Segments of active streams and
  pinned titles are never evicted for it.
- **Managed preloading:** Opt-in movie and TV preloading ranks candidates using
  a configurable blend of household watch history and catalogue popularity, with
  a rolling episode window around each active viewer's progress. Preloading
  pauses while anyone is streaming unless explicitly allowed, respects a
  bandwidth cap and free-space reserve, and never writes watch history.
- **Explaining the cache:** **Streaming › Cache › Explorer** shows a **Reason**
  column and the stored file name under each title; search also matches file
  names. Filters cover **Watching now**, **Paused**, **Unfinished**, **Shelf**,
  **Prefetched**, **Checked**, **Kept by you**, **Unexplained**, and
  **Reclaimable**. Expand a row for **Fetched because**, **Kept because**,
  bytes, play history, and source. Pin badges use the same human labels.
- **Overview and shelf states:** Overview splits protected bytes by cause and
  names the title and cause currently warming. **Preloading** lists the **Shelf
  manifest** with **Cached**, **Queued**, or **Failed** per title, and
  **Preload selection** explains each choice without untranslated server prose.
  Releases cached before this version show **Unexplained** until a new cause is
  recorded; nothing is guessed.
- **Shared NAS storage:** Under **Edit policy → Where your cache lives → Shared
  NAS**, point a standalone server or every participating PostgreSQL cluster
  node at the same dedicated folder on an OS-mounted NFS or SMB share. Keep
  Ether's SQLite database and transcode temp directory on local storage, never
  on the NAS share.

## Catalog

Open **Library › Catalog** to browse the released movies and shows in your
library. Switch between posters and a list, search by title, filter by genre
or decade, and sort by popularity, release date, rating, date added, or title.
Results are paginated across the full local catalog. Select a title to inspect
its artwork, overview, ratings, cast, directors, and catalog identity.

**Find on TMDB** retains the broader title lookup for released titles outside
the browsable library. **Catalog settings** contains the TMDB key and connection
test, language, region, refresh interval, **Hide obscure titles** (Off, Some or
Most; exact minimum popularity and votes are under **More options**), adult
content option, and catalog health. Settings apply without a restart. Catalog
is at **Catalog**; Metadata no longer keeps a catalogue alias.

**Show upcoming episodes**, under **Upcoming content** in Catalog settings, is
off by default. When enabled, a season that has already started can list unaired
episodes with known air dates.
They show an **Unaired** label and the scheduled date in their description, with
no playable media. Unknown dates and wholly future seasons stay hidden. Autoplay,
Continue Watching and alternate episode-order lists keep their released episodes.
Turning the option off removes access to these placeholders and their artwork.
**Show upcoming movies**, also off by default, lists movies before their release date.

## Metadata artwork

**Metadata → Artwork** previews real catalogue posters with your unsaved design.
Its **Poster source** picks TMDB (the default) or a poster provider you have set
up under **Integrations**.
Use the refresh button to try another title, or compare your design with the
original. The canvas stays fitted to the window; the controls scroll separately.

Cinema combines scores and title details in glass badges. Spotlight uses the
accent colour for prominent badges and adds recent-release signals when they
exist. Essential keeps a small, unboxed score. Finishes, placement, size, colour
and individual layers remain editable after choosing a preset.

Ratings use the catalogue's TMDB score when no cached external-provider score is
available. Sources are always named. IMDb and other provider scores require the
owner's configured provider; Ether never relabels a TMDB score as IMDb. Unrated
titles receive no invented score. **Poster layers** shows the actual labels or
explains which data is unavailable for the selected title and viewer.

Release status highlights releases within 30 days, unwatched recent episodes or
seasons, and scheduled collection-row expiry. An ongoing series is not labelled
“Returning” merely because TMDB says it is still in production. Quality badges
start with **PLAYED**: they describe verified media from this viewer's current
playback or a matching resume release probed within 30 days. They do not promise
the next selected release has the same format. Release-name estimates and other
viewers' playback are excluded.

Ether retains the original image and renders a separate poster copy on demand.
It does not modify video or upload replacement posters into a separate Plex
server. Plex receives versioned image URLs; disabling customization restores
original artwork after the client refreshes its metadata.

Identical rendered results share a cache across viewers. Each node defaults to
16 MiB of generated posters in memory and 256 MiB on its local disk under
`cache/overlays`; least-recently-used entries are evicted within those limits.
Generated files survive restarts, validate their content before reuse, and can be
regenerated from the original artwork. Simultaneous requests for the same image
share one render, with at most two cold renders running at once. Unneeded queued
work is cancelled when its last requester leaves. Disk failures fall back to
memory/rendering rather than making posters unavailable. These caches are local
to each server process; they are not a shared media-cache directory.

### Release monitoring and recent availability

Ether automatically follows the recent releases of the indexers already
configured under Sources, the way Radarr and Sonarr use RSS. No separate RSS URL
is needed. Newznab/Torznab indexers are read through their API; built-in sites
through their latest-releases page (one page, the way Prowlarr builds RSS for
them). Stremio addons and plugin sources only answer title searches. Debrid
provides streaming, not a global new-release feed. If an indexer returns empty
results for a combined category query but has a working unrestricted feed,
Ether learns that format with one additional budgeted request and applies your
categories locally, then tries the category-filtered feed again once a day,
because an unrestricted feed needs several pages to cover what one filtered page
covers. The learned format survives restarts and is reset when the indexer
configuration changes. Empty feeds are not repeatedly probed on every poll.
Monitoring defaults on for new installs. Existing
owners can enable it under Catalog › Settings › General › New releases. Turning monitoring off stops
polling, but local evidence and the recent-release visibility rules still apply.

Before the release or air date, titles remain hidden unless you turn on
**Upcoming content**. From release until 60 days
for movies, and until 24 hours for individual episodes, an eligible observed
release is required for Plex discovery, library lists, search and detail pages.
A suitable upload unlocks the item immediately: these are evidence windows, not
waiting periods. Older titles keep normal catalogue visibility without scanning
the back catalogue. The age cutoffs are assumptions, not availability guarantees.
A missing new episode does not remove the show's older seasons or episodes.
Watch history and saved lists are retained even when an item is temporarily hidden.
This is the **When they can play** mode under **New releases** in Catalog settings.
**On digital release** shows a movie once its digital or physical release date has
passed, and **Right away** shows every new movie.

Feeds, existing search-cache entries, requested searches and successful playback
checks share local evidence. Entries arriving before an episode's metadata are
retained for later local matching. CAM/HQ Pre and explicitly incompatible dubs
are not suitable evidence. Current quality policy, disabled/reconfigured sources,
source invalidations and recent failed searches are respected; a genuinely new
eligible release can supersede an older failed search. Merely browsing does not
launch per-title availability searches.

Each indexer's feed is read once every 30 minutes (48 requests a day), and every
new item is matched locally against all the titles waiting for a release, at no
indexer cost per title. A release usually becomes visible within one sync
interval of being posted. When more releases arrived since the last sync than one
page holds, Ether reads only the further pages needed to reach the previous
sync's newest release, at most eight per sync; after downtime the rest of the
gap is read in later cycles from what the day's allowance has left. **Catch-up**
(off by default) additionally walks the recent feed history once, ending at the
feed's available history or a page entirely older than 61 days. Unfinished gaps
retain their boundaries and page progress across restarts, and a mismatched
boundary triggers a rescan. Provider retention, rate limits and exhausted daily
allowances can still leave gaps; these stay visible instead of being reported as
a successful sync. Feed discovery is not equivalent to an Arr-completed download.
These are normal API hits, not free RSS traffic.

Ether searches a recent movie by ID only on events, never on a timer: once when
it enters the window (its earlier releases predate the feeds), once when its
digital or physical release date arrives, and once after a found release proves
unplayable. A search cut short by an outage is finished six hours later, and one
cut short by an exhausted allowance after the next UTC midnight. Each search asks
one source at a time in priority order and stops at the first release your
quality settings accept; Play still asks every source. At most ten titles are
searched per minute, so a restart never bursts every indexer at once. To keep
background feeds and searches on particular indexers while playback still uses
all of them, list them in `availability.indexerIds`.

Background feeds and searches have a daily allowance per indexer (UTC): 10% of
the indexer's daily API limit (Gentle 5%; Unlimited up to your `reservePercent`
share). The limit is the one you set on the indexer, else the one it reports
(Newznab `apilimits`), else 1,000 a day, the entry tier of strict private
indexers, so an indexer that reports nothing gets 100 background requests a
day. The day's remaining head polls are reserved first, so gap pages and
searches can never starve the feeds, and a feed whose allowance is too small for
30-minute polls is read less often. Background requests to one indexer are at
least three seconds apart. On a typical household with two Usenet indexers and
four built-in sites, a replayed day used 50–64 requests on each Usenet indexer
and 49 on each site. The total across indexers can be lowered under Sources >
Indexers (Daily budgets). A `429` response, a `Retry-After` header or a Newznab
API-limit error pauses that indexer for background and playback alike until the
time it gives (15 minutes when it gives none, 24 hours at most).

Work nobody is waiting on (detail-page and next-episode warm-ups, preloads,
prefetches, watchlist checks) shares another 10% of each indexer's daily API
limit for searches, and 10% of its daily NZB download limit (set on the
indexer, reported as `grabmax`, else 1,000) for NZB downloads, together with
availability checks. Play is never limited by these shares; once one is spent,
the warm-up simply does not happen and Play searches for itself. A downloaded
NZB is kept on disk and reused for as long as it is kept (until unused for 60
days, or least recently used beyond 1 GiB), so a release is downloaded from its
indexer once, however often it is resolved or played. Indexer statistics count
only these real downloads. A preloaded title whose fill fails is tried again
after an hour, then two, four and so on up to a day, instead of on every
15-minute preload pass.

Indexer results are cached per title: six hours for titles out under a month,
rising to a week for titles out over a year. A search that found nothing is
cached for three minutes for a title out under a week, so a new episode is
found soon after its upload, and thirty minutes otherwise. When the first,
ID-based query finds nothing, a Play search fires its fallback queries
(other IDs, title and year, alternative titles) at once. A search nobody is
waiting on, such as a detail-page or next-episode warm-up, a watchlist check or
a preload, tries them one at a time and stops at the first that finds
releases. Live TV channels built from your collections pre-load upcoming
programmes only while someone has watched the channel in the last two hours. Optional background health verification has separate request,
grab and byte budgets and pauses during playback; it is disabled by default.

A listing establishes that an eligible release was observed, not guaranteed
picture quality, spoken language or current playability. Playback still validates
its source. Original-language foreign films remain eligible. An explicit Play
Version choice can select a different dub; the global CAM restriction still applies.

### Embedding the dashboard

Ether blocks framing by default. To embed its web interface in Organizr or
another trusted dashboard, set `server.webFrameOrigins` to that dashboard's
exact origins, for example `["http://dashboard.example:8090"]`, through the
owner-authenticated server settings API, in `config.json` while Ether is
stopped, or under **Server › General › How apps connect › More options › Sites
that may embed this page**. Include the scheme and port, but no path or wildcard. API changes take
effect immediately. Point the dashboard tab at Ether's `/ether/` URL.

Only those origins may frame Ether; Plex sign-in and owner authentication remain
required. Use HTTPS for both pages when the dashboard is served over HTTPS.
An empty list restores the default framing prohibition.

### Automatic RTX Video setup

RTX Video Super Resolution remains **Off** by default. On Windows x64 with an
NVIDIA RTX GPU as device 0 and a working NVIDIA driver, selecting **Automatic**
or **On** downloads the pinned NVEncC 9.38 package from its upstream GitHub
release. Ether checks its exact size and SHA-256 before extraction, installs it
under `<data directory>/tools/nvenc-9.38-windows-amd64`, then verifies NVEncC,
NGX and the required muxers. Windows' built-in `System32/tar.exe` is used to
extract the package. The initial download is about 247 MB; subsequent starts
reuse the installed runtime.

Setup runs in the background and the hardware panel refreshes its status.
Ordinary transcoding remains available while setup runs or if it fails.
Switching back to **Off** cancels setup; it does not delete a completed runtime.
Failures are shown in the hardware panel. Correct the reported problem and
switch Off, then back to Automatic or On to retry. Ether does not install GPU
drivers or repeatedly download a failing package in the background.

An explicit **NVEncC executable path** remains owner-managed: Ether validates
it but never downloads over it. Setup and path changes apply without restarting
Ether. Enabling support does not guarantee every stream is enhanced: existing
codec, source resolution, bandwidth, audio/subtitle and concurrency rules still
apply, and an actual enhancement failure falls back to ordinary transcoding.

### Library visibility and discovery collections

**Catalog › Settings › Rules** controls which titles Ether exposes to Plex.
Add ordered rules that show or hide titles, choose movies, TV, or both, and
combine conditions with **All** or **Any**. The first matching rule wins, and
**Everything else** sets what no rule matches. A hidden title leaves browsing,
search, recommendations and collections. To keep titles out of
browsing without hiding them from search, set a hide rule to **Hide from
browsing only**: matching titles leave library browsing, Home and hub rows,
recommendations, New Releases, Trending and catalogue collections, but anyone
can still search for them, open them and play them. For example, **Hide ·
Country is not United States · Browsing only** keeps discovery to US titles
while a foreign film stays one search away. The page shows the catalogue going
in, your rules, and the result, and its **Browse** drawers list exactly what is
shown or hidden. It uses only the local catalogue; it does not search indexers or fetch every title's details.
Saving preserves metadata and watch history, and clearing the rules restores
visibility subject to the normal release, availability and viewer restrictions.

Country matches TMDB's origin/production country metadata, including
co-productions, separately from original language. For example, a country rule
for India also matches an English-language Indian production. Unknown country,
genre, year and language never match an exclusion. Votes are different: a
title nobody has voted on has 0 votes, so a rule such as **Votes ≤ 99** also
matches titles with no votes, and **Votes is not set** matches only those. The
preview reports country
coverage. Existing normal metadata refreshes fill gaps without a quota-consuming
bulk lookup.

#### Content ratings

Ether compares content ratings by **age level**, everywhere: library
visibility, collections, viewer rating caps, Live TV limits and
recommendations. Film, TV and other countries' ratings at the same level are
one choice, so the picker offers levels rather than individual labels:

| Level | US film · US TV | Other boards, for example |
| --- | --- | --- |
| All ages | G · TV-G · TV-Y | U (UK), 0 (DE), L (BR), G (CA) |
| Parental guidance | PG · TV-PG · TV-Y7 | PG (UK, CA, AU), 6 (DE), 7 (SE), 10 (BR, FR) |
| Teens 13+ | PG-13 · TV-14 | 12A (UK), 12 (DE), 14A (CA), M (AU) |
| Mature 16+ | R · TV-MA | 15 (UK), 16 (DE), MA15+ (AU), 18A (CA) |
| Adults 18+ | NC-17 | 18 (UK, DE), R18+ (AU), R (CA) |
| Not rated | NR, or no rating at all | |

An allowlist such as "Content rating is none of G · TV-G · TV-Y, PG · TV-PG ·
TV-Y7" therefore keeps a Hallmark movie certified G, a TV-G series, a UK PG
film and a TV movie only Germany rates 6. Kids' 7+ ratings share PG's level, so
choosing G and PG never hides something milder than PG.
"Is none of" hides titles without a rating unless you also choose **Not
rated**. To keep family titles without a rating but hide unrated adult ones,
add a second group: Content rating is Not rated, and Genre is any of Horror,
Thriller. Rules can also use **is at least** and **is at most** a level; these
never match unrated titles. A viewer's rating cap admits everything at or
below its level and always hides unrated titles.

Ether picks each title's rating from TMDB: your catalogue region first, then
the US, considering theatrical, digital, physical and TV releases in that
order (TV movies are usually certified on their TV premiere). When neither
has one, another country's rating is kept with its country (shown as
"G (CA)", sent to Plex as `ca/G`), never passed off as a US rating. If you
configure an OMDb or MDBList key under Metadata, IMDb's US certificate fills
titles TMDB leaves unrated, as Plex Media Server does; Ether looks up a few
unrated titles every ten minutes so your provider quota stays mostly for
badges. The title inspector shows where each rating came from.

Library collections and discovery rows are independent. Ether ships movie
collections by genre and TV collections by locally known networks/studios.
These match the local catalogue dynamically, and deleting a default keeps it
deleted. Newly encountered TV networks/studios are added automatically. Use the
collection editor to change rules, artwork, audience, or publication targets:
**Collection only**, **Discovery rows**, or **Both**. Shipped trending and other
feed sources stay available for rows but no longer clutter Plex's Collections tab.

Default discovery starts with Continue Watching when populated, then Trending
Now, personal picks and New & Noteworthy. Global TMDB daily and weekly charts
refresh hourly, share one durable cache across viewers, and retain the last
successful chart during temporary outages. Catalog settings show freshness;
charts older than seven days are not advertised as current. Owner/viewer Home
Designer placements still override defaults.

Discovery rows stay stable during a browsing visit, including watched-state
changes. A new client session or five minutes without browsing starts a fresh
selection. Clients that do not send a session identifier use the idle boundary;
Ether cannot distinguish an immediate app reopen from an ordinary refresh on
those clients. Continue Watching remains live throughout.

## Stremio addon

An owner can enable Stremio under **Apps**. Each viewer can then open the
viewer portal, choose **Connect an app**, and create an individual Stremio
install link. Open the link in Stremio or copy it to the device running the
app. Keep this link private: it gives access to that viewer's permitted media.
The viewer can revoke the connection under **Devices** and create another one
if needed.

Stremio requires an HTTPS address. If the portal cannot create an install
link, add a certificate under **Apps › Away from home › Use your own
certificate** and try again.
Catalogue items and streams follow the viewer's library and safety settings.

## Read-only WebDAV

After an owner enables **Files** under **Apps**, a viewer can create a WebDAV
app password in the viewer portal under **Connect an app**. Copy the address,
username and password into a WebDAV client such as Infuse, VLC, Kodi or rclone.
The password is shown once; revoke the connection under **Devices** if it is
lost or no longer needed. Each viewer sees only media allowed by their account.
The Movies, TV and Collections folders are read-only. Opening a file starts a
normal Ether playback session and counts toward the viewer's stream limits.

## DLNA on the local network

The owner can enable **DLNA** under **Apps** and select the viewer whose library
and safety settings apply to shared devices. DLNA stays unavailable until a
viewer is selected, and accepts requests only from the local network. The owner
can also create a living-room guest in the DLNA settings. It starts with the
Kids preset (PG) and can be edited like other viewers. Browse for Ether in a
DLNA-compatible player after saving the selection.

## M3U and XMLTV

When an owner enables **M3U and XMLTV** under **Apps**, each viewer can create
an individual playlist address in the viewer portal under **Connect an app**.
Add that address to a compatible IPTV client. Keep it private: it grants access
to the channels allowed for that viewer. For the matching XMLTV guide address,
replace `playlist.m3u` at the end of the address with `guide.xml`. Revoke the
connection under **Devices** when it is no longer needed.

Revoking a connection blocks its new playback requests immediately. An existing
download or playback response is checked again while it is transferring, so it
may continue briefly before stopping; that interruption is not instantaneous.


## IPTV apps (Xtream)

In v0.3.5, enable **IPTV apps (Xtream)** on **Apps**, open its settings,
choose a viewer, and create a connection. Enter the displayed server address,
generated username and app password into an IPTV player app. The password is
shown once. Create a separate login for each device; each login permits one
active playback connection. Revoke a login from the same page.

Live TV includes that viewer's On Air and IPTV channels and guide. Movies and
series are another view of Ether's regular TMDB catalogue, with the same viewer
restrictions and release-availability rules. Playback uses Ether's normal source
resolver and cache, prefers the original media, and supports byte ranges for VOD.
Live playback uses the continuous shared HLS output. Timeshift is not advertised.

The API lives under the displayed server address: `player_api.php`, `xmltv.php`,
`get.php`, and `live/`, `movie/`, and `series/` stream paths. Connection details are
secrets because the protocol carries them in URLs. Ether redacts them in logs
and support captures. Use a private network or HTTPS when connecting remotely.
Generated-media testing covered Televizo movie/episode playback, movie seeking,
and live channel switching on an Android emulator. TiviMate guide/live playback
and switching were also exercised; its Premium VOD/series paths were not tested.
Smarters Pro for Mac was checked for login and decoder activity, but sustained
playback remains unverified. These are scoped checks, not certification of every
IPTV app, physical TV, or provider. Upstream provider connection limits are
enforced per Ether process; they are not a cluster-wide account quota.
