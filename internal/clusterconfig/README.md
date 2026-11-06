# Deployment enrollment and local drain control

Standalone SQLite and historical full-server startup are unchanged when
`ETHER_DEPLOYMENT_FILE` is absent. Deployment mode uses PostgreSQL authority,
including with an external database. A bundled singleton PostgreSQL is NOT DB HA.

`ETHER_DEPLOYMENT_FILE` names a mounted node-local JSON document (not a shared
config section). Initial supported runtime sets are full-server (empty roles or
all five roles) and transcode-only, subject to runtime composition acceptance.
Other role sets remain rejected until their actual service graph is integrated.

Example document (all Secret values are separate files):

```json
{
  "bootstrap": true,
  "roles": ["transcode"],
  "internalAddress": "https://ether-transcode-0.ether-private:32401",
  "privateListen": ":32401",
  "probeListen": ":32402",
  "databaseURLFile": "/run/ether-secrets/database-url",
  "clusterKeyFile": "/run/ether-secrets/cluster-key",
  "caFile": "/run/ether-issuer/ca.crt",
  "caKeyFile": "/run/ether-issuer/ca.key",
  "rootsFile": "/run/ether-issuer/roots.pem",
  "capacity": 2,
  "clockBoundKnown": true,
  "clockSkewMillis": 1000,
  "tempDir": "/scratch/transcode",
  "cacheDir": "/scratch/cache",
  "drainSeconds": 120
}
```

Private and probe ports default to 32401/32402. `capacity` (1..4096) is explicit;
clock bound must be explicitly known and skew 0..60000ms. Deployment operators
must maintain that pairwise bound between source, database and encoder clocks.
`resourceScope` optionally names a shared physical encoder budget; otherwise the
persistent node ID supplies the scope. Old boot claims retain capacity on that
scope until authenticated retirement proof. Capacity is admission accounting,
not proof that detected hardware can encode a requested format.

Startup holds the data-directory lock, opens/migrates PostgreSQL, takes a shared
provisioning lock, then seeds or verifies shared settings. `bootstrap:false`
requires existing shared settings. Multiple bootstrap pods with the SAME
provisioned encryption key serialize and adopt the first committed identity.
An existing cluster with a different key fails closed. Cluster key is exactly
32 RAW bytes, not base64 text (Kubernetes Secret data is already decoded on disk).
Never put DB passwords, encryption keys, CA keys or join tokens in Helm values,
command arguments or logs. File paths may be stored in deployment configuration.
The deployment Secret database URL takes precedence over legacy URL environment
overrides in deployment mode.

Every replica needs its own retained PVC/data directory. It generates and retains
its own `node.json`; NEVER distribute or clone that file. Repeated startup with
matching local key/config is idempotent, including after interruption between
local writes or after shared seeding. Mismatched local keys and existing config
without its key require explicit offline recovery; no automatic overwrite or
adoption occurs. An existing configured standalone server requires explicit
`ether cluster init` migration. Trusted provisioning does not consume join tokens:
it can recover a blank replica after a legacy token was consumed before its local
files were installed. Never reuse a single-use token across replicas.

The cluster encryption key and database credential already grant access to the
trusted cluster state. Specialists are NOT a sandbox for untrusted machines.
Issuer custody follows that trust model: a separately generated Ed25519 CA cert
and PKCS8 private key live in a separately administered Secret, restricted to
trusted Ether service accounts. Never derive signing keys from encryption keys.
The issuer cert is directly trusted (no intermediate chain). Each process mints
an ephemeral Ed25519 leaf for its EXISTING membership Incarnation and configured
DNS/IP host, valid twelve hours, refreshed hourly. No extra TLS boot ID is minted.

For CA rotation first distribute an overlapping old+new explicit `roots.pem`,
then swap issuer key/cert, then remove the old root after all members have rotated.
Secret contents are reopened and checked on rotation; node policy and identities
are immutable until restart. Invalid rotation fails closed and does not publish
new capabilities. Encryption key rotation is separate, requires coordinated
credential resealing/restart, and never happens by replacing local settings from
arbitrary mounted bytes. DB password rotation requires application restart.

Probe listener contains only GET `/livez`, `/startupz`, `/readyz`, `/admissionz`.
Liveness has no DB dependency. Startup means real compatible services started;
readiness describes existing-work service availability. Admission additionally
rejects a draining node. Private headless DNS may publish not-ready addresses;
placement still requires verified current membership, exact boot, role, protocol
and admission. Probes never carry media, provider credentials or drain controls.

`ether cluster drain --data-dir /config --wait 120s` (`--timeout` alias) is local
control authenticated by the data-directory owner's filesystem permissions.
It atomically writes a random operation and exact current boot to `drain.request`,
then waits for matching `drain.status` with proven zero pending obligations.
Files must be regular 0600 files; symlinks and group/world-accessible files are
rejected. A stale PVC request/ack cannot drain or acknowledge a new boot.
Timeout returns nonzero, retaining all unresolved ownership/capacity. No public
HTTP drain route exists. Use explicit drain-before-delete and StatefulSet
OnDelete for planned upgrades; preStop is only a bounded fallback, not a promise
of seamless replacement. SIGTERM stops new admission and preserves private
existing-media handlers, membership and lease reconciliation during drain.
Source cleanup runs separately from heartbeat/expiry and may block in backend
Close: a deadline/network silence never proves remote encoder or source retirement.

If SIGTERM reaches `drainSeconds` with unresolved source/encoder obligations,
Ether reports the unresolved drain and KEEPS membership, private handlers and
cleanup running until proof arrives or an external supervisor terminates the
process. Arbitrary backend Close is not cancellable. CLI drain times out nonzero
without stopping the runtime. Kubernetes terminationGracePeriod may ultimately
kill the process; that does not retire its durable reservations. Planned upgrades
must require a successful explicit drain before deletion. Set the external grace
period above the intended drain window, allowing control and shutdown overhead.

The supported deployment topology uses full servers as frontend/source/delivery
coordinators and transcode-only nodes as remote execution targets. Full servers
keep their historical local encoder but are EXCLUDED from private transcode
placement: local legacy sessions do not yet participate in the durable placement
budget, so admitting private encodes there could double-book hardware. This does
not disable their existing local capabilities. Transcode-only nodes use a
separate fixed-plan encoder manager with adaptive ladders/governor/enhancement and
automatic source mutation disabled. Unified local+private physical accounting is
required before full servers can become remote encoding targets.

`ledgerCapacity` is distinct from concurrent encoder `capacity` and boot-local
history retention. It defaults to 1000000, accepts 4096..10000000, and must cover
concurrent/history capacity. Every deployment replica must use the SAME value.
Placement migration 2 stores that fleet ceiling. Initial startup seeds or verifies
it under the serialized placement lock. Every new-generation Reserve checks policy
in the SAME transaction as allocation; conflicting coordinator settings fail
closed. Existing assignment replay/renewal/retirement remain available on a stale
coordinator so correcting policy never destroys ownership. Drain old pre-policy
candidate binaries before introducing this policy; they predate this enforcement.

Changing JSON alone cannot silently raise the ceiling. The trusted operator runs
`ether cluster ledger-quota --data-dir /config --expect 1000000 --capacity 2000000`,
then rolls matching `ledgerCapacity:2000000` settings across the fleet. This is an
explicit monotonic compare-and-swap, never a decrease or table cleanup. Stale
coordinators stop NEW allocations until their configured ceiling matches the new
policy. `/admissionz` includes a `ledger` object with capacity/used/retired/exhausted/
matches; exhaustion returns 503 for new admissions and logs the quota transition,
while `/readyz` and existing work remain available. A larger ceiling is bounded
operational mitigation, not safe history compaction or unlimited admission. Retired
generation floors remain permanent; never delete them to manufacture capacity.

## Role boundary and public routing

The staged isolated graphs construct these concrete dependencies. Source, server
and delivery singleton activation remains a separate acceptance guard; construction
checks alone do not establish shipping playback or replacement continuity.

| Graph | Local services | Public responsibilities |
| --- | --- | --- |
| Historical full server | Existing providers, source manager, frontends and local encoder | Existing public APIs and representations |
| Source singleton | Providers, resolver, cache, native/ffprobe metadata probing and source manager | None; authenticated private source operations only |
| Transcode singleton | Dedicated fixed-plan encoder and private input gateway | None; authenticated private encoding only |
| Server singleton | Catalogue, identity, owner console, protocol frontends and remote playback adapters | Browse/control and existing protocol routes that combine admission or authentication with response bytes |
| Delivery singleton | Identity/policy readers and indexed authenticated representation forwarding | Bounded GET/HEAD representation routes only |

Server isolation excludes local provider connections, source/cache ownership and
encoder construction. It does not promise that the server sends no response bytes.
Existing Plex `start`/`start.mkv`, direct `/library/parts` and query subtitle routes
retain their URLs and control semantics, including track-selection PUT requests.
Their authenticated proxy responses are legitimate frontend work. Silo's
viewer/device-authenticated playback routes likewise stay on the API frontend
until an explicit equivalent authenticated delivery seam exists.

Mixed-role deployments need same-origin routing to API (full/server) and media
(full/delivery) Services. Route only the supported GET/HEAD pure representation
namespaces to media: canonical `/sessions/media/` and the typed Plex
`/video/:/transcode/universal/session/{id}/{rung-or-representation}/{resource}`.
The latter supports bounded playlists, headers, init and TS/M4S/MKV segments;
ASS and query subtitle routes remain API. Never route the broad universal or
`/library/parts` prefix to delivery. Jellyfin/Emby `/jellyfin/sessions/media/` and
`/emby/sessions/media/` require the explicit protocol-bound alias adapter, preserving
grant, query and original request policy. A generic prefix rewrite without that
owner-side surface check is not equivalent. Full-only routing stays unchanged.

Independent source selection prefers currently eligible source-only peers before
full-server peers. Once preparation has an operation identity, an uncertain result
retains that exact owner and operation until proof; it never retries on another
source merely because a connection failed.

The existing owner-authenticated `GET /ether/api/cluster/` optionally includes
`selfRuntime` for a deployment process only. `nodeId` and `boot` match the same
response's captured `self`; `runtimeSampledAt` timestamps its local cached atomic
state (`protocol`, `started`, `readyExisting`, `acceptingNew`, `draining`).
`quota.sampledAt` retains the last successful quota observation time and
`quota.status` its measured ledger status. A failed current quota read closes
admission without refreshing the retained observation. Optional `resources`
retains each metric's own `at` and `state`; these are resource sample times, not
response times. Missing runtime/quota/resources are unknown, not measured zero.
This is local process diagnostics, not fleet readiness, placement or retirement
proof. Drain can leave `readyExisting:true` with `acceptingNew:false`.

The frozen real Plex writer inventory (`internal/plex/representation_paths.md`)
confirms that public DASH uses the API `start.mpd` response plus typed session
`{numeric-representation}/header` and `{number}.m4s` URLs. ASS uses the existing
API query subtitle routes. Encoder filesystem `manifest.mpd` and subtitle ASS
names are not authored session-namespace URLs. Do not expand the delivery grammar
from those filenames. A canonical-HLS-only lab restriction records unproven mixed
role playback/capability acceptance, not a demonstrated authored ASS/MPD URL gap.

Source-only nodes require the existing bundled/configured `ffprobe` for genuine
native-incomplete formats (for example MPEG-4 Visual `mp4v` in MP4). The source
manager retains native probing first and uses a probe-only fallback without an
encoder, hardware inventory or transcode session manager. It accepts only the
source manager's literal-loopback HTTP capability, preserves actual parsed facts,
and admits at most two concurrent probes with a 20-second cancellation deadline
including queue time and a shared 2 MiB output bound. Caller cancellation and
shutdown stop the probe; its slot and service remain owned until the runner's
child and output readers join. Close requests cancellation and joins, never
substitutes elapsed time for completion. Errors omit capability URLs and raw
ffprobe stderr. Genuine failed probes remain estimated and are still rejected by
remote encoder preflight; no estimate or capability gate is relaxed.

New runtimes may include optional numeric `pendingRuntime`, `pendingSessions` and
fixed `pendingCategories` in protected `drain.status`. These count outstanding
barriers, not distinct playbacks: one preparation can contribute to a runtime
barrier and the app session list independently. Categories cover source
preparations/lifetimes/callbacks, encoder starts/tokens, inputs, remote preparation
states, executions, reopens and placements. A runtime accounting error leaves
`pending:-1` and omits its unknown runtime/category fields; known app-session
counts may remain. Missing fields on older binaries are unknown. The existing
exact boot/operation, permissions and 4 KiB bound remain mandatory, and errors
never acknowledge quiescence. This cannot retrofit diagnostics or terminal proof
into an older running process.
