# Trailer preview backend and Vela handoff

`frontend.TrailerPreviews` is an optional capability on the runtime's `Sessions`.
It is forwarded by `sessions.BindSurface` and the scorecard session wrapper.
The app wires `BrokerDeps.PreviewSource` to the existing extras discovery and
resolver, then to the durable trailer cache. No home/row projection calls it.

## Vela route contract

On an authenticated `POST /vela/v0/titles/:id/trailer-preview`, validate the
body `{idempotencyKey, client}`, map client capabilities using the same bounded
mapping as ordinary playback, and invoke `TrailerPreview` with the authenticated
viewer/device, protocol, authorized title key, language and caps. Never accept
viewer identity or remote/LAN policy from the JSON body. Return the JSON
`TrailerPreviewResult` union (`state`, not `status`); set `Retry-After` from
`retryAfterSeconds` for preparing. The optional capability being absent means
unavailable. No movie Plan/Open call belongs in this route.

Mount `/sessions/media/` through the same broker `ServeMediaFor` or
`ServeMediaAccess` handler. URLs are relative, same-origin, viewer-bound grants.
They expire two minutes after issuance. Both numeric wire generations start at
1 because a session has exactly one immutable trailer source. The SHA256 of
source identity/settings is internal, never a wire generation. A new focus key
replaces the viewer's prior preview and revokes its bytes immediately. Repeating
an active key for the same viewer/device/title reuses the grant if its cached
file remains valid. Preparing has no grant/session; poll using a fresh key.

Vela's existing session command route must recognize preview session IDs before
looking up ordinary movie plans: validate the expected numeric generations
(1,1), use broker `Stop`/`CommandStop` with authenticated viewer/device, and
return its normal ended-plan response with purpose `preview`. The broker's
ordinary Sessions listing includes previews with `Purpose == "preview"` for
stop-command discovery. No movie session, resume read/write, watch receipt,
scrobble, timeline callbacks, feature prefetch or encoder reservation occurs.
A late ready response after focus cancellation must be stopped by the client.
The route must advertise the feature only when integrated; discovery/hero
projection must never mint grants. The adapter integration below supplies command lineage and generation validation.

## Resource and playback semantics

Preview allowance is separate from feature-film stream slots: one active
preview per viewer, at most 128 grants and four pending requests per broker.
The extras cache admits only one fill at a time. Busy requests return preparing.
Cold focus starts bounded preparation, waits at most 100 ms, then returns
preparing while that focus-triggered work completes. No grant/session exists
until a fresh poll rechecks current policy. Four preparations maximum, each
with a 90-second bound; replacement and broker shutdown cancel preparation.
Ordinary LocalTrailers downloads retain their requesting open context.
Stop, replacement, surface teardown and broker close revoke active grants and
cancel ongoing response reads. Grants are local to this node; cluster routing
must keep preview POST, media and commands on the same node (fail closed on
other nodes). There is no distributed preview session persistence.

Bytes live under `<config.cache.dir>/trailers`. The owner setting
`extras.trailerCacheMaxBytes` defaults to 512 MiB at zero; negative disables the
cache. LRU is durable filesystem mtime, updated on cache hits and media opens;
cache accesses enforce the latest cap and a maximum of 512 files, independent
of library size. The bounded directory index is read only on use. Each MP4 is
at most 64 MiB; staging bytes are reserved inside the cap, oversized or non-MP4
responses are rejected. A single crash staging file is retired on next use.
There are no startup scans, sweeps or prewarming. Unix may retain an evicted
file's open inode until an in-flight response closes; Windows refuses eviction
of an open file and the fill fails closed. Cache root changes take effect with
app reconstruction, like the existing cache service.

Jellyfin/Emby `/Items/{id}/LocalTrailers` retains its existing extras discovery.
The authenticated `/Videos/{extraId}/stream[.mp4]` path uses the same cached MP4
through optional `frontend.CachedExtraStreams`, with the existing remote stream
fallback if caching cannot supply it. No movie playback session is opened.
Moonfin native/MP4 focus playback needs device verification; an Apple player
requiring HLS may need a subsequent trailer-only remux path (this implementation
serves MP4 byte ranges, never launches an encoder).

## Verification

Focused offline tests cover cap reduction, LRU, persistence, source config
invalidation, oversized files, fill coalescing, and preview purpose isolation,
viewer/device authorization, idempotency, replacement, expiry and command stop.
The coordinator must verify real Vela focus/cancellation/stop after route wiring,
Moonfin LocalTrailers focus (including Apple/HLS behavior), and Jellyfin/Emby
explicit trailers. Confirm downloads only start on focus/open/play and preview
never changes Continue Watching/resume/history or starts a feature release.

## Vela adapter integration

The integration branch `w0312/vela-trailer-preview` merges the Vela server chain, owner row controls, and this backend. It mounts POST /vela/v0/titles/{id}/trailer-preview, preserves ordinary POST idempotency, maps bounded authenticated client capabilities, and forwards the exact ready/preparing/unavailable union. Current owner extras/resolver/cache readiness is exposed through optional TrailerPreviewAvailability on the broker, surface and scorecard wrapper. The route enforces 20 new focus requests per viewer per ten seconds with a bounded lazy rate table; no worker or timer exists at idle.

Row.previewOnFocus is taken from shared row snapshots, including continuation pages. Hero.previewOnFocus comes only from its actual featured source row. Neither capability support nor optional trailer metadata enables a row/hero by itself. Preview grants keep lightweight, expiring command lineage (bounded to 128) in the Vela adapter; they do not open ordinary plans/watchers. Existing commands accept only generation-matched stop for previews and return an ended plan with purpose=preview. Observations and SSE are rejected for preview sessions. Canceled issuance revokes a late broker grant. A ready grant is immutable and broker-local; the existing node-affinity limitation still applies.

Owner hints are translated in all 19 locales. The collection control is disabled with a reason until a Home or Library row is enabled. Collection-tab grids retain ordinary artwork; only opted-in Home/library rows receive preview presentation. Console screenshots are mock-console verification, not proof of real-device focus playback. Real Vela and Moonfin focus/media/stop flows remain the coordinator's release gate.

Cold Vela focus now waits at most 100 ms for source preparation, then returns preparing without a session/grant. At most four focus-triggered preparations exist; each has a 90-second cancellation bound, cancels on another title/device focus or broker shutdown, and starts no idle worker. A fresh poll rechecks viewer/title policy before issuing a grant. The prior viewer grant is revoked before cold work begins. Completed source handles are held for at most a minute on lazy access (four total); cached bytes remain under the durable owner cap.
