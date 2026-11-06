# Shield subtitle metadata contract (#191)

This follow-up targets official Plex Android TV / Shield detail requests.
Other clients retain their existing metadata and discovery policy.

## Detail-page behavior

- Prefer the matching session, persisted/resumed probe or completed warm probe
  when rendering subtitle tracks and their stable IDs.
- With warming enabled, resolve and probe the source without creating a live
  playback session, writing watch progress or prefetching video.
- Probe and foreground version lookup share a four-second wait allowance.
  A source failure or timeout returns the normal metadata response, including
  estimates when exact facts are unavailable. It never turns a probe failure
  into a 503 detail page.
- An admitted probe can finish in the background within the existing 45-second
  preparation budget. Subsequent metadata reads use its tracks. A slow source
  can therefore still lack embedded tracks on the first response.
- Existing concurrency, rate limits and source validation remain authoritative.

## Owner settings and IO

| Setting | New detail-page source IO on Android TV |
| --- | --- |
| Warming enabled | Search-cache hit avoids an indexer search; a miss may search. An uncached source may acquire a debrid link and read container bytes for probing. No playback video prefetch. |
| `DetailWarmup: off` | No new resolve, indexer search/refill, debrid API request, source open or probe. |
| Speculation disabled | Same restriction as detail warming off. |
| Cache mode `off` | Same restriction as detail warming off. |

Under disabled settings, existing session/warm/persisted probe facts and cached
version search results may still be read. A cached search result alone does
not authorize source opening or probing. Missing probe facts remain estimated
until a permitted operation (such as Play) obtains them. Detail metadata and
its version watcher must not trigger an indexer refill under these settings.
This intentionally favors the owner's traffic limit over complete first-open
track discovery. Normal catalog and database metadata reads remain unchanged.

## Lasting checks

- `TestShieldTroyCaptureFollowup`: HEVC Main 10 DV/HDR10, DTS-HD MA 5.1,
  embedded SRT/PGS; first metadata, queue, decision/start, PGS PUT and rereads;
  stable IDs, Matroska text copy and PGS burn in both conversion paths.
- `TestShieldSlowMetadataKeepsNormalResponseAndPublishesLater`: blocked probe
  plus slow version lookup return normal metadata in one shared allowance;
  releasing the probe supplies tracks on the next read.
- `TestShieldMetadataProbeFailureKeepsNormalResponse` and
  `TestShieldDisabledDetailDoesNoSourceIO`: errors preserve normal metadata;
  all disabled settings produce zero new indexer/debrid/source requests,
  whether search results are cached or absent.
- Playback package metadata-probe tests verify no live session/video prefetch,
  cached reuse, active-player behavior and owner setting admission.
- `TestShieldDetailMeasurement` is opt-in (`ETHER_191_MEASURE=1`). It measures
  cold/repeated detail p50/p95 and indexer, debrid API and media HTTP counts
  using production resolver, playback, Real-Debrid provider and range engine
  against local fixtures. It is an offline policy/latency test, not a physical
  Shield or external-provider benchmark. Keep run samples outside the repo.

The captured request queries are redacted and raw profile headers are absent;
replay parameters are reconstructed, not an exact request recording. Real
HEVC/HDR10 output segments retain SRT in the media test. Actual tone-map output
requires an FFmpeg with the appropriate filters; physical Shield verification
remains outstanding.
