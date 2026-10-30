# Using Ether

Open <http://localhost:32400/ether/> on the server to manage Ether. Use your
chosen media apps to browse and watch, subject to the support notes below.
If Ether uses another port, such as
32450 alongside an existing Plex server, use that port instead.

Start here: [libraries and the catalogue](#libraries-catalogue-and-home),
[put your watchlist on Home](#put-your-watchlist-on-home), or
[choose Home rows](#choose-home-rows).

## Jellyfin and Emby apps

Jellyfin and Emby support is in development. No app has been certified yet;
the matrix will be filled in only after testing the listed features on the
specified app version. A dash means not yet verified.

Downloads: not supported.

| Client | Version | Browse | Play | Resume | Watched | Skip intro | Subtitles | Live TV | Quick Connect |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| Jellyfin for Android TV | Pending | — | — | — | — | — | — | — | — |
| Swiftfin | Pending | — | — | — | — | — | — | — | — |
| Findroid | Pending | — | — | — | — | — | — | — | — |
| Streamyfin | Pending | — | — | — | — | — | — | — | — |
| JellyCon | Pending | — | — | — | — | — | — | — | — |
| Jellyfin for Roku | Pending | — | — | — | — | — | — | — | — |
| Emby Web (externally hosted) | Pending | — | — | — | — | — | — | — | — |
| Emby for TV (Apple TV, 2.x web-shell) | 2.0.9 (unsupported) | — | — | — | — | — | — | — | — |

Jellyfin for Android phones and Jellyfin Desktop are unsupported: they wrap
Jellyfin Web, which Ether does not ship. Ether's `/web` page gives connection
instructions; it is not a browser media player.
Ether does not bundle Emby Web; the externally hosted client has not yet been
certified against Ether. Emby for TV 2.0.9 on Apple TV is unsupported: it
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
if that port is occupied, check the Apps page for the actual listening address.
Add that address in the Silo app and sign in with an Ether viewer account, or
approve the device's code in the viewer portal. A dash means the feature has
not yet been verified on the named client.
Settings beyond revision 8 are not supported.
PQ video variants with unknown HDR10+ metadata are not offered until the
server can determine that metadata; SDR and HLG variants remain available.
For media with an unknown transfer function, Silo's required HDR10+ boolean
means HDR10+ has not been positively identified; the media remains playable.
Watch details omit marker ranges until they can be bound to the selected file;
this is an outstanding Silo schema and skip-intro compatibility gap.
Last-played codec, resolution and HDR describe the last recorded playback
sample and can briefly lag if storing a newer sample fails. The last-played
file identifier is omitted until it can be matched to an authorized version.
Native certification also requires verifying that Ether's stream request
headers reach direct playback and HLS playlists and segments, including after a
seek; this has not yet been verified on the physical devices.

| Client | Version | Browse | Play | Resume | Watched | Subtitles | Device pairing |
| --- | --- | --- | --- | --- | --- | --- | --- |
| Silo for Apple TV, iOS and macOS | Pending | — | — | — | — | — | — |
| Silo for Android and Android TV | Pending | — | — | — | — | — | — |

## Set up

The setup wizard starts with app selection and owner-account setup. If you
select Plex, sign in with Plex and claim the server; otherwise, create a local
Ether owner account. Then choose where Ether streams from: a
debrid account (Real-Debrid, TorBox, AllDebrid, Premiumize, Debrid-Link or
Offcloud), a Usenet provider, or both, each tested before you continue. Next add at least one
indexer: a Stremio stream addon's install link or a Torznab URL for debrid,
and a Newznab URL for Usenet. Finally choose a cache folder and budget.
Configure quality profiles for your network and remote connections afterwards.
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
  service can serve it now, so playback never waits for a download.
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
puts the source that starts faster first. Set this under **Releases › Playback
start › Source for equal-quality releases**:

- **Fastest (automatic):** once both sources have enough plays, the one with
  the lower measured start time goes first. Until then, popular titles use
  cached debrid first. The measured times are shown beside the setting.
- **Prefer Usenet** or **Prefer debrid:** always try that source first.

The best release from the other source is checked at the same time, and both
remain available for failover. **Releases › Pinned** (when you find a title) and **History** show when
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

Under **Sources › Indexers › Site definitions**, below the indexer list:

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
address (for example `http://localhost:8191`) under **FlareSolverr**. Ether
asks it to solve the challenge only when a site presents one, then reuses
the clearance it returns.

Sign-ins that need a captcha are not supported; if the definition offers
cookie sign-in, paste the cookie from a signed-in browser instead.

## Share with viewers

Use Plex library sharing to invite people or remove their access. They appear
in Ether's **Viewers** page after signing in. Open a viewer to see their taste,
activity and Home, or set content and playback limits. Each viewer keeps their
own history and recommendations.

## Libraries, catalogue and Home

Ether appears as its own server in Plex, with Movies and TV Shows libraries.
These libraries contain the titles exposed by Ether's catalogue and visibility
rules; they are not folders of downloaded media. A catalogue entry does not
guarantee that your configured sources have a playable release.

Your existing Plex Media Server keeps its own libraries and files. Ether does
not merge or replace them. Plex Home can show rows from multiple pinned
libraries, so check the server name beneath a row and pin the Ether libraries
you want to browse in your Plex app.

Use **Catalog › Library visibility** to control what Ether exposes, and
**Catalog › Catalog settings** to configure the catalogue. **Collections**
groups titles within those libraries. **Home** arranges recommendation rows;
changing a Home row does not delete a library or remove a title from the
catalogue. Plex's own Watchlist and other Plex-provided navigation remain
client features.

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

Movies appear from their theatrical release date. Shows, seasons and episodes
appear from their air dates. Items with no known release date stay hidden.

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
2. In Ether, open **Viewers › Defaults › Watchlist alerts**, enable
   **Watchlist monitoring** and **Plex watchlists**, then save.
3. Open **Viewers › your viewer › Watchlist**. **Sync viewer** imports the
   list; **Check availability** checks for playable releases. These actions
   are separate: importing a title does not prove it can stream.
4. Return to **Home** and inspect the same viewer's preview. The watchlist
   rows are **Waiting for a stream** and **Now available**. Check that your
   saved row policies do not hide them, and reload Plex Home.

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
| Inspect a failed source search | **Releases › History**, then open the attempt |
| Inspect observed playback | **Activity › History** |
| Change one viewer's track preferences | **Viewers › viewer › Audio & subtitles** |
| Change defaults for viewers | **Viewers › Defaults** |
| Arrange rows in Plex | **Home** |
| Change artwork providers | **Metadata › Artwork** |

A playback attempt can fail before Activity history begins. Releases history
records source-selection results, including failures; open its details for
candidate rejection reasons and effective playback limits. Neither a
catalogue poster nor an indexer's “cached” label guarantees a playable source.

## Versions, trailers, downloads and previews

Detail pages can list several versions of a title, such as 4K, a
high-compatibility 1080p release or an extended cut. Choose one with Plex's
**Play Version**; otherwise Ether picks the best version for the device. Search
also accepts descriptions such as "90s thrillers under 100 mins" and offers a
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
- `transcode`: `fastStart`, `abrLadder`, `vbvReservoir` and `spikeGovernor`
  control adaptive streaming.
- `usenet.minIdle`: warm provider connections kept ready for playback.

Preview thumbnails appear while a title plays and fill in as playback
progresses.

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

## Import a quality profile from TRaSH Guides

Coming from Radarr or Sonarr? Open **Releases › Quality profiles** and choose
**Import from TRaSH Guides**. Pick **Radarr** for movies or **Sonarr** for
shows, then choose a profile, such as *HD Bluray + WEB* or *[SQP] SQP-1
(2160p)*. Ether shows what it will add before saving:

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

Rename the profile if you like, then choose **Add profile**. Your other
profiles keep ranking releases exactly as before: they score the newly added
formats 0, as Radarr and Sonarr do for formats a profile doesn't list. The new
profile's drawer shows its imported rules; **Allow all** removes the quality
limits.

Ether reads the profiles from TRaSH Guides on GitHub and keeps them for an
hour. If GitHub is unreachable or its hourly request limit is used up, switch
to **Paste JSON** and paste or upload a profile file from the guide's
`docs/json/radarr/quality-profiles` or `docs/json/sonarr/quality-profiles`
folder. Custom formats Ether can't fetch are marked **Not found**; paste them
under **Custom scoring** and import the profile again.

## Choose and fix releases

**Releases** explains and overrides which release plays for each title.

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

Use **Libraries & backups** to add existing media or preview a Plex import. Map
each source viewer to the correct Ether viewer and review unmatched titles
before importing.

To download a snapshot, sign in as the owner and open **Backups & import ›
Snapshots** after setup is complete. Enter a backup password of at least 12
characters, then choose **Download**. The downloaded `.etherbackup` file is
encrypted. Ether does not keep this password; save it somewhere safe because
it is needed to restore the file.

To restore, enter the same password and select the `.etherbackup` file. Ether
validates it and stages a new data folder; uploading a backup does not replace
the running server. Follow the displayed instructions to stop Ether and start
it with that folder. An older, unencrypted `.tar.gz` backup requires an owner
sign-in and explicit confirmation. See [installation and restoration](install.md#backups)
for service and database considerations.

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
**Open manager** opens a peer node’s own settings; **Updates** handles rolling
cluster updates.

Elsewhere, **Backups & import** shows which node holds each backup and offers
a shared backup folder, the Usenet and Debrid pages show this node's share of
each account's connections, and **General** holds this node's own settings,
including its node weight.

## Monitor and troubleshoot

Use **Activity & notifications** for playback activity and alerts. **Logs** shows
newest entries first. If playback fails, check provider and indexer connectivity,
quality limits and the client's supported formats. Share redacted logs when
reporting an issue.

## Owner sign-in and playback access

After setup, sign in with the Plex account that owns Ether, including when you
open the owner UI on localhost. Viewers access shared libraries through their
own Plex accounts. Refreshing Plex sharing removes revoked viewers' access;
stream links stop working when the viewer is inactive or the session ends.

## Source interruptions

Ether tries other providers before changing a release. Copy-only HLS or DASH
can restart on another release at the current media time. Direct-play MKV and MP4
have stricter replacement limits because the player retains the original file's
headers. Provider outages and timeouts are retriable and do not permanently mark
a release bad. Different cuts of a title can still skip or repeat a short section
when Ether maps the playback timestamp.

## Cache management

**Server → Cache** controls media retention, opening and resume warming, and an
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
- **Managed preloading:** Opt-in movie and TV preloading ranks candidates using
  a configurable blend of household watch history and catalogue popularity, with
  a rolling episode window around each active viewer's progress. Preloading
  pauses while anyone is streaming unless explicitly allowed, respects a
  bandwidth cap and free-space reserve, and never writes watch history.
- **Shared NAS storage:** Under **Edit policy → Where your cache lives → Shared
  NAS**, point a standalone server or every participating PostgreSQL cluster
  node at the same dedicated folder on an OS-mounted NFS or SMB share. Keep
  Ether's SQLite database and transcode temp directory on local storage, never
  on the NAS share.

## Catalog

Open **In Plex → Catalog** to browse the released movies and shows in your
library. Switch between posters and a list, search by title, filter by genre
or decade, and sort by popularity, release date, rating, date added, or title.
Results are paginated across the full local catalog. Select a title to inspect
its artwork, overview, ratings, cast, directors, and catalog identity.

**Find on TMDB** retains the broader title lookup for released titles outside
the browsable library. **Catalog settings** contains the TMDB key and connection
test, language, region, refresh interval, popularity and vote thresholds, adult
content option, and catalog health. Settings apply without a restart. Existing
links to **Metadata → Catalogue** redirect to Catalog.

**Show scheduled episodes** in Catalog settings is off by default. When enabled,
a season that has already started can list unaired episodes with known air dates.
They show an **Unaired** label and the scheduled date in their description, with
no playable media. Unknown dates and wholly future seasons stay hidden. Autoplay,
Continue Watching and alternate episode-order lists keep their released episodes.
Turning the option off removes access to these placeholders and their artwork.

## Metadata artwork

**Metadata → Artwork** previews real catalogue posters with your unsaved design.
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

Ether automatically follows recent-release feeds on the Newznab/Torznab indexers
already configured under Sources. No separate RSS URL is needed. Built-in scraper
sites without a recent-feed API remain searchable; debrid provides streaming,
not a global new-release feed. If an indexer returns empty results for a combined
category query but has a working unrestricted feed, Ether learns that format
with one additional budgeted request and applies your categories locally. The
learned format survives restarts and is reset when the indexer configuration
changes. Empty feeds are not repeatedly probed on every poll.
Monitoring defaults on for new installs. Existing
owners can enable it under Sources > Availability. Turning monitoring off stops
polling, but local evidence and the recent-release visibility rules still apply.

Before the release or air date, titles remain hidden. From release until 60 days
for movies, and until 24 hours for individual episodes, an eligible observed
release is required for Plex discovery, library lists, search and detail pages.
A suitable upload unlocks the item immediately: these are evidence windows, not
waiting periods. Older titles keep normal catalogue visibility without scanning
the back catalogue. The age cutoffs are assumptions, not availability guarantees.
A missing new episode does not remove the show's older seasons or episodes.
Watch history and saved lists are retained even when an item is temporarily hidden.

Feeds, existing search-cache entries, requested searches and successful playback
checks share local evidence. Entries arriving before an episode's metadata are
retained for later local matching. CAM/HQ Pre and explicitly incompatible dubs
are not suitable evidence. Current quality policy, disabled/reconfigured sources,
source invalidations and recent failed searches are respected; a genuinely new
eligible release can supersede an older failed search. Merely browsing does not
launch per-title availability searches.

The default feed allowance is 400 API requests per UTC day across all indexers,
with a 15-minute polling interval and at most five pages per sync. Two indexers
need 192 requests/day for their regular first pages, leaving up to 208 for
catch-up. Deeper pages reserve the remaining day's scheduled first-page polls,
so a busy feed cannot consume that allowance on history. An allowance too small
for the configured feeds and interval is shown in Sources > Availability.

Unfinished feed gaps retain their boundaries and page progress across restarts.
Each poll reads fresh arrivals first, then resumes the older gap, checking that
the saved page boundary still matches. A mismatched boundary triggers a rescan.
First setup and recovery of old gaps page through recent feed history only,
ending at the feed's available history or a page entirely older than 61 days.
This uses feed pages, never a search per catalogue title. Provider retention,
rate limits and exhausted daily allowances can still leave gaps; these stay
visible instead of being reported as a successful sync. Sites without a feed
remain search-only. Feed discovery is not equivalent to an Arr-completed download.
These are normal API hits, not free RSS traffic. Playback retains the configured
foreground reserve. Optional background health verification has separate request,
grab and byte budgets and pauses during playback; it is disabled by default.

A listing establishes that an eligible release was observed, not guaranteed
picture quality, spoken language or current playability. Playback still validates
its source. Original-language foreign films remain eligible. An explicit Play
Version choice can select a different dub; the global CAM restriction still applies.

### Embedding the dashboard

Ether blocks framing by default. To embed its web interface in Organizr or
another trusted dashboard, set `server.webFrameOrigins` to that dashboard's
exact origins, for example `["http://dashboard.example:8090"]`, through the
owner-authenticated server settings API or in `config.json` while Ether is
stopped. Include the scheme and port, but no path or wildcard. API changes take
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

**Catalog → Library visibility** controls which titles Ether exposes to Plex.
Add named exclusion groups, choose movies, TV, or both, and combine conditions
with **Match all** or **Match any**. Any matching group hides the title across
browsing, search, recommendations and collections. The preview uses only the
local catalogue; it does not search indexers or fetch every title's details.
Saving preserves metadata and watch history, and clearing the rules restores
visibility subject to the normal release, availability and viewer restrictions.

Country matches TMDB's origin/production country metadata, including
co-productions, separately from original language. For example, a country rule
for India also matches an English-language Indian production. Unknown metadata
never matches an exclusion; the preview reports country coverage. Existing
normal metadata refreshes fill gaps without a quota-consuming bulk lookup.

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
link, add a certificate under **Apps → Secure connections** and try again.
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
