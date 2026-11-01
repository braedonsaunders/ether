# Upgrading from 0.2.x to 0.3.0

Ether 0.3.0 upgrades existing 0.2.x installations in place. Your Plex server identity, shared viewers, watch history, ratings, collections, quality profiles, Usenet providers, debrid accounts, and indexers carry over automatically.

## 1. Automatic in-place upgrade and first start

Install 0.3.0 using the method you already use:

- **In-app updater:** Open **Server › Maintenance › Updates** and install `0.3.0`.
- **macOS (`.dmg`):** Quit the Ether menu-bar icon, drag the new **Ether** app into `Applications` to replace the old one, and launch it.
- **Windows (`.exe`):** Run `EtherSetup-0.3.0.exe`. It stops the service, updates the binaries in place, and restarts the service while keeping `%ProgramData%\Ether`.
- **Linux (`.deb` / `.rpm`):** Install the new package with `apt` or `dnf`. It restarts `ether.service` with `/var/lib/ether` intact.
- **Docker:** Pull `ghcr.io/braedonsaunders/ether:latest` (or `:0.3.0`) and recreate the container (`docker compose up -d`).

### Database migration on first start

On its first start, 0.3.0 takes an **automatic backup snapshot** of your 0.2.x database and configuration before changing anything. It then migrates the database and builds the new full-text trigram and people search indexes.

On a large library (hundreds of thousands of catalogue rows), this first start can take a few minutes. While it runs, opening `/ether/` shows the live **Startup status** page with migration and integrity-check progress. Wait for it to finish; do not kill the process during the initial migration.

---

## 2. What moved where in the owner console

The owner console sidebar is now organized into **Overview / Activity / Viewers**, **Library**, **Streaming**, and **Server**. Existing settings are preserved, and several tabs have moved to clearer locations:

| In 0.2.x | In 0.3.0 |
| :--- | :--- |
| **Sources** nested sub-tabs (*Overview*, *Debrid*, *Usenet*, *Indexers*, *Sites*, *Availability*, *Usage*) | **Streaming › Sources** on one level: **Debrid**, **Usenet**, **Indexers** (including Prowlarr import, Stremio addons, and Cardigann sites), and **Stats**. |
| **Sources › Availability › Settings** (movie evidence window and feed budgets) | **Library › Catalog › Settings › New releases** (three modes: *When they can play*, *On digital release*, *Right away*). |
| **Sources › Availability** engine search timings (*Releases to keep ready*, *Health check samples*, *Remember searches for*, *Retry unavailable titles after*) | **Streaming › Releases › Searching** (under *How Ether searches*). |
| **Releases › Quality profiles** and **Custom scoring** | **Streaming › Releases › Profiles** (profile cards and drawer) and **Releases › Searching** (*How Ether searches* and *Versions in the app*). |
| **Releases › Selection rules** (blocked words/terms, required words, preferred rules) | Inside each profile drawer under **Releases › Profiles**: **Never play** (avoid/only-play rules and blocked terms) and **What matters most** (prefer rules). Server-wide blocked groups and terms are also on **Releases › Blocklist**. |
| **Releases › Client rules** | **Streaming › Releases › Device rules** (includes a new toggle to convert Dolby/DTS audio to AAC). |
| **Usenet or debrid** (*Source for equal-quality releases*) | **Tie-breaker** at the bottom of the profile drawer in **Releases › Profiles** (*Start fastest*, *Usenet first*, *Debrid first*; applies to all profiles). |
| Global **Allow CAM** switch | Removed as a separate global toggle; controlled per profile by **Allow CAM** under **Releases › Profiles › Never play**. |
| **Viewers › Defaults** page | Removed. Household audio and subtitle defaults are in **Streaming › Playback › Audio & subtitles**. Watchlist alerts are in **Server › Notifications › Watchlist alerts**. Per-viewer `plex.tv` language and restriction defaults appear read-only as **From Plex** on each viewer's **Access** and **Playback** tabs. |
| **Viewers** adding a local viewer or invite | **Viewers › Add viewer** drawer (*Username and password* or *Invite link*). |
| **Use Plex's metadata for Plex apps** | **Server › Apps › Plex › Account** (under *Metadata*, off by default; requires a claimed Plex server). |
| **Catalog › Library visibility** | **Library › Catalog › Settings › Visibility** (ordered show/hide rules using unified age-level ratings). |

---

## 3. Turning on Jellyfin, Emby, Silo, and other apps

Existing 0.2.x installs keep **Plex** enabled as before. To turn on additional protocols, open **Server › Apps**, enable the app, and check the listening address shown on its tab.

### Default ports and firewall

| App / protocol | Default port | Shared-port path on main listener (`32400`) |
| :--- | :--- | :--- |
| **Plex** | `32400` TCP (`32410–32414` UDP GDM) | `/` |
| **Jellyfin** | `8096` TCP (`7359` UDP LAN discovery) | `http://HOST:32400/jellyfin` |
| **Emby** | `8097` TCP | `http://HOST:32400/emby` |
| **Silo** | `8090` TCP | `http://HOST:32400/silo` |
| **Stremio** | Main HTTPS port | Per-viewer install link from `/ether/me` |
| **Files (WebDAV)** | Main port | Per-viewer credentials from `/ether/me` |
| **M3U & XMLTV** | Main port | Per-viewer `playlist.m3u` and `guide.xml` links from `/ether/me` |
| **DLNA** | Local network | Enabled under **Apps › DLNA** after choosing a guest viewer profile |

If another server on the same machine already holds `8096`, `8097`, or `8090`, Ether still serves that protocol on the main port under `/jellyfin`, `/emby`, or `/silo` and shows the active address on **Server › Apps**. Allow the dedicated TCP ports through your host firewall (or uncomment `8096`, `8097`, and `8090` in `docker-compose.yml` if running Docker in bridge mode) when you want clients to connect on the default ports.

### How viewers sign in

- **Plex logins are not typed into Jellyfin, Emby, or Silo apps.** Those apps authenticate against your Ether server, not `plex.tv`.
- **Quick Connect and device codes:** In a Jellyfin app that supports Quick Connect (or in Silo using a device code), start sign-in on the TV or app, then open `http://HOST:32400/ether/me` in a browser. A viewer who already uses Plex can sign in to `/ether/me` with their **Plex** account (or with their local Ether password) and enter the 6-character code to approve the device.
- **Username and app password:** Any viewer can open `/ether/me` (or the owner can open **Your account** at `/ether/profile` or **Viewers › viewer › Access**) and set a local username and password under **Sign-in methods**. For **Files (WebDAV)**, **Stremio**, and **M3U**, viewers create per-app connection credentials under **Connect an app** in `/ether/me`.
- **Infuse:** Infuse does not use Jellyfin Quick Connect. Connect Infuse using a Jellyfin, Emby, or WebDAV connection with the viewer's username and password (or WebDAV app password) created in **Sign-in methods** / **Connect an app**.
- **Unsupported web-wrapper apps:** Ether does not bundle `jellyfin-web` or Emby Web. The **Emby for TV (2.x)** app on Apple TV, **Jellyfin for Android phones**, and **Jellyfin Desktop** wrap server-hosted web assets and are not supported. On Apple TV, use **Infuse**, **Swiftfin**, **Silo**, or **Plex**; on Android phones, use **Findroid** or **Streamyfin**.

---

## 4. Remote access for the new apps

- **Plex** continues to use its existing remote access (`plex.tv` publishing, `*.plex.direct` TLS, UPnP, or your custom connection URL). No changes are needed for Plex clients.
- **Jellyfin, Emby, Silo, Stremio, WebDAV, and M3U** do not route through `plex.tv`. For access outside your home network, expose their listener port (or the `/jellyfin`, `/emby`, `/silo` path on your main port) through **Tailscale / WireGuard** or a **reverse proxy with HTTPS**, and give viewers that external URL.
- **Stremio** requires an `https://` address to install an addon; configure a certificate under **Apps › Secure connections** or terminate TLS at your reverse proxy.

---

## 5. New-release modes and quality profile ranking

### Three New releases modes

Under **Library › Catalog › Settings › New releases**, the old availability table is replaced by three modes:

1. **When they can play** *(Recommended)*: A new movie appears in your libraries once Ether finds a release that passes your quality profile (excluding CAM/TS if disallowed, and meeting resolution and size floors). Ether checks in-window movies automatically within a daily indexer budget computed from your configured indexers. Installs still on the old `400 requests / 15m / 5 pages` defaults move to automatic *Normal* budgeting on upgrade.
2. **On digital release:** A new movie appears on its TMDB streaming or digital release date without background release checking.
3. **Right away:** Every new movie appears immediately, including titles still only in theatres.

### Quality profile ranking ladder

Under **Streaming › Releases › Profiles**, each profile now uses an explicit, draggable **What matters most** ranking ladder (*Prefer* rules, *Audio language*, *Connection fit*, *Resolution*, *Release type*, *HDR*, *Played fine recently*, *Video codec*, *Audio format*, *Larger file*).

- **Existing profiles keep their exact 0.2.x ranking order automatically.** You do not need to edit your profiles after upgrading unless you want to change the priority order (for example, dragging *Release type* above *Resolution* so a 1080p Blu-ray Remux beats a 4K WEB-DL).
- **Play Version ceiling:** By default, **Play Version** no longer offers alternative releases above your active profile's resolution, HDR, or size limits (#205). To allow taller alternatives in version menus, turn on **Offer higher versions** under **Releases › Searching**.

---

## 6. PostgreSQL and high-availability clusters (optional)

- **Single-server SQLite installs:** Nothing to configure. SQLite remains the default and now runs with separate read/writer pools and a background WAL checkpointer.
- **Upgrading an existing 0.2.x PostgreSQL HA cluster:** Because 0.3.0 introduces multi-app identity and session authority tables (`auth/5`, `sessions/9`), **the first upgrade from 0.2.x to 0.3.0 is not a mixed-version rolling upgrade** (`docs/acceptance/upgrade.md`):
  1. Take a full PostgreSQL database backup and back up each node's configuration and credential key.
  2. Stop **all** `0.2.x` nodes, disable automatic restart jobs, and verify no `0.2.x` process holds an open database connection (revoke or rotate old writer credentials if needed).
  3. Upgrade all nodes to `0.3.0`, let the writer run the pending migrations, and verify every serving node is on `0.3.0` before resuming traffic.

---

## 7. Known limitations and what is coming in 0.3.1

Deferred from the 0.3.0 freeze to **0.3.1**:

- **Hardware transcode fallback ladder and automatic deinterlacing:** Stepping down `full GPU -> hardware encode-only -> software CPU` on mid-session filter failures, GPU image-subtitle overlay burn-in across all backends, and automatic `bwdif`/`yadif` deinterlacing for interlaced H.264 and broadcast Live TV streams.
- **Cold-start resolver improvements:** Preventing duplicate-named torrent info-hashes from sharing a speculative uncached check during live search, and skipping PAR2 repair probes during initial obfuscated Usenet header identification so dead obfuscated posts fail in under a second.
- **Remembering the Play Version list (#214):** Keeping the full alternative version list available on **Watch Version** when returning to a previously played title.
- **Configurable indexer User-Agent presets (#213):** Per-indexer and global User-Agent / client identity presets.

---

## 8. How to roll back to 0.2.x

**Ether 0.2.x binaries cannot open a database that has been migrated to 0.3.0.** Never downgrade the binary against a migrated 0.3.0 data directory.

To roll back to 0.2.x:

1. While still running `0.3.0`, open **Server › Maintenance › Backups & import** (or the **Startup recovery** page if the server is not serving) and click **Restore** next to the automatic **pre-migration snapshot** taken when `0.3.0` first started.
2. Once the snapshot is restored into the data directory, stop `0.3.0` before it re-migrates (or replace the binary/container image with your `0.2.58` release immediately) and start `0.2.58`.
3. For a PostgreSQL cluster, stop all `0.3.0` nodes, restore your pre-upgrade PostgreSQL dump and node configurations, and start the `0.2.58` nodes.
