# Artwork extraction checkpoint

Public-only base: `b4ef791d53a68741c0f1513071328594b62eb9ed` (`origin/prism`).
No private RC/security work is included. No public push or runtime deployment.

## Implemented boundary

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

## Remaining required extraction (not complete)

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
