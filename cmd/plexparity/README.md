# Plex parity harness

Run `cmd/plexparity/remote.sh BASELINE CANDIDATE` from the worktree. The
candidate may be a Git revision or a worktree path. The remote runner builds
both trees with test-only seams and runs the real components against copied,
offline fixture data. Exit 0 means no unapproved differences and complete
coverage; 1 means a difference or an invalid fixture; 2 means no unapproved
differences but incomplete route coverage. CI fails on either 1 or 2.

`c2057bfe` restricted Home backfill: not exercised by fixture; covered by
experience unit tests. Do not allowlist a difference without an exact, verified
response pair and its commit attribution.

Live DASH manifests use a fixed test-only clock in both staged binaries. This
checks serialization under identical times; it does not test wall-clock
progression. Production builds retain the normal live clock and its tests.

Full runs exercise both the ordinary fixture and a second fixture that uses
deterministic synthetic viewer activity through the real APIs to train an
active learned reranker under the unchanged production holdout gate. Its
self-comparison passed; an integrated passing cross-tree replay remains
outstanding after exact difference review. This is synthetic evidence, not
real-user or device evidence. Focused diagnostic modes run only their named
fixture and return incomplete coverage.

Intentional difference `42ef28ac`: viewers who saved scoped language or
quality overrides have those preferences applied to Plex playback selection.
The no-override path is covered by `TestNoScopedOverridesPreserveSelection`
and `TestScopedQualityOnlyTightensPlexRequest`. This is recorded for
attribution only: no response is allowlisted without a verified exact pair.

Intentional scope fix `19bdfcd1`: `/hubs/home/recentlyAdded` no longer
widens unsupported types, comma-separated types, or explicit section filters
into mixed results. Read-only PMS probes returned empty for the affected
requests. Unscoped mixed Home responses remain supported. Exact before/after
pairs must be recorded before any difference is approved.

Owner-approved resume improvement: a nonzero position is retained at 60
seconds or 5% of duration, whichever comes first, until the existing watched
threshold. This shared-state change affects `/:/timeline`,
`/library/metadata/{{movieKey}}`, and `/hubs/continueWatching` for those
positions. The earlier PMS threshold was not established: the Guest probe
stopped at authorization. This note does not allowlist any response; exact
reviewed before/after pairs are still required.

v0.2.58 (`22c160aba`) against `14a6b0890`, both halves, 3081 unapproved
pairs each. Nothing in that set was an unlabeled byte change. Counts are
per half.

- addedAt only: 1631 ordinary, 1256 active-ranker. The approved release-date
  rule. Every other pair below also carries addedAt where the body has dates.
- Subtitle stream identity: 1250 ordinary, 1625 active-ranker. Embedded
  subtitle ids follow language, codec, and source (`14a6b0890`).
- Subtitle sidecar location: 50 and 50 (`tracks:probe`, `tracks:selected`).
  Direct play serves the embedded text track as a sidecar (`3654964cc`),
  so `location` is `sidecar` and the stream key is `/library/streams/...`.
  Segmented Matroska text is different (`f8f762a7b`): the decision is
  `copy` with `location="segments-av"` and no sidecar key, because Plex
  tvOS reads that track from the segments. MPEG-TS stays a sidecar.
  The `14a6b0890`..`f8f762a7b` corpus (one model, exit 0) did not select
  a Matroska text track, so there is no response pair.
  `TestMatroskaTextSubtitleIsInBand` and
  `TestIntegrationMatroskaSubtitleSeekMatchesVideo` cover the shape and
  the seek clock.
- `/accounts` subtitleMode 0 to 2: 10 and 10. The parity seed stores
  subtitle mode `always` (`feaa49f10`). Untouched accounts stay manual;
  this fixture does not.
- Live TV, IPTV playlist, and DASH bytes: 60 and 60. The media URL carries
  `X-Ether-Stream-Token` instead of `X-Plex-Token`. Segment timing is unchanged.
- Child and teen search and postplay: 80 and 80. Those viewers are capped at
  PG and PG-13. The candidate fills the requested limit with titles that cap
  allows (`a1044fcba`, `697db5ea2`, `4d01e1de6`). No shared title changed its
  contentRating string.
- Content-rating label text on the same title: 0.
- Version labels (`ed2bc8916`, #188): 0 pairs in this corpus.
- Timeout playback (`e7a3bf2dd`, #198): not in this candidate, 0 pairs.

These notes do not allowlist a response. Exact reviewed pairs are still required.

v0.2.58 (`22c160aba`) against `7b8b053dd` and subsequent v0.3 commits, both
worlds. Pairs already approved on an earlier baseline stayed approved: 417
release-date pairs (`77af603e`), 129 watched-state pairs (`23d9fffb2`), and
100 DVR `refreshedAt` pairs (`2ef11dc0c`). The remaining differences in each
world are the documented set, now exact pairs in `approved_v0258.json` and
`approved_v0258_token.json`:

- addedAt only: 1631 ordinary, 1256 active-ranker, plus 15 subtitle-metadata
  responses in each world on `5da09b358` (`77af603e`).
- Subtitle stream identity, which also carries addedAt: 1250 and 1625
  (`14a6b0890+77af603e`).
- Sidecar location on `tracks:probe` and `tracks:selected`: 50 and 50
  (`3654964cc+77af603e`).
- `/accounts` subtitleMode: 10 and 10 (`feaa49f10`).
- Live TV, IPTV, and DASH grant name: 60 and 60 (`9b8fa7546`). The grant
  value is a fresh secret, so the candidate hash uses the harness placeholder.
- Child and teen search and postplay, which also carry addedAt: 80 and 80
  (`c82a7830+77af603e`).
- PMS 17-hub search order and indexed people search hubs on `/hubs/search`
  and `/hubs/search/voice`, composed with the v0.2.58 subtitle/addedAt and
  restricted-viewer pairs: 100 ordinary and 100 active-ranker (`b5e6074c3`).
- `/devices` owner device list (`createdAt` stamped from the server clock by
  `91f5756f6`) and non-owner `403 Forbidden`: 50 and 50 (`32e51bfaf`).
- HLS playback documents (`start.m3u8`, `media-playlist`, `reconnect-playlist`,
  `seek-master`, `seek-playlist`, and `subtitle:start`) carrying
  `&X-Ether-Read-Token=<READ-TOKEN>` alongside `X-Ether-Stream-Token`: 275 and
  275 (`32e51bfaf`).
- DASH manifest (`playback:dash:manifest`) carrying
  `&amp;X-Ether-Read-Token=<READ-TOKEN>` (`32e51bfaf`) and `minBufferTime`
  `PT10S` to `PT5S` (`76e1dbce3`, composed as `76e1dbce3+32e51bfaf` against
  v0.2.58): 25 and 25.
- Content-rating label text, Play Version labels, timeout playback (#198),
  pre-1970 date strings, section filter operators (`bd55103ff`), iOS Home
  hero art gating (`b1ad78851`, #210), and `guid=` library lookups
  (`ad2cd3c50`, Rex T1-4): 0.

This note does not allowlist a route.

Intentional track selection: a viewer with no audio list no longer follows the
file's default flag ahead of a language they can use. The order is the
viewer's trackprefs audio languages (their row, otherwise user 0, which the
setup wizard writes), then the catalog language (`en-US` matches English),
then the title's original language, then the default flag. PreferOriginal
still ranks the original language first and is separate from that chain.
The same order picks subtitles. With subtitle mode left off, a forced
subtitle in the viewer's language is selected only when the chosen audio
is not one of those languages. This note does not allowlist any response.

Intentional playback change `1d4dee05c` (PR #198, merge `e7a3bf2dd`): when
every candidate's health check times out, playback starts the first pooled
candidate in the resolver's own order (preferred release, then primary, then
downgrades already sorted by quality) instead of returning an error. Failover
stays armed: other candidates without a confirmed failure remain alternates.
A provider that cannot answer, and any confirmed failure, still fails. The
unverified winner is not cached as verified or available. This note does not
allowlist any response.

The complete v0.2.45 corpus covers every registered Plex GET route, including
real offline IPTV resource, relay and DASH media. A zero-unapproved result with
exit 2 means a future inventory route is uncovered and is not a parity verdict.

Jump bar: `/library/sections/{id}/firstCharacter` lists one `#` for titles
whose sort title is before "a", then A–Z. Titles that sort after "z" stay in
the library and are not a second `#`. Plex tvOS adds every directory titled
`#` into the offset of later letters, so the trailing bucket shifted
jump-to-letter by that count. `/firstCharacter/~` still filters those titles.
This note does not allowlist any response.

Play Version: an unparsed release no longer advertises 1080p H.264 or E-AC-3
5.1. Resolution class is derived from both width and height (so scope-aspect
`1920x800` is 1080p, not `800p`). After playback or warm-up, when the probed
file's resolution class or audio codec disagrees with Auto's estimate, Auto's
media title preserves the parsed release metadata (source, group, matching
audio details) and replaces only what the probe proves wrong (falling back to
`Auto · SD · AAC` when unparsed); when Auto plays a same-tier alternative,
Auto's label and Media bitrate/attributes match the chosen release row. A
known 1080p candidate is still tried before a known SD file; the downgrade
trace is unchanged. This note does not allowlist any response.

Scrobble and watch refresh (`23d9fffb2`, #6): the `80208da4d` versus
`0901e52df` run differed only here. Both worlds produced the same 129
response pairs, recorded in `approved_watch.json`: 25 scrobble and 25
unscrobble sequences, 25 eventsource and 50 websocket timeline entries, and
four Infuse resume resets (eight labeled cases, one pair each). `PUT` and
`GET` `/:/scrobble` and `/:/unscrobble` return an empty MediaContainer
(`size="0"`) instead of an empty 200, with `Content-Type` and
`Cache-Control: no-cache`. That is the PMS container for this call: the same
encoder already used for the other empty library replies. An empty body has
no content type, and tvOS treats that PUT as a failure. Timeline eventsource
and websocket entries set `metadataState` to `created`. A movie's
`rootItemID` is its own rating key, which is how PMS refreshes the open
detail; an episode still names its season and show. The per-connection
websocket handshake token is not part of the pair. This note does not
allowlist a route.

DVR lineup refresh (`2ef11dc0c`): `GET /livetv/dvrs` and `GET /livetv/dvrs/1`
include `refreshedAt`. PMS puts that attribute on the Dvr element; clients
compare it to drop a cached lineup, and a guide reload advances it. The
`80208da4d` versus `0b675e5d6` capture recorded 100 pairs in each world
(five viewers, five clients, XML and JSON, both paths). Both worlds hashed
the same pairs, now in `approved_livetv.json`. Every other byte of those
bodies matches. This note does not allowlist a route.

Section filters (`bd55103ff`): `GET /library/sections/{id}/all` and
`GET /library/all` honour operators those routes used to ignore or invert.
Negations are `genre!=`, `country!=`, `contentRating!=`, `collection!=`,
`director!=`, `writer!=`, and `actor!=`. A person filter stays on the role
it names. Title and studio accept `=`, `!=`, `==`, `!==`, `<=`, and `>=`.
Year, decade, duration, audienceRating, and originallyAvailableAt accept
numeric and date operators, including relative dates. `unwatched!=` and
`inProgress` select watched progress instead of being dropped or flipped.
A request that does not send one of those operators is unchanged. The
v0.2.58 corpus does not send them, so that fixture has 0 pairs and there
is no signature to store. This note does not allowlist any response.

Search hubs and indexed people search (`b5e6074c3`, Rex T1-5):
`/hubs/search?query=fixture&limit=10&includeCollections=1` and
`/hubs/search/voice?query=fixture` return PMS's 17-hub set in recorded order
(`show`, `artist`, `album`, `movie`, `episode`, `track`, `collection`,
`photoalbum`, `autotag`, `photo`, `tag`, `actor`, `director`, `genre`,
`playlist`, `shared`, `place`) with `hubIdentifier`, `type`, `title`,
`context=""`, `size`, `more`, and `style="shelf"`. Indexed `actor` and
`director` people hubs return person `Directory` entries with `reason`,
`reasonID`, `reasonTitle`, and viewer-visible `count` (60 for unrestricted
viewers, 40 for restricted viewers), and append credited titles to the
`movie`, `show`, and `episode` hubs (`more="1"` on `movie`). The capture
recorded 100 pairs in each world (200 unique pairs across both worlds in
`approved_search_hubs.json`, and 200 composed `v0.2.58` pairs in
`approved_v0258.json`), with 0 differences elsewhere. This note does not
allowlist a route.

iOS Home hero art gating (`b1ad78851`, #210): `/hubs` and `/hubs/promoted` on
iOS keep only art-ready items in the hero slot, promoting the next art-ready
shelf or omitting the hero when no item has backdrop art. Every seeded parity
fixture title has `art` and `thumb`, so `b1ad78851^` versus `b1ad78851`
produced 0 pairs in both worlds and there is no signature to store. This note
does not allowlist any response.

Movie intros (`766c16dcd`): a movie intro that starts at the head of the file
(within one second) and ends within 90 seconds is not offered as Skip Intro.
TheIntroDB's row for The Matrix is one submission from the start to 40
seconds; that range is a studio card, not a title sequence. An intro that
starts later, and every episode intro, stays. This note does not allowlist
any response.

Rex batch (`32e51bfaf`, merge of `64ab75605`, plus `eda0de766` and
`91f5756f6`):
1. `/devices` (50 pairs per world, 100 total across both worlds in
   `approved_v0258.json`): the owner receives the 7-device list stamped with
   the server clock (`91f5756f6`), and non-owner viewers (`adult`, `teen`,
   `child`, `friend`) receive `403 Forbidden` instead of an empty `200`
   container.
2. HLS and DASH playback documents (`start.m3u8` for remux and transcode,
   `media-playlist`, `reconnect-playlist`, `seek-master`, `seek-playlist`, and
   `subtitle:start`, and `playback:dash:manifest`, 300 pairs per world in `approved_v0258_token.json`):
   every media URI gains `&X-Ether-Read-Token=<minted per session>` alongside
   `X-Ether-Stream-Token` (slice H, P5a session read capability, device-bound
   and revoked with the session; masked to `<READ-TOKEN>` by `eda0de766`).
Also intended in `32e51bfaf` and not probed by the corpus:
- `/statistics/bandwidth|media|resources` and `/devices/{id}` are new
  owner-only routes (recorded in `unprobedRoutes` because pre-`32e51bfaf`
  baselines answer `404`);
- `/video/:/transcode/universal/start` with `protocol=http` is new (Plex for
  Mac live FFmpeg stream, recorded in `unprobedRoutes`);
- `POST /library/metadata/{id}/subtitles` upload is owner-only;
- section browse honours plex.tv share predicates for share-filtered viewers
  (none in the parity world);
- the Plex-metadata toggle is off by default (byte-identical off).
This note does not allowlist a route.

DASH startup buffer (`76e1dbce3` / `5773afc59`, Rex T1-2):
`playback:dash:manifest` changes MPD `minBufferTime` from `PT10S` to `PT5S`
(one segment, so Plex Web/Shaka starts after one segment instead of two; live
MPD `timeShiftBufferDepth` stays `PT120S`). Recorded as 25 pairs per world (50
across both worlds) in `approved_v0258_token.json` (`76e1dbce3` against
`32e51bfaf`, and `76e1dbce3+32e51bfaf` against `v0.2.58`). This note does not
allowlist a route.

Library `guid=` lookup (`ad2cd3c50`, Rex T1-4):
`/library/sections/{id}/all` and `/library/all` with
`?guid=<plex://|tmdb://|imdb://|tvdb://|legacy agent guid>` (including
`type=3` season and `type=4` episode forms) return only the matching item, or
an empty container, instead of the whole section. The parity corpus does not
send `guid=`, so there are 0 pairs and no signature to store. This note does
not allowlist any response.
