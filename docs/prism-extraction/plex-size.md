# Plex size accounting (rename-aware)

Measured on this worktree after the non-playback extraction closure. Counts are
newlines in non-test `*.go` files, including comments and blanks, which is the
same method as `TestCheapSurfaceSize`. The proposal's Phase 3 baseline is about
18.8k non-test lines in `internal/plex`. Half of that baseline is 9,400.

Renames and extractions are not treated as deleted product behavior. Lines that
now live in `internal/experience` still exist; they are no longer Plex wire code.

## Snapshot

| Bucket | Lines | What it is |
| --- | ---: | --- |
| `internal/plex` production | 17,238 | Whole adapter, including playback |
| Broker-owned Plex files | 3,213 | Playback, transcode, sessions, decision and DASH files listed below. This slice does not edit them |
| Plex renderer remainder | 14,025 | Production lines minus the broker-owned files |
| `internal/experience` production | 7,860 | Shared home, continue, search, policy and rows |
| Named extracted experience files | 1,917 | The files that now own the old Plex continue, search, home, policy and cold-home decisions |

The renderer remainder is 14,025, which is 4,625 above half of the 18.8k baseline.
That gap is still wire rendering: hubs, metadata, library, Live TV and downloads.
It is not a second continue, search or home-policy implementation. Closing it to
9,400 depends on the Plex broker cutover deleting the playback orchestration
that this slice is not allowed to touch, and on further renderer thinning after
that cutover.

## Broker-owned files

These stay with the playback cutover. Their lines are excluded from the renderer
remainder so a playback edit is not counted as this slice growing or shrinking
the Home renderer.

| File | Lines |
| --- | ---: |
| `playback.go` | 1,387 |
| `playqueue.go` | 881 |
| `variants.go` | 405 |
| `handlers_dash.go` | 213 |
| `stream_auth.go` | 167 |
| `stop.go` | 113 |
| `playback_lifecycle.go` | 47 |

`convert.go` is the release-date worker's file. It remains inside the renderer
remainder and was not edited here.

## Named extracted files

| File | Lines |
| --- | ---: |
| `rows_service.go` | 300 |
| `continue.go` | 278 |
| `policy.go` | 202 |
| `home.go` | 143 |
| `search.go` | 123 |
| `postplay_selection.go` | 112 |
| `candidate_rows.go` | 111 |
| `recent.go` | 110 |
| `layout.go` | 103 |
| `row_candidates.go` | 100 |
| `home_selection.go` | 98 |
| `vibe.go` | 52 |
| `cold_home.go` | 47 |
| `postplay.go` | 44 |
| `playability.go` | 39 |
| `show_on_deck.go` | 30 |
| `search_discovery.go` | 25 |

Recompute with newline counts of non-test Go files. Do not treat a later line
change as a failed gate; update this snapshot when the buckets move.
