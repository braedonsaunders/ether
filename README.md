<p align="center">
  <img src=".github/assets/ether-logo.svg" alt="Ether" width="360" />
</p>

<h2 align="center">The whole TMDB catalogue, inside the apps your family already uses.<br />Plex, Infuse, Swiftfin, Jellyfin, Emby, Silo, Stremio: one on-demand server, running alongside your local library.</h2>

<p align="center">
  Press <b>Play</b> on a title in Plex, Infuse, Swiftfin, Jellyfin, Emby, Silo, or Stremio. Ether resolves the best release across <b>your Usenet providers</b> and <b>your debrid accounts</b>, health-checks it, and streams the exact byte ranges the player asks for.
</p>

<p align="center">
  <a href="https://github.com/braedonsaunders/ether/releases/latest"><img alt="Latest release" src="https://img.shields.io/github/v/release/braedonsaunders/ether?label=release&color=1f6feb" /></a>
  <a href="https://discord.gg/t8ewTZ7gpa"><img alt="Discord" src="https://img.shields.io/discord/1567272301672796331?label=discord&logo=discord&logoColor=white&color=5865F2" /></a>
  <img alt="Platforms" src="https://img.shields.io/badge/runs%20on-macOS%20%7C%20Windows%20%7C%20Linux%20%7C%20Docker-555" />
  <img alt="Pure Go" src="https://img.shields.io/badge/Go-CGO__ENABLED%3D0-00ADD8" />
  <a href="LICENSE"><img alt="GPL-3.0" src="https://img.shields.io/badge/license-GPL--3.0-555" /></a>
  <a href="#what-outbound-connections-does-ether-make"><img alt="Outbound connections documented" src="https://img.shields.io/badge/telemetry-none%20(see%20outbound%20connections)-2ea043" /></a>
</p>

<p align="center">
  <a href="#things-to-know-first">Things to know first</a> ·
  <a href="#why-ether-exists">Why Ether exists</a> ·
  <a href="#every-app-one-server">Every app, one server</a> ·
  <a href="#alongside-your-local-library">Alongside your library</a> ·
  <a href="#under-the-hood">Under the hood</a> ·
  <a href="#the-owner-experience">Owner console</a> ·
  <a href="#install-in-five-minutes">Install in 5 minutes</a> ·
  <a href="#questions-youre-about-to-ask">FAQ</a>
</p>

<p align="center">
  <img src=".github/assets/overview.png" alt="Ether Server Overview" width="100%" />
</p>

## Why Ether exists

A typical automated media setup is built from several separate projects: Sonarr, Radarr, Prowlarr, SABnzbd, Overseerr, Kometa, Tautulli, and debrid mount tools. Wiring them together means download clients, path mappings, webhooks, and FUSE or WebDAV mounts that a media server has to scan. A new title still has to download, unpack, and be indexed before anyone can press Play.

Ether takes a different approach to the on-demand case. Instead of presenting virtual files for another media server to scan, **Ether speaks your apps' own languages.** It is a Plex server to Plex apps, a Jellyfin server to Infuse, Swiftfin, and Findroid, an Emby server to Emby apps, and a Silo server to Silo, backed by the live TMDB catalogue and an on-demand byte-range streaming engine.

When you open your app, **the catalogue is already there.** When you press **Play**, Ether resolves, health-checks, and streams the bytes on the fly. When you scrub to `01:14:22`, Ether seeks to the matching keyframe offset inside a remote Usenet RAR5 set or debrid HTTPS stream and serves those bytes.

| What your family experiences | What you stop maintaining |
| :--- | :--- |
| **Press Play and watch.** Titles are browsable, searchable, and playable inside stock Plex, Jellyfin, Infuse, Swiftfin, Emby, Silo, and Stremio apps. | No request queue. No *"Can you add this?"* texts. No waiting for downloads, extractions, or library scans. |
| **A Home screen that knows them.** Every viewer gets their own taste model, row ordering, and runtime-aware picks. | No single *Recently Added* row shared by everyone in the house. |
| **Pick up anywhere.** Start a movie in Plex on the living-room TV and resume it in Infuse, Swiftfin, or Silo on an iPad. | No separate watch-sync scripts or mismatched progress across apps. |
| **Plays on demand.** Automatic provider backfill, mid-stream release failover, synced subtitles, and Skip Intro / Skip Credits. | No dead mounts, stuck queues, out-of-sync `.srt` files, or broken archives to chase on a Friday night. |
| **Storage you choose.** Run in 0 GiB pass-through mode or let a multi-terabyte shelf pre-warm what your household watches next. | No cleanup rules to tune, and no need to buy storage for everything you might watch. |

---

## Every app, one server

Ether is a Plex server to one app and a Jellyfin server to another, at the same time. Turn on the protocols your household uses under **Server › Apps**.

| App / protocol | Default port | Supported clients and how they connect |
| :--- | :--- | :--- |
| **Plex** | `32400` TCP (`32410, 32412–32414` UDP GDM) | Official Plex apps on Apple TV, iOS, Android, Android TV / Shield, Fire TV, Roku, PlayStation, Xbox, Smart TVs (LG, Samsung, VIDAA), Desktop, and Web. Discovered automatically through `plex.tv` and LAN GDM. |
| **Jellyfin** | `8096` TCP (`7359` UDP discovery, or `/jellyfin` on main port) | Infuse, Swiftfin, Findroid, Streamyfin, Jellyfin for Android TV, JellyCon, and Jellyfin for Roku. Sign in with Quick Connect or an Ether viewer password. *(Web-wrapper apps such as Jellyfin for Android phones and Jellyfin Desktop are not supported.)* |
| **Emby** | `8097` TCP (or `/emby` on main port) | Native Emby apps (Android TV, iOS, Android, Roku, Smart TVs) and Infuse. Sign in with an Ether viewer password or Quick Connect. *(Apple TV Emby for TV 2.x web-shell is not supported.)* |
| **Silo** | `8090` TCP (or `/silo` on main port) | Native Silo apps for Apple TV, iOS, macOS, Android, and Android TV. Sign in with an Ether viewer password or approve a device code in `/ether/me`. |
| **Stremio** | Main HTTPS port | Any Stremio app. Each viewer creates a private addon install link in the viewer portal (`/ether/me`). **Requires an `https://` server address** (except `http://127.0.0.1` on the same machine): use your `https://*.plex.direct:32400` address when Plex is claimed, Tailscale HTTPS, or a reverse proxy with TLS. |
| **Files (WebDAV)** | Main port | Infuse, VLC, Kodi, and rclone. Each viewer creates a read-only WebDAV app password in `/ether/me`. |
| **M3U & XMLTV** | Main port | Channels DVR, TiviMate, and IPTV players. Each viewer creates private `playlist.m3u` and `guide.xml` links in `/ether/me`. |
| **IPTV apps (Xtream)** | Main port (`/xtream`) | TiviMate, Televizo, IPTV Smarters and other Xtream-compatible players get Live TV, movies and series. Off by default; turn it on under **Apps**, then create a revocable login per device (one connection each). No timeshift or catch-up. |
| **DLNA** | Local network | LAN TVs and media players with a ContentDirectory browser, governed by the owner-selected guest viewer's library and rating rules. |

- **Viewers are protocol-neutral:** Ether keeps its own viewer accounts, and Plex is optional. If you connect Plex, people you share your Plex server with appear automatically as Ether viewers and can approve Jellyfin Quick Connect or Silo device pairing in `/ether/me` using their Plex sign-in.
- **Cross-app progress and preferences:** Watch history, resume positions, ratings, and subtitle preferences (mode, language order, and styling) live on the Ether viewer account. Setting subtitle preferences in Silo or Jellyfin applies across every app. Silo Watch Party and Jellyfin SyncPlay clients also share a common room clock.
- **One playback engine:** Every protocol routes through one session broker (`internal/sessions`). Concurrent stream limits, bandwidth budgets, household safety rules, and owner **Stop** actions apply across all apps together, and concurrent plays of the same release share one upstream fetch.
- **Verified protocol conformance:** Jellyfin (target 12.4), Emby (target 4.10.2), and Silo responses are verified offline against recorded reference corpora (`internal/conform`, `cmd/conformdiff`) with zero blocking diffs. Plex responses are checked against recorded Plex Media Server fixtures (`internal/conform/plex`, `cmd/conformdiff`, `cmd/plexparity`) with a CI gate on load-bearing fields.
- **No borrowed web clients:** Ether ships no third-party web client code. Opening `/web` on a Jellyfin or Emby listener serves a translated Ether page naming the supported native apps and how to connect.

---

## Alongside your local library

Keep Plex Media Server, Jellyfin, or Emby and the tools that curate the files you own. Ether runs alongside them for on-demand titles outside your local disk collection. 

| If you run today | What Ether does instead |
| :--- | :--- |
| **Plex Media Server, Jellyfin, or Emby** *(for streaming)* | Clean-room implementation of the Plex, Jellyfin, Emby, Silo, Stremio, WebDAV, M3U/XMLTV, and DLNA protocols from one server. *(Keep your existing server for local files on disk.)* |
| **Sonarr & Radarr** | Resolution happens at **Play** time (or during detail-page warmup) using your quality profiles, a draggable "What matters most" ranking ladder, Never play filters, edition rules, release pins, and **TRaSH Guides** or **Profilarr (Dictionarry)** profile imports. |
| **Prowlarr & Jackett** | Native Newznab and Torznab manager with daily quota tracking, `Retry-After` backoff, result caching, 1-click **Prowlarr import with tag sync**, a built-in **Cardigann engine** for the definitions you add (no bundled definitions), and **Stremio stream addons** as sources. |
| **SABnzbd, NZBGet, NzbDAV, AltMount** | Native NNTP connection pools, cross-tier busy spillover, learned connection limits, wire-pipelined `BODY` fetching, SWAR yEnc decoding, and direct byte-range seeking inside store-mode RAR4, RAR5, and 7z sets (including password-protected stored archives). |
| **zurg, rclone, Riven, plex_debrid** | Native HTTPS range streaming for **Real-Debrid, TorBox, AllDebrid, Premiumize, Debrid-Link, and Offcloud**, including single-episode extraction from torrent season packs without FUSE or WebDAV mounts. |
| **Overseerr & Jellyseerr** | Released titles need no requests. Viewers' Plex and Trakt Watchlists sync automatically; newly playable titles appear in a *Now available* row on that viewer's Home, and the owner can be alerted. |
| **Kometa (Plex Meta Manager)** | Live visual poster studio and rule-based collection builder in the browser. Renders `4K / DV / Atmos` and rating badges in pure Go on demand from verified release facts. |
| **Bazarr** | Automatic OpenSubtitles matching, on-pick subtitle timing correction (against embedded text tracks or speech audio), mojibake repair, and piecewise dialogue alignment across different cuts without downloading video. |
| **Tautulli & Notifiarr** | Built-in real-time session monitor, Plex Web Dashboard `/statistics` support, viewing analytics (7 to 365 days), provider throughput graphs, weekly HTML digests, and rule-filtered alerts (Discord, Slack, Telegram, Push, Email, ntfy, Gotify, or webhook). |
| **Maintainerr** | Self-evicting segment cache with repeat-watch retention credit and rolling TV episode preloading that slides forward as your household watches. |
| **ErsatzTV, Tunarr, Threadfin** | Turn any collection, filter, or weighted mix into a 24/7 virtual TV channel with a paintable weekly schedule grid alongside M3U/XMLTV and Xtream feeds, served over HLS and DASH. |

---

## Under the hood

```
     Plex · Infuse · Swiftfin · Findroid · Jellyfin · Emby · Silo · Stremio · WebDAV · DLNA
                                    │  browse · search · Play · scrub
                                    ▼
  ┌──────────────────────────────────────────── ETHER ────────────────────────────────────────────┐
  │  App Protocols           Plex · Jellyfin · Emby · Silo · Stremio · WebDAV · M3U/XMLTV · DLNA  │
  │  Shared Experience       Personalization · Taste models · Unified age ratings · 17-hub search │
  │  Session Broker          Capability registry · Stream grants · Shared limits · Watch Party    │
  │  Smart Resolver          Ranked quality ladder · TRaSH formats · Measured source racing       │
  │  PAR2 & Health Oracle    16 KiB MD5 de-obfuscation · Mid-file BODY probes · Slice checksums   │
  │  Zero-Copy Streamer      Seekable RAR4/RAR5/7z (plain & AES) · HTTPS ranges · Disk/NAS tier   │
  │  Subtitle & Cut Aligner  On-pick timing sync · Mojibake repair · Piecewise dialogue solver    │
  │  Media & GPU Transcode   Direct play · Copy remux · HW ABR HLS/DASH/HTTP · RTX Video VSR      │
  └──────────────────────────────────┬───────────────────────────────────────┬────────────────────┘
                                     │                                       │
                        Usenet Providers (Primary + Backfill)       Debrid Accounts (RD · TorBox · AD · PM · DL · OC)
                        + Newznab Indexers & Cardigann Sites        + Torznab, Stremio Addons & Cardigann
```

### 1. Native Usenet and debrid streaming without extraction
* **Lazy store-mode archive indexing (including encrypted sets):** To open a 60-part RAR4, RAR5, or 7z post on Usenet, Ether reads only the **first, second, and last volume headers** to infer the uniform layout and expose a seekable `io.ReaderAt` immediately, verifying volume boundaries lazily before serving bytes and supporting bounded native AES decryption on password-protected stored RAR4/RAR5/7z sets when the password is supplied in the NZB metadata.
* **PAR2 as a zero-cost de-obfuscation and integrity oracle:** Ordinary playback does not wait on full PAR2 repair (bounded repair of missing byte ranges is opt-in). Instead, Ether fetches the `<1 MiB` `.par2` index packet in parallel with the first article, matches the MD5 of the first `16 KiB` of obfuscated posts against PAR2 file descriptions to recover filenames and volume order, and verifies slice checksums on the spot. Repost message-IDs are kept as per-segment alternates.
* **Truthful mid-file health checks and live hedging:** Providers often answer `223` to `STAT` for DMCA-removed articles that fail with `451` on `BODY`. Ether combines spread `STAT` checks with mid-file `BODY` yEnc-header reads, bails out on the first confirmed miss across all providers, spills over to the next primary provider when connections are busy, and hedges stalled live article reads on spare capacity.
* **Surgical Matroska damage concealment and live failover:** Priority semaphores protect live reads over read-ahead and background prefetch (and yield debrid prefetch to mid-file range seeks). If an article is missing mid-stream inside an unlaced Matroska cluster, Ether skips unaffected clusters via container cues and conceals the gap while warming an alternate release in the background. If a release stalls or fails, Ether swaps to another release at the same scene with matched audio loudness.

### 2. Server-side NVIDIA RTX Video Super Resolution (`nvenc-ngx-vsr`) and GPU pipelines
90s sitcoms, classic anime, and DVD-era 480p/576p/720p shows look soft on a 4K TV, and client-side upscalers exist on only a few devices.
* **Tensor-Core neural upscaling on the server:** On Windows hosts with an NVIDIA RTX GPU and `NVEncC64` (with NVIDIA NGX VSR runtime DLLs), Ether can run **NVIDIA RTX Video Super Resolution** inside the server's HLS/DASH transcode pipeline, cleaning compression artifacts and reconstructing edges before the stream reaches an Apple TV, Roku, iPad, or browser. It takes 8-bit SDR, progressive sources (H.264, HEVC, MPEG-2, VP9, AV1) and outputs H.264; interlaced sources, selected subtitles, and audio that needs converting use the ordinary transcoder.
* **Self-protecting ABR and fallback:** RTX VSR is opt-in (off by default) with owner-configurable source/target resolution caps (for example `<=720p -> 1080p`), quality tiers, and concurrency limits. Enhancement is reserved for the top ABR rendition; if client bandwidth dips or the GPU concurrency limit is reached, Ether drops to ordinary hardware or software ABR renditions without interrupting playback.
* **Probed FFmpeg capabilities:** At startup, Ether probes the installed `ffmpeg` for available filters and hardware encoders across NVIDIA NVENC, Intel QuickSync, VAAPI, and Apple VideoToolbox. Builds without `libzimg` (`zscale`) use built-in `colorspace`/`scale` filters for SDR conversions and `libplacebo`, OpenCL, or hardware tone mapping for HDR-to-SDR conversions.

### 3. Subtitle timing correction and piecewise dialogue alignment
* **On-pick timing correction and mojibake repair:** Subtitles are downloaded only when a viewer explicitly selects one. When a viewer picks an external text subtitle, Ether aligns its cue times against an embedded text subtitle track read via container byte ranges (or speech-energy cross-correlation across short audio windows), fitting both constant offset and frame-rate ratio (`23.976` / `24` / `25` fps) within a bounded time budget. Mis-decoded Windows-1252/Latin-1 UTF-8 text (mojibake) is repaired automatically, and viewers can upload `.srt`, `.vtt`, `.ass`, or `.ssa` files directly from Plex Web.
* **Cross-cut alignment:** When a stream fails over from a Theatrical WEB-DL to an Extended Blu-ray rip, or when **TheIntroDB** skip markers were timed against a different cut, Ether aligns the subtitle dialogue timelines with a piecewise-linear solver without downloading video, accounting for studio logo offsets, PAL speedup, and inserted scenes, and stops *Skip Credits* right before mid- or post-credits stinger dialogue.

### 4. Personalization across your apps
Every person's Home experience is assembled for *them*, *their device*, and *the current hour*:
* **Per-viewer taste models:** Tracks affinities across genres, keywords, directors, actors, decades, and languages with exponential time decay, separating completions and comfort rewatches from early abandons and unplayed impressions.
* **Hierarchical Thompson-sampling bandits and self-gated re-ranker:** Row ordering uses a Beta-Bernoulli bandit that blends hand-tuned priors, server-wide evidence, and the viewer's own play rate, held stable during an active browsing visit. A background logistic-regression re-ranker activates only when it beats the baseline on held-out AUC.
* **Server-driven Home and virtual libraries:** Plex and Silo receive the full server-driven Home. For Jellyfin and Emby apps (including Infuse, Swiftfin, Findroid, and Streamyfin), Ether publishes each viewer's top personalized rows as virtual libraries in `/UserViews` (*For You*, *Because you watched…*, *Trending*), populates Jellyfin's *Suggestions* tab (`/Movies/Recommendations`), and seeds default home section ordering for new viewers while keeping *Continue Watching*, *Next Up*, and *Latest* per viewer.

---

## The owner experience

Everything is managed through `/ether/`, a fast, keyboard-friendly web console with live previews, slide-over drawers, an **Overview Next steps** bar, and full translations in **19 languages** alongside English.

The sidebar groups everything by what you are doing:
- **Overview, Activity & Viewers** at the top for live sessions, 30-day playback history, and household accounts.
- **Library:** **Catalog** (browser, three New releases modes, and ordered show/hide **Rules**), **Collections**, **Home** (Home Designer), **Live TV**, **Calendar**, and **Metadata** (Ratings, Artwork, Episode order).
- **Streaming:** **Sources** (Debrid, Usenet, Indexers, Stats on one level), **Quality** (Profiles, Rules, Check quality, Searching, Blocklist, Pinned, History), **Cache**, and **Playback**.
- **Server:** **General** (every app's port and address under **How apps connect**, plus an optional outbound HTTP/SOCKS5 proxy for source traffic), **Apps**, **Integrations**, **Notifications**, **Cluster**, and **Maintenance** (Backups & import, Updates, Health, Logs). Press ⌘K (Ctrl+K elsewhere) to search pages and individual settings; choosing a setting opens its page, expands **More options** if needed, and highlights the row.

### Release profiles, sources, and new-release modes
Quality profiles are scoped to a media type, the way Radarr and Sonarr work: every profile is for **Movies** or for **Shows**, and each media has its own **At home** and **Away** default. **Quality › Profiles** has a **Movies | Shows** picker; the profile table, its default badges, new profiles and imported presets follow the selected media. Import presets from **TRaSH Guides**, **Profilarr** or pasted Radarr/Sonarr JSON (a Radarr profile becomes a movie profile, a Sonarr one a show profile), and turn on **Keep imported profiles in sync** to pick up guide changes on a schedule or with **Sync now**. Upgrading from 0.3.4 splits each existing profile into a movie and a show copy with the same name, so every rule and default keeps choosing exactly what it did before.

Each profile opens in a drawer organized in the order Ether evaluates candidates:
1. **Never play:** resolution and runtime-based size bounds for movies and episodes, *Allow CAM*, *Allow HDR*, *Allow Dolby Vision*, and *Only play* / *Never play* rules.
2. **What matters most:** a single numbered, drag-to-reorder ranking ladder (*Prefer* rules, *Audio language*, *Connection fit*, *Resolution*, *Release type*, *HDR*, *Played fine recently*, *Video codec*, *Audio format*, *Larger file*).

**Quality › Rules** pick a profile per device, app, platform or viewer (first match wins) and can apply to movies, shows or both; a rule for both uses the same-named profile of each media. **Check quality**, a tab of its own, shows the quality a viewer gets in a given app, at home or away, for a movie or a show, and which rule decided. A profile’s editor sets the Away default, and **Remote bitrate limit** sets a shared limit in Mbps. **Which source goes first** applies to every profile: *Start fastest*, *Usenet first*, *Debrid first*, *Always Usenet*, or *Always cached debrid*. The **Version menu** sets how many same-quality versions to offer.

In **Catalog › Settings**, **New releases** offers three modes: **When they can play** (recommended: new movies appear once Ether finds a playable release matching your quality profile), **On digital release** (a digital or physical release date has passed), or **Right away**. **Upcoming content** lists unreleased movies and episodes, and is off by default. Content ratings across **Catalog › Settings › Rules** and viewer content rating limits use a unified age-level model so equivalent ratings (`PG`, `TV-PG`, regional `6+`/`7+`) match together.

### Taste intelligence and viewer accounts
Inspect what the personalization engine has learned for each viewer alongside age-level rating limits, time windows, stream limits, cross-app subtitle preferences, and read-only **From Plex** defaults (language preferences, share filters, and download permissions synced from `plex.tv`). Add local viewers or share one-time invite links from the **Add viewer** drawer.

<table>
  <tr>
    <td width="50%" valign="top">
      <a href=".github/assets/viewer-taste.png"><img src=".github/assets/viewer-taste.png" alt="Viewer Taste Profile & Recommendations" width="100%" /></a>
      <p align="center"><sub><b>Per-viewer taste profile:</b> learned affinities, top picks, confidence, and viewing habits</sub></p>
    </td>
    <td width="50%" valign="top">
      <a href=".github/assets/viewers.png"><img src=".github/assets/viewers.png" alt="Viewers & Household Policies" width="100%" /></a>
      <p align="center"><sub><b>Viewers and household guardrails:</b> activity totals, age presets, From Plex defaults, and stream limits</sub></p>
    </td>
  </tr>
</table>

### Home Designer and Collections Studio
Compose your server's Home screen with global rules and per-viewer overrides, previewing any viewer's live rows before saving. Build rule-based or **Look Ahead** (upcoming) collections visually in **Collections**, or let viewers type natural descriptions like **`"80s sci-fi under 2 hours"`** into search to generate an instant browsable collection (with zero AI required, and optional local or API language-model refinement when enabled under **Integrations › Language model**).

<table>
  <tr>
    <td width="50%" valign="top">
      <a href=".github/assets/home-designer.png"><img src=".github/assets/home-designer.png" alt="Home Designer Live Preview" width="100%" /></a>
      <p align="center"><sub><b>Home Designer:</b> live per-viewer row composition, pinning, and scheduling</sub></p>
    </td>
    <td width="50%" valign="top">
      <a href=".github/assets/collections.png"><img src=".github/assets/collections.png" alt="Smart Collections & Generated Artwork" width="100%" /></a>
      <p align="center"><sub><b>Smart Collections:</b> rule builder, generated posters, and split preview workspace</sub></p>
    </td>
  </tr>
</table>

### Live poster studio, On Air channels and 24/7 Live TV guide
Design versioned poster overlays (`Cinema`, `Spotlight`, `Essential`) with IMDb, Rotten Tomatoes, Letterboxd, and TMDB scores, or connect self-hosted PostersPlus or XRDB, hosted BetterPosters or Rating Poster Database (RPDB), or a custom URL template, each set up under **Integrations** and picked as the **Poster source** under **Metadata › Artwork**. For claimed Plex servers, **Apps › Plex › Account** also offers an optional **Use Plex's metadata for Plex apps** toggle (off by default). **On Air** turns virtual channels into continuous TV channels with generated channel looks, break reels, personal channels for each viewer, off-air and can't-play screens that splice into the same stream, and a **Style Studio** for on-air graphics. In **Live TV**, combine M3U/XMLTV and Xtream feeds (IPTV providers live under **Sources › IPTV**, and an Xtream provider can also supply movies and shows) with 24/7 virtual channels built in a single drawer with a paintable weekly schedule grid in your timezone, playout modes (in order, shuffle, random, marathon, newest first), and repeat-avoidance windows.

<table>
  <tr>
    <td width="50%" valign="top">
      <a href=".github/assets/metadata.png"><img src=".github/assets/metadata.png" alt="Live Poster Overlay Studio" width="100%" /></a>
      <p align="center"><sub><b>Metadata and Poster Studio:</b> pure-Go versioned badges from verified release facts</sub></p>
    </td>
    <td width="50%" valign="top">
      <a href=".github/assets/livetv.png"><img src=".github/assets/livetv.png" alt="Live TV Guide & Virtual Channels" width="100%" /></a>
      <p align="center"><sub><b>Live TV and Virtual Channels:</b> paintable weekly schedule, M3U/XMLTV, and HLS/DASH playback</sub></p>
    </td>
  </tr>
</table>

### Adaptive media cache, guided recovery, and PostgreSQL clustering
Dial your storage footprint from **Pass-through (0 GiB)** to **Everyday (32 GiB)** up to **Edge library (20 TiB)** with household-aware movie preloading and rolling TV episode windows on local disk or a **Shared NAS**. **Maintenance › Backups & import** provides 1-click guided restore with automatic rollback, in-app service restart, and a startup status and recovery page. Start on standalone SQLite, and when you need multi-node scale, migrate to an active-active multi-node cluster on a **PostgreSQL 14+ database you provide** through the guided **Cluster** wizard (Ether does not make PostgreSQL itself highly available). On **Kubernetes**, the Helm chart (`packaging/helm/ether`, see [docs/kubernetes.md](docs/kubernetes.md)) can also run **server roles**: besides full nodes, a pod can be a dedicated **source** or **transcode** (CPU or GPU) node, and playback moves to another node with no stall if one is lost mid-stream. Specialist roles are Kubernetes-only and need PostgreSQL; the normal installers and Docker Compose always run one full server.

<table>
  <tr>
    <td width="50%" valign="top">
      <a href=".github/assets/cache.png"><img src=".github/assets/cache.png" alt="Adaptive Cache & Household Preloading" width="100%" /></a>
      <p align="center"><sub><b>Adaptive media cache:</b> rolling household shelves, warm windows, and Shared NAS support</sub></p>
    </td>
    <td width="50%" valign="top">
      <a href=".github/assets/cluster.png"><img src=".github/assets/cluster.png" alt="Standalone SQLite & PostgreSQL Cluster Wizard" width="100%" /></a>
      <p align="center"><sub><b>Cluster Manager:</b> guided migration from SQLite to an active-active cluster on PostgreSQL</sub></p>
    </td>
  </tr>
</table>

---

## Install in five minutes

| Platform | Download / image | Details |
| :--- | :--- | :--- |
| **macOS 13+** | [`Ether-<version>.dmg`](https://github.com/braedonsaunders/ether/releases/latest) | Universal menu-bar app (Apple Silicon & Intel) with bundled FFmpeg. *(Ad-hoc signed, not notarized: if macOS blocks it, use System Settings › Privacy & Security › Open Anyway.)* |
| **Windows (x64)** | [`EtherSetup-<version>.exe`](https://github.com/braedonsaunders/ether/releases/latest) | Windows service and system tray app with bundled FFmpeg and automatic firewall rule. |
| **Linux** | [`.deb` / `.rpm`](https://github.com/braedonsaunders/ether/releases/latest) (`amd64`, `arm64`) | Dedicated `ether` systemd service, hardware transcoding group setup (`render`/`video`), bundled FFmpeg. |
| **Docker** | `ghcr.io/braedonsaunders/ether:latest` | Multi-arch (`amd64`, `arm64`). Ready-made templates for **Unraid, TrueNAS SCALE, and Proxmox**, plus Synology instructions, in [`packaging/community`](packaging/community/README.md). |

PR previews (`linux/amd64` only) are published for PRs whose author has write access to this repository, from a branch here or in the author's own fork, as `ghcr.io/braedonsaunders/ether:pr-<number>`. **Previews are unreviewed PR code: only run one if you trust its author, and never in place of a release.** The `pr-<number>` tag moves with each new head. A second tag, `pr-<number>-<branch>-<12-char head SHA>`, is never replaced; for a strictly immutable reference, pull by the digest shown in the publish run. Eligibility is checked again before every publication, previews remain after a PR closes, and preview tags never collide with release tags.

```yaml
services:
  ether:
    image: ghcr.io/braedonsaunders/ether:latest
    container_name: ether
    restart: unless-stopped
    network_mode: host            # Linux only: enables LAN GDM auto-discovery and UPnP
    environment:
      TZ: Etc/UTC
      # ETHER_PORT: "32450"       # already running Plex on 32400? pick another port
    volumes:
      - ./config:/config
      - ./cache:/cache            # used automatically on a fresh install
    # devices: ["/dev/dri:/dev/dri"]   # optional: Intel QuickSync / VAAPI hardware transcoding
    # Docker Desktop (Windows and macOS) or Linux bridge mode:
    # Remove network_mode: host above and uncomment ports:
    # ports:
    #   - "32400:32400/tcp"       # owner console, Plex, and /jellyfin, /emby, /silo paths
    #   - "8096:8096/tcp"         # optional: dedicated Jellyfin port (not needed if using :32400/jellyfin)
    #   - "8097:8097/tcp"         # optional: dedicated Emby port (not needed if using :32400/emby)
    #   - "8090:8090/tcp"         # optional: dedicated Silo port (not needed if using :32400/silo)
```

> **Docker Desktop (Windows and macOS):** Docker Desktop runs containers inside a Linux VM where `network_mode: host` does not publish ports to your computer or LAN. Comment out `network_mode: host` and uncomment `ports: ["32400:32400/tcp"]` (plus `8096:8096/tcp` only if you want the dedicated Jellyfin port instead of `http://HOST:32400/jellyfin`).

Open **`http://localhost:32400/ether/`** and complete the setup wizard. If Plex already uses 32400, Ether moves to 32450, and the log shows the address.

1. **Choose your language** for the console and default household audio and subtitle tracks.
2. **Choose your apps** (Plex, Jellyfin, Emby, Stremio, Silo, DLNA, Files/WebDAV, M3U & XMLTV) and **create your owner account**: sign in with Plex and claim the server, or create a local Ether owner account (password at least 12 characters).
3. **Add a streaming source and indexer:** a debrid account (**Real-Debrid, TorBox, AllDebrid, Premiumize, Debrid-Link, Offcloud**), a Usenet provider, or both, plus a Stremio addon link, Torznab/Newznab URL, or Prowlarr import (tracker sites can be added later under **Sources › Indexers**).
4. **Choose a cache preset** (an SSD with 32–50 GiB is plenty to start).
5. **Catalogue:** official builds include a TMDB key; custom builds without a bundled key need a free [TMDB API key](https://www.themoviedb.org/settings/api). Add Trakt, Simkl, OpenSubtitles, or a local language model (Ollama/LM Studio) later under **Integrations**.

**Documentation:** [**Installation & Clustering Guide**](docs/install.md) · [**Owner & User Guide**](docs/user-guide.md) · [**Multi-App Architecture & Watch Party**](docs/apps.md) · [**Performance Architecture**](docs/performance.md) · [**NAS & Homelab Templates**](packaging/community/README.md)

---

## Things to know first

- **What Ether is not:** Ether is not a local-file media server. It does not scan folders of video files on your disk, and it does not replace Plex Media Server, Jellyfin, or Emby for your personal media collection. Ether also ships no media content, no Usenet providers, no debrid accounts, no indexers, and no tracker definitions.
- **What accounts you need:**
  1. Catalogue metadata and artwork come from TMDB. Official builds include a TMDB key; custom builds without a bundled key need a free [**TMDB API key**](https://www.themoviedb.org/settings/api).
  2. At least one **streaming source**: a Usenet provider, a debrid service (Real-Debrid, TorBox, AllDebrid, Premiumize, Debrid-Link, or Offcloud), or both.
  3. At least one **indexer**: a Newznab or Torznab indexer, a Prowlarr instance, or a Stremio stream addon.
  4. **Optional:** a Plex account if you want to use official Plex apps. Jellyfin, Emby, Silo, Stremio, WebDAV, M3U/XMLTV, and DLNA work with local Ether accounts alone.
- **System requirements:**
  - **CPU and RAM:** 2 CPU cores and 1 GiB of RAM are enough for direct play, container remuxing, and audio transcoding. Video transcoding requires a supported GPU (NVIDIA NVENC, Intel QuickSync, VAAPI, or Apple VideoToolbox) or 4+ CPU cores for software encoding.
  - **Disk space:** Configurable from **0 GiB** (pass-through mode) to **32+ GiB** on an SSD for the default segment cache, or multiple terabytes if you enable household preloading.
- **Outbound network activity:** Ether sends no analytics or usage telemetry. By default it checks GitHub's signed release feed once a day for updates and queries TheIntroDB (`api.theintrodb.org`) for intro and credits skip timestamps during playback; both can be turned off in the console. When Plex is claimed and enabled, it communicates with `plex.tv`; turning the Plex app off stops Plex sign-in, publishing, sharing, and metadata traffic. See [What outbound connections does Ether make?](#what-outbound-connections-does-ether-make) for the full list.
- **Legal responsibility:** You supply your own accounts, providers, and indexers, and you are responsible for complying with applicable laws and service terms. Read [**LEGAL.md**](LEGAL.md).

---

## Questions you're about to ask

<details>
<summary><b>Do I need Plex Pass or Emby Premiere?</b></summary>

For **Jellyfin, Swiftfin, Findroid, Silo, Stremio, WebDAV, and DLNA**, no paid pass is needed at home or away.

For **Plex apps**, local network streaming is free. **To stream in official Plex apps outside your home, yes:** Ether follows Plex's rules like a standard Plex Media Server, so remote playback in Plex apps requires Plex Pass on the server owner's account or a Remote Watch Pass on the viewer's account.

For **Emby apps**, Emby's own per-platform app unlock or Emby Premiere rules apply (see Emby's Premiere feature matrix).

Ether never emulates, unlocks, or bypasses Plex Pass, Emby Premiere, or any paid client feature.
</details>

<details>
<summary><b>Which apps work, and why are some web-wrapper apps unsupported?</b></summary>

- **Plex:** Every official Plex client: **Apple TV, iPhone, iPad, Android, Android TV / Nvidia Shield, Fire TV, Roku, PlayStation, Xbox, Smart TVs (LG, Samsung, VIDAA), Plex Desktop, and Plex Web.** (Plex for LG and Plex for VIDAA receive one automatic media choice so their playback settings remain stable; Play Version is not offered there.)
- **Jellyfin:** Native clients including **Infuse, Swiftfin, Findroid, Streamyfin, Jellyfin for Android TV, JellyCon, and Jellyfin for Roku.**
- **Emby:** Native Emby clients (**Android TV, iOS, Android, Roku, Smart TVs**) and **Infuse**.
- **Silo:** Native **Silo** apps for **Apple TV, iOS, macOS, Android, and Android TV**.
- **Stremio, WebDAV, DLNA, and M3U/XMLTV:** Stremio apps, WebDAV players (Infuse, VLC, Kodi), LAN DLNA TVs, and IPTV clients (Channels DVR, TiviMate).

**Unsupported web-wrapper apps:** Ether does not bundle third-party web clients (`jellyfin-web` or Emby Web). Client apps that are wrappers around a server-hosted web bundle, specifically **Jellyfin for Android phones**, **Jellyfin Desktop**, and **Emby for TV 2.x on Apple TV**, are unsupported. On Android phones use **Findroid** or **Streamyfin**; on Apple TV use **Infuse**, **Swiftfin**, **Silo**, or **Plex**.

**Emby for Windows:** Compatibility is unverified. The current native Windows app bundles its own UI, but Ether does not serve Emby's `/web` assets; connections that require those assets will not work. Older hash-only Emby sign-in clients are also unsupported: Ether requires the plaintext `Pw` field to verify its stored password hash.
</details>

<details>
<summary><b>How do I connect Stremio, and why does it require HTTPS?</b></summary>

Stremio apps refuse plain `http://` addon URLs unless the address is `http://127.0.0.1` on the same device. To install a viewer's Stremio addon from `/ether/me` on phones, TVs, or other computers, serve Ether over **`https://`** using one of these options:

1. **Claimed Plex server (`plex.direct`):** When Plex is claimed and enabled, Ether automatically obtains a `*.plex.direct` TLS certificate and `/ether/me` uses that `https://` address for the Stremio install link.
2. **Tailscale HTTPS:** Enable HTTPS certificates in your tailnet and run `tailscale serve --bg 32400`, then open Ether at your `https://<machine>.<tailnet>.ts.net` address.
3. **Reverse proxy with TLS:** Put Caddy, Nginx, Traefik, or Cloudflare Tunnel in front of port `32400` with a valid certificate and open `/ether/me` through that `https://` domain.
</details>

<details>
<summary><b>Can I use Ether without a Plex account?</b></summary>

Yes. In the setup wizard, select Jellyfin, Emby, Silo, Stremio, WebDAV, or DLNA without Plex, and create a local Ether owner account. Viewers sign in with local usernames and passwords, invite links, or Quick Connect / device pairing in `/ether/me`. You can enable or disable Plex at any time under **Server › Apps**.
</details>

<details>
<summary><b>Does watch progress sync between Plex, Infuse, Jellyfin, Emby, and Silo?</b></summary>

Yes. Watch history, in-progress resume offsets, favourites/ratings, and subtitle preferences belong to the viewer's Ether account. Starting an episode in Plex on a TV and opening Infuse, Swiftfin, or Silo on a phone or tablet resumes from the same spot, and real-time WebSocket updates notify connected Jellyfin, Emby, and Silo clients when watch state changes. If a viewer also connects their own Plex account under **Viewers**, Ether can keep watched marks and ratings in step with that account in either or both directions.
</details>

<details>
<summary><b>Does it download whole movies? How much disk space do I need?</b></summary>

By default, no. Ordinary playback fetches only the byte ranges the player is watching plus a bounded read-ahead window (a 20 Mbit/s stream uses about 150 MB per minute) into a self-trimming disk cache. If you have a tiny disk, choose **Pass-through**. If you have terabytes to spare, you can opt into **Managed preloading** (the **Cinema node** or **Edge library** cache presets), which preloads top household movies and a rolling window of next TV episodes during idle hours.
</details>

<details>
<summary><b>Won't this hammer my indexers and Usenet providers?</b></summary>

Ether is built not to:
- **No background crawling of the catalogue:** Indexers are queried when a viewer opens or plays a title, when the next episode is about to play, and on a slow schedule for watchlisted items. *When they can play* follows each indexer's recent releases the way Radarr/Sonarr RSS does: one feed request per indexer every 30 minutes, matched locally against every waiting title, plus a single targeted search when a movie enters the window or reaches its digital release date (one source at a time, stopping at the first hit).
- **Bounded daily budgets:** Background work has its own daily allowance per indexer: 10% of the indexer's API limit (the one you set, the one it reports, or else 1,000/day, the strictest private tier), and work nobody waits on (warm-ups, preloads) shares another 10% for searches and 10% of the NZB download limit. A replayed day of a two-Usenet-indexer, four-site household used 50–64 background requests per indexer. Downloaded NZBs are reused from disk, so a release is grabbed once, and background requests to one indexer are spaced at least three seconds apart.
- **Detail-page reuse:** Opening a detail page resolves, probes, and caches the keyframe index for 10 minutes; pressing Play reuses that work with no extra queries.
- **Quota and `Retry-After` discipline:** Per-indexer daily API and download caps are tracked durably across restarts; `429`, `Retry-After` and Newznab API-limit errors pause that indexer cleanly while falling back to others, and concurrent `FetchNZB` requests for the same release are coalesced.
- **Minimal health sampling:** Health checks sample about 12 spread articles and stop on the first confirmed miss across all providers.

*Always check your indexer's terms of service; some private indexers restrict streaming engines.*
</details>

<details>
<summary><b>Will my debrid account get flagged for multiple IPs?</b></summary>

No. Every stream is fetched by your Ether server and served from Ether to the viewer. Your debrid provider sees only your Ether server's IP address (or your configured outbound proxy IP under **Server › General**), even when family members stream remotely from their phones or TVs. If you run a multi-node cluster, keep your nodes behind the same WAN IP or outbound proxy; Ether's **Cluster** page warns you if nodes egress from different public IPs.
</details>

<details>
<summary><b>How does Ether treat my Real-Debrid account?</b></summary>

Real-Debrid no longer reports which torrents it holds in bulk, so Ether adds only the top few candidates for a title you open, keeps the ones that are ready immediately, and removes the rest. Answers are remembered so the same torrent is not checked again, API calls are limited to four at a time, and rate limits pause the account cleanly. Files Real-Debrid refuses to serve (`infringing_file`) are removed and skipped in favour of the next release; when it refuses new torrents in a row, Ether pauses adding for a few minutes rather than retrying. A Stremio addon configured with your key can mark which torrents are cached, saving those checks.
</details>

<details>
<summary><b>Can I run Ether alongside my existing Plex, Jellyfin, or Emby server?</b></summary>

Yes, and if you have local media files on disk, that is the recommended setup. On the same machine, Ether notices if port `32400` or `8096` is already in use and moves its listener to a free port (such as `32450` for the main console, while keeping `/jellyfin`, `/emby`, and `/silo` available on the main port), and the **Apps** page shows the active address for every protocol. You can also import your existing Plex watch history, ratings, collections, and playlists into Ether in read-only mode.
</details>

<details id="what-outbound-connections-does-ether-make">
<summary><b>What outbound connections does Ether make?</b></summary>

Ether sends **no usage analytics, crash reports, or behavioural telemetry**. Every outbound connection has a specific purpose, and all but cluster address detection can be configured or turned off:

1. **Update check (on by default):** Ether checks the signed release feed on GitHub once a day (first check 5 minutes after startup). Turn this off under **Maintenance › Updates** (*Check for updates*).
2. **TheIntroDB skip markers (on by default):** When a title plays, Ether queries `api.theintrodb.org` by TMDB ID, season/episode number, and duration for intro, recap, and credits timestamps. Turn this off with the lookup switch under **Playback › Extras** or the TheIntroDB switch on **Integrations**.
3. **TMDB and your configured sources:** Ether queries `api.themoviedb.org` and `image.tmdb.org` with your key or the one built into official builds, plus the Usenet providers, debrid services, indexers, Stremio addons, and optional integrations (OpenSubtitles, Trakt, Simkl, ratings, artwork, notifications, language model) that you configure.
4. **`plex.tv` (only when Plex is claimed and enabled):** When the Plex app is enabled and claimed, Ether contacts `plex.tv` for PIN sign-in, server and connection registration, friend/share sync, `plex.direct` TLS certificate renewal, and public IP detection when **Remote access** is on (**Apps › Plex › Remote access**). Ether also matches titles to Plex IDs through `metadata.provider.plex.tv`. Full Plex metadata (**Apps › Plex › Account**), per-viewer Plex account watch sync, and Plex watchlist sync (both `discover.provider.plex.tv`) are off until you enable them. Turning the **Plex** app off under **Server › Apps** stops all of this `plex.tv` traffic.
5. **Features you turn on:** collection sources (StevenLu, AniList), quality-profile imports (TRaSH Guides and Dictionarry, from GitHub), indexer site definitions, the anime ID map (GitHub), the NVEncC runtime download for RTX Video Super Resolution, and trailers (YouTube) contact their own services only when you use them.
6. **Cluster address detection (clustered nodes only):** each node checks its public IP every 10 minutes (`v4.plex.tv` first, then `api.ipify.org`, `icanhazip.com`, `ifconfig.me`). This has no switch.
</details>

---

## Where it stands today

Ether is backed by 5,700+ offline unit, fault-injection, and fuzz tests, a 540k-title CI performance gate (`internal/perfharness`), recorded protocol conformance suites against Plex Media Server (`internal/conform/plex`), Jellyfin 12.4, Emby 4.10.2, and Silo (`internal/conform`), and hardware transcoding verification across Apple VideoToolbox, NVIDIA NVENC (plus optional Windows RTX Video Super Resolution), Intel QuickSync, and VAAPI.

| Capability | Status |
| :--- | :--- |
| **On Air Channels, Style Studio & Personal Channels** | **New in 0.3.5** |
| **IPTV Apps (Xtream) Server & Xtream Movies/Series Sources** | **New in 0.3.5** |
| **Kubernetes Server Roles (Source, Transcode) with Seamless Node-Loss Recovery** | **New in 0.3.5** |
| **Movie and Show Quality Profiles with TRaSH/Profilarr Import & Sync** | **New in 0.3.5** |
| **Jellyfin, Emby (Infuse, Swiftfin, Findroid) & Silo Apps** | **New in 0.3** |
| **Stremio Addon, Read-Only WebDAV, M3U/XMLTV & DLNA** | **New in 0.3** |
| **Cross-App Progress, Subtitle Preferences, Personalized Views & Watch Party** | **New in 0.3** |
| **TMDB Catalogue, Browse, Conversational Search & Collections** | Working |
| **Personalization Engine, Taste Models & Home Designer** | Working |
| **Usenet Streaming Engine, PAR2 Oracle, Health Checks & Failover** | Working |
| **Debrid Streaming (Real-Debrid, TorBox, AllDebrid, Premiumize)** | Working |
| **Debrid Streaming (Debrid-Link, Offcloud)** | Working (built from published APIs) |
| **Official Plex Apps (TV, Mobile, Desktop, Web)** | Working |
| **Pure-Go Poster Overlays, Skip Markers & On-Pick Subtitle Alignment** | Working |
| **Hardware Transcoding & Optional Windows NVIDIA RTX Video VSR** | Working |
| **Live TV Guide, Paintable Virtual Channels & M3U/XMLTV/Xtream** | Working |
| **PostgreSQL Active-Active HA Clustering & Shared NAS Cache** | Working |

**Current boundaries:**
- Usenet archives must be store-mode (`m0`, plain or password-encrypted RAR4/RAR5/7z); compressed archives and RAR-wrapped torrents are skipped during resolution.
- NVIDIA RTX Video Super Resolution (`nvenc-ngx-vsr`) requires native Windows, an NVIDIA RTX GPU, and `NVEncC64` 9.38 with NGX DLLs (installed automatically when enabled unless a custom path is configured), is off by default, targets SDR H.264 HLS/DASH streams within configured height and concurrency bounds, and falls back to ordinary ABR renditions when bandwidth or concurrency requires it.
- No bundled third-party web clients (`jellyfin-web` or Emby Web); web-wrapper apps (Jellyfin for Android phones, Jellyfin Desktop, Emby for TV 2.x on Apple TV) are unsupported.
- No Plex Relay support: remote viewers connect directly via UPnP/NAT-PMP, a port forward, Tailscale, or your reverse proxy.
- No DVR recording.

---

## Credits

Thank you to the community contributors and reporters who shaped 0.3.0:
- **FarkasLabs** for profiling and fixing the Plex settings-copy hotspot (#206) and designing the admission fence and atomic receipt commit for uncertain outbound Plex watch-state sends (#207).
- **AlexBlomkvist (`bmkhib`)** for the live CPU and allocation profile on the Plex Home watched-episode path (#203) and the Tracearr library-wide episode/season sync diagnosis (#209).
- **TheIndieArmy** for limiting Plex Web HDR remux promotion to affirmative 10-bit/PQ client profiles (#190).
- **mvanbaak** for reporting and verifying on FreeBSD the source-swap failure on FFmpeg builds compiled without `libzimg`/`zscale` (#201).
- **XenoUniv3rse** for diagnosing why `plex.tv` marked LAN connections as remote when `Secure connections` was set to `Required` (#192).
- **evulhotdog** for adding built-in TRaSH release group custom formats to quality profile imports (#172) and contributing configurable indexer User-Agent presets (#213, in 0.3.2).

---

## Legal

Ether is an independent, clean-room implementation of the HTTP protocols used by compatible media client applications. It contains no proprietary Plex, Jellyfin, Emby, or Silo code, assets, binaries, or web clients. Ether ships **no media content, no Usenet providers, no indexers, no Cardigann site definitions, no debrid accounts, and no third-party API keys** other than the TMDB catalogue key built into official builds. Owners supply their own accounts and are solely responsible for their configuration and compliance with applicable laws and service terms. See [**LEGAL.md**](LEGAL.md).

"Plex" is a trademark of Plex, Inc. "Jellyfin" is a trademark of the Jellyfin project. "Emby" is a trademark of Emby LLC. "Silo" and "Stremio" are trademarks of their respective owners. Ether is **not affiliated with, sponsored by, or endorsed by Plex, Inc., the Jellyfin project, Emby LLC, Silo, or Stremio.** This product uses the TMDB API but is not endorsed or certified by TMDB.

[**GPL-3.0 License**](LICENSE) · [**Legal**](LEGAL.md) · [**Third-Party Notices**](THIRD_PARTY_NOTICES.md)
