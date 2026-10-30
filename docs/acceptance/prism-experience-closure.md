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

- Movie: release date. Episode: air date.
- Series: most recent aired episode, falling back to first-air date.
- Season: most recent aired episode in that season, then season air date,
  then the series rule.
- Future/unknown dates are absent from Recently Added. No catalogue insertion,
  metadata refresh, file modification, publication or observation fallback.

The catalogue's `airAddedSQL` and `CachedAddedTimes` already implement these
series/season fallbacks. They remain unchanged. `KnownAddedDate` adds the same
SQL expression to the filter **before counting and pagination**, only for
Recently Added queries. Plain library date-added sorts still include unknown
items. Spike's local-file catalogue now sorts by release instead of modification.

Approved Plex differences: movie/episode `addedAt` ignores conflicting `AddedAt`;
unknown/future release omits `addedAt` instead of falling back to `UpdatedAt`.
Example: catalogue `1704067200` (2024-01-01) for a movie released at
`1072915200` (2004-01-01) becomes **1072915200**. `originallyAvailableAt`
remains **2004-01-01**; metadata `updatedAt` is unchanged. Series/season
latest-aired dates remain unchanged. Recently Added excludes unknown/future
items before its limit. No date differences are normalized out of comparisons.

Jellyfin and Emby `DateCreated` use this title date; `PremiereDate` remains the
original release. Latest grouped rows retain their catalogue air-derived date.
Silo version `added_at` uses the same title date, omitting it when unknown.
Version `Publication` and persisted `FirstObservedAt` remain distinct facts;
neither is serialized by protocol adapters as an added date.

## Verification scope

Exact approved date values: `TestApprovedReleaseDateAddedAtDelta`.
Catalogue insertion/order/filter regression: `TestMovieAddedSortIgnoresCatalogueInsertion`.
Series/season fallback and airing boundaries: existing `TestAirAddedAndAiredEpisodeBrowse`,
`TestSeasonAirAddedFallbacks`, `TestSectionDateSortRoutesUseAirDates`.
Neutral mixed-type sorting: `TestDateAddedSortAcrossTypesAndChildren`.
Owner/restricted postplay frozen XML/JSON: `TestPostplaySelectionMatchesFrozenBytes`.
Existing Continue/search/home/row tests retain legacy controls and compare raw
bytes without normalization. Remote stage receipts accompany the handoff.
