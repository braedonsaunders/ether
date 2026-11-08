# LG MediaVersion (#339): revised fix and measurements

> Superseded for Plex for LG by #375: the stable play-flow list below did not
> cover the season lists, rows and cold details where the app first caches its
> playback settings with one Media. Plex for LG now receives one stable Auto
> Media everywhere, as Plex for VIDAA (same 5.94 app) has since #89.

Issue: https://github.com/braedonsaunders/ether/issues/339 (paustin316-cell).
Base: release/0.3.5 at b4d8d5827. This revision supersedes the broad behavior in
35b8c5ada. The reporter's server/app versions remain unknown; no physical LG
retest or reporter support ZIP was available.

## Cause and reference limits

The prior flow could change version membership between ordinary detail,
pre-play, queue and admission. A singleton was treated as "no issued list" for
mediaIndex, so a stored position could select a newly discovered release.
The #267 stream/identity work on this base began freezing only at admission.

This release's reference paths are `testdata/conform/plex`, `internal/conform`,
`internal/conform/plex`, and `internal/plex/conform_test.go` (the earlier
`testdata/pms`/`internal/pmsconform` paths have moved). Recorded PMS decisions
project only the selected Media; start returns a playlist/bytes, not a new
version catalogue. Equal Media counts across those transport responses would
be a protocol regression.

No client bundle containing MediaVersion was available to inspect. The
official hosted desktop main bundle (4.163.0) contains neither MediaVersion nor
the screenshot's error literal; the hosted /tv/ URL returns 404. The exact TV
creation/lookup branches therefore remain unverified. The screenshot and the
supplied shared-client behavior support the failure mechanism, but do not prove
Indie's Web error is identical. No proprietary code/assets were copied.

## Browse measurements

Opt-in offline harness: `ETHER_339_MEASURE=1 go test ./internal/plex -run
'^TestVersionBrowseMeasurement$' -v -count=1`.

Same Mac and harness for release base, 35b8c5ada, and revision. Production
resolver; 200 catalogue films, five shelves of 40 candidate titles; 24 release
candidates cached for 80% of titles. Cache seeding is outside timed requests.
Normal section/discovery shelf composition remains enabled. Library page has
50 films. Cold means first render with empty Plex play identities and warm
resolver search cache, not a cold indexer search. Warm p50/p95 are 15 repeated
requests. JSON payload sizes are uncompressed bytes. These are local fixture
measurements, not real-network/client timing; p95 is the maximum of 15 samples.

| Endpoint | Version | Cold ms | Warm p50 ms | Warm p95 ms | Bytes |
|---|---|---:|---:|---:|---:|
| /hubs/sections/1?count=40 | release base | 11.138 | 9.721 | 12.805 | 404280 |
| same | 35b8c5ada | 129.302 | 14.966 | 16.010 | 726648 |
| same | revised | 9.577 | 8.884 | 13.887 | 404280 |
| /hubs?count=40 | release base | 7.228 | 7.092 | 10.852 | 336708 |
| same | 35b8c5ada | 128.584 | 11.836 | 13.004 | 605348 |
| same | revised | 7.193 | 7.075 | 9.395 | 336708 |
| 50-item library page | release base | 2.347 | 1.961 | 2.492 | 78697 |
| same | 35b8c5ada | 30.221 | 3.458 | 4.188 | 145857 |
| same | revised | 1.919 | 1.957 | 2.417 | 78697 |

The first patch grew section/home payloads by 79.7%/79.8%, and library by
85.3%. Revised payload growth is **0%**. Release base and revision both make
**zero Variants calls** on all three endpoints. The first patch made 200,
200 and 50 calls respectively on cold renders. Every measured version made
**zero indexer searches and zero descriptor fetches** during all timed requests.

The production `resolver.Service.Variants` uses `peekSearchCache`, then cached
ranking/picking; it never calls searchCached, Resolve or RefillSearchCache.
`TestVariantsCacheOnlyNoIndexerCalls` and
`TestVariantsAlternativesFromStrictPoolCacheOnly` verify that behavior.
The revised browse path does not enter Variants at all. Existing explicit
single-detail discovery/warm-up remains unchanged; browsing starts no searches
or per-title variant watchers.

## Revised flow and cluster behavior

Ordinary details may gain the version picker immediately when discovery
finishes. Auto-only/unresolved ordinary details are not retained as resolved
lists and create no shared rows. Hubs/library listings keep their original
single Auto Media ID, equal to the first Media ID in a detail/flow list. They
are browse projections, not full version lists; demanding equal counts there
caused the reviewed performance and picker regressions.

A single-title checkFiles/asyncCheckFiles pre-play request or the selected
play-queue item starts the stable flow. It adopts a previously offered real
version list, or the current list if none was offered. That flow retains its
ordered list (including an unresolved Auto if pre-play actually starts cold)
for 90 seconds pending admission. This is distinct from pinning Auto during
browsing for 30 minutes. Queued, unselected items do not enter the new shared
store. A stale mediaIndex falls back to Auto; a stable explicit Part ID still
names its release. A positional list lost after restart is not recomputed
against a different current search answer.

A live flow ends at terminal timeline/encoder stop or failed preparation.
An abandoned pre-play expires after 90 seconds; live cluster rows use the
existing 30-minute identity retention, refreshed by playing/paused timelines
only when less than 20 minutes remain. No title-wide cached search is frozen.

Membership is **not** assumed deterministic across nodes. Only release-derived
Media/Part IDs are deterministic; node-local cached candidate lists can differ.
Clustered single-detail/selected-queue boundaries use `plex_version_flows`:
a primary-keyed viewer/device/title row containing bounded variant render facts
(no full releases, URLs or credentials). Rows are capped at MaxVariants and
64 KiB. A per-server worker drains up to 64 operations into one transaction;
its queue is bounded at 128, queries use the primary key, and expiry cleanup is
indexed and bounded to 128 rows per sweep. There are no shared-store calls on
hub/library renders or for unselected queue items.

Writes run on the batching worker, outside the HTTP goroutine. Publication is
awaited before returning the chosen list: fire-and-forget writes cannot protect
an immediate next-node request. Atomic SQL preserves the first frozen list even
when another node offers different candidates. A shared-store failure returns
503 for metadata/queue or a playback error before preparation, rather than
silently emitting a node-local replacement list. Local stream aliases remain
local; the new store's scope is version membership and ordering.

The older shelf_cut/home_shared code on this exact release base is itself
process-local; it cannot be reused to claim HA guarantees. This uses a separate
compact shared table rather than copying those local caches.

## Every Plex-visible change versus release base

- Hubs/library listings: **no count or payload change**; still one Auto Media,
  same deterministic ID as the first item version. No new resolver calls.
- Ordinary details: original immediate picker discovery restored; no 30-minute
  singleton freeze. New shared offered lists allow pre-play on another node to
  identify the versions already shown.
- Explicit pre-play and selected queue item: retain the same version IDs/order
  through discovery, node changes, admission and start. Pending cold singleton
  retention is limited to the actual flow, not browsing.
- Non-Web live metadata: **no new projection change**. The release base already
  retains multiple detail versions for all clients except VIDAA (see
  TestNonWebClientsSeeThePlayingVersionDuringPlayback). The earlier comment
  claiming other apps always see one version was stale. /status/sessions keeps
  its admitted single-Media projection. VIDAA remains single-version.
- Decisions: unchanged PMS-selected single-Media projection, with an identity
  drawn from the issued list. Start: unchanged playlist/byte transport.
- Stale/unknown positional choice: Auto instead of recomputing against today's
  candidates. Explicit Part ID choices remain supported; the one-Part model
  still ignores stale partIndex. This prevents a choice from naming an unissued
  version. It does not invent a client-facing MediaVersion setting.
- Cluster snapshot outage: fail closed at the item/play boundary rather than
  expose inconsistent versions. Stopping releases the flow for immediate picker
  discovery on the next detail page.

Recorded conformance covers field types, shapes and the selected projection;
this corpus has no measured multi-version LG flow. The cross-response identity
invariant is inferred from stable PMS file identities and the supplied client
behavior. We do not claim the recorded single-file corpus proves LG hardware
compatibility or every multi-version selection condition.

## Gates and lasting checks

`TestVersionIdentityAcrossPlayFlow`: LG/Samsung/Android TV/Web, zero/one/three
initial variants; hub Auto identity, explicit pre-play → queue → decision →
start → metadata; stale position and Part IDs. `TestVersionFlowSharedAcrossNodes`
uses different node caches and shared SQL, switches nodes through that flow,
and checks terminal cleanup/picker recovery. Its fake playback is routed to
one broker owner to isolate version publication from encoder transport.
`TestVersionFlowPendingExpiry` checks that an abandoned cold pre-play stops
hiding the picker. Existing refill/picker tests retain their original immediate
upgrade expectations. The opt-in measurement is skipped in normal package runs.

Passed gates:

- `go test ./internal/plex -count=1` (38.326 s).
- `CGO_ENABLED=0 go test ./internal/plex -run 'Test(VersionIdentityAcrossPlayFlow|VersionFlowSharedAcrossNodes|VersionFlowPendingExpiry|PlayIdentity|ResumedDetail|DetailPageRefreshesWhenVersionsArrive|MediaIndex)' -count=1` (3.448 s, after the final expiry cleanup change).
- Focused decision/detail/TV conformance, VIDAA and non-Web playback checks.
- `go test ./internal/resolver -run '^TestVariants(CacheOnlyNoIndexerCalls|AlternativesFromStrictPoolCacheOnly)$' -count=1` (0.538 s).
- `go vet ./internal/plex` and `git diff --check`.

No repository-wide, race or web suites were run. Cross-node storage tests use
shared SQLite and an in-memory cluster bus; PostgreSQL was not exercised here.

## GitHub reply draft (not posted)

A fix for this version-list inconsistency is prepared for v0.3.5. Ether could
change the list between pre-play, the play queue and playback admission, leaving
the LG app with playback settings that referenced a version no longer listed.

The fix keeps the version IDs and order stable within a play flow and prevents
stale version preferences from choosing a newly discovered release. Ordinary
detail pages can still gain the version picker as discovery finishes. Automated
checks cover unresolved, single-version and multi-version titles, including
requests switching between server nodes.

Please retry on v0.3.5. If the error remains, the Ether version, LG/webOS and
Plex app versions, affected title, and a support ZIP captured while reproducing
it will help confirm the remaining cause on your TV.
