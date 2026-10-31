# Multi-app architecture

**Ether is the product. Ether's multi-app support is the shared protocol-neutral
server the app frontends run in.** Plex, Jellyfin, Emby and Silo are frontend
adapters. The repository, executable, services and installation paths use the
Ether name; there is no separate installation for the shared server.

The [README diagram](../README.md#under-the-hood) shows the ownership model.
Clients connect to their frontend, which translates requests and responses
between the client's protocol and shared contracts. Plex clients speak the PMS
protocol to the Plex frontend. Jellyfin, Emby and Silo do not need that frontend,
Plex authentication or Plex wire objects to define their server behavior.
An optional personal Plex Media Server for local files remains a separate
external server, not a dependency of the shared server.

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
[artwork extraction evidence](plex-adapter/artwork.md). Other legacy
dependencies remain migration debt to remove while preserving Plex behavior.

The import boundary for implementations under `internal/frontends` is recorded
in [`internal/frontend/depguard_test.go`](../internal/frontend/depguard_test.go).
Plex production imports are ratcheted there too: new internal imports fail.
The package still imports playback domain packages, so it is not yet held to
the frontends-only rule. Rename-aware line accounting is in
[plex size](plex-adapter/size.md). Protocol adapters and shared
contracts also do not establish native-client certification, complete
conformance or performance parity.

[Jellyfin and Emby support](user-guide.md#jellyfin-and-emby-apps) and
[Silo support](user-guide.md#silo-apps) remain in development. The user guide
records pending device verification, unsupported web-shell clients, downloads
and Silo schema/playback limits. Those holds remain in force regardless of the
architecture name. Plex-specific setup and feature instructions continue to
apply to Plex clients.

## Candidate-3 first-pass acceptance notes

This describes candidate-3 (`82daa1c7`), before final Plex broker adoption.
Multi-app support is part of Ether, with no separate service or installer. The proposal is
a target architecture; it is not a certification or release acceptance receipt.

`internal/app/frontend_runtime.go` composes a `frontend.Registry`, mounts the
configured protocols and supplies a surface-bound `frontend.Host`. The registry
owns mount/close and listener configuration. The owner console remains available
independently of protocol listeners. Plex uses a lazily constructed existing
`internal/plex` server. The adapter dependency guard covers `internal/frontends`;
it does not yet establish that Plex uses only Host contracts.

`internal/experience` owns shared policy-filtered views, semantic rows, continue
and next-up selection, search, details, versions and track ordering. Home layout
facts are captured when a Plex row is painted; the layout pass reorders those
facts and does not rebuild them by scanning wire sidecars. App adapters bind
catalogue, viewer state and personalization into those ports. Protocol renderers
translate the results into their own DTOs and IDs. Playback routing is still the
Plex broker cutover. Date added means the title's release date, not the catalogue
ingestion or first-observed timestamp.

`internal/sessions` supplies shared planning, opening, reporting, stopping,
recovery, scoped media access and cluster events. New frontends receive a broker
bound to their surface. Final Plex lifecycle adoption, retirement and combined
HA acceptance remain pending. Do not infer mixed-protocol failover guarantees
from individual broker tests. Plex wire behavior must remain byte-identical,
except for approved restricted-viewer policy leak repairs.

Identity retains the household viewer ID as the owner of history and preferences.
Identity links attach provider subjects; grants bind a viewer to a surface and
device, with expiry and revocation. Local accounts and Quick Connect permit
Plex-free use. An identity link does not merge people by display name. Protocol
setup routes cannot claim Ether; setup and ownership belong to the owner console.
See [the threat model](../design.md) for credential, proxy and cluster boundaries.
Its requirements must be checked against the final build and security receipts;
they are not a claim that mixed-version or lifecycle acceptance is complete.

The Apps overview enables protocols and reports status. Enabled protocols expose
configuration tabs, with changes edited in place. Drawers are for records. There
is no generic Settings menu. The [owner guide](user-guide.md) distinguishes native
client targets from unsupported web shells. No third-party web client is shipped.
Conformance, native-client certification, performance comparisons and owner-device
acceptance are pending until versioned receipts cover the exact integrated build.

The first upgrade onto multi-app support is a maintenance transition, including in HA deployments.
Follow the [upgrade runbook](acceptance/upgrade.md) before serving traffic.
Reference recording and offline drift checks are described in
[the reference workflow](acceptance/reference-drift.md).
