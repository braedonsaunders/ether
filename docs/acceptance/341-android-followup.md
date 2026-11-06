# #341 Android follow-up for v0.3.5

Branch: `fix/341-android-followup`, based on `release/0.3.5` at
`4492062c9a`, including the earlier Android fixes through `b85d9ae68` and
#285 admission work. Package tests only; no live playback or device claim.

## Capture evidence

The issue and comments identify v0.3.4 (`7330b3ced`), container install,
Plex for Android (Mobile) 2026.22.0, SM-F976B (`canoe`), LAN with/without VPN,
and posters missing throughout the app. Web works. The reporter also has an
Android TV, but this recording identifies the phone. The supplied files were
read as untrusted data; no capture contents or credentials are committed.

There are no image routes in the recording. This **does not prove the app
never requested images**. At `7330b3ced`, `plex.observeRequest` records every
route while recording, including images; `supportDocumentPath` excludes image
**body sampling**, not request observation. However,
`diagnostics.Recorder.subjectLocked` retains requests only for the selected
`DeviceKey`. An image loader using only the viewer token, without
`X-Plex-Client-Identifier`, disappears from the capture. No fallback to a
viewer/session existed for ordinary request events. Image failures with a
client identifier (including missing-token 401 responses) are already visible.

Reconstructed HTTP requests against the v0.3.4 package reproduce this blind
spot: four requests (photo and thumb, with and without the device header)
yield only two retained events. The same test on the fix retains all four.
An explicitly different device is excluded. This establishes a recorder gap
(case a), **not proof that these particular missing requests occurred**.
The old ZIP cannot distinguish the gap from a client making no requests.

The recorder now includes viewer-authenticated artwork requests without a
device header as `uncorrelated:true`, with no invented device attribution,
only for the viewer snapshotted when selecting the recorded device. Other
identified devices/viewers, unauthenticated unidentified traffic and unrelated
routes remain excluded. The archive explains this limitation. Existing query
redaction and body exclusions still apply; nested URLs/tokens are not exported.
A viewer may use several clients: unattributed events can belong to any of
that viewer's clients, and are explicitly described that way.

## What v0.3.4 returns

`TestIssue341V034ArtworkProjection` was run unchanged on both `7330b3ced`
and the fix. It uses the reported key, authored poster/backdrop facts, and
both light/detail rendering for Mobile, TV and Web. All return:

```
thumb=/library/metadata/1000000000718789/thumb/1790812800
art=/library/metadata/1000000000718789/art/1790812800
```

The timestamp above belongs to the test fixture, not the reporter's database.
`convert.go` builds these relative URLs from the title key and `UpdatedAt`
(or stamp `1`). Plex metadata overlays change the stamp, not the URL origin.
`decorateDiscoveryMetadata` can instead emit relative
`/library/metadata/<key>/discoveryPoster/<version>?lang=...` URLs (and
`kind=backdrop` for art). Provider/CDN access occurs server-side.
Thus the normal metadata responses do not embed an external TMDB/provider
origin, scheme, port, plex.direct hostname or account token. Clients attach
credentials to their image requests on their chosen server connection.

The ZIP stores titles/counts and decisions, not raw XML/art URLs or the
catalogue/poster settings required to recover the exact stamp or selected
provider. It cannot establish that the reporter received nonempty artwork
fields, selected the wrong connection, or omitted credentials. Case b as
proposed is not established by the capture or these code paths.

The existing v0.3.5 cross-host nested photo route and explicit image format
fixes remain covered by package tests. They fix demonstrated handler defects;
there is insufficient evidence to assert they solve this reporter's missing
posters. No further speculative URL/authentication rewrite was made. A new
capture on the fixed release can now expose viewer-only image failures.

## Request #76: cancelled lookup, not a deleted movie

`events.jsonl` #76 is a 1 ms GET for the Lightyear key, with two database
queries, **`cancelled:true`**, HTTP 404. #77 returns the same title with HTTP
200 approximately 126 ms later. Earlier requests also returned it repeatedly.
There is no evidence of eviction or loss of the played title.

Metadata lookup and visibility can fail closed on interrupted storage/policy
reads. Previously `handleMetadata` could map that interrupted lookup to 404,
including running placeholder fallbacks. An authored cancelling catalogue
reproduces 404 on v0.3.4. The fix exits after lookup/visibility if the request
context has ended, without writing a missing-title response or attempting
fallbacks. The support observation uses 499 when a cancelled request wrote no
HTTP status (an observation convention, not a wire response). The regression
test checks that observation and a subsequent successful detail open. The
ZIP cannot identify which exact cancelled storage/policy operation failed.

## Phone subtitles

`TestIssue341PhonePUTDecisionStart` uses the reported product, app version,
device and model headers. For each of embedded SRT, ASS and PGS it:

1. Opens metadata and obtains an issued subtitle ID.
2. Selects it through `PUT /library/parts/<part>?subtitleStreamID=...&allParts=1`.
3. Calls decision **without repeating the subtitle ID**, getting code 1001
   (conversion permitted), the selected track and `segments-av` delivery.
4. Starts playback, asserting the encoder receives the selected stream index
   and Matroska target; text is retained, PGS is burned in.
5. Reopens the played title successfully.

All pass. Existing TV/Mobile subtitle decision/start, switching, Off, probing,
photo authorization, format and provider tests are included in the selected
Plex gate. These prove the server contract and encoder handoff, not physical
phone/TV subtitle rendering. The capture's stream IDs and request profiles
are redacted, so this is a reconstructed flow, not a byte-for-byte replay.

## Validation

- v0.3.4 package: artwork projection passes; recorder and interrupted lookup
  regression tests fail with the expected two-events and HTTP-404 behavior.
- Fix: full `internal/diagnostics`, `internal/artwork`, and
  `internal/artwork/service` packages, plus selected `internal/plex` Android,
  subtitle, artwork, photo, support and new #341 tests.

## GitHub reply draft (not posted)

Thanks for the version, device details and recording. We found a gap in the
recorder: image requests that include your viewer token but omit the app's
device identifier were left out. We've fixed that for v0.3.5, so the lack of
poster requests in this ZIP doesn't mean your app never tried to load them.

We checked v0.3.4's metadata generation for the affected title: normal poster
and background links point back to Ether using relative paths; TMDB/poster
provider URLs and credentials stay on the server. The recording doesn't save
the full image links, so we can't yet confirm the exact poster failure. The
v0.3.5 image fixes cover photo requests using another connection hostname and
requested image formats, but we can't promise those explain your case yet.

The one 404 after playback was a cancelled request. The same movie opened
successfully again about 0.13 seconds later. We've corrected that interrupted
lookup so it doesn't report the title as missing.

The v0.3.5 phone subtitle fix passes tests for selecting a track, receiving a
conversion decision, then starting playback: SRT/ASS stay in the stream and
PGS is burned in. These are server tests; we haven't verified rendering on
your physical devices. After updating to a v0.3.5 build containing these
changes, if posters or subtitles still fail, a fresh recording while opening
a library and selecting a subtitle will give us better evidence.
