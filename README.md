<p align="center">
  <img src=".github/assets/ether-logo.svg" alt="Ether" width="360" />
</p>

<h2 align="center">The whole TMDB catalogue, inside the Plex app your family already uses.<br />Your on-demand server alongside Plex Media Server.</h2>

<p align="center">
  Press <b>Play</b> on a title in any official Plex app. Ether resolves the best release across <b>your Usenet providers</b> and <b>your debrid accounts</b>, health-checks it, and streams the exact byte ranges the player asks for.
</p>

<p align="center">
  <a href="https://github.com/braedonsaunders/ether/releases/latest"><img alt="Latest release" src="https://img.shields.io/github/v/release/braedonsaunders/ether?label=release&color=1f6feb" /></a>
  <img alt="Platforms" src="https://img.shields.io/badge/runs%20on-macOS%20%7C%20Windows%20%7C%20Linux%20%7C%20Docker-555" />
  <img alt="Pure Go" src="https://img.shields.io/badge/Go-CGO__ENABLED%3D0-00ADD8" />
  <a href="LICENSE"><img alt="GPL-3.0" src="https://img.shields.io/badge/license-GPL--3.0-555" /></a>
  <img alt="No telemetry" src="https://img.shields.io/badge/telemetry-none-2ea043" />
</p>

<p align="center">
  <a href="#why-ether-exists">Why Ether Exists</a> ·
  <a href="#under-the-hood">Under the Hood</a> ·
  <a href="#the-owner-experience">Product Tour</a> ·
  <a href="#alongside-your-local-library">Alongside Your Library</a> ·
  <a href="#install-in-five-minutes">Install in 5 Minutes</a> ·
  <a href="#questions-youre-about-to-ask">FAQ</a>
</p>

<p align="center">
  <img src=".github/assets/overview.png" alt="Ether Server Overview" width="100%" />
</p>

---

## Why Ether exists

A typical automated Plex setup is built from several excellent projects—Sonarr, Radarr, Prowlarr, SABnzbd, Overseerr, Kometa, Tautulli and more—each of them very good at its job. Wiring them together still means download clients, path mappings, webhooks and, for debrid users, FUSE/WebDAV mounts that Plex Media Server has to scan. A new title still has to download, unpack and be scanned before anyone can press Play.

Ether takes a different approach to the on-demand case. Instead of presenting files for Plex Media Server to scan, **Ether speaks the PMS protocol itself.** It talks directly to official Plex clients (Apple TV, iOS, Android TV, Shield, Fire TV, Roku, Smart TVs, Web) and backs the library with the live TMDB catalogue and an on-demand byte-range streaming engine.

When you open Plex, **the catalogue is already there.** When you press **Play**, Ether resolves, health-checks, and streams the bytes on the fly. When you scrub to `01:14:22`, Ether seeks to the matching keyframe offset inside a remote Usenet RAR5 set or debrid HTTPS stream and serves just those bytes.

| What your family experiences | What you stop maintaining |
| :--- | :--- |
| **Press Play and watch.** Titles are browsable, searchable, and playable inside the stock Plex app. | No request queue. No *"Can you add this to Plex?"* texts. No waiting for downloads, extractions, or library scans. |
| **A Home screen that knows them.** Every viewer gets their own taste model, row ordering, and runtime-aware picks. | No single *Recently Added* row shared by everyone in the house. |
| **It just plays.** Automatic provider backfill, mid-stream release failover, synced subtitles, and Skip Intro / Skip Credits. | No dead mounts, stuck queues, out-of-sync `.srt` files, or broken archives to chase on a Friday night. |
| **Storage you choose.** Run in 256 MiB pass-through mode or let a multi-terabyte shelf pre-warm what your household watches next. | No cleanup rules to tune, and no need to buy storage for everything you might watch. |

---

## Alongside your local library

Keep Plex Media Server and the tools that curate the files you own. Ether adds a second server in the same Plex apps for titles outside that collection. Its availability depends on your configured sources; an on-demand catalogue is not a durable local library.

The table maps familiar workflows to Ether’s on-demand features. It does not claim feature parity with these mature projects. Clustering, virtual Live TV, hardware enhancement and poster customization are optional integrations; ordinary playback needs none of them.

| If you run today | What Ether does instead |
| :--- | :--- |
| **Plex Media Server** *(for streaming)* | Clean-room implementation of the PMS HTTP/HLS/DASH/GDM protocol, `*.plex.direct` TLS, and `plex.tv` sharing. *(Keep PMS for your local files.)* |
| **Sonarr & Radarr** | Resolution happens at **Play** time (or during detail-page warmup) using your quality profiles, custom formats, edition rules, and release pins. |
| **Prowlarr & Jackett** | Native Newznab and Torznab manager with daily quota tracking, Retry-After backoff, result caching, 1-click Prowlarr import, a built-in **Cardigann YAML** engine, and **Stremio stream addons** as sources (paste an install link): torrent, NZB and HTTP stream results, looked up by IMDb, TMDB or anime database ID. |
| **SABnzbd, NZBGet, NzbDAV, AltMount** | Native NNTP connection pools, wire-pipelined `BODY` fetching, SWAR yEnc decoding, and direct byte-range seeking inside store-mode RAR4, RAR5, and 7z sets (including password-protected stored archives). |
| **zurg, rclone, Riven, plex_debrid** | Native HTTPS range streaming for **Real-Debrid, TorBox, AllDebrid, Premiumize, Debrid-Link, and Offcloud**—including single-episode extraction from torrent season packs without mounts. |
| **Overseerr & Jellyseerr** | Released titles need no requests. Viewers' Plex and Trakt Watchlists sync automatically; newly playable titles appear in a *Now available* row on that viewer's Home, and the owner can be alerted. |
| **Kometa (Plex Meta Manager)** | Live visual poster studio and rule-based collection builder in the browser. Renders `4K / DV / Atmos` and rating badges in pure Go on demand, configured in the browser. |
| **Bazarr** | Automatic OpenSubtitles matching plus **piecewise dialogue-timing alignment** that syncs subtitles and skip markers across different cuts without downloading video. |
| **Tautulli & Notifiarr** | Built-in real-time session monitor, 30-day analytics, provider throughput graphs, weekly HTML digests, and rule-filtered alerts (Discord, Slack, Telegram, Push, Email, ntfy, Gotify). |
| **Maintainerr** | Self-evicting segment cache with repeat-watch retention credit and rolling TV episode preloading that automatically slides forward as your household watches. |
| **ErsatzTV, Tunarr, Threadfin** | Turn any collection into a 24/7 virtual TV channel alongside M3U/XMLTV and Xtream feeds—served straight into Plex's native Live TV guide. |

---

## Under the hood

```
                       Official Plex Apps (Apple TV · iOS · Android TV · Web · Roku)
                                     │  browse · search · Play · scrub
                                     ▼
  ┌──────────────────────────────────────────── ETHER ────────────────────────────────────────────┐
  │  PMS Protocol Layer      Native hubs, metadata, PlayQueues, HLS/DASH, GDM, plex.direct TLS   │
  │  Personalization Engine  Per-viewer taste model · Beta-bandit row ranker · Learned re-ranker  │
  │  Smart Resolver          Quality profiles · Custom formats · Measured-latency source racing   │
  │  PAR2 & Health Oracle    16 KiB MD5 de-obfuscation · Mid-file BODY probes · Slice checksums   │
  │  Zero-Copy Streamer      Seekable RAR4/RAR5/7z (plain & AES) · HTTPS ranges · Disk/NAS tier   │
  │  Dialogue Aligner        Piecewise subtitle-cue solver for cross-cut failover & skip markers  │
  │  Media & GPU Transcode   Direct play · Copy remux · HW ABR HLS/DASH · NVIDIA RTX Video VSR    │
  └──────────────────────────────────┬───────────────────────────────────────┬────────────────────┘
                                     │                                       │
                        Usenet Providers (Primary + Backfill)       Debrid Accounts (RD · TorBox · AD · PM)
                        + Newznab Indexers & Cardigann Sites        + Torznab Indexers & Cardigann Sites
```

### 1. Native Usenet & Debrid streaming without extraction
* **Lazy store-mode archive indexing (including encrypted sets):** To open a 60-part RAR4, RAR5, or 7z post on Usenet, Ether touches only the **first, second, and last volume headers** to infer the uniform layout and expose a seekable `io.ReaderAt` immediately—verifying volume boundaries lazily before serving bytes and supporting bounded native AES decryption on password-protected stored RAR4/RAR5/7z sets when the password is supplied in the NZB metadata.
* **PAR2 as a zero-cost de-obfuscation & integrity oracle:** Ordinary playback never waits on PAR2 repair (bounded repair of just the missing byte ranges is opt-in). Instead, it fetches only the `<1 MiB` `.par2` index packet in parallel with the first article, matches the MD5 of the first `16 KiB` of obfuscated posts against PAR2 file descriptions to recover true filenames and volume order, and verifies slice checksums on the spot. If one dominant video file is identified, decoy files aren't even probed.
* **Truthful mid-file health checks:** Providers routinely answer `223` to `STAT` for DMCA-pulled articles that fail with `451` on `BODY`. Ether combines spread `STAT` checks with real mid-file `BODY` yEnc-header reads and bails out on the first confirmed missing article across all providers.
* **Surgical Matroska damage concealment & live failover:** Priority semaphores strictly protect `PriorityLive` reads over read-ahead and background prefetch. If an article is genuinely missing mid-stream inside an unlaced Matroska frame payload, Ether conceals the gap while warming an alternate release in the background—and if a release stalls or breaks, fails over to another release at the exact same scene, with the volume matched and baked-in black bars cropped.

### 2. Server-side NVIDIA RTX Video Super Resolution (`nvenc-ngx-vsr`) & GPU pipelines
90s sitcoms, classic anime, and DVD-era 480p/576p/720p shows tend to look soft on a 4K TV, and client-side upscalers only exist on a handful of devices.
* **Tensor-Core neural upscaling on the server:** On Windows hosts with an NVIDIA RTX GPU and `NVEncC64` (with NVIDIA NGX VSR runtime DLLs), Ether can run **NVIDIA RTX Video Super Resolution** directly inside the server's HLS/DASH transcode pipeline—cleaning compression artifacts and reconstructing crisp edges on SDR H.264 streams before they ever reach an Apple TV, Roku, iPad, or browser.
* **Self-protecting ABR & fallback:** RTX VSR is opt-in (off by default) with owner-configurable source/target resolution caps (e.g. `≤720p → 1080p`), quality tiers, and concurrency limits. Enhancement is reserved for the top ABR rendition; if measured client bandwidth dips or the GPU concurrency limit is reached, Ether seamlessly drops to ordinary hardware/software ABR renditions without interrupting playback or mislabeling an ordinary scaler as RTX VSR.
* **Zero-copy GPU surface transcoding:** For standard transcodes across NVIDIA NVENC, Intel QuickSync, VAAPI, and Apple VideoToolbox, Ether keeps decode, scaling, and encode on GPU hardware surfaces end-to-end and bounds software fallback worker threads.

### 3. Piecewise dialogue alignment across different cuts
What happens when a stream fails over at `00:52:18` from a Theatrical WEB-DL to an Extended Blu-ray rip—or when **TheIntroDB** skip markers were timed against a different release? Duration math lands you in the wrong scene.
* Ether aligns the **subtitle dialogue timelines** of the two releases using a piecewise-linear solver—**without downloading a single byte of video**—accounting for studio logo offsets, PAL 25fps speedup, and inserted/removed scenes.
* That same dialogue engine inspects subtitle cues inside closing credits to **detect mid- and post-credits stinger scenes**, automatically ending the *Skip Credits* marker right before the stinger begins.

### 4. Personalization inside stock Plex apps
Viewers never leave the official Plex client, yet every person's Home screen is dynamically assembled for *them*, *their device*, and *the current hour*:
* **Per-viewer taste models:** Tracks affinities across genres, keywords, directors, actors, decades, and languages with exponential time decay—separating completions and comfort rewatches from early abandons and unplayed impressions.
* **Hierarchical Thompson-sampling bandits:** Row ordering uses a Beta-Bernoulli bandit that blends hand-tuned priors → server-wide evidence → the viewer's own play rate, held stable during an active browsing visit, with fresh exploration when a new client session starts or after an idle break.
* **Self-gated learned re-ranker:** A pooled logistic-regression model trains in the background during quiet hours on 48-hour impression-to-play attribution (debiasing row and item position at inference). It **refuses to activate** until it beats the hand-tuned baseline by a strict held-out AUC margin.
* **Session-intent & Micro-Reason headers:** Clicking through 90s thrillers without pressing Play subtly lifts related rows for the next hour. Detail pages greet the viewer with a calibrated decision header:
  > **`96% Match · Directed by Denis Villeneuve · Ends at 10:45 PM`** *(resume-aware, local time, never faked for new users)*.

---

## The owner experience

Everything is managed through `/ether/`—a fast, keyboard-friendly web UI built around live previews and slide-over drawers. Zero YAML files required.

Global TMDB trending charts refresh hourly and survive restarts. Continue Watching leads the default discovery layout, followed by Trending Now. Browseable collections and discovery rows have independent placement controls, with movie-genre and TV-network/studio defaults. Catalog → Library visibility adds optional, previewable country, language, genre and numeric exclusion rules without replacing the catalogue browser or launching provider searches.

**Sources → Availability → Settings** controls the movie evidence window. It defaults to enabled for 60 days after release; change it to 1–366 days or disable the movie requirement. A suitable observed release unlocks the movie immediately during that window: this is not a mandatory wait or a guarantee of quality/playback. Episode evidence remains a separate 24-hour rule.

### Taste Intelligence & Per-Viewer Controls
Inspect what the personalization engine has learned for each viewer—their top picks, taste confidence, peak viewing hours, typical session length, and genre/person affinities—alongside granular per-person content ratings, time windows, and stream caps.

<table>
  <tr>
    <td width="50%" valign="top">
      <a href=".github/assets/viewer-taste.png"><img src=".github/assets/viewer-taste.png" alt="Viewer Taste Profile & Recommendations" width="100%" /></a>
      <p align="center"><sub><b>Per-Viewer Taste Profile</b> — learned affinities, top picks, confidence & viewing habits</sub></p>
    </td>
    <td width="50%" valign="top">
      <a href=".github/assets/viewers.png"><img src=".github/assets/viewers.png" alt="Viewers & Household Policies" width="100%" /></a>
      <p align="center"><sub><b>Viewers & Household Guardrails</b> — activity totals, age presets & stream limits</sub></p>
    </td>
  </tr>
</table>

### Home Designer & Conversational Collections
Compose your server's Home screen with global rules and per-viewer overrides, previewing any viewer's exact live rows before saving. Build smart collections visually—or let viewers type natural descriptions like **`"80s sci-fi under 2 hours"`** straight into the Plex search bar to generate an instant browsable collection (with zero AI required, and optional local/API LLM refinement when enabled).

To create a **Look Ahead** collection, use the whole-catalogue source and choose **Upcoming** under **Release dates**, then set a 1–366 day window. **Both** combines released and upcoming matches. These opt-in collections appear only in the Collections tab; normal browsing, search and existing collections remain released-only. Upcoming items show their scheduled date and cannot start playback or release searches before that date. This covers movie releases and new series premieres, not future episodes or seasons of existing shows. TMDB discovery is bounded and warns when truncated; dates refresh on the collection's configured schedule and may change. Future placeholders use base TMDB artwork; Ether overlays and custom poster providers remain for released titles.

Collection sorts include **Latest aired episode** and **Latest season premiere**. These use regular-episode dates, fetching and caching missing show dates from TMDB in bounded batches. They exclude future episodes and specials and put shows with unknown dates last. They do not search indexers or debrid providers; missing dates and capped source results are reported in the builder.

<table>
  <tr>
    <td width="50%" valign="top">
      <a href=".github/assets/home-designer.png"><img src=".github/assets/home-designer.png" alt="Home Designer Live Preview" width="100%" /></a>
      <p align="center"><sub><b>Home Designer</b> — live per-viewer row composition, pinning & scheduling</sub></p>
    </td>
    <td width="50%" valign="top">
      <a href=".github/assets/collections.png"><img src=".github/assets/collections.png" alt="Smart Collections & Generated Artwork" width="100%" /></a>
      <p align="center"><sub><b>Smart Collections</b> — rule builder, generated posters & split preview workspace</sub></p>
    </td>
  </tr>
</table>

### Live Poster Studio & 24/7 Live TV Guide
Design versioned poster overlays (`Cinema`, `Spotlight`, `Essential`) with IMDb, Rotten Tomatoes, Letterboxd, and TMDB scores. Quality badges (`4K`, `HDR`, `Dolby Vision`, `Atmos`) are **strictly truthful**—rendered only when backed by observed media facts. In **Live TV**, combine M3U/XMLTV and Xtream feeds with 24/7 virtual channels generated from your own collections.

Under **Metadata → Artwork**, choose self-hosted PostersPlus, XRDB, BetterPosters, Rating Poster Database, or a custom URL template for movie and show posters. Supply your own provider settings; unavailable posters fall back to TMDB. Provider keys and template URLs stay on the server. Ether overlays can be added on top. See [provider setup and limits](internal/artwork/PROVIDERS.md).

<table>
  <tr>
    <td width="50%" valign="top">
      <a href=".github/assets/metadata.png"><img src=".github/assets/metadata.png" alt="Live Poster Overlay Studio" width="100%" /></a>
      <p align="center"><sub><b>Metadata & Poster Studio</b> — pure-Go versioned badges from verified release facts</sub></p>
    </td>
    <td width="50%" valign="top">
      <a href=".github/assets/livetv.png"><img src=".github/assets/livetv.png" alt="Live TV Guide & Virtual Channels" width="100%" /></a>
      <p align="center"><sub><b>Live TV & Virtual Channels</b> — 24/7 collection channels, M3U/XMLTV & multi-source failover</sub></p>
    </td>
  </tr>
</table>

### Adaptive Media Cache & Zero-CLI High Availability
Dial your storage footprint from **Pass-through (0 GiB)** to **Everyday (32 GiB)** up to **Edge Library (20 TiB)** with household-aware movie preloading and rolling TV episode windows on local disk or a **Shared NAS pool**. Start on standalone SQLite, and when you're ready, migrate to an active-active **PostgreSQL cluster** through a guided wizard—complete with automatic connection-budget partitioning, leader election, and zero-downtime rolling updates.

<table>
  <tr>
    <td width="50%" valign="top">
      <a href=".github/assets/cache.png"><img src=".github/assets/cache.png" alt="Adaptive Cache & Household Preloading" width="100%" /></a>
      <p align="center"><sub><b>Adaptive Media Cache</b> — rolling household shelves, warm windows & Shared NAS support</sub></p>
    </td>
    <td width="50%" valign="top">
      <a href=".github/assets/cluster.png"><img src=".github/assets/cluster.png" alt="Standalone SQLite & PostgreSQL Cluster Wizard" width="100%" /></a>
      <p align="center"><sub><b>Cluster Manager</b> — 1-click guided migration from SQLite to active-active PostgreSQL HA</sub></p>
    </td>
  </tr>
</table>

---

## Install in five minutes

| Platform | Download / Image | Details |
| :--- | :--- | :--- |
| **macOS 13+** | [`Ether-<version>.dmg`](https://github.com/braedonsaunders/ether/releases/latest) | Universal menu-bar app (Apple Silicon & Intel) with bundled FFmpeg. *(Ad-hoc signed: right-click → Open on first launch.)* |
| **Windows (x64)** | [`EtherSetup-<version>.exe`](https://github.com/braedonsaunders/ether/releases/latest) | Windows service + system tray app with bundled FFmpeg and automatic firewall rule. |
| **Linux** | [`.deb` / `.rpm`](https://github.com/braedonsaunders/ether/releases/latest) (`amd64`, `arm64`) | Dedicated `ether` systemd service, hardware transcoding group setup (`render`/`video`), bundled FFmpeg. |
| **Docker** | `ghcr.io/braedonsaunders/ether:latest` | Multi-arch (`amd64`, `arm64`). Ready-made templates for **Unraid, TrueNAS SCALE, Proxmox, and Synology** in [`packaging/community`](packaging/community/README.md). |

```yaml
services:
  ether:
    image: ghcr.io/braedonsaunders/ether:latest
    container_name: ether
    restart: unless-stopped
    network_mode: host            # enables LAN GDM auto-discovery in Plex apps
    environment:
      TZ: Etc/UTC
      # ETHER_PORT: "32450"       # already running Plex on 32400 with host networking? pick another port
    volumes:
      - ./config:/config
      - ./cache:/cache            # select /cache during the setup wizard
    # devices: ["/dev/dri:/dev/dri"]   # optional: Intel QuickSync / VAAPI hardware transcoding
```

Open **`http://localhost:32400/ether/`** and finish the 4-step wizard:

1. **Sign in with Plex** and claim the server. It appears in every Plex app on your account immediately.
2. **Add a source:** a Usenet provider + Newznab indexer, *or* a debrid account (**Real-Debrid, TorBox, AllDebrid, Premiumize, Debrid-Link, Offcloud**) + torrent indexer, Cardigann site or Stremio addon, *or* both.
3. **Choose a cache preset** (an SSD with 32–50 GiB is plenty to start).
4. **Paste a free [TMDB API key](https://www.themoviedb.org/settings/api)** and optional integrations (Trakt, OpenSubtitles, local Ollama/LM Studio).

**After setup:** [Understand Plex libraries](docs/user-guide.md#libraries-catalogue-and-home) · [Put your watchlist on Home](docs/user-guide.md#put-your-watchlist-on-home) · [Choose your Home rows](docs/user-guide.md#choose-home-rows) · [Check Stremio addon compatibility](docs/user-guide.md#stremio-addons-as-sources)

**Full documentation:** [**Installation & Clustering Guide**](docs/install.md) · [**Owner & User Guide**](docs/user-guide.md) · [**NAS & Homelab Templates**](packaging/community/README.md)

---

## Questions you're about to ask

<details>
<summary><b>Do I need Plex Pass?</b></summary>

At home on your local network, no. **To stream outside your home, yes.** Ether follows Plex's rules just like a standard Plex Media Server: remote playback requires Plex Pass on the server owner's account or a Remote Watch Pass on the viewer's account.

Ether never emulates, unlocks, or bypasses Plex Pass or any paid Plex feature. Every viewer signs in through `plex.tv` and receives the exact entitlements Plex grants their account. If you love the official Plex apps, support Plex.
</details>

<details>
<summary><b>Which Plex apps work?</b></summary>

Every official Plex client: **Apple TV, iPhone, iPad, Android, Android TV / Nvidia Shield, Fire TV, Roku, PlayStation, Xbox, Smart TVs (LG, Samsung, VIDAA), Plex Desktop, and Plex Web.** To the client app, Ether is simply a Plex Media Server on your account.

Plex for VIDAA receives one automatic media choice to keep its playback settings stable, so its Play Version menu does not offer individual releases. Automatic selection and playback quality controls remain available.
</details>

<details>
<summary><b>Does it download whole movies? How much disk space do I need?</b></summary>

By default, no. Ordinary playback fetches only the byte ranges the player is watching plus a bounded read-ahead window (a 20 Mbit/s stream uses ~150 MB per minute) into a self-trimming disk cache. If you have a tiny disk, choose **Pass-through**. If you have terabytes to spare, you can opt into the **Managed Shelf**, which proactively preloads top household movies and a rolling window of next TV episodes during idle hours.
</details>

<details>
<summary><b>Won't this hammer my indexers and Usenet providers?</b></summary>

Ether is engineered specifically not to:
- **Zero background crawling:** Indexers are queried only when a viewer opens/plays a title, when the next episode is about to air/play, or on a slow schedule for watchlisted items.
- **Detail-page reuse:** Opening a detail page resolves, probes, and caches the keyframe index for 10 minutes—pressing Play reuses that work with zero extra queries.
- **Quota & Retry-After discipline:** Per-indexer daily API and download caps are tracked durably across restarts; `429` and `Retry-After` responses pause that indexer cleanly while falling back to others.
- **Minimal health sampling:** Health checks sample ~12 spread articles and bail out on the first confirmed miss across all providers.

*Always check your indexer's terms of service—some private indexers restrict streaming engines.*
</details>

<details>
<summary><b>Will my debrid account get flagged for multiple IPs?</b></summary>

No. Every stream is fetched by your Ether server and served from Ether to the viewer. Your debrid provider only ever sees your Ether server's IP address, even when family members stream remotely from their phones or TVs. (If you run a multi-node cluster, keep your nodes behind the same WAN IP; Ether's Cluster page warns you if nodes egress from different public IPs.)
</details>

<details>
<summary><b>How does Ether treat my Real-Debrid account?</b></summary>

Real-Debrid no longer reports which torrents it holds, so Ether adds only the top few candidates for a title you open, keeps the ones that are ready instantly and removes the rest. Answers are remembered so the same torrent isn't checked again, API calls are limited to four at a time, and rate limits pause the account cleanly. Files Real-Debrid refuses to serve (`infringing_file`) are removed and skipped in favour of the next release; when it refuses new torrents in a row, Ether pauses adding for a few minutes rather than retrying. A Stremio addon configured with your key can mark which torrents are cached, which saves those checks.
</details>

<details>
<summary><b>Can I run Ether alongside my existing Plex Media Server?</b></summary>

Yes—and if you have local media files on disk, that's the recommended setup. On the same machine, Ether notices Plex Media Server already holds port 32400 and moves itself to **32450** (the installers' shortcuts follow it), so open `http://localhost:32450/ether/`. In Docker with `network_mode: host`, set `ETHER_PORT` to the port you want. Both servers then appear side-by-side in your Plex apps. You can also import your existing PMS watch history, ratings, collections, and playlists into Ether in read-only mode.
</details>

<details>
<summary><b>What does Ether phone home to?</b></summary>

Nothing. There is **zero telemetry or analytics**. Ether communicates only with `plex.tv` (authentication, claiming, sharing, certificates), TMDB, the providers/indexers/services *you* configure, and GitHub's release feed when checking for updates. If a viewer connects their own Plex account under Limits & preferences, Ether can also send that viewer's future watched and unwatched marks and whole-star ratings to `scrobbles.plex.tv`. That delivery is optional, off until they enable it, and it is not telemetry. Import from the Plex account is a separate button, not a continuous two-way sync. Turning sync off stops queued changes. An update already being sent may still finish. A send does not by itself create a Discover Activity card. Uncertain sends are held durably instead of retried: a lost reply, server failure, or crash may have changed Plex. The viewer status shows held sends. Retry, reconnect, enabling sync again, and journal cleanup do not release a hold; there is no automatic reconciliation or release. Other known titles and aspects continue. Old undelivered rows have no trustworthy original target, so upgrading conservatively holds the affected account’s entire watch or rating aspect. Playback stays local and continues.
</details>

---

## Where it stands today

Ether is backed by 2,600+ offline unit, fault-injection and fuzz tests, recorded protocol conformance checks against real Plex Media Server fixtures (`internal/pmsconform`), and hardware transcoding verification across Apple VideoToolbox, NVIDIA NVENC (plus optional Windows RTX Video Super Resolution), Intel QuickSync, and VAAPI.

| Capability | Status |
| :--- | :--- |
| **TMDB Catalogue, Browse, Conversational Search & Collections** | Working |
| **Personalization Engine, Taste Models & Home Designer** | Working |
| **Usenet Streaming Engine, PAR2 Oracle, Health Checks & Failover** | Working |
| **Debrid Streaming (Real-Debrid, TorBox, AllDebrid, Premiumize)** | Working |
| **Debrid Streaming (Debrid-Link, Offcloud)** | New: built from the published APIs, awaiting reports from live accounts |
| **Official Plex Apps (TV, Mobile, Desktop, Web)** | Working |
| **Pure-Go Poster Overlays, Skip Markers & Subtitle Alignment** | Working |
| **Hardware Transcoding & Optional Windows NVIDIA RTX Video VSR** | Working |
| **Live TV Guide, Virtual Channels & M3U/XMLTV/Xtream** | Working |
| **PostgreSQL Active-Active HA Clustering & Shared NAS Cache** | Working |

**Current boundaries:**
- Usenet archives must be store-mode (`m0`, plain or password-encrypted RAR4/RAR5/7z); compressed archives and RAR-wrapped torrents are skipped during resolution.
- NVIDIA RTX Video Super Resolution (`nvenc-ngx-vsr`) requires native Windows, an NVIDIA RTX GPU, and `NVEncC64` 9.38 with NGX DLLs (installed automatically when enabled unless a custom path is configured), is off by default, targets SDR H.264 HLS/DASH streams within configured height/concurrency bounds, and falls back to ordinary ABR renditions when bandwidth or concurrency requires it.
- No Plex Relay support—remote viewers connect directly via UPnP/NAT-PMP, a port forward, or your reverse proxy.
- No DVR recording.

---

## Legal

Ether is an independent, clean-room implementation of the HTTP protocol used by compatible media client applications. It contains no proprietary Plex code, assets, or binaries. Ether ships **no media content, no Usenet providers, no indexers, no Cardigann site definitions, no debrid accounts, and no third-party API keys**. Owners supply their own accounts and are solely responsible for their configuration and compliance with applicable laws and service terms. See [**LEGAL.md**](LEGAL.md).

"Plex" is a trademark of Plex, Inc. Ether is **not affiliated with, sponsored by, or endorsed by Plex, Inc.** This product uses the TMDB API but is not endorsed or certified by TMDB.

[**GPL-3.0 License**](LICENSE) · [**Legal**](LEGAL.md) · [**Third-Party Notices**](THIRD_PARTY_NOTICES.md)

### Movie release window

In Catalog settings, **Wait for digital or physical release** optionally hides movies until the earliest TMDB digital or physical release date anywhere in the world. It is off by default. Movies whose completed TMDB release-date details have no digital or physical date become visible 90 days after their theatrical release; once admitted by that fallback, they remain admitted if TMDB later changes its dates. Movies awaiting details stay hidden while bounded background batches check the catalog. Missing and future dates are rechecked, and older dates are periodically refreshed. The original theatrical date and year remain unchanged, and a date does not verify release quality.
