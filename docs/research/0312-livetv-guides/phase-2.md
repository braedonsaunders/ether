# Owner guides: implementation and verification

Implemented after coordinator approval, on the approved one-server / named-filter design.

## Delivery

- `w0312/livetv-guides`: research 12bca233a + 0289e4db2; implementation 92b1c3880.
- `w0312/vela-livetv-guides`: merge + adapter d7d233225, based on `origin/w0312/vela-crew-art`. This branch includes the main implementation. The merge resolves overlapping XMLTV category parsing and the duplicate neutral Genres field.
- Additive `livetv/20` table stores owner guides, ordered group membership, visibility, and stable IDs. Defaults come from existing channel groups; groups smaller than five channels and ungrouped channels share Other channels. Defaults cap the number of guides at 200. Existing group text and channel IDs are preserved.
- Owner drawer on the existing Live TV page supports rename, reorder, hide, combine, add, delete, and selecting groups or All channels. Nothing is saved until Save. Console copy has all 19 non-English translations.
- Plex remains one DVR/source. GridChannelFilter advertises guide channel-list keys and distinct programme-category keys. XMLTV categories survive into programme Genres; Plex metadata emits them for unmatched programmes too. Standard category selection uses explicit programme categories and media types, not channel-group names.
- Emby receives guide Tags, `/LiveTv/ChannelTags`, tag filtering before pagination, and programme booleans. Jellyfin receives standard programme booleans/genres and category filters; no promise of arbitrary guide tabs.
- Neutral `LiveChannel.Guides` descriptors and Vela `Channel.guides` preserve stable IDs, owner order, overlapping memberships, and rename changes. Unknown nil membership is omitted; known-empty membership is `[]`. `Channel.group` retains provider grouping.

No new refresh loop or provider fetch is introduced. Group choices reuse a lineup-generation cache, guide memberships are reused for channels sharing a source/group, and filtering preserves viewer visibility. Programme category matches use a bounded guide window. Unknown/localized categories are retained as Genres; unrecognized category spellings do not invent standard flags. Alias coverage is deliberately conservative.

## Checks

Passed on the main branch:

- `CGO_ENABLED=0 go build ./...`
- `CGO_ENABLED=0 go vet ./internal/livetv ./internal/frontend ./internal/frontends/plex ./internal/frontends/mediabrowser`
- `go test ./internal/livetv -run '^TestOwnerGuideMembership$' -count=1`
- `go test ./internal/frontends/plex -run '^TestOwnerGuideFilterDiscovery$' -count=1`
- `go test ./internal/frontends/mediabrowser -run '^TestSwiftfinLiveTVChannelsProgramsAndTune$' -count=1`
- `pnpm typecheck`, `pnpm i18n sync`, then each locale's `pnpm i18n todo` / `merge`; all 19 merges reported zero missing.

Passed on the merged Vela branch:

- `CGO_ENABLED=0 go build ./...`
- `go vet ./internal/frontends/vela`
- `go test ./internal/frontends/vela -run '^TestLiveOwnerGuides$' -count=1`

The Vela test covers nil versus empty, ordered overlapping memberships, unchanged provider group, and rename invalidation of the channel response ETag. The other new invariants cover stable rename/order/hidden membership and Plex XML/JSON discovery with channel-list keys.

## Lab evidence (2026-11-14, 20:14–20:17 UTC)

Coordinator deployed `0.3.11-lab.13`, including d7d233225, and explicitly authorized the lab profile. At `http://192.168.68.65:32550/ether/livetv`:

1. Opened Guides and confirmed defaults USA TV Next (169 channels) and Other channels (the four Decades channels).
2. Created **Worker guide check**, selected All channels, saved, reopened the drawer, and confirmed persistence.
3. Deleted that test guide, saved, reopened, and confirmed only the two original guides remained. No channel or source settings were changed.
4. Closed the browser session; the persistent profile is released.

Screenshots were opened and visually inspected:

- [Guide editor](screenshots/console-guides.png)
- [Persisted test guide](screenshots/console-test-guide.png)
- [Plex sign-in blocker](screenshots/plex-sign-in-blocker.png)

Plex Web in that profile is signed out (visible Sign In / Sign Up). Per coordinator instruction, no sign-in was attempted. **An Ether guide-tab screenshot and actual Plex Web selection/tune behavior remain blocked**, despite the established bundle behavior and passing protocol test. The Plex screenshot is evidence of that blocker, not a successful guide render.

No Android TV / Apple TV application bundles were available in the inspected local setup, and adb was unavailable. Native Plex GridChannelFilter rendering on Android TV, Apple TV, Roku, and TV apps remains unverified. Real-device category/tune behavior, native tab support, Emby tag UI, and Vela group rendering remain coordinator acceptance checks. Rename/order/hidden behavior is covered offline; the lab UI check intentionally changed only a disposable test guide.
