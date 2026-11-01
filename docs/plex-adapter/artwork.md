# Artwork extraction checkpoint

Public-only base: `b4ef791d53a68741c0f1513071328594b62eb9ed`.
No private RC/security work is included. No public push or runtime deployment.

## Initial checkpoint boundary (historical)

`internal/artwork/service` owns the variants and metadata queues, their database
migration registration, and raster transformation/four-corner palette rendering.
It imports neither `internal/plex` nor `internal/plex/model`. The palette contains
only colours; Plex converts its wire palette at the boundary. Existing queue
limits, retry intervals, viewer revalidation, leases, hash format, cache keys,
image formats and resize algorithms are retained. Runtime cancellation now joins
queue workers, including failed runtime initialization.

`subscribeSessionState` publishes shared state events without a Plex callback.
The mounted Plex adapter owns committed watch refresh subscriptions and releases
them on Close. Cross-surface explicit scrobbles retain origin suppression; the
completed/paused/stopped behavior is retained in the adapter.

## Initial checkpoint remaining extraction (historical)

Runtime STILL constructs `plex.FrontendArtwork{Server: a.plexServer}`. Artwork
selection/read still delegates to Plex collection, policy, discovery, UltraBlur
cache and HTTP image routines. This checkpoint does not establish Plex-disabled
construction without an unmounted Server, or full sharedcore consolidation.

Next dependencies to extract into neutral implementations, preserving exact
opaque reference inputs and byte output:

- cache-only title/virtual-season selection and inherited viewer policy;
- collection authorization/stamps/generated artwork and playlist selection;
- localized overlay selection/versioning and rendering (currently the discovery
  decorator's Plex URLs form part of opaque reference hashes);
- palette cache/computation and source fetching, including distinct TMDB width
  bucket behavior (Plex selects original above 1280; artwork.Images caps at 1280);
- channel fallback and scoped person image retrieval;
- neutral app composition and Plex-disabled construction tests.

Plex routes, serialization, capability mapping and HTTP header behavior remain
in the adapter. Do not replace those dependencies with callbacks bound to Server.
Coordinate runtime/lifetime wiring with Experience and playback extraction owners.

## Verification handoff

No local build/test/dev-server/browser job was run. No remote job is allocated.
Formatting and `git diff --check` were run. Tests below are **pending**, not passes.
Root must allocate an isolated remote/data runner after private RC gates.

```sh
go test ./internal/artwork/service ./internal/plex ./internal/app -run 'Artwork|UltraBlur|WatchRefresh|FrontendWatchSubscription|SessionStateEvents|VariantsWorkers|MetadataWorkers|Neutral' -count=1
go test -race ./internal/artwork/service ./internal/plex ./internal/app -run 'WorkersJoin|FrontendWatchSubscription|SessionStateEvents' -count=1
```

Follow with the existing ordinary and active Plex parity gates only after root
review; this checkpoint has no parity claim. Preserve all previous failure logs.
Full acceptance still requires Plex-disabled/local-owner artwork and state tests,
shared revocation/cold-cache tests, and dependency checks for the entire neutral
selection implementation once that implementation exists.

## Frozen 4fc460e1 gate evidence

The single authorized offline gate ran with helper SHA256
`e4a5f08cbf6eefc506177e54917948a728e00d73b422579279fd04b4bf435cdf`,
outer timeout 600s, package timeout 5m, network none. It **failed with exit 1**:

- selected `internal/artwork/service` tests passed (0.037s);
- selected `internal/app` tests passed (6.839s);
- `internal/plex` failed to compile its lifecycle test: `surface` was declared
  `string` but `state.Origin.Surface` requires `core.Protocol`.

Preserved remote evidence on the allocated runner:
`/data/prism-test/artwork-extract-4fc460e1.3oMX8Lqu/test.log` and `exit-code`.
Local log and exit are in BB thread storage:
`thr_jrz97zjn9h/artwork-extract-4fc460e1/runner.log` and `exit-code`.
Runner cleanup and an independent SSH check both confirmed the exact owned
container absent. No retry occurred. The subsequent type-only test correction
is untested and requires a new allocation; it does not supersede the failed
snapshot evidence or establish any Plex parity result.

## Full selector successor (pending executable validation)

The successor replaces runtime `plex.FrontendArtwork` construction with
`artwork/service.Source`, composed from actual catalog/collections/playlists,
visibility, discovery, remote image cache and palette services. It does not
construct an unmounted Plex server. `plex.Frontend.NewServer` constructs the
adapter only on mount; runtime owns its cleanup. Shared state events and
capability reporting no longer need the server. Spike library construction is
independent and retains its canonical Movies-only section configuration.

Actual algorithms moved: stored-title/virtual-season enumeration, parent fallback,
collection/playlist selection, opaque reference generation, variants/people/live
channel selection, direct image reads and transforms, TMDB width buckets and
remote-cache behavior, palette computation/cache/persistence, discovery cached
localization/selection and authorized overlay rendering. Plex converts palette
wire types, constructs protocol URLs and preserves conditional HTTP handling.
Historical discovery URL-shaped hash inputs are retained only as artwork-v1
identity material, not as shared routes. Plex ordinary image routes use the
shared path selection with their existing refreshing catalogue lookup.

The single download remuxer is now composed before frontends. App capability
reporting and Plex use the same extracted entitlement rule, avoiding a disabled
Plex dependency. Experience/playback owners cleared this dependency adjustment.

New tests cover local-owner/Plex-disabled app construction, image reads and state
events with no Server, enable-on-demand, neutral cold selection/revocation,
stored parent lookup, and discovery opaque identity/byte equivalence to Plex.
Existing app authorization tests now use the actual neutral source. No claim of
full-project multi-app consolidation or native-client certification is made.

Suggested successor gate (requires root allocation):

```sh
go test -p 1 -timeout 5m -count=1 -v ./internal/artwork/service ./internal/discovery ./internal/experience ./internal/plex ./internal/app -run 'Artwork|UltraBlur|WatchRefresh|FrontendWatchSubscription|SessionStateEvents|VariantsWorkers|MetadataWorkers|Neutral|PlexRegistryToggle|ViewerCapabilities|Download'
```

Remaining acceptance: execute successor gate, ordinary/active Plex parity,
race/lifecycle checks, and root integration with the other shared Experience,
playback and handle slices. No local build/test, production/reference/device
action or public push has been performed.

## Corrected cfdc80e1 gate evidence

The separately allocated corrected checkpoint gate passed (exit 0): selected
artwork/service 0.036s, Plex 0.427s, app 9.429s. Remote evidence remains at
`/data/prism-test/artwork-extract-cfdc80e1.3MG4KpS5/test.log` and `exit-code`.
Local evidence remains at BB thread storage
`thr_jrz97zjn9h/artwork-extract-cfdc80e1/runner.log` and `exit-code`.
Runner cleanup and independent exact-container verification both reported absent.
This result validates the initial queues/watch checkpoint only, not the full
selector successor. The earlier failed 4fc460e1 evidence remains unchanged.

## b35f9298 focused gate and lifecycle follow-up

The allocated frozen full-selector gate **passed**, exit 0:
artwork/service 0.143s; discovery compiled (no test files); experience 0.013s;
Plex 0.668s; app 48.603s. This includes disabled-startup/local-owner artwork and
state without a Server, existing Plex image/palette/download checks, discovery
identity/byte equivalence, registry toggle and viewer capability checks.

Evidence is retained at
`/data/prism-test/artwork-selector-b35f9298.Arc44uBK/{test.log,exit-code}`;
local BB thread storage contains
`thr_jrz97zjn9h/artwork-selector-b35f9298/{manifest,runner.log,exit-code}`.
The reviewed helper hash matched; the runner and a separate SSH check both
confirmed the exact owned container absent. No retry occurred.

### Supported lifetime contract

- Disabled startup constructs no Plex Server or Plex GUID worker. Artwork,
  discovery and watch state are independently composed domain services.
- Once enabled, disabling withdraws registry routing and discovery and removes
  the mounted watch-refresh subscription. It **does not destroy the cached Plex
  Server**. Requests that already selected that instance, notification streams,
  protocol session registries and server-owned background workers remain alive.
  A route lookup after completed unmount cannot select Plex. Existing playback
  is not force-stopped; existing idle expiry/stop policies still apply.
- Remount retains the Server and protocol session state. Each mount captures its
  own immutable `Host.Sessions`; it never mutates the retained Server's broker
  scope. The new nil-checked seam does not yet route Plex through the broker.
- Runtime close owns final server/Plex GUID shutdown, shared queue joins,
  neutral-state unsubscribe and broker close. App close owns the shared event
  bus and remaining domain services. Unmount is not a full protocol teardown.

The post-b35 audit adds a synchronized accessor for lazy server publication to
playback callbacks, checking runtime cancellation under the construction lock,
one partial-initialization-safe runtime cleanup path, and a watch-subscription
barrier so a copied/in-flight callback cannot run after unsubscribe returns.
Callbacks into Plex execute after releasing the construction mutex.

First mount uses current configuration for identity/settings while retaining the
spike library selected at domain construction. Remount reuses the same server;
its existing Config getter continues to provide dynamic settings. Neutral
section IDs, ordering and movie/show/season/episode mapping are tested against
Plex defaults and spike's Movies-only library.

New successor tests cover a held old-instance request plus playing session
tracker through remount, distinct immutable mount broker scopes, nil broker
failure, cancelled factory calls, cancelled construction waiting for the lock,
partial cleanup exactly once, current config at late first mount, and blocked
watch callbacks during unsubscribe. They are **not covered by the b35 gate**.

Suggested successor gate, only after allocation:

```sh
go test -p 1 -timeout 5m -count=1 -v ./internal/artwork/service ./internal/discovery ./internal/experience ./internal/plex ./internal/app -run 'Artwork|UltraBlur|WatchRefresh|FrontendWatchSubscription|FrontendRetains|FrontendCancelled|FrontendPartial|PlexFactory|SessionStateEvents|VariantsWorkers|MetadataWorkers|Neutral|PlexRegistryToggle|ViewerCapabilities|Download'
```

Remaining broker migration debt: recovery/transcode/owner-stop fallbacks can
still call the retained Plex adapter. Their pointer reads are synchronized here;
behavioral migration belongs to the playback owner. Full enabled-to-disabled
server destruction, native-client acceptance, ordinary/active parity and race
acceptance are not claimed by this artwork extraction.

## 69df4b1a normal gate failure

The allocated normal gate for immutable
`69df4b1aad3b9d6b4cb5008ab48c776195a5063e` exited **1**. The Plex test
package did not compile: the `mountSessions` fake embedded `frontend.Sessions`,
whose field name shadowed the interface's `Sessions` method. No Plex tests ran.
The successor changes only that fake's embedded type name and adds a compile-time
interface assertion; production code is unchanged. This correction is untested
until a separately allocated successor gate.

Independent source enumeration expected 67 top-level matches: artwork/service
13, experience 6, Plex 38, app 10. The raw log contains exactly 29 runs/passes
(13 service, 6 experience, 10 app), zero skips and zero test-level failures;
the package compile failure still makes the gate fail. Discovery has no test
files. Package times: service 0.156s, experience 0.020s, app 39.341s.

Evidence remains at
`/data/prism-test/artwork-lifetime-69df4b1a.U6tRx7tn/{test.log,exit-code}`
and local BB thread storage
`thr_jrz97zjn9h/artwork-lifetime-69df4b1a/{manifest.json,runner.log,exit-code,counts.json,cleanup.log}`.
The generic helper SHA256 was verified as
`e4a5f08cbf6eefc506177e54917948a728e00d73b422579279fd04b4bf435cdf`.
Runner cleanup and an independent SSH check both confirmed the exact owned
container absent. The conditional race gate was **not dispatched**, because
normal-pass was required. No retry occurred; earlier evidence is preserved.
