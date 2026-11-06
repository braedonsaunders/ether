# Plex Android posters and subtitles — #341 / v0.3.5

Base: `release/0.3.5` at `45eab97cf`. Branch:
`fix/341-android-posters-subtitles`. At the time of the initial investigation, #341 had no version or
connection details. Later comments and the support recording are analyzed in
[the follow-up](341-android-followup.md), which supersedes the initial reply
draft and reporter assumptions below.

## Evidence and limits

The committed `testdata/conform/plex/manifest.json` lists Web, iOS, Apple TV
and one decision capture. There are **no Android request captures** in that
corpus or Android Plex profiles under `internal/conform` on this base.
Do not describe the new tests as recorded Android requests. Existing PMS
1.43.4 metadata/decision comparisons and the documented PMS 1.42.2 subtitle
conversion facts in #191 supply the existing wire contract. New image requests
are explicitly reconstructed scenarios. No live server or device was used.

The reporter's exact poster failure remains ambiguous. The tests demonstrate
server defects, not that the reporter used those precise requests.

## Reproduced server defects

- #191's delivery and detail-probe corrections were gated by `androidTVClient`.
  Official `Plex for Android (Mobile)` missed PGS selection/burn, explicit text
  burn, the late direct-play-to-HLS Matroska restoration and the detail-page
  probe. Extending the existing decision/start and cold/warm detail tests to
  Mobile covers these paths, track changes and Off.
- The photo handler treated an absolute URL on another connection hostname as
  a foreign proxy request: an authenticated request for
  `https://previous-connection.invalid/library/metadata/<key>/thumb/<stamp>`
  returned 403 even though the relative URL and a `plex.direct` URL worked.
  This models switching local/remote/relay connection bases; it does not prove
  an actual Plex relay session. Android now resolves recognized local artwork
  routes under the outer viewer without dialing the nested hostname. Unknown
  routes/remote image hosts retain the proxy restrictions.
- `format=jpeg` was ignored: a PNG upstream yielded PNG bytes and Content-Type.
  The artwork service caches explicit JPEG/PNG output variants, with
  a decoded-pixel limit, inside the existing artwork byte budget. The original
  fetched bytes remain format-neutral. WebP can
  be decoded; AVIF is not introduced as an output format. There is no recorded
  AVIF request or response in this issue to justify an AVIF-specific change.

## Other poster checks

- Existing integer width/height and 0/1 minSize/upscale requests remain valid;
  malformed values retain validation. No captured Android boolean-string
  grammar is available to justify relaxing validation.
- Both light and detail metadata use relative, token-free thumb/art URLs;
  clients attach their own credentials. Tests exercise both URLs through the
  photo wrapper. Nested owner tokens cannot grant the outer viewer access.
- Provider URLs and API keys remain server-side (`Images.GetPoster`), and
  provider poster output is already JPEG. Providers refuse redirects and use
  the TMDB fallback on failure; there is no provider redirect for the Android
  app to follow. Existing discovery/provider tests cover this path. Changing
  provider redirect policy would affect every client and is outside the
  demonstrated Android failures.
- No changes to Plex Web, iOS, Apple TV or Plezy policy. No device capture was added. Only package-scoped Go tests were used.

## Gates

- Against the unmodified release handlers, the expanded tests fail: Mobile
  detail reports `tracks=[] probes=0 prepares=0`; the late HLS encoder receives
  `Container:mpegts` for SRT and ASS, explicit burn remains false, and PGS rows
  remain `decision="ignore"`. Both Android products return PNG to `format=jpeg`
  and return 403 for the different-connection artwork URL. Existing TV paths
  and the Web/Plezy image controls remain passing.
- Focused corrected tests passed (`internal/plex`, 1.787s) before expanding the
  image gates to include provider JPEG-to-PNG and HEAD behavior.
- Full package gates passed: `internal/artwork` (0.980s),
  `internal/artwork/service` (0.963s), `internal/conform` (0.384s),
  `internal/conform/plex` (3.085s), each with `-count=1 -timeout=120s`.
- Broader Plex package gate passed (4.075s):
  `GOMAXPROCS=2 go test -p 1 ./internal/plex -run 'Test(Android|ShieldSubtitleDeliveryContract|ShieldTroyCaptureFollowup|Photo|.*Subtitle.*|.*Artwork.*|.*ConformsToPMS|ServerMediaURLsOmitAccountToken|DecisionListsAlternateTracksForAndroidOnly|DirectStreamCarriesSelectedTextSubtitle|MetadataListsWarmProbedTracks|MetadataWithVariantsListsWarmProbedTracks|FirstOpenMovieAndShowSeasonWarmProbedTracks)' -count=1 -timeout=180s`.
  This is a selected package gate, not the full Plex suite. No repository-wide
  suite or build was run. Physical Android phone/TV playback remains unverified.

## Review follow-up: conversion cost and format provenance

The PNG response was authored by `TestAndroidPhotoConnectionAndFormat`'s
httptest upstream. It demonstrates the photo handler ignoring the requested
output format; it does **not** establish that XRDB/RPDB or a real reporter's
upstream served PNG. The client asked Ether's photo wrapper for JPEG; Ether
built/fetched a source URL without passing that format to the CDN. A source
image need not have the output format of a photo-transcode request.
`artwork.ResizePoster`, overlay rendering and generated collection posters
already encode JPEG. No provider/generator default needs changing based on
this evidence, and ordinary JPEG-to-JPEG requests already bypass conversion.

The initial format helper repeated decode/encode on every mismatched-format
request. `RemoteCache.RasterFormat` now stores converted bytes in the existing
artwork LRU, sharing its configured byte budget and TTL with source images.
SHA-256 of the source bytes identifies both content and the actual sized
variant; the output format completes the key. Changed source bytes, source
size and format select a different entry. Concurrent identical conversions
coalesce. Authorization stays before cache access in all three Plex handlers.
Warm hits perform a hash and lookup, with no pixel decode or encode; eviction
or expiration can require conversion again.

Measured through the actual photo HTTP handler, with 50 distinct authored
300x450 PNG posters, six workers, 20 cold pages and 20 warm pages, explicit
Android JPEG output, Go 1.27.3 darwin/arm64, `GOMAXPROCS=2`:

| Cache state | CPU/image before | CPU/image after | Page p95 before | Page p95 after |
| --- | ---: | ---: | ---: | ---: |
| Cold | 9.1209 ms | 9.3499 ms | 696.395 ms | 856.515 ms |
| Warm | 8.4706 ms | 0.2071 ms | 562.247 ms | 29.197 ms |

Warm process CPU/image fell 97.6%, with zero upstream fetches in both warm
runs. Cold conversion still happens once per variant: CPU rose 2.5% in this
sample, and wall p95 rose 23.0%. This shared Mac was paging and other workloads
were active, so no cold latency improvement is claimed. These are not isolated
device/network measurements. CPU includes handler/auth work, GC, loopback
upstream work and encoding; generation and compilation are excluded. Cold
clears both caches per page; warm preloads once. p95 is the 19th sorted page
time of 20. Raw results and setup are in
`341-android-poster-performance.json`.

Reproduce on `f81515db6` and the follow-up with the same opt-in measurement
file (it is protocol-only and works with both implementations):

```
ETHER_ANDROID_POSTER_MEASURE=1 GOMAXPROCS=2 go test -p 1 ./internal/plex -run '^TestAndroidPosterPagePerformance$' -count=1 -v -timeout=180s
```

New service tests prove encoded-buffer reuse across repeated/concurrent
requests, source/size/format separation, TTL expiry, shared-budget eviction,
matching-format passthrough and errors not being cached. The full artwork,
artwork/service, conform and conform/plex package gates passed again. The
selected Plex package gate above passed again (3.347s), including the Android
image, provider, subtitle, outer-viewer authorization and other-client controls.

## GitHub reply draft

Thanks for reporting this. I found that the Android TV subtitle fixes in
v0.3.5 did not also apply to Plex for Android phones. The fix extends the
Matroska text-subtitle handoff, PGS burn when conversion is needed, explicit
text burn, and detail-page track probing to the phone app too.

I also reproduced two image-handler failures: absolute artwork URLs using a
different connection hostname were rejected, and explicit JPEG requests could
receive another image format. The Android fix handles those cases while
keeping artwork authenticated. These are offline server tests; I have not
confirmed your exact failure on a phone or TV.

Which Ether version are you running, and does this happen locally, remotely,
or both? If it still happens on v0.3.5 with this fix, please attach a support
ZIP captured after opening an affected poster and trying a subtitle, and
include the Plex app version and phone/TV model. That will let us distinguish
these fixed paths from a different image request or playback profile.
