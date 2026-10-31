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

## Personalized home in Jellyfin and Emby

Ether publishes a viewer's top Home rows as extra libraries on the shared
MediaBrowser engine (`internal/frontends/mediabrowser`). Plex still paints Home
from Experience directly. These libraries use the same rows, ranker and bandit.
Their ids are stable for a viewer and a row identity, and the items inside them
are the real titles.

Clients that prefix the library name should read a noun phrase. Jellyfin Web's
English string is `Recently Added in {0}` (`LatestFromLibrary`). A row titled
"Because you watched Dune" is published as "Dune and Similar", so the shelf
reads "Recently Added in Dune and Similar". "Trending" stays "Trending".

`/Items/Latest?ParentId=<view>` and `/Items?ParentId=<view>` return that row's
ranked order. A `SortBy` on the folder is ignored, because the folder is the
ranking. Played titles stay out of Latest when the viewer still has the default
hide-played setting. Opening an item uses its real id, so detail and playback
are the library title, not a copy. Showing the folder records a row impression.
The next play of one of those items, on that device, records a row play.

`/Movies/Recommendations` and `/Shows/Recommendations` return Jellyfin
recommendation categories filled from the same rows. `because-you-watched` is
`SimilarToRecentlyPlayed` ("Because you watched {name}" in Jellyfin Web). A
person row is `HasDirectorFromRecentlyPlayed` or `HasActorFromRecentlyPlayed`.
Other rows are `SimilarToLikedItem`.

When the viewer has not saved `usersettings` home sections, Ether sets
`homesection0` to `latestmedia`, then resume, next up and library tiles, so the
new shelves lead. A saved `homesection*` key is left alone. When the viewer has
not saved `OrderedViews`, personalized libraries come first.
`POST /Users/{id}/Configuration` stores an explicit order and is not overwritten.

The owner control is Apps → Jellyfin and Apps → Emby. Personalized rows are on
until turned off. The count defaults to 4. Kinds left all on means every
personalized row except Continue Watching and Up Next, which those apps already
have. Date added on a real title remains its release date.

Silo Home (`GET /api/v2/home/sections`) paints those same Experience rows and
does not invent a shelf when Experience returns none. `GET /api/v2/recommendations/discover`
returns the recommendation rows, including **Now available**, without recording
a second impression. The watchlist hub id is `watchlist.available`. Silo also
publishes it as library `now-available` when Experience has not already published
a library of that name. Jellyfin and Emby publish that hub as a library without
counting it against the owner's personalized-row limit.

| Client | How Home is built | Where Ether's rows appear | Order |
| --- | --- | --- | --- |
| jellyfin-web | `homesections.js` reads `usersettings` `homesection0`..`9`, else tiles, resume, audio, books, live TV, next up, latest. Latest Media walks every user view except playlists, live TV, box sets, channels and folders, and views in `LatestItemsExcludes`. | One shelf per view, titled "Recently Added in {name}", from `GET /Items/Latest` with no sort. Suggestions uses `GET /Movies/Recommendations`. | User-view order, which Ether leads with the personalized libraries. Server array order is what the cards use. |
| jellyfin-androidtv | Home rows come from the same display preferences. `HomeFragmentLatestRow` adds "Latest in {library}" for each user view except playlists, live TV, box sets and books, via `GetLatestMedia`. Resume and Next Up are separate rows. | The virtual libraries become those Latest rows. | The view list order. No client sort on Latest. |
| Swiftfin | The shipped profile loads `/Users/{id}/Views`, resume, Next Up, then `/Items?ParentId=&SortBy=SortName` for a library. It does not call Latest in that profile. | Personalized libraries show in the view list. Opening one returns ranked items even though the request asks for `SortName`. | Folder order is the ranking. |
| Findroid | Same profile shape as Swiftfin: views, resume, Next Up, then a library query with `SortBy=SortName`. | Same as Swiftfin: the extra views, ranked when opened. | Folder order is the ranking. |
| Streamyfin | Same profile shape as Swiftfin. | Same as Swiftfin. | Folder order is the ranking. |
| JellyCon | Same profile shape as Swiftfin. | Same as Swiftfin. | Folder order is the ranking. |
| Infuse (Jellyfin) | Captured traffic: resume, Next Up, `GET /Items/Latest?ParentId=` for each known library (no sort), then `/UserViews`, grouping options, and `GET /DisplayPreferences/usersettings?client=emby`. Opening a library uses `/Items?SortBy=SortName`. | After Infuse learns the new view ids, each one gets a Latest shelf in capture order. The closed client may still sort that shelf by `DateCreated`; the folder itself stays in rank order. | Latest is server order unless Infuse re-sorts by the date field. |
| Emby SDK sample | Profile: `/Users/{id}/Views`, resume, Next Up, then `/Users/{id}/Items?ParentId=`. No Latest call. | Personalized views are in the view list. Browsing one returns ranked items. | Server item order. |
| Infuse (Emby) | Uses the Emby profile's library browse, not Jellyfin Latest. | The extra Emby views, ranked on browse. | Server item order. |

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

## Watch Party and SyncPlay

One room engine (`internal/watchparty`) keeps a shared title and timeline.
Silo and Jellyfin are two dialects of that room, so a Silo viewer and a
Jellyfin viewer can be in the same party. Each member opens their own playback
session through the broker, so each device keeps its own transcode decision and
its own stream limit. A member who is already at their limit is left out of
playback; the host is refused if they themselves are at the limit. A member who
is not allowed to see the title cannot join, and the response does not include
that title. Selecting a title is refused when any current member cannot see it.

The room schedules play, pause, and seek a short time ahead (`When` on
Jellyfin, `execute_at` on Silo) so slower clients land on the same playhead.
Jellyfin Web measures that clock with `GET /GetUtcTime`. Silo measures it with
the room socket's ping and pong. The shared clock can run above 1x only when
every member is a Silo client, because Jellyfin SyncPlay has no playback-rate
command. A mixed room stays at 1x so a Jellyfin player does not drift away from
the couches that changed speed.

The party pins the title, not a file. Silo may record the host's file id on the
room, and each member's own session still chooses the file it can play.

| Client | Watch party | What Ether serves |
| --- | --- | --- |
| Silo (Apple, API v2) | Yes. Create, join by code or `/rooms/join?token=`, membership, play, pause, seek, leave, and end. | `GET /api/v2/watch-together/capabilities` reports `silo.room.v2`. Playback capabilities include `watch_party_coordinator_v1` and `fixed_media_file_v1`. Suggestions, the picker, and vote mode stay off. |
| jellyfin-web 10.10.7 | Yes. `src/plugins/syncPlay` creates a group, edits the queue, and follows `SyncPlayGroupUpdate` and `SyncPlayCommand`. | `SyncPlayAccess` is `CreateAndJoinGroups`. `POST /SyncPlay/New`, join, leave, list, pause, unpause, seek, stop, buffering, ready, ping, queue, repeat, and shuffle. |
| jellyfin-androidtv | No SyncPlay source files in current master. | The server API is there if a later build calls it. The app does not show it today. |
| Findroid | No SyncPlay source in current main. | Same as above. |
| Streamyfin | No SyncPlay source in current main. | Same as above. |
| Swiftfin | Admin permission only (`SyncPlaySection`). No player. | The user policy tells the app SyncPlay exists. Playback is not in this client. |
| Emby | No. The pinned Emby SDK contract (`internal/conform/profiles/emby/emby-sdk.json`) has no SyncPlay and no Parties path. | Emby responses omit `SyncPlayAccess`. `/SyncPlay/*` is not registered. Emby Parties is a different API and is not part of that contract. |
| Plex | Unchanged. | Plex clients are not given a watch-party route. |

In a Jellyfin-only room any member can change the queue, which is how Jellyfin
Web behaves. As soon as a Silo member is in the room, only the host changes the
title or ends the party. Guests can still play, pause, and seek unless the host
sets the room to host-only. The host is the member who created the room. If
that Jellyfin member leaves, the next member becomes host. A Silo guest who
drops the socket stays a member and can rejoin with the same code.
