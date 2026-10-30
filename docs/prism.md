# Prism architecture

**Ether is the product. Prism is the shared protocol-neutral server architecture
that Ether runs on.** Plex, Jellyfin, Emby and Silo are frontend adapters within
Prism. The repository, executable, services and installation paths retain the
Ether name; Prism is not a separate installation.

The [README diagram](../README.md#under-the-hood) shows the ownership model.
Clients connect to their frontend, which translates requests and responses
between the client's protocol and shared contracts. Plex clients speak the PMS
protocol to the Plex frontend. Jellyfin, Emby and Silo do not need that frontend,
Plex authentication or Plex wire objects to define their server behavior.
An optional personal Plex Media Server for local files remains a separate
external server, not a Prism dependency.

## Shared service ownership

Frontends own protocol routes, serialization, client capability translation and
protocol-specific discovery. The shared server owns the catalogue, identity and
viewer policy, Experience, session broker, resolver and playback. Experience
covers browsing, search, Home rows, personalization and watch-state behavior;
the session broker coordinates playback plans, sessions and progress. Frontends
adapt those services to their clients rather than making Plex representations
the common data model.

Source entry points for this boundary:

| Responsibility | Current source |
| --- | --- |
| Neutral domain types and frontend contracts | [`internal/core`](../internal/core), [`internal/frontend`](../internal/frontend) |
| Catalogue and Experience | [`internal/catalog`](../internal/catalog), [`internal/experience`](../internal/experience) |
| Identity providers, grants and local accounts | [`internal/identity`](../internal/identity) |
| Shared session broker | [`internal/sessions/broker.go`](../internal/sessions/broker.go) |
| Release resolution and playback | [`internal/resolver`](../internal/resolver), [`internal/playback`](../internal/playback) |
| Plex adapter | [`internal/plex/frontend.go`](../internal/plex/frontend.go) |
| Jellyfin and Emby adapters | [`internal/frontends/mediabrowser`](../internal/frontends/mediabrowser) |
| Silo adapter | [`internal/frontends/silo`](../internal/frontends/silo) |
| Application composition and frontend registration | [`internal/app/frontend_runtime.go`](../internal/app/frontend_runtime.go) |

The frontend host supplies identity bound to each protocol. Plex sign-in and
claiming belong to the Plex integration; local Ether accounts support setup
without Plex. See [setup](user-guide.md#set-up) for the user-facing flow.

## Migration and support status

The ownership above is the architectural direction, not a claim that every
implementation has already been extracted. Legacy `internal/plex` and
app-specific paths still contain behavior being moved behind shared contracts.
Artwork selection, rendering, palettes and queues now live in
`internal/artwork/service`; discovery localization and overlay selection live in
`internal/discovery`. Application composition constructs those services directly.
The Plex adapter is constructed on mount and owns its watch-state subscription.
This extraction still requires the recorded focused and Plex parity gates; see
[artwork extraction evidence](prism-extraction/artwork.md). Other legacy
dependencies remain migration debt to remove while preserving Plex behavior.

The import boundary for implementations under `internal/frontends` is recorded
in [`internal/frontend/depguard_test.go`](../internal/frontend/depguard_test.go).
It does not establish that the legacy Plex package or application wiring is
fully neutral. Protocol adapters and shared contracts also do not establish
native-client certification, complete conformance or performance parity.

[Jellyfin and Emby support](user-guide.md#jellyfin-and-emby-apps) and
[Silo support](user-guide.md#silo-apps) remain in development. The user guide
records pending device verification, unsupported web-shell clients, downloads
and Silo schema/playback limits. Those holds remain in force regardless of the
architecture name. Plex-specific setup and feature instructions continue to
apply to Plex clients.
