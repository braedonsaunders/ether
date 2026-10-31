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

The owner decision of 2026-10-29, restated on 2026-10-30:

- A movie's date added is its release date. An episode's is its air date.
- A series' date added is the air date of its most recently aired episode,
  falling back to its first-air date.
- A season's date added is the latest aired episode in that season, falling
  back to the season premiere. It does not borrow another season or the series premiere.
- Only episodes aired at or before now count. Unreleased and unaired items
  are absent from Recently Added.
- Catalogue insertion, metadata refresh, file modification, publication and
  observation times are never date added. A virtual season with no premiere
  of its own may still carry a derived air date on `AddedAt`.

`airAddedSQL` and `CachedAddedTimes` implement that rule.
`KnownAddedDate` adds that expression to the filter **before counting and
pagination**, only for Recently Added queries. Plain library date-added sorts
still include unknown items. Spike's local-file catalogue sorts by release
instead of modification.

Plex `addedAt` follows the same rule. A movie or episode ignores a conflicting
catalogue `AddedAt`: `1704067200` (2024-01-01) for a title released at
`1072915200` (2004-01-01) becomes **1072915200**. A series whose latest aired
episode is 2024-01-01 sorts ahead of a movie released in 2020.
`originallyAvailableAt` remains the premiere; metadata `updatedAt` is unchanged.
Recently Added excludes unknown and future items before its limit. No date
differences are normalized out of comparisons.

Jellyfin and Emby `DateCreated` use this date; `PremiereDate` remains the
original release. Latest grouped series rows use the latest aired episode.
Ungrouped episode rows use each episode's air date.
Silo version `added_at` uses the same title date, omitting it when unknown.
Version `Publication` and persisted `FirstObservedAt` remain distinct facts;
neither is serialized by protocol adapters as an added date.

## Verification scope

Exact approved date values: `TestApprovedReleaseDateAddedAtDelta`.
Catalogue insertion/order/filter regression: `TestMovieAddedSortIgnoresCatalogueInsertion`.
Latest aired episode, then premiere: `TestAirAddedAndAiredEpisodeBrowse`,
`TestSeasonAirAddedFallbacks`, `TestNewEpisodeLiftsShowInRecentlyAdded`,
`TestSectionDateSortRoutesUseAirDates`.
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
