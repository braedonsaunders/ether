# Live TV guides: Phase 1 findings (2026-11-14)

## Decision

A single Ether server can advertise multiple grid providers, but **current Plex Web labels every local provider with the server name**. Changing provider `title` or `friendlyName` cannot give the source picker distinct Canada / Kids labels. The coordinator approved the alternative: one source, owner-defined Guides published as named `GridChannelFilter` tabs. Native-client rendering remains a device verification requirement.

This report precedes product changes. Base: `origin/integration/0311`.

## Evidence and limits

Authenticated, read-only GETs to the reference PMS used `agent-secrets/plex-ref.env`; no token is included. `reference-summary.json` retains only structural observations. `/livetv/dvrs` contains one DVR (key 48), with `epgIdentifier=tv.plex.providers.epg.xmltv:48`, one device, and device status `dead`. `/media/providers` contains one matching provider: id 49, parentID 48, generic title Live TV & DVR, XMLTV source, and server name as friendlyName. Web, iOS and Apple TV fixtures at `testdata/conform/plex/*/*_json_media_providers.json` have the same provider structure. These establish discovery shapes, not native client behavior with several providers.

The supplied Dispatcharr screenshot shows six device cards; the source-picker screenshot shows only WilbergFamily and Plex Channels. Six tuners are not evidence for six distinct picker sources. Reference PMS has no working tuner to demonstrate current programme category generation. No reference settings were changed, and no tuner was added.

Plex Web was fetched directly from [app.plex.tv/desktop](https://app.plex.tv/desktop/), version **4.163.0 / 8246fd2**. `bundle-index.json` records the retrieved script hashes and byte offsets so the conclusions can be checked against this exact build. Downloaded bundles remain outside the product; no third-party code was copied into Ether.

## How the Web source picker works

- Main bundle module 69612 walks servers and their providers, retaining each provider with a grid feature. It does not deduplicate providers by server.
- Module 29943 builds source identity from server machineIdentifier AND provider identifier. Module 49101 removes entries in the viewer's disabledEpgSources preference using that full identity.
- Module 2601 supplies source labels: for local servers it returns server.friendlyName. Cloud providers have separate Plex Channels/provider-title rules. Provider friendlyName is not even included in the provider projection.
- Chunk 4119, function `xl` (SourcesMenuButton), renders one checkbox for each provider with the module-2601 label and full source identity. Function `Br` renders the source tabs using the same label. Thus N local providers yield N identically named checkboxes/tabs, as a source-code inference; this was not demonstrated by modifying PMS.
- Channels are fetched from `/<provider identifier>/lineups/dvr/channels`, so a real multi-provider implementation must scope every channel/grid/metadata/search/tune path, not merely advertise another DVR.

## Named Guide tabs and programme categories

Chunk 4119 function `Br` reads `Feature[type=grid].GridChannelFilter[]`. Each entry has `key`, `genreRatingKey`, `title`. Entries with a nonempty genreRatingKey become named guide tabs, deduplicated across providers by genreRatingKey. Function `Or` gathers matching filter keys from enabled providers and fetches those keys for channels. The key must therefore return a **channel list**, not grid programme metadata. Use stable guide IDs for keys and an Ether namespace for genreRatingKey, separate from conventional programme categories.

`PromotedGridChannelFilter` is another named-tab mechanism, keyed by provider and filter key, but is unnecessary for the approved design. Arbitrary custom tabs are an observed Web protocol mechanism; they are not established as universally supported by Plex clients.

Programme categories are distinct from provider M3U channel groups and owner Guides. Preserve XMLTV programme category values into Item.Genres, carry them through every facade, emit Plex Genre tags, and classify movie/series/sports/news/kids separately for the standard programme views. Movies also need the correct media type. Server responses must advertise standard filters/hubs and actually apply their query predicates; adding Genre tags alone does not create custom tabs in Web.

Ether currently parses xmlProgramme.Category but discards it when creating Item in `internal/livetv/import.go`. Plex emits Item.Genres in metadata, but `handleLiveWatchNow` exposes only All Channels and there are no grid filters. The neutral LiveProgramme facade omits genres and type; mediabrowser programmeDTO hardcodes IsMovie=false and only infers IsSeries from episode numbers. These are the propagation points to change. Localized XMLTV categories need conservative normalization; do not classify every unknown programme as a movie or a kids programme. The reference `/watchnow` currently returns only All Channels, so the precise PMS XMLTV-to-category algorithm is **not proven** here.

[Plex's Program Guide documentation](https://support.plex.tv/articles/225877387-program-guide/) describes source selection and content categories. [Big Screen navigation](https://support.plex.tv/articles/navigating-the-big-screen-apps/) describes server/FAST source choices and What's On views. Neither promises custom owner group tabs.

## Client confidence matrix

| Client | Established | Still requires actual client verification |
| --- | --- | --- |
| Web 4.163.0 | Per-grid-provider identity; server-name source labels; server-provided GridChannelFilter tabs and filtered-channel requests, established from bundle | Render the approved tabs against Ether; select, tune, switch, rename/reorder/hide, refresh |
| Android TV / Fire TV | Official Live TV support | GridChannelFilter rendering and unknown genreRatingKey handling; no app bundle inspected |
| Apple TV | Official support; captured provider response matches Web | Whether current app renders arbitrary filters; capture does not prove behavior |
| Roku | Official support | Same filter and tune behavior; no inspectable app bundle available |
| LG / Samsung / VIZIO TV apps | Listed as supported by Plex | Version-specific tab support; Web behavior cannot be assumed |

Do not claim separate friendly source names, universal custom tabs, or actual native validation. [Plex support lists Live TV platforms](https://support.plex.tv/articles/226463767-frequently-asked-questions-dvr-live-tv/). Changing server identity or cloud flags to work around names would be a separate architecture and is outside the approved scope.

## Jellyfin, Emby, Vela

**Emby:** [ChannelTags](https://dev.emby.media/reference/RestAPI/LiveTvService/getLivetvChanneltags.html) discovers channel tags, and [Channels](https://dev.emby.media/reference/RestAPI/LiveTvService/getLivetvChannels.html) supports a pipe-delimited Tags query. Publish Guide names as channel Tags, implement discovery and filtering before pagination, and expose standard programme booleans plus Genres. [Emby's Live TV API](https://dev.emby.media/doc/restapi/Live-TV.html) documents IsMovie/IsSeries/IsSports/IsNews/IsKids. Guide tags can overlap without duplicating channel/playback identity.

**Jellyfin:** official server master LiveTvController.GetLiveTvChannels has standard category booleans but no Tags parameter. Current [Web guide](https://github.com/jellyfin/jellyfin-web/blob/master/src/components/guide/guide.js) sends those category booleans; it does not create arbitrary tag tabs. Sending Tags in a DTO alone does not add an owner Guide picker. [XmlTvListingsProvider](https://github.com/jellyfin/jellyfin/blob/master/src/Jellyfin.LiveTv/Listings/XmlTvListingsProvider.cs) retains programme categories as genres and derives the standard booleans with configured category matches. [M3uParser](https://github.com/jellyfin/jellyfin/blob/master/src/Jellyfin.LiveTv/TunerHosts/M3uParser.cs) reads group-title as ChannelGroup, but that does not establish a standard group-selection UI. Approved scope: standard category booleans; no promise of custom guide tabs.

**Vela:** local checkout `Documents/vela`, inspected at c6f9d6d, client/src/live/guide.ts uses Channel.group (one string), builds distinct GuideModel.groups, and carries the string into GuideChannel. The existing neutral Ether LiveChannel already has Group. Single group cannot express overlapping Guides; coordinate an additive field with the Vela worker rather than duplicating channels or overwriting provider grouping. This file inspection is not a screenshot or a claim about every skin's rendered controls.

## Implementation constraints and acceptance

Persist stable owner Guide IDs and ordered group membership, independent of rename. Derive defaults from provider groups without opening streams, scanning the content library, or scheduling new idle work. Filter viewer-visible channels by Guide membership before paginating; hidden/private/disabled channels must remain hidden. Keep existing channel IDs and tune authorization. Owner actions belong in the existing Live TV page and drawers, with translated copy.

Coordinator must deploy to the authorized lab and verify Web tabs, selection, tune, rename/order/hide/combine, Emby discovery/filtering, Jellyfin category results, and Vela groups. Real-device native tabs remain release evidence to collect. Phase 2 requires console and deployed Web screenshots; this report has not captured them.
