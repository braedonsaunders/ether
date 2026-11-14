# Trailer preview backend and Vela handoff

`frontend.TrailerPreviews` is an optional capability on the runtime's `Sessions`.
It is forwarded by `sessions.BindSurface` and the scorecard session wrapper.
The app wires `BrokerDeps.PreviewSource` to the existing extras discovery and
resolver, then to the durable trailer cache. No home/row projection calls it.

## Vela route integration (not in this branch's base)

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
projection must never mint grants. Integration with Vela's plan response store
and generation validation remains the route owner's work.

## Resource and playback semantics

Preview allowance is separate from feature-film stream slots: one active
preview per viewer, at most 128 grants and four pending requests per broker.
The extras cache admits only one fill at a time. Busy requests return preparing;
there is no detached download job. Download, discovery and resolver operations
inherit the requesting focus/open context, and downloads time out at 90 seconds.
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
