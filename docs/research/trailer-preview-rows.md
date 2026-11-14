# Trailer preview rows: Phase 1 protocol research

Research date: 2026-11-14. Discussion: https://github.com/braedonsaunders/ether/discussions/487.
Base: 16053c6572ff7c53d4ad5452458e4ec9af6573a4. Research only; device playback has not been verified.

## Findings by client

| Client family | Server-controlled mechanism | Evidence and limit |
| --- | --- | --- |
| Plex Android TV / Google TV 2026.19.1 | No arbitrary PMS hub autoplay trigger established | Focused tile playback consumes an `autoPreview` action from resolved `link.previewData`. The PMS hub converter does not read a hub preview flag; normal PMS trailer actions request explicit playback. Newest 2026.22.x follow-up pending. |
| Plex Apple TV | Existing Extras / primaryExtraKey supports trailer actions; static backdrop fallback for focus | PMS captures include hero/promoted/primaryExtraKey; these are insufficient evidence of focus autoplay. Closed client; no device proof. |
| Plex Web 4.163.0 | Explicit trailer playback using primaryExtraKey; artwork presentation | Main/vendor bundles contain no previewable or HubAutoPreviewType consumer found. Hero autoAdvanceDelayMs advances the carousel. Lazy chunks and HTPC remain unverified. |
| Plex HTPC | Static backdrop fallback pending device proof | Do not infer identical autoplay behavior from Web. |
| Plex Roku | Static backdrop fallback pending device proof | No accessible current source or captured focus-preview request. |
| Plex LG / Samsung | Static backdrop fallback pending device proof | No accessible current source or captured focus-preview request. |
| Jellyfin Web / Web-based clients | GET /Items/{id}/ThemeMedia, ThemeVideosResult.Items | Plays background video on detail view opening with viewer Theme Videos preference. This is not Home row focus playback. |
| Jellyfin Android TV | Static backdrops; explicit trailers | Current BackgroundService loads ImageBitmap artwork. No theme video path found. |
| Swiftfin | Explicit local / remote trailer buttons; static cinematic backdrop | Source exposes trailers as actions; no theme-video or focus-trailer consumer found. |
| Moonfin Core | Focus → debounce → LocalTrailers → video stream | True focus previews when viewer enables Trailer Preview; server cannot force this preference or limit it per row through standard Jellyfin protocol. |
| Emby Web / standard Android | ThemeMedia / ThemeVideos on detail opening | Documented server API; depends on client preference. Android TV app differs. |
| Emby Android TV | Static backdrop fallback | Official community moderator reports no Theme Videos support; verify installed version. |
| Vela | Bootstrap.heroes[].trailer.cachedPlayback | Client already validates ready, unexpired same-origin /sessions/media/ grants. Arbitrary rows need additive protocol/client support. Ether heroes branch currently deliberately omits trailer grants. |

## Plex evidence

Decoded the base64 response bodies in testdata/conform/plex, rather than searching the capture envelopes. Across JSON captures: 1,791 primaryExtraKey fields, 87 theme fields; no previewable field. Hub styles observed are hero and shelf, including a shelf of clip type. Web, iOS and Apple TV captures share normal movie shelves carrying primaryExtraKey and promoted=true; Continue Watching carries style=hero. These are presentation/trailer-discovery fields, not proof of focus playback.

Candidate fields are not interchangeable:

- `art` supplies background landscape artwork; `thumb` is the item's thumbnail/poster. Use a row presentation hint where consumed; maintain movie/show identities and detail navigation.
- `style=hero`, `promoted`, `hubIdentifier`, and `context` describe layout, promotion and identity. No inspected PMS conversion path uses these to issue an autoplay action.
- `type=clip`, `subtype=trailer`, `extraType=1`, Extras and primaryExtraKey expose a playable extra. Replacing movies with clips would change navigation and playback semantics without proving focus autoplay.
- `theme` appears on shows as ordinary theme media; do not supply a trailer video in an audio theme slot. `previewable` was not found in these PMS bodies or the inspected Web bundles.

Plex Android TV 2026.19.1 was obtained from APKMirror (Android TV bundle, versionCode 971050476; concepts only). APKM SHA256: c63535bcc344bf2669ad52f9e27278d6962d9faf26b567ba4b8de85fdda562b6. Its assets/index.android.bundle is Hermes bytecode v96, SHA256 a5e85675d967279a44faf1685af1594a5ee9c0a8c4dd20da3f89fdbedada0d2c. Disassembled with P1sec/hermes-dec using Python stdlib; no decompiled code is incorporated here.

Reproduction anchors in that exact bytecode:

- #48465 checks action id autoPreview; #48466 FocusedTileBackground / #48468 resolve the focused tile's link.previewData, inspect actions, and start background video on focus. Anonymous registration checks and current-screen checks apply.
- #21292 setAutoPreviewBackgroundVideo consults #25245 getShouldDisableAutoPreview (existing playback / connected sender state).
- #10100 pmsHubToTileScroller reads Metadata, title, more, hubIdentifier and context; no style or preview opt-in read.
- #10016 pmsMetadataItemToLink creates lazy previewData through #10017 → #10051 pmsMetadataItemToMetadata. The actions builder #10008 offers normal PMS trailer actions, including #10006 pmsWatchTrailerAction, not an autoPreview action.
- HubAutoPreviewType defines primary and primaryExtra in the bundle; a definition alone does not establish a PMS consumer.

The evidence supports a client UI action as the actual trigger, and no proven arbitrary PMS hub equivalent. It does **not** prove every released Plex client is Discover-only. The screenshot establishes Discover behavior, not a server API contract. Required next proof: a current Android TV trace focusing an Ether-owned movie row and observing its trailer metadata/media request.

Public Plex Web index: https://app.plex.tv/desktop/. Inspected main-8792-0fadf59092c873ab638b-plex-4.163.0-8246fd2.js (SHA256 bdb3e4ce5d49b5eb6be48e11a4aea0f33a357561f8bf1e1303d31255f701cd97) and vendor 9054-9054-65a669ed61de90f41431-plex-4.163.0-8246fd2.js (SHA256 b3c920640042838362dd3a41f8715f806fe4ec3e34f3ba7bbf5eff31cdda789a). primaryExtraKey is used by an explicit trailer playback action. No HubAutoPreviewType/previewable/autoPreview consumer was found in these two bundles. This does not establish absence in every lazy chunk or another TV application.

## Jellyfin, Emby and Moonfin shapes

Jellyfin Web at e9c5a40cdfab49ecffc5b572dc9f9c64f8e30fed, src/components/themeMediaPlayer.js, listens to viewshow for an item id, requests getThemeMedia with inheritFromParent=true, and selects ThemeVideosResult when the viewer enables theme videos. Supply ThemeVideosResult, ThemeSongsResult and SoundtrackSongsResult, each with Items, TotalRecordCount and OwnerId. Video items need normal authenticated playback support. ThemeVideos is a related endpoint; BackdropVideos is a filesystem concept, not a substitute home-row playback instruction.

Ether already implements these endpoints in internal/frontends/mediabrowser/client_compatibility.go, returning empty results after item authorization. It implements both /Items/{id}/LocalTrailers and /Users/{userId}/Items/{id}/LocalTrailers in extras.go/browse.go.

Moonfin Core at 00da928c26747650abadc25198265ddf2acfac63, lib/ui/screens/home/home_screen.dart: _schedulePreview checks the viewer mode and playback state, delays (default 1200 ms), and _resolvePreviewTrailer fetches the first LocalTrailers item. Movie/Series use their own id; Episode/Season use SeriesId. _buildPreviewUrl requests /Videos/{trailerId}/stream (native), stream.mp4 (web), or master.m3u8 (Apple player), with PlaySessionId/DeviceId, H.264/AAC and a bounded 720p profile. Native clients can fall back to RemoteTrailers YouTube resolution; Web requires a local trailer. A regular URL in RemoteTrailers does not replace LocalTrailers for this flow. This works independently of any new Ether owner toggle; standard Jellyfin rows do not convey per-row autoplay policy.

Swiftfin local checkout 2ff622d0a7cd9a5218646545fc47f420d3005d02: Shared/Views/ItemView/Components/ActionButtons/TrailersActionButton.swift explicitly plays selected local trailers and opens remote trailers externally. Jellyfin Android TV source at 238058e6811d38e3762df35a54a596f06acd3482, BackgroundService.kt, loads artwork bitmaps and rotates images.

Primary references:

- https://jellyfin.org/docs/general/server/media/movies/#theme-media
- https://github.com/jellyfin/jellyfin-web/blob/e9c5a40cdfab49ecffc5b572dc9f9c64f8e30fed/src/components/themeMediaPlayer.js
- https://github.com/Moonfin-Client/Moonfin-Core/blob/00da928c26747650abadc25198265ddf2acfac63/lib/ui/screens/home/home_screen.dart
- https://github.com/jellyfin/jellyfin-androidtv/blob/238058e6811d38e3762df35a54a596f06acd3482/app/src/main/java/org/jellyfin/androidtv/data/service/BackgroundService.kt
- https://dev.emby.media/reference/RestAPI/LibraryService.html
- https://emby.media/community/topic/138419-background-video/

## Vela and implementation boundaries

Read-only Vela checkout 9d173fd973ad436fc1c4a2dbe6b8d5a42de58ea0: client/src/discovery/models.ts accepts at most five heroes and validates cachedPlayback.status=ready, future expiry, same origin and /sessions/media/ path. client/player/preview.ts creates a muted inline player with preload=none; it stops when inactive, offscreen, hidden, expired, save-data constrained, or reduced-motion/still is requested. Arbitrary rows currently map ordinary cards without trailer metadata.

Ether origin/w0312/vela-heroes at e1c9af110fb35233eac880ed9266166b327c7d09, hero.go, intentionally omits trailer because extras discovery/resolution is not a cached playback capability. Phase 2 needs a lazy authorized grant issuer and a client focus call; adding metadata alone will not start downloads.

Existing internal/extras List and Resolve caches are bounded **memory caches**, with expiring remote stream URLs. They are not a durable downloaded trailer cache. Home generation must never call List/Resolve per card. A durable preview facility must be invoked by focus/open, coalesce work, enforce viewer policy, bound concurrency/bytes/retention, and avoid warming the feature film just because a preview plays. Reuse the existing resolver, not a separate trailer provider. Signed URLs need renewal; durable bytes and durable metadata are different promises.

Acceptance after implementation: real Google TV focus trace (current app), other Plex families show correct landscape art and navigation without false playback; Moonfin requests and plays the extra on focus with Trailer Preview enabled; Jellyfin/Emby detail themes play only when opted in; Vela lazily requests a grant, respects cancellation and displays art on failure. Verify fast focus changes do not fan out resolver calls, inactive feature costs zero background work, and preview playback never changes watch state or warms the full movie.

## Approved newest-client follow-up

Plex Android 2026.22.0 (971050549, TV-capable unified release) was also downloaded from APKMirror and disassembled as Hermes v96. APKM SHA256: 5c8db86f517a84162062cac7375fdda4b02195aa8e1ae23173f3fdce8be8cbd7; bundle SHA256: 577560dd9367b1c1f3e73be237a8469b1b8cdeced8b736a99ec2f9433084edd8. The same path remains: #17524 pmsHubToTileScroller does not read a hub-level preview attribute; #17473 pmsMetadataItemToMetadata / #17430 pmsMetadataItemToActions expose ordinary PMS actions; #46855 FocusedTileBackground uses resolved preview actions. HubAutoPreviewType appears twice (definition/export), with no consumer found; no previewable field. Public Web 4.163.0 HubModel also excludes autoPreview from syncedAttrs. Therefore implementation keeps native movie/show types and sends landscape artwork as fallback, without claiming that Plex server hubs can force focus autoplay or force the client's card aspect ratio.

Phase 2 adds the owner flag to CollectionSpec and three-state RowPolicy overrides (Default inherits the collection, On/Off override it). It propagates through candidate ranking, neutral layout, row snapshots and protocol paint without extras discovery during browsing. Jellyfin/Emby use scoped backdrop image grants as row card artwork, Silo uses backdrop_url, Stremio row catalogs use posterShape=landscape with scoped backdrop art, and Plex supplies backdrop thumbs. DLNA/WebDAV do not have interactive focus rows.
