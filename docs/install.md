# Installing Ether

[Ether's multi-app support](apps.md) is the shared server the app
frontends, including Plex, run in. Install Ether as
below; there is no separate installer or service for the shared server.

If you do not use a terminal, start with the macOS or Windows section below.
Download a macOS DMG or a Windows setup EXE from the
[Releases page](https://github.com/braedonsaunders/ether/releases/latest),
open it, and follow the wizard in the browser. No terminal is needed.
Linux packages and Docker are further down. Packaged installers bundle
FFmpeg and FFprobe.

Signed macOS releases open normally. An unsigned build needs one right-click → Open the first time.
The Windows installer is unsigned and may trigger SmartScreen.
The automatic-update manifest is signed separately with Ether's release key.

If you already have an Ether executable, run it and open
<http://localhost:32400/ether/>. The setup wizard walks you through app selection,
owner-account setup,
provider and indexer configuration, and cache settings. A standalone executable
needs access to FFmpeg and FFprobe for media processing.

FFmpeg built with zscale (libzimg) gives the best HDR-to-SDR tone mapping.
Without zscale, Ether uses libplacebo, then an OpenCL or hardware tone map
when the binary has one. If none of those are available it does not guess a
picture: playback reports that this FFmpeg cannot tone-map and tries another
release.

The ready-made Torrentio source looks titles up on torrentio.strem.fun. To keep lookups on a server you run, add a Torznab indexer (Prowlarr, Jackett, or Zilean) instead.

After any install, open the web UI and finish the setup wizard. Choose your
app integrations, sign in with Plex if selected or create a local Ether owner
account, add your sources and indexers, and pick a cache folder. Jellyfin,
Emby and Silo do not require a Plex account or a separate Plex Media Server.
Review the
[client support notes](user-guide.md#jellyfin-and-emby-apps) for integration limits.
Ether ships no content, providers or indexers; see
[LEGAL.md](../LEGAL.md).

## Windows

1. Download `EtherSetup-<version>.exe` from the releases page and run it
   (administrator approval is needed to register the service).
2. Ether installs to `C:\Program Files\Ether`, registers the **Ether**
   Windows service (delayed automatic start; restarted automatically if
   it ever stops), adds a firewall rule for TCP 32400, and puts the Ether
   tray icon in your login items.
3. Settings, database, cache and logs live in `%ProgramData%\Ether`.
   The folder holds credentials, so it is readable only by SYSTEM and
   Administrators. Uninstalling keeps this data; delete the folder to
   purge.
4. To upgrade, run a newer `EtherSetup-<version>.exe`. It waits for the
   service to stop, replaces the program files and updates the existing
   service in place, keeping its data directory (including one you set
   with `ether service install --data-dir`).

Manage the service with `ether.exe service start|stop|restart|status`
from an elevated prompt, or from the tray menu.

## macOS

1. Download `Ether-<version>.dmg`, open it, drag **Ether** to
   Applications, and launch it. Ether appears as a menu-bar icon (no
   Dock icon).
2. On first launch the tray installs a per-user LaunchAgent
   (`~/Library/LaunchAgents/ether.plist`) that runs the
   server (`Ether.app/Contents/Resources/ether`) while you are logged in.
3. Data lives in `~/Library/Application Support/Ether`, logs in
   `~/Library/Application Support/Ether/logs`.
4. To upgrade, quit the tray, replace **Ether** in Applications with the
   new version and launch it. When the tray finds the server still
   running the previous release, it restarts the server once onto the
   new one.

Requires macOS 13+. Signing and notarisation depend on the published package;
check its release notes before installing.

## Linux (.deb / .rpm)

```sh
sudo apt install ./ether_<version>_linux_amd64.deb    # Debian/Ubuntu
sudo dnf install ./ether_<version>_linux_amd64.rpm    # Fedora/RHEL
```

The package creates a dedicated `ether` system user, installs to
`/usr/lib/ether`, enables and starts `ether.service` (systemd), and
keeps data in `/var/lib/ether`. The segment cache defaults to
`/var/lib/ether/cache`; the package also creates `/var/cache/ether`
(writable by the service), which you can pick as the cache directory in
the setup wizard or on the Cache page.
`systemctl status ether` shows state; `journalctl -u ether` shows logs.
Upgrades are just installing the new package (or a new Docker image) —
Linux packages do not self-update.

For hardware transcoding, the `ether` user is added to the `render` and
`video` groups when they exist (VAAPI/QuickSync). NVIDIA needs the
proprietary driver visible to ffmpeg as usual.

## Docker

```sh
cp docker-compose.example.yml docker-compose.yml
docker compose up -d
```

or plain Docker:

```sh
docker run -d --name ether --restart unless-stopped \
  --network host \
  -v /path/to/config:/config -v /path/to/cache:/cache \
  ghcr.io/braedonsaunders/ether:latest
```

**Host networking is recommended on Linux.** Plex apps discover servers on the
LAN with GDM (UDP 32410–32414) and Ether's remote-access setup talks to
your router over UPnP; both need the host network namespace. Docker Desktop
on Mac and Windows does not provide that namespace. Bridge mode
with `-p 32400:32400` works for Plex Web and for claimed/remote
connections, but LAN auto-discovery will not find the server. Do not publish
port 8096. Jellyfin, Emby, and the other apps are reached at
`http://HOST:PORT/jellyfin` (and `/emby`, `/silo`) on the console port.

Already running Plex on 32400? Use bridge mode and `ETHER_PORT` so Ether does
not take that port:

```yaml
services:
  ether:
    image: ghcr.io/braedonsaunders/ether:latest
    environment:
      ETHER_PORT: "32800"
    ports:
      - "32800:32800"
    volumes:
      - ./config:/config
      - ./cache:/cache
```

Open `http://HOST:32800/ether/`. Jellyfin is `http://HOST:32800/jellyfin`.

Volumes: `/config` (config, database, identity — back this up) and
`/cache` for the segment cache. The cache defaults to `/config/cache`.
On a fresh install, a mounted `/cache` volume is used automatically, so
no restart is required for that path. The budget is set in the setup wizard
or later on the Cache page.

User and permissions: Ether runs as `PUID`:`PGID` (default 999:999, the
image's `ether` account). Set them to the user that owns your folders, as
for other NAS containers (e.g. `PUID=1026 PGID=100` on Synology, `PUID=99
PGID=100` on Unraid). On start the container gives `/config` and `/cache`
to that user when they belong to someone else (a one-time walk of their
contents), adds the groups owning any passed-through `/dev/dri` nodes, and
drops root before Ether starts. `UMASK` (e.g. `002`) is honoured too.
`--user`/`user:` still works and runs Ether exactly as that user, in which
case both volumes must already be writable by it.

Hardware transcoding: for Intel QuickSync/VAAPI or AMD VAAPI pass
`--device /dev/dri:/dev/dri`. The entrypoint adds the group that owns the
render node (usually `0660 root:render`) automatically; with `--user` you
add it yourself: `--group-add "$(stat -c %g /dev/dri/renderD128)"`. The image is Debian 13 and ships libva 2.22, which the bundled Jellyfin
FFmpeg needs in order to load VAAPI. It also ships Mesa (AMD) and, on
amd64, Intel's iHD and i965 VAAPI drivers plus Debian's oneVPL Quick Sync
runtime. That oneVPL package is not the legacy Media SDK runtime older
Intel GPUs use for Quick Sync. The Transcoding page shows what the startup
probe found.
NVENC: use the NVIDIA container toolkit flags shown in
`docker-compose.example.yml`.

## From a tarball/zip (any OS)

Release archives contain `ether`, the tray (Linux/Windows), `ffmpeg` and
`ffprobe` (must stay next to the `ether` binary — that's where it looks
first). Unpack anywhere, then either run `./ether` directly or register
the service:

```sh
sudo ./ether service install   # systemd / launchd / Windows SCM
sudo ./ether service start
./ether service status
```

`ETHER_DATA_DIR` (or `--data-dir`, which `ether service install
--data-dir DIR` bakes into the service) overrides the data directory
(defaults: `/var/lib/ether` as root on Linux,
`~/Library/Application Support/Ether` on macOS, `%ProgramData%\Ether`
on Windows). Logs are written to `<data dir>/logs/ether.log` (rotated)
and fatal crash traces to `<data dir>/logs/crash.log`.

## Remote access / port forwarding

Ether’s main listener defaults to **TCP 32400**. Other frontends can have
their own listeners; use the addresses shown on the Apps page. The following
discovery and address-publication instructions apply to the **Plex frontend**
for remote access outside your LAN:

- Easiest: enable *Remote access* in the web UI — Ether asks your router
  for a UPnP mapping and publishes the address to plex.tv, for Plex clients.
- Manual: forward one external TCP port (e.g. 32400) to the Ether host's
  32400, then set that public port in the web UI.
- LAN discovery additionally uses UDP 32410–32414 (GDM) — only relevant
  inside your network, never forward it.
- Behind a reverse proxy/TLS terminator, keep the path and `X-Plex-*`
  headers intact and point *Custom connection URLs* (or *Public host* on
  port 443 with *Secure connections* set to *Required*) at your `https://`
  domain; Ether publishes that HTTPS endpoint directly to plex.tv without
  requiring a `plex.direct` certificate.
  A proxy on the Ether host must set `X-Forwarded-For` to the connecting
  client's address (or append it to the existing chain). If using
  `X-Real-IP` instead, overwrite it and remove incoming `X-Forwarded-For`.
  Never pass client-supplied forwarding headers unchanged.
  Ether considers forwarded addresses only from loopback peers and treats
  any public hop in the trusted suffix as remote for playback restrictions.
  Keep the owner UI path `/ether/` private at the proxy; complete initial
  setup before exposing the server.

## How Ether reaches the internet

Ether can send indexers, Stremio addons, debrid and Usenet through one proxy
on this computer, and leave Plex, updates, UPnP and remote access on the
normal connection. The setting is off until you turn it on, under
**Server → General → How Ether reaches the internet**. A username and password
are stored with Ether's other secrets and are not put in the address.

Two layouts are common:

1. **Split tunnel.** Ether stays on the host network so Plex discovery and
   remote access keep working. A VPN container publishes a proxy. Gluetun's
   HTTP proxy listens on port 8888 when `HTTPPROXY=on`. From a host-network
   Ether process that address is `http://127.0.0.1:8888`, because Docker DNS
   names are not visible there. On a shared Docker network it is
   `http://gluetun:8888`. SOCKS on port 1080 is only useful if you enabled a
   SOCKS proxy yourself; gluetun does not publish one by default.
2. **Whole stack in the VPN network.** Leave Ether's proxy off. Publish
   Ether's ports on the VPN container, and point remote access at the VPN
   address. Do not also turn Ether's proxy on.

Indexer names are looked up on this computer. If the ISP's DNS sinks those
names, point this computer's resolver at the VPN. `ALL_PROXY` is ignored.
`HTTP_PROXY` and `HTTPS_PROXY` apply only while Ether's own proxy setting is
off. While it is on, a routed call that cannot reach the proxy fails; Ether
does not dial that call directly.

```yaml
services:
  gluetun:
    image: qmcgaw/gluetun
    cap_add:
      - NET_ADMIN
    devices:
      - /dev/net/tun:/dev/net/tun
    environment:
      - VPN_SERVICE_PROVIDER=custom
      - VPN_TYPE=openvpn
      - HTTPPROXY=on
    ports:
      - 8888:8888/tcp
      - 32400:32400/tcp
  ether:
    image: ghcr.io/braedonsaunders/ether:latest
    network_mode: host
```

Use the published proxy address in Ether. Do not set
`network_mode: service:gluetun` on Ether when Plex remote access has to stay
on your home address. Publish Ether's own port on gluetun only for the
whole-stack layout, and then leave Ether's proxy off.

## Running a cluster

A cluster is two or more Ether servers ("nodes") on one network that share a
PostgreSQL database and appear in Plex as a single server. Viewers can be
served by any node; if one stops, their apps carry on through the others. A
single server needs none of this: it keeps everything in its own SQLite
database.

The **Cluster** page in the web UI (under *Server*) shows whether a server is
standalone or a node, guides cluster creation and joining, and manages nodes
and one-time join tokens. No terminal commands are required for setup.

### What you need

- **PostgreSQL 14 or newer**, with a dedicated, empty database for Ether.
  Create it with the C collation so text sorts exactly as on a single server:

  ```sql
  CREATE DATABASE ether LC_COLLATE 'C' LC_CTYPE 'C' TEMPLATE template0;
  ```

- **A database that stays up.** Ether does not install, replicate or fail over
  PostgreSQL; if the database goes away, so does the cluster's shared state.
  Run it highly available yourself, for example with Patroni, CloudNativePG
  on Kubernetes, or a managed PostgreSQL service. Ether tolerates brief
  database outages without ending streams, but not long ones.
- **Nodes on the same LAN.** Every node must reach the database (allow it
  through the firewall). Ether defaults to `sslmode=verify-full`, which checks
  the certificate and hostname. Use a trusted CA certificate on PostgreSQL;
  only select `require` (no identity verification) or `disable` (plaintext)
  deliberately for a controlled deployment.
- **A port forward per node** for remote access: each node needs its own
  external port (for example 32400 to the first node, 32401 to the second),
  or UPnP. Set the public port on each node's *Connectivity* page.
- **One public IP for providers.** Debrid services may flag one account used
  from several addresses at once. Keep every node behind the same internet
  connection. The Cluster page warns when the address nodes use for indexers,
  debrid and Usenet differs. Set the same proxy on every node under
  **Server → General → How Ether reaches the internet**. Usenet uses SOCKS5 or
  an HTTP CONNECT tunnel, and the provider certificate is still checked.
  A proxy that is down fails the connection; Ether does not fall back to a
  direct address. Plex, updates and the router stay on the normal connection.

### Start a cluster from an existing server

1. Finish this server's normal setup, then open **Server › Cluster** and choose
   **Create cluster**.
2. Enter the empty database's host, port, name, user, password and TLS setting.
   Choose **Test connection and continue**. Ether checks connectivity, the
   PostgreSQL version and that the schema is empty without modifying it.
3. Review the migration plan and choose **Create cluster and restart**.
   Stop active streams first. Ether creates a portable backup, shuts down its
   writers, copies its SQLite data into PostgreSQL and restarts automatically.
4. The page reconnects and shows this node in the cluster. Viewers, history,
   playlists and Plex sign-in come along; Plex apps see the same server.

The original SQLite database remains in the data directory. If applying the
change fails, Ether restores the previous local settings and credential key
and reopens the manager with an error. An unsuccessful migration can leave data
in the destination database; use a fresh empty database before retrying creation.

A deployment with `ETHER_DATABASE_URL` set controls the database through its
environment, so guided database changes are disabled there. Existing cluster
management remains available. New container nodes can omit that override and
use the join wizard to save the database connection securely in their local
configuration.

### Add a node

1. On the **Cluster** page of a running node, choose **Create join token**,
   pick its validity (at most an hour), create it and copy the token.
2. Install and open Ether on the new machine. Choose **Join a cluster instead**
   from initial setup, or open **Server › Cluster › Join cluster**.
3. Paste the token. If this server was already configured, explicitly confirm
   replacing its identity and shared settings. Its existing history is backed
   up and is not merged into the cluster. An optional database URL override
   lets this node use a different address for PostgreSQL.
4. Choose **Test connection and continue**, review the plan, then choose
   **Join cluster and restart**. Ether backs up the local server, adopts the
   cluster's credentials and settings and restarts automatically. Sign in as
   the cluster owner if prompted.
5. Set this node's remote-access port forward if needed. It appears in the
   cluster membership list within a few seconds.

The token is shown once and is a secret: it carries the database password and
credential key. Use it promptly and do not paste it into chats or tickets.
Checking the connection does not consume it. A successful join consumes it;
a failed join may require a fresh token. Unused tokens can be revoked from
the Cluster page. Expiration, use and revocation only prevent Ether enrollment;
they do not invalidate the embedded database password or credential key. A
saved copy stays sensitive afterward. If a token leaks, rotate the database
password on every node and any stored provider or Plex credentials the
recipient could have read. Revocation alone does not contain that exposure.

The `ether cluster init`, `token` and `join` commands remain available for
scripted deployments; the web flow performs their operations automatically.

### Manage nodes

**Manage this node** changes its relative provider capacity weight and pauses
or resumes new streams. Pausing preserves existing playback, removes the node
from published connections and hands scheduled work to another available node.
It resumes after a restart. Pausing the only available node prevents new playback
until it is resumed. A rolling update owns its maintenance state while draining.

Use **Open manager** beside another node to manage that machine's local settings.
Owner credentials are not forwarded between nodes; sign in there if needed.
The **Updates** page handles software updates. The first upgrade onto multi-app support requires
a maintenance outage: stop and fence every old database writer, upgrade all
nodes, then serve traffic. Follow the [mandatory upgrade
runbook](acceptance/upgrade.md); this transition is not a mixed-version
rolling upgrade.

### Settings: shared and per node

Almost everything is shared: providers, indexers, metadata keys, policies,
viewers, the server identity and plex.tv sign-in, the update channel. Change
it on any node and the others follow within 30 seconds.

Each node keeps its own port and listen address, log level, cache location
and size, transcoding hardware, ffmpeg paths and transcode limit, public
port/host and UPnP for its port forward, custom certificate file paths, the
database URL and its node weight. Edit these on that node's own pages (or in
its `config.json`). A node's `config.json` also keeps a copy of the shared
settings, which is replaced from the database at every start.

### Node weight

Usenet connection counts, debrid API limits and TMDB request rates are
account-wide, so the live nodes divide them between them; the provider pages
show each node's part ("this node: 7 of 20 connections"). When a node leaves,
its share returns to the others within about 30 seconds.

There is nothing extra to configure. Enter each provider's connection limit
once, as you would for a single server, and the cluster divides it. Every live
node gets at least one connection per provider. The warm connections a node
keeps open for fast starts count towards its own share and never exceed it.
Debrid API calls (adding torrents, creating links) are limited per account and
divided in the same way. The number of connections a node uses to read one
debrid file is a per-node setting and is not divided: it only changes how
quickly a single stream fills. Indexer daily limits are counted in the shared
database, so together the nodes never exceed an indexer's quota.

To give a node a larger share, raise its **Node weight** in **Cluster › Manage
this node** or on that node's *General* page (`server.clusterWeight` in its `config.json`). A node at `2`
gets twice the share of a node at the default of `1`. Weights are listed on
the Cluster page.

### Backups

Scheduled backups run on whichever node leads and are saved on that node.
The backup list on any node shows every node's backups and which node holds
each one; download a backup from the node that holds it. To keep them in one
place, set a **Shared backup folder** on *Backups & import*: a folder every
node reaches at the same path, such as a network share.

Creating snapshots, changing their schedule, and downloading them require the
owner account after setup is complete. In **Backups & import › Snapshots**, enter
a password of at least 12 characters before downloading. Downloads use the
encrypted `.etherbackup` format. Ether does not store the password.

Restore from **Backups & import**: choose **Restore** on a snapshot, or
upload a downloaded file with the same password. Ether first checks the
backup (password, checksums, database) into `<data directory>/restored-<timestamp>`
and shows what will change and who is watching. On a standalone server,
**Restore and restart** then does the rest: on the next start, before the
settings or database are opened, the current `config.json`, `.credentials-key`
and `ether.db` (with its `-wal`/`-shm` files) move into
`<data directory>/rollbacks/restore-<timestamp>/` and the backup takes their
place. A failure at any point, including a crash part way through, puts the
original files back. The Backups page then shows a report (what was restored,
from when, the database integrity check, and the rollback folder) with
**Undo restore**, which puts the rollback folder back the same way. The three
newest rollback folders are kept. A cluster's PostgreSQL backup restored on a
standalone server is imported into its own database; the server stays
standalone.

A cluster node never replaces the shared PostgreSQL database by itself. The
backup is checked and staged, and the console guides you: stop every node,
create an empty PostgreSQL database, start one node with `ETHER_DATABASE_URL`
pointing at it and `ETHER_DATA_DIR` set to the staged folder (the logical dump
is imported on first start), then point the other nodes at the new database.

### Restarting from the console

**Maintenance › Restart Ether** (and the restart prompts after storage
changes) restarts the server without a terminal. The console shows who is
watching first and can wait until nobody is. On Linux and macOS, and in a
container or a terminal, Ether replaces itself with a fresh process (the
same PID, so systemd and launchd keep running it without their restart
delay, and a container keeps running whatever its restart policy). A
Windows service exits and the Service Control Manager starts it again after
5 seconds (15 and then 60 seconds when it restarts several times within a
day, the service's recovery actions). The console follows the restart and
reconnects by itself.

### While Ether starts

Ether listens as soon as its data directory is locked, before it opens the
database. Until it is ready, `/ether/` shows what it is doing (waiting for
PostgreSQL, updating the database step by step, restoring a backup) and
`/ether/api/startup-status` reports the same as read-only JSON without
credentials; every other request, the Plex API included, gets `503` with
`Retry-After`, and GDM discovery and plex.tv publishing wait. A start that
fails (for example a damaged database or a database that refuses the
connection) stays on that page with what to do and retries by itself; the
data directory stays locked meanwhile.

On a standalone server the page can restore a backup or undo the last
restore. Sign in as the owner first, with:

- the owner's local username and password: Ether keeps a copy of that
  sign-in (its Argon2id hash, never the password) in `owner-recovery.json`
  in the data directory, refreshed at every owner sign-in and password
  change, and otherwise reads the database read-only;
- the Plex account that owns the server;
- or the one-time **recovery code** Ether writes to `recovery-code.txt` in
  the data directory and to its log while it cannot start. It works once,
  and not through a reverse proxy.

Sign-ins are rate limited and only open the restore actions on that page,
for 15 minutes. Before setup is finished, the local network needs none.
The recovery code also reaches whatever collects Ether's log: the system
journal under systemd, `docker logs` in a container. Treat those as being as
private as the data directory.

When `config.json` itself cannot be read (or is not a settings file:
empty, `null` or `{}`), Ether treats setup as finished, offers no restore from the page and listens
on `127.0.0.1` only. In a container that loopback address is the
container's own, so the page is unreachable from outside until the settings
are fixed (`docker exec` into the container, or restore `config.json` into
the volume); this is deliberate, so damaged settings never expose the
server more than the owner chose. A partial `config.json` is fine:
whatever it leaves out takes its default, as it does once Ether is running.

A restore counts once the restored server is about to serve. If it cannot
start on the backup (a migration fails, the database is damaged, the
settings don't match), Ether puts the original files back automatically,
starts on them and says so in the report.

### Database integrity

A standalone server checks its SQLite database for damage (`PRAGMA
quick_check`) two minutes after it starts and in the daily maintenance. A
damaged database appears in the console's health checklist with a link to
restore a backup; Ether keeps running so you can.

Encrypted backups can also be uploaded during initial setup through the local
setup interface. Restoring an older unencrypted `.tar.gz` requires an owner
sign-in and explicit plaintext confirmation. Backups containing sealed secrets
must include their credential key.

Scheduled and internal snapshots remain plaintext on disk with restrictive
file permissions; they contain both credentials and the credential key. Protect
the backup directory and any network share accordingly. Encrypting a download
does not encrypt existing snapshots or older downloads.

### When a node fails

Plex apps know every node's address. When a node stops or loses its network,
apps reconnect through another node and playback resumes from the last
reported position, usually after a brief pause. A stream that was cut off
mid-response restarts rather than continuing seamlessly. If the node that
failed was the leader, another node takes over plex.tv publishing and
scheduled work within seconds.

The Cluster page shows a stopped node as offline with when it was last seen.
A cleanly stopped node leaves the list immediately; one that crashed stays
listed as offline for up to a day.

### Remove a node

Stop Ether on it and uninstall it, then delete its data directory (it holds
the cluster's credential key). Its share of provider connections returns to
the other nodes. Revoke any join tokens you no longer need.

**Never copy a node's data directory** to make another node: two nodes with
the same `node.json` would overwrite each other, so the copy refuses to start.
If that happens, stop the copy, delete `node.json` from its data directory and
start it again; it joins as a new node. A node whose credential key does not
match the cluster's also refuses to start; join it with a fresh token.

### Updating a cluster

Nodes update one at a time during the maintenance window, so the others keep
serving viewers while each one restarts. A node rejoins only once its database
changes are compatible with the rest of the cluster. If you update by hand
(for example with your package manager or a new Docker image), do the same:
update and restart one node, wait until it shows as online on the Cluster
page, then move on to the next.

## Updates

The tray checks a signed update feed (stable/beta channel, chosen in the
web UI). Depending on your setting it prompts before updating or applies
updates automatically: a verified binary self-replace (with automatic
rollback copy) or the platform installer (DMG/setup.exe). Linux packages
update through your package manager or a new Docker image. Nothing is
ever installed without a valid ed25519 signature and SHA-256 match.

### Database snapshots before upgrades

When an upgrade brings database migrations, Ether first writes a consistent
copy of a SQLite database (`ether.db.pre-migrate.bak` in the data directory).
It stores the matching `config.json` and credential key, as they were before
the upgrade touched them, in `ether.db.pre-migrate.bak.files/`. The three newest
sets are kept: older ones become `.pre-migrate.bak.1` and `.2`. Everything is
readable only by the server account. To roll back by hand, stop Ether, install
the previous version, and replace `ether.db`, `config.json` and
`.credentials-key` with a set's copies.

Do not start binaries from before the multi-app upgrade against a migrated database. Older
binaries may merely log newer migration versions and continue running; that
behavior does not establish schema or writer compatibility. For that
transition, stop and fence every old writer, upgrade every serving node,
and follow the [upgrade and rollback runbook](acceptance/upgrade.md).
Rollback requires restoring the matching database and configuration backup
with every new writer stopped and fenced.

Not covered: PostgreSQL databases get no pre-upgrade snapshot (use scheduled
backups), and rolling back a release never restores a snapshot automatically,
because that would discard everything written since the upgrade.

### Signed feed freshness

Update feeds carry signed issue and expiry times, valid for 30 days. Ether rejects
expired or undated feeds instead of treating them as proof that no update exists.
The repository’s weekly **Refresh signed update feeds** workflow renews the
current release without changing its binaries; it can also be run manually.
Maintainers must keep that workflow enabled and the release signing secrets
configured. A failed freshness check leaves the installed server running.
Check the system clock and the feed publisher’s workflow if it occurs.

Feeds use detached `.json.sig` signatures so older Ether clients can verify the
new freshness fields. Custom feed publishers must supply both files and renew
them before expiry with `ether-release-sign renew`; never renew unverified input.

### Stored credentials

Ether seals the credentials it stores, in `config.json` and in the database
(Trakt tokens, notification channels, Live TV sources, built-in indexer
logins, the ratings key), with the credential key `.credentials-key` in the
data directory. Each one is bound to its setting and to where it is sent, so
a credential copied to another setting, or left behind when a host is
changed by editing the files, no longer works. Backups include the key and
restore only with it; keep them private. If the key file is lost while the
database still holds sealed credentials, Ether refuses to start, on every
start, until the original `.credentials-key` is restored.

After updating from a release that stored database credentials in plain
text, Ether converts them once the update is confirmed healthy and, in a
cluster, every node runs the new release. Until then older releases can
still read everything, so an automatic rollback is safe. Going back to an
older release by hand afterwards leaves its stored credentials unusable until
you update again or re-enter them. Backups and database copies made before
the conversion still hold the old plain-text values: re-create backups you
keep and delete old ones you no longer need. Search results Ether caches no
longer contain indexer API keys.

Ether warns at startup when the data directory or the credential key can be
read by other accounts on the machine; it does not change permissions you
chose.
