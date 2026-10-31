# Experience extraction and date audit (slice 5)

Base: candidate-3 `82daa1c79`. Experience A and B selections are retained;
this slice closes the following remaining production decisions.

| Remaining decision | Canonical owner after this slice | Adapter responsibility retained |
| --- | --- | --- |
| Row content/section filtering, discovery availability priority, next-up availability guard | `experience.RowSelection` | Plex hub keys, contexts, promoted fields, XML/JSON metadata |
| Restricted viewer classification | `experience.ViewerRestricted` | Pass configured access-policy capability |
| Duplicate episode source-evidence check | `experience.SameDayUnavailable` | Bind local availability source and clock |
| Show detail resume/next-episode selection | `experience.ShowOnDeck` | Load episode order/watch states; render selected episode |
| Row-entry and interaction admission switches | `experience.RowEntryPolicy` | Bind favorite/current-playlist dependencies |
| Candidate-to-row projection, semantic-purpose classification and row-source assembly | `experience.CandidateRowSource`, `CandidateRows`, `CandidatePurpose`, `HomePurpose` | Personalization dependency binding; Plex home layout serialization |
| Version date publication/update fallback | `core.Title.DateAdded`, `VersionOfferFrom` | Convert neutral offers into frontend contract |
| Date-added sorting for global/recursive/children neutral views | `experience.Service.sortTitles` | Parse protocol sort names |

`internal/app/frontend_experience*.go` retains construction, current configuration,
service dependency callbacks, artwork warming, source enrichment and persistence
bindings. The callbacks call Experience policy/selection rather than select an
alternative Plex path. `internal/plex` retains request parsing, wire renderers,
client presentation quirks and thin dependency wrappers. Broker orchestration is
owned by the broker slices; playlist/container/placeholder extraction is owned
by Experience B (`3a8c9a3bf`, `18846f422`), not duplicated here.

## Date contract and approved Plex differences

Root confirmed the owner decision in thread `thr_6hmqzepnsk` on 2026-10-30:

- Movie, episode, series and season: that title's own release or air date.
- A series or season premiere is not replaced by a later episode.
- A show or season with no premiere of its own may still advertise a derived
  air date stored on `AddedAt` (virtual DVD seasons). That is not a catalogue
  insertion time.
- Future/unknown dates are absent from Recently Added. No catalogue insertion,
  metadata refresh, file modification, publication or observation fallback.

`releaseAddedSQL` and `CachedAddedTimes` read `released_ts` only.
`KnownAddedDate` adds that expression to the filter **before counting and
pagination**, only for Recently Added queries. Plain library date-added sorts
still include unknown items. Spike's local-file catalogue sorts by release
instead of modification.

Approved Plex differences: `addedAt` is the title's own release date, including
series and seasons. A supplied catalogue `AddedAt` of `1704067200` (2024-01-01)
for a title released at `1072915200` (2004-01-01) becomes **1072915200**.
`originallyAvailableAt` remains the premiere; metadata `updatedAt` is unchanged.
Recently Added orders and filters by that same release date, so a 2004 series
does not sort ahead of a 2020 movie because a descendant aired in 2024.
Recently Added excludes unknown/future items before its limit. No date
differences are normalized out of comparisons.

Jellyfin and Emby `DateCreated` use this title date; `PremiereDate` remains the
original release. Latest grouped series rows use the series premiere, not the
newest episode. Ungrouped episode rows still use each episode's air date.
Silo version `added_at` uses the same title date, omitting it when unknown.
Version `Publication` and persisted `FirstObservedAt` remain distinct facts;
neither is serialized by protocol adapters as an added date.

## Verification scope

Exact approved date values: `TestApprovedReleaseDateAddedAtDelta`.
Catalogue insertion/order/filter regression: `TestMovieAddedSortIgnoresCatalogueInsertion`.
Series and season premieres, not descendant air dates: `TestAirAddedAndAiredEpisodeBrowse`,
`TestSeasonAirAddedFallbacks`, `TestSectionDateSortRoutesUseAirDates`.
Neutral mixed-type sorting: `TestDateAddedSortAcrossTypesAndChildren`.
Owner/restricted postplay frozen XML/JSON: `TestPostplaySelectionMatchesFrozenBytes`.
Existing Continue/search/home/row tests retain legacy controls and compare raw
bytes without normalization. Remote stage receipts accompany the handoff.

## Virtual DVD seasons (separate owner-approved follow-up)

Episode-order virtual seasons have no own premiere. Their added date is now the
latest aired date among their reordered children, with the series date as the
fallback and unknown/future-only dates omitted when that fallback is unknown.
The neutral added-date sorter retains this derived date when the catalogue has
no physical row for the virtual season key.

Root approved this additional Plex delta on 2026-10-30: a virtual season whose
latest aired child is 2004-01-01 gains `addedAt=1072915200`, where that attribute
was previously absent. `originallyAvailableAt` remains absent. The exact value,
future-child exclusion, series fallback and unknown case are regression-tested.
