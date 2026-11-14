# Vela original playback and secure origin

Vela may send `client.originalPlayback` independently of native media-element
capabilities: `{version:1, range:true, demuxContainers:[], videoDecoders:[],
audioDecoders:[], webgpu:boolean}`. These arrays identify installed decoder
families; clients must still probe exact demuxed decoder configurations and
performance. Missing or unsupported negotiation retains the native decision.
Live originals are not supported.

An admitted original has a ready `direct`/`file` path with `original:true`,
`range:true`, optional `byteLength`, and `sourceId`/`timelineId` naming the pinned
source revision. `originalVideo` contains only observed source codec, picture,
colour and Dolby Vision facts; missing facts stay absent. Audio and subtitle
`streamIndex` values are actual source indexes, not track-list positions.
No width, bitrate or tone-map conversion is applied to original bytes.

The existing broker grant and shared engine serve GET/HEAD, bounded and suffix
ranges, multipart ranges, Content-Length, Accept-Ranges, Content-Range, ETag and
Last-Modified. Invalid original ranges return 416; an If-Range mismatch returns
412 instead of sending the entire file. A changed pinned source is rejected.
Each media request checks the refresh family, active viewer and current media
policy. Refreshing the same family keeps the session, source and media URL;
revocation/expiry denies subsequent reads. Reopen a lost/ended session through
normal playback admission; do not combine its bytes with an earlier sourceId.

Compatible native HLS fallback paths have `readiness:"on-demand"` and no URL.
The canonical `switch` command starts the selected output at `atSeconds` on the
same pinned source, returns its ready URL and advances seekGeneration. It never
resolves a different edition. Other output URLs become on-demand. Native HLS
paths carry `timelineOriginSeconds` at the requested absolute title position;
original paths use zero. Empty boundaries do not claim an independently proven
automatic splice point. Clients must request the server switch.

## Independent audio fallback

`features.audioFallback.available` requires a real local AAC encoder and HLS/
MP4 muxers. With that capability and an installed AAC decoder, a runtime that
supports the original video may admit it even when it cannot decode the source
audio. No encoder runs until requested.

`POST /vela/v0/sessions/{id}/audio-fallback` accepts
`{idempotencyKey,trackId,positionSeconds,sourceGeneration,seekGeneration,
preferredCodec:"aac"}`. It returns `{id,audioGeneration,trackId,url,
mimeType:"application/vnd.apple.mpegurl",codec:"mp4a.40.2",
timelineOriginSeconds:0}`. The URL serves only the selected audio as AAC fMP4
through the existing HLS pipeline, under the session's media grant. Packet PTS
remain absolute title time; playlist numbering starts at zero for that requested
position. Schedule the first playlist entry at positionSeconds. Source and seek
generations stay unchanged. Replacing audio invalidates the old rendition URL
and increments only audioGeneration. `DELETE` on
`/vela/v0/sessions/{id}/audio-fallback/{audioId}`, with `{idempotencyKey}`, cancels
only that rendition and returns `{accepted:true}`. Session stop retires audio
before releasing the original's source pin. Opus/raw output is not advertised.

## HTTPS discovery and handoff

Capabilities, negotiate and bootstrap expose optional `secureOrigins:string[]`
from the same published connection snapshot as Plex resources, preferred LAN or
remote first. Values are HTTPS base origins without paths, query or fragments;
unknown origins are omitted. There are no request-time network probes or
hostnames synthesized from Host/forwarded headers.

The coordinator verified this lab origin and certificate:

`https://192-168-68-65.9cf6a8a376b9403baf1de133251c4c46.plex.direct:32550/vela`

The shared listener serves Vela assets, SPA routes, `/vela/sw.js` (worker allowed
scope `/vela`), API and relative media URLs there. `/vela/` redirects relatively
to the canonical `/vela`. Vela uses bearer/device grants, not origin cookies;
no Vela asset CSP blocks workers, WebAssembly or WebGPU. No extra port or
certificate override is required.

An authenticated `POST /vela/v0/auth/web-code` accepts
`{idempotencyKey,targetOrigin,device}` and returns `{code,targetOrigin,expiresAt}`.
The source device authenticates issuance only. The target must exactly match a
trusted advertised origin. The shared console Watch-button issuer/table stores
a hashed, single-use, 60-second code bound to account, Vela and destination.
Navigate with `#code=<encoded-code>` only, scrub it immediately, create a new
HTTPS-origin device, and use the existing `/auth/web-code/redeem`. Direct TLS and
an exact trusted target Host are required for origin-bound redemption. A wrong
origin/surface does not consume the code. Legacy console handoffs still use the
same existing redeem path. TLS-terminating proxy handoff is not advertised by
this implementation; direct plex.direct TLS is supported.

Real-device release verification must cover HTTP→HTTPS new-device handoff,
service-worker/deep-link loading, MKV/HEVC/DV original seeks and tail reads,
long playback across refresh, revocation, native HLS fallback position and
track preservation, and independent AAC audio switch/cancel/A-V sync. The worker
did not access production or the lab; deployment must pair the runtime client
and server before enabling original admission.
