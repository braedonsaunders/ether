# Owner node controls

The owner API exposes `GET /ether/api/cluster/nodes/{id}/control?boot=...`
and `POST /ether/api/cluster/nodes/{id}/control`. Both require an authenticated
owner, completed setup, and the exact live membership incarnation. Standalone
and existing local controls retain their behavior.

POST accepts only this bounded document (4096 bytes, unknown fields rejected):

```json
{"requestId":"owner-generated-unique-id","boot":"membership-incarnation","expectedVersion":0,"patch":{"weight":2}}
```

Weight is 1–100. The typed `draining` field remains recognized but is unsupported
on every runtime. The target rejects queued drain patches, including mixed
weight/drain requests, before any mutation.
GET returns `nodeId`, `boot`, `requestedVersion`, `appliedVersion`, `state`
(`idle`, `pending`, `applied`, `failed`), optional `requestId`/`errorCode`,
`updatedAt`, `observed: {weight, draining}`, and `capabilities` entries with
`key`, `scope`, `supported`, and `reason`. No credentials or raw config are read
into this response.

Use GET's requestedVersion for expectedVersion. Reject stale observations
(over 30 seconds), boot mismatch, and pending requests before enabling writes.
The backend repeats these checks. New submissions return 202 pending; bus
publication never means applied. The exact target applies locally and stores
its acknowledgement. UI completion requires matching requestId and boot,
state applied, and appliedVersion equal to requestedVersion. Failed application
keeps the previous appliedVersion. updatedAt is the target's latest observation,
not the requesting node's clock or a promise that admission is permanently fixed.
External local controls can subsequently change observed values.

Request IDs are durably bound to their canonical payload. Retrying the current
request returns its actual current state without reapplying it. Reusing the ID
with another payload, or replaying an older completed request after another
request replaced it, returns conflict. Only one request per node/boot can be
pending. Database CAS also orders target observations against requests. A missed
bus hint is recovered by a five-second reconciliation loop owned by the existing
app lifetime. A restarted node never consumes a prior boot's request. Old boot
records and request history are retained; this increment does not implement
history expiry or a fleet event feed.

| Setting | Actual scope and behavior | Remote increment |
| --- | --- | --- |
| Weight | Node-local share of account budgets; live config and heartbeat | Supported |
| Draining | Admission state; updater/CLI/shutdown ownership needs a shared atomic interlock | Unsupported on every runtime, ownership_interlock_required |
| maxStreams/defaultStreamLimit | Shared fleet configuration | Unsupported per-node, fleet_setting |
| maxTranscodes | Ordinary manager has a hot setter; private manager uses startup deployment capacity | Ordinary not_allowlisted; deployment restart_required |
| hwAccel | Node-local config; manager hardware detection requires restart | Unsupported, restart_required |
| pause | No universal node pause; cachefill pause uses shared cluster state | Unsupported, no_safe_setter |
| roles | Node-local startup responsibilities; no reviewed executor | Unsupported, role_lifecycle_unavailable |

Fleet status still has membership `lastSeen`, active playback `load`, advertised
capabilities, and a local `selfRuntime` observation. There is no authoritative
peer resource/health/readiness/retirement event DTO in this increment. Unknown
peer values remain unknown; never apply selfRuntime to a peer. Command receipts
are not session-retirement receipts or a fleet event stream.

Join-token owner reads return only unused, unexpired tokens. Owner reads and
creation each prune at most 256 expired records, replacing the old seven-day
on-create cleanup. This is lifecycle-triggered maintenance, not a scheduled
wall-clock sweep. The UI must filter expiry as time passes after a response.
Used/expired/invalid credentials never become valid through cleanup; pruned
records become unknown and their historical used/expired reason is no longer
retained. No background worker is started for token cleanup.

## Read-only role guidance

`GET /ether/api/cluster/nodes/{id}/role-preflight?boot=...` requires the same
owner and exact live boot. It always returns `eligible: false`, `executor: none`.
Membership timestamps are preserved; authoritative active sessions and ownership
retirement remain unknown. A last observed full server or leader adds explicit
refusal reasons. No timeout can change this response into restart authorization.

The guidance includes a copyable full-pool replica example verified against
this checkout's Helm values and templates. It changes the desired count of the
whole pool, not this exact node's identity or roles. Installed deployment
ownership is unknown. The chart disables service-account token mounting and
uses OnDelete StatefulSet updates; this API executes no Helm, Kubernetes,
restart, drain, or role operation. Mixed-public-role runtime guards stay closed.
A future durable role lifecycle needs a separately reviewed executor, exact
ownership proof, fresh authoritative session/retirement checks, last-server and
leader-handoff guards, and new-boot verification before it can report applied.
