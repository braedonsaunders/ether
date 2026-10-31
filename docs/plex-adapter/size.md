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
| `internal/plex` production | 17,228 | Non-test Go files directly in `internal/plex` (not `model/`) |
| Broker-owned Plex files | 3,203 | Playback, transcode, sessions, decision and DASH files listed below. This slice does not edit them |
| Plex renderer remainder | 14,025 | Production lines minus the broker-owned files |
| `internal/plex/model` | 1,602 | Plex XML/JSON types and encoding, counted separately because it is another package |
| `internal/experience` production | 7,860 | Shared home, continue, search, policy and rows |
| Named extracted experience files | 1,917 | The files that now own the old Plex continue, search, home, policy and cold-home decisions |

The renderer remainder is 14,025, which is 4,625 above half of the 18.8k baseline.
That gap is Plex wire. Continue, search, home layout, visibility, cold home,
playability, rating caps, downloads admission and discovery selection already
call Experience. Halving the remainder means moving the files below, which are
the Plex document, the Plex query vocabulary, or `convert.go`. This slice does
not do that.

## Broker-owned files

These stay with the playback cutover. Their lines are excluded from the renderer
remainder so a playback edit is not counted as this slice growing or shrinking
the Home renderer.

| File | Lines |
| --- | ---: |
| `playback.go` | 1,388 |
| `playqueue.go` | 881 |
| `variants.go` | 407 |
| `handlers_dash.go` | 213 |
| `stream_auth.go` | 167 |
| `stop.go` | 100 |
| `playback_lifecycle.go` | 47 |

## Renderer remainder, largest files

These are the lines that keep the remainder above 9,400. Each one is Plex wire
or a Plex quirk. Decisions inside them already delegate; the bulk is encoding,
queue state, or the Plex parameter vocabulary.

| File | Lines | What remains |
| --- | ---: | --- |
| `convert.go` | 1,337 | Plex media and date mapping. Release-date worker owns it. Not edited here |
| `handlers_downloads.go` | 941 | Sync queue, item state and decision URLs. Admission is `experience.DownloadsAllowed` |
| `handlers_library.go` | 910 | Library XML and the Plex browse grammar (type codes, sort names, filters). Continue, recent and search call Experience |
| `handlers_hubs.go` | 868 | Hub XML, identifiers and search scores. Row inclusion is `experience.HomeAssembly`; impressions look up the paint-time map |
| `handlers_metadata.go` | 729 | Metadata, children and collection XML. Visibility and same-day unavailability call Experience |
| `livetv_api.go` | 703 | Live TV, DVR and grid XML |
| `handlers_extras.go` | 695 | Extras and related XML, plus client capability fields. Play and bitrate checks use the shared access-policy interfaces |
| `handlers_state.go` | 564 | Timeline, scrobble and session XML. Left in place because it sits on the playback route |
| `handlers_playlists.go` | 535 | Playlist XML. Membership uses Experience |
| `server.go` | 530 | Process lifecycle and route mount |
| `cluster.go` | 529 | Plex server identity and cluster protocol |
| `caps.go` | 518 | Plex client-capability headers |
| `subtitles.go` | 446 | Plex subtitle stream XML |
| `middleware.go` | 442 | Plex request authentication and context |
| `notify.go` | 403 | Plex notification payloads |
| `handlers_server.go` | 400 | Server identity and preference XML |

Those sixteen files are 10,550 lines. The rest of the remainder is smaller
route, image, enrichment and adapter glue of the same kind. Moving any of them
out of `internal/plex` would move wire code, which does not make the renderer
smaller in the sense Phase 3 meant. `internal/plex/model` (1,602 lines) is the
same kind of code in the child package.

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
