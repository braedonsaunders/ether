# On Air: channel graphics, personal channels and channel packs

Status: in build for v0.3.5 (integration branch `feat/035-virtual-channels`). Proposal written 2026-11-04. Needs the owner decisions in
[§13](#13-owner-decisions-needed) before Phase 0 starts.

This spec covers three features:
- **Styles:** a generated on-air presentation layer for virtual channels (idents, up-next, breaks, back-from-break, off-air, tonight cards, and optional corner logo and lower thirds), designed in the console.
- **Personal channels:** one channel that each viewer sees programmed for them.
- **Channel packs:** shareable bundles of channels, styles, break reels and assets that use the existing plugin package format.
- **Break reels:** owner-uploaded short videos (commercials, promos) that play in breaks.
- **Plugin content:** user-authored plugins that feed live data and segments (weather, headlines) into channels, styled by each channel's style.

Every customization is done in the console. No code is needed.

The sample reel this spec is based on is
`thread-storage/thr_cp7svgfc62/reports/channel-graphics.html`.

---

## 1. What exists today

All references are to `main` at v0.3.3.

| Area | Today | Consequence for this spec |
| --- | --- | --- |
| Schedule | `livetv.Schedule(c, from, until)` is stateless and deterministic (`schedule.go:20`). Programme IDs are `<channel>-<startMs>` (`schedule.go:423`), and Plex metadata lookups parse that ms (`plex/livetv_api.go:345`). `legacyCalendar` keeps old channels' guides identical (`schedule.go:37`). | Interstitials must be deterministic, opt-in per channel, and must not change real programmes' IDs except when the owner saves a change. |
| Playout | One viewer session per HLS playlist, built from at most 3 per-programme ffmpeg tracks (`playout.go:28,638,696`). Boundaries are `#EXT-X-DISCONTINUITY` (`playout.go:754`). Segments are 4 s (`playout.go:21`). Plex Web uses a single-Period dynamic MPD (`dash_virtual.go:532`). Plex for Mac's Matroska path has no programme rollover (`http.go:96`). | A clip is another track, so splicing is native to HLS. The 3-track cap must become a budget. Clip durations must be multiples of 4 s. DASH clips must match the hard-coded representation. |
| Off air | v0.3.4 renders a real off-air card: `HLSRequest.SlateText` makes `transcode.Manager.slate` (`transcode/slate.go`) draw a card in Go, encode a 4 s MP4 whose codecs, size and audio layout follow the previous programme's negotiated output (`livetv.slateInfo`, `livetv/slate.go`), and loop it as the HLS input (`loopInput`). Copy comes from `offAirCopy`. | This is the path every generated clip reuses: render a clip that matches the stream contract, then hand it to `StartHLS` as the input. A clip is never a codec change mid-stream. |
| Tune time | `tuneVirtual` has a 12 s budget (`virtual_tune.go:23`) and the playlist can't be served until `Playback.Prepare` succeeds. | A cached ident track can be served immediately while the programme prepares: an instant first frame on every channel with a style. |
| Video generation | Nothing renders video from frames. Pre-rolls convert uploads to H.264/AAC MP4 (`prerolls/media.go:294`). Subtitle burn-in already builds an `overlay` filter chain (`transcode/args.go:1063-1130`). | The encode profile and the burn-in chain are reusable. The renderer is new work. |
| Go 2D | `golang.org/x/image` (vector, opentype, draw) and `srwiley/oksvg`+`rasterx`. Poster compositing lives in `artwork/poster.go`; generated channel logos in `artwork/svg.go:322`. No TTF files are bundled. | The renderer is built on `x/image`, with no new CGO. Fonts are new embedded assets. |
| Per-viewer | Lineup, guide and tune are already computed per request user for Plex, Jellyfin, Emby and M3U (`plex/livetv_api.go:204,280,310`, `app/frontend_livetv.go:66`). `Schedule` takes no user. | A personal channel is one channel ID whose programmes are resolved per viewer. The model already fits. |
| Personalize | `Engine.PreviewHome` returns rows with `ItemReasons` ("Because you watched X", "Next episode of Y") (`personalize/owner.go:31`). `Taste.Fit` returns a score and a reason (`taste.go:619`). There's no core interface for "N picks with reasons". | Add a narrow `core.PersonalPicker`, implemented by reusing the home assembly. |
| Trailers | `extras.List` and `Resolve` return direct URLs through an owner-configured yt-dlp (`extras/extras.go:208,344`). Live TV playout only plays catalogue keys. | Watchlist trailer breaks need a URL input path into `StartHLS`. When extras aren't configured, they degrade to generated cards. |
| AI | Describe-a-collection is the pattern: an xl drawer, a live result on top, `AgentPanel` below, a route-scoped agent with a `*_draft` tool (`collections/DescribeDialog.tsx`, `agent/loop.go:17-35`). There's no TTS and no image generation. | The style and channel describers copy that pattern exactly. An announcer voice is a later, opt-in phase. |
| Plugins | `.etherpkg` is a zip with a manifest (a Stremio superset) and digest integrity, installed from upload, URL, GitHub or the signed catalogue (`plugins/install.go`). A runtime is required (`manifest.go:209`). The only record-creating contribution is `collections.upsert` with links (`app/plugin_collections.go:54`). No asset route exists. Studio has no export. | A channel pack is a plugin with a new `data` runtime. It needs contributions for styles and channels, link tables, an asset store, and an export endpoint. |
| Cluster | Live TV sessions and caches are per node. The schedule is identical on every node. | Clips are content-addressed and rendered on whichever node serves the session. No leader is needed. |
| Console | Live TV is a `Workspace` (1/3 aside, 2/3 guide). Channels open `ChannelStudio` in a `Drawer size="lg"` with tabs Programming, Schedule, Viewers, Details. Home Designer has the canvas↔inspector focus pattern. The only animated canvas is Studio's `TestCard` (`plugins/Stage.tsx:550`). There's no colour picker and no web-font loading. | Reuse Workspace, Drawer (full size), SubtabNav, ToolbarPicker, Segmented, ChipGroup, MoreOptions, SaveBar and AgentPanel. Build a canvas stage. Build a palette editor in AppKit. |

**The Live TV page is protected** (owner rule, 2026-10-31). Only the Add flow
and the virtual channel creator are unlocked. This spec adds three things and
redesigns nothing:
- a **Styles** sub-tab in the aside;
- a **Style** tab in the virtual channel drawer;
- two Add-menu entries.

The owner has to approve these additions ([§13](#13-owner-decisions-needed)).

---

## 2. Concepts

- **Style:** a record. It holds a brand (name, logo, palette, type, texture, motion energy, sound, copy) plus one **moment** template for each on-air moment. Styles are edited in a full-size drawer. Many channels can share a style.
- **Family:** a built-in set of moment templates with a design language. v1 ships with:
  - *Clean*: the neutral default;
  - *Kids*: goo, mascot, squash and stretch;
  - *Late night*: grain, flicker, typewriter;
  - *Cinema*: gold line work, serif;
  - *Retro*: synth grid, chrome, VHS;
  - *Personal*: aurora, glass.

  Packs can add families. Families are data, not code.
- **Moment:** one kind of clip. `ident`, `upnext`, `tonight`, `break`, `back`, `offair`, `welcome` (personal channels), and the overlay moments `bug` and `lowerthird`.
- **Template:** a declarative scene document (§5) for one moment. Its slots (`channel.name`, `next.title`, `viewer.name`…) bind to guide data at render time.
- **Clip:** a rendered moment for specific data: a high-quality H.264/AAC master MP4 at 1080p30, content-addressed and cached per node. Each viewer session plays it through the v0.3.4 slate path, which encodes it to that session's stream contract.
- **Interstitial:** a schedule entry that plays a clip. It's an `Item` with `Key == 0` and a `Slate` kind.
- **Personal channel:** a virtual channel with a `Personal` recipe instead of fixed programming. Each viewer gets their own rundown.
- **Signal screen:** what a channel shows when it is off air or can't be tuned: Modern card, Analogue static, Colour bars, We'll be right back or Test card (§3.4a, §4.9).
- **Break reel:** a record holding owner-uploaded short videos (commercials, station promos, old ads, anything) that play during a channel's breaks. Ether never generates these. A reel works with or without a style, and many channels can share one.
- **Channel pack:** a plugin (`.etherpkg`, `runtime: data`) that holds channels, styles, families, reels and assets.
- **Plugin segment / feed:** a programme piece or a bindable data field supplied by a code plugin's `onair_content` capability (§7a).

---

## 3. Console design

The bar is the Library pages (Catalog, Collections, Home Designer, Live TV).
Everything below uses existing components and tokens: `Workspace`, `Drawer`,
`SubtabNav`, `ToolbarPicker`, `Segmented`, `ChipGroup`, `SettingsRow`,
`MoreOptions`, `SaveBar`, `AgentPanel`, `Badge`, `EmptyState`, `lucide-react`
icons, AppKit motion tokens and `--ch-*` colours.

Help text is one line at most. Explanations go behind ⓘ. There are no
decorative subtitles. All copy is wrapped in `t()`/`tn()`/`tx()`, and every
string goes through `pnpm i18n sync` and all 20 locales.

### 3.1 Live TV: Styles and Assets

The URL-backed aside remains one third of the workspace, with Channels, Sources,
Guide and **Styles**. The stable route is `/livetv/looks`; `?look=<id|new>` opens
an editor. **Styles** is the recommended owner-facing name. The review gallery
also shows Themes, Branding and On-air package as real tab/title variants.
`onair/naming.ts` centralizes the tab, page name and editor terminology; record/API identifiers remain
`Look`, `lookId` and `/onair/looks`.

Style cards come first in the main pane. Each opens its full drawer and names
its family and explicit channel usage. The aside contains styles only.

Below the cards, **Default style** selects the owner default and saves through
one SaveBar. It applies to virtual channels without an explicit style. An empty
default preserves existing channels. **Fallback screens without a style** is a
collapsed singleton section. Below it, **Assets** is the shared media library:
kind filter, search, image/logo/font/audio uploads, usage, and clip collections.
Reels are collections inside Assets, never a main sub-tab or an aside footer.
The reel storage/API names remain stable. Collection records use a drawer.

### 3.2 The style drawer

The full-size drawer has a one-third inspector and two-thirds live stage. Narrow
screens stack the stage above the inspector. Save and Discard are in the header;
changes are drafts until Save. Existing styles, family starts and duplicates all
use the same editor. Describe is primary when creating a style.

Inspector tabs:

- **Describe:** the route-scoped `/livetv/look` AgentPanel uses `look_draft` when
  the owner has a configured model. Tool outputs update the whole stage and have
  reversible change chips. Without a model, an explicit template form remains
  available: mood keywords select a complete built-in family deterministically,
  including all its supported idents, up-next, breaks, return cards, bug and
  lower thirds. This is labeled as a template, never represented as AI. Drafting
  never saves. The same template path is available with a model.
- **Design:** brand name, logo upload or monogram, tagline, four role colours,
  presets/shuffle and live contrast, bundled font pairs and uploaded fonts,
  motion energy, grain, uploaded audio sting and volume. More options has seed,
  per-moment words and 4/8/12-second duration buttons. Unsupported synthetic
  stings, language switching and texture controls were removed.
- **Layers:** lists the actual layers of the selected family/moment, including
  nested layers. Text, fit, size, position, opacity and visibility persist as
  patches consumed by browser and Go renderers. Text controls appear only on
  text layers. Visibility is saved in the style. The decorative timeline,
  keyframe counters, locks and unimplemented behaviour picker were removed.
  This is a scene-template editor; arbitrary keyframe authoring is not exposed.
- **Screens:** off-air, can't-play and station-ID choices belong to this named
  style. The drawer Save persists them with the rest of the record.
- **Assets:** opens the same shared library used on the Styles page. Font licences
  are required. Video uploads belong to clip collections.

The stage has play/pause, seek, loop, half speed, safe area, uploaded sting
preview, moment filmstrip, and overlay preview. Reduced motion starts paused;
focus/hover tiles stay still. Preview channel/time bind guide data. Undo/redo
applies to draft changes. Selecting a scene element opens Layers; all real layers
are also keyboard-accessible through the inspector. The shipped control audit
and persona evidence are in the W15 gallery.

### 3.2a Corner logo controls

The Design inspector includes **Corner logo**: four-corner picker, 15–60% opacity,
Monochrome / Original colour, Small / Medium / Large, Logo / Wordmark, and fades
during breaks and credits. Defaults: bottom right, 30%, monochrome, medium, logo
(wordmark when absent), break fades on and credit fades off. No badge or disc.
The channel Style tab has **Customize corner logo**; off inherits the style, on edits
the same controls over a programme preview. Channel fields override style fields.

### 3.3 The channel drawer: a Style tab (addition)

Virtual channel tabs go from **Programming | Schedule | Viewers | Details** to
**Programming | Schedule | Style | Viewers | Details**. IPTV channels keep a screens-only Style tab; virtual-channel graphics controls do not apply to third-party programming.

```
│ Style                                                                          │
│  ┌──────────────── live tile 16:9 ────────────────┐  [Nocturne ▾]             │
│  │  ident on this channel, tonight's programmes   │  [Edit style]              │
│  └────────────────────────────────────────────────┘                           │
│  [Ident][Tonight][Up next][Break][Back][Off air]   (filmstrip, click to view) │
│                                                                               │
│ ON THIS CHANNEL                                                               │
│  Ident when someone tunes in                                         [ ● ]   │
│  Up next before each programme                                       [ ● ]   │
│  Breaks              [ Off | Between programmes | Inside programmes ]        │
│  Off-air screen                                                      [ ● ]   │
│  Corner logo and lower thirds  ⓘ                                     [   ]   │
│ ▸ More options   (break length, how often, tagline and accent for this channel)│
```

- **Style picker:** a `ToolbarPicker` with a still per style, plus **Use default style** and **New style…**.
- **Edit style** opens the style drawer stacked over the channel drawer, using the Drawer's automatic `stacked` nesting.
- **Hero timeline:** interstitials show as thin hatched segments between programmes, so the owner sees exactly where clips land. The 500 ms debounced draft preview already in `ChannelStudio` covers this.
- **Break contents.** When Breaks is on, a second row appears: **During breaks** [ Style's break screen | Clips from a reel ]. Choosing clips shows a reel `ToolbarPicker` (with **New reel…**) and **Break length** chips (30 s, 1 min, 2 min, 3 min). With a style, a break plays the style's `break` clip, then reel clips up to the break length, then `back`. Without a style, the reel plays on its own.
- **"Inside programmes"** is disabled with a one-line ⓘ until Phase 5's break-point analysis exists. It is hidden in Phase 2 if the owner prefers ([§13](#13-owner-decisions-needed), decision 2).
- **Corner logo ⓘ:** "Needs transcoding. Ether only adds it when the stream is already being converted, unless you choose Always."

### 3.4a Signal screens (off air and can't play)

Era break pods do not change signal screens. Station ID uses the style's `ident`
moment; `break`, `back` and `ident` can select a video asset through the additive
`Moment.assetDigest`. Authored sound stays with its video, and the background
master conversion matches the requested moment duration and output contract.
Scene documents remain the default; the empty asset digest preserves them.


The owner picks what a channel shows when it is off air, or when it cannot be
tuned (IPTV provider down, tune failure, connection limit reached, failed
retune), instead of an error.

**Styles** (each a built-in clip with real audio):

| Style | Picture | Sound |
| --- | --- | --- |
| **Modern card** (default) | A clean card in the channel's style; without a style, the v0.3.4 slate card. Says "Back at 6:00 PM", "Reconnecting…" or "Waiting for a free connection". | Silent |
| **Analogue static** | Old-TV snow, a slow rolling bar, slight horizontal tear. | Hiss with a low mains hum |
| **Colour bars** | Classic SMPTE-style bars, optional station ID (channel number and name). | Silent (owner decision: no tone) |
| **We'll be right back** | A card in the channel's style. | A soft generated bed |
| **Test card** | An original geometric test card with the channel name. | Silent |

The set is open: more styles are scene documents in a built-in "Signal" family (§5), so packs and later releases can add them.

**Where it's set**
- **Owner default:** selects a shared style below the style cards. Each style owns its screens in the full drawer. Server fallback screens are collapsed below the default selector and use the singleton SaveBar.
- **Per channel:** the Style tab states Default style or Override for the channel. Screen overrides win over the selected style, which wins over server fallback. Each screen picker names its effective default. IPTV channels retain their screens-only tab.
- **The pickers:** a row of 16:9 style cards. Each card plays its own animated preview on hover, with a ▶ button for its sound (muted by default). The selected card has the primary ring. There is one line of ⓘ help per picker.

### 3.4 Break reels (owner-uploaded commercials)

**v0.3.5 additive era extension.** Reels and the asset library have an **Add
from link** action accepting one public media URL per line (up to 50). A single
link uses `url`; a batch uses `urls`. `POST /onair/reels/{id}/clips/import` and
`POST /onair/assets/import` return HTTP 202 with import jobs. Both accept optional
`name`; assets require `kind` (`image`, `audio`, `video`). Jobs appear in the
normal clip/asset list with queued/downloading/error status and download
progress; a downloaded clip continues through UploadClip's converting/ready
states. Jobs survive restarts. The original source and attribution remain on
the result. Archive details links resolve through the public metadata API,
preferring an h.264 MP4 derivative; title and license URL form attribution.
No media or network work runs on the request path. Outbound requests use Ether's
plugin egress policy, reject non-public addresses at every redirect and direct
connection, validate MIME and probe content. Reel imports retain the existing
500 MiB/3 minute limits; library assets retain 32 MiB/30 second limits.

The channel Look tab's Breaks section offers **Fixed length** (the unchanged
default), four era presets and **Custom**. Opting in plans an out-bumper (`break`),
whole reel commercials, an in-bumper (`back`), and optionally a station ID
(`ident`). Target seconds or spot count can be zero to use the other target;
minimum/maximum seconds and spots constrain the whole pod, including bumpers
for time bounds. If available material cannot meet minimums, the pod is skipped
without consuming repeat history. Whole spots never overrun the maximum.

| Preset | Target seconds | Spots | Min–max seconds | Min–max spots | Pods/half-hour | Clock | Station ID |
| --- | ---: | ---: | --- | --- | ---: | --- | --- |
| 1980s | 120 | 4 | 60–180 | 2–6 | 2 | half | yes |
| 1990s | 150 | 5 | 60–180 | 2–6 | 2 | none | yes |
| 2000s | 180 | 6 | 60–240 | 2–8 | 3 | none | no |
| 2010s | 210 | 7 | 60–270 | 2–9 | 3 | none | no |

These are editable design approximations of the progression from shorter,
fewer commercial interruptions to longer, more frequent pods, rather than
claims about every network or jurisdiction. Every preset uses break/back
bumpers, inside-programme placement when saved points exist, and a 60 minute
spot cooldown. Shuffle is seeded by channel seed and channel-local day; pod
clock rotation varies selection within that daily ordering. Frequency is a
ceiling with evenly spaced minimum intervals. `half` also prevents a pod from
crossing the next half-hour boundary. Sparse points or material can reduce the
actual frequency. Existing reel cooldown applies when it is longer.

Inside-programme pods use the additive sorted `Item.breakPointsMs` offline
chapter/black/silence timestamps; no analysis runs while scheduling. This
checkout has no offline analysis producer, so a source without saved points
uses between-programme pods. Each resumed piece retains its media offset.
Guide rows fold pods and resumed pieces into one programme slot.


Reels are records, so they open in a `Drawer size="lg"`, modelled on the
pre-roll drawer (`pages/playback/PrerollDrawer.tsx`). They're reached three ways:
- **Styles › Assets › Clip collections**;
- the reel picker's **New reel…** in the channel Style tab;
- **Add › Break reel**.

```
│ Saturday morning ads                         [Unsaved] [Cancel] [Save reel]  │
│                                                                              │
│  ┌ Drop videos here, or [Choose files] ───────────────────────────────────┐ │
│  │  MP4, MOV, MKV, WebM · up to 3 min each                                 │ │
│  └─────────────────────────────────────────────────────────────────────────┘ │
│                                                                              │
│  ⠿ ▣ Turbo Crunch '87        0:30   ━━━━━━━━━━ converting 62%      ⋯          │
│  ⠿ ▣ Station promo            0:15   Ready                         ⋯          │
│  ⠿ ▣ Laser tag arena          0:30   Ready                         ⋯          │
│  ⠿ ▣ Mall opening             1:00   Ready                         ⋯          │
│                                                     4 clips · 2:15 total     │
│  Order    [ Shuffle | In order ]                                             │
│  Repeats  Don't repeat a clip within [ 1 hour ▾ ]                            │
│ ▸ More options   (loudness match, trim silence at start and end)             │
│                                                                              │
│  Used on   Nocturne 7 · NEON 84                                              │
```

- **Uploads:** multipart with progress, exactly like pre-rolls. Each row shows a thumbnail, duration and conversion state. Clicking the thumbnail plays it in the existing `PreviewPlayer`.
- **Rows:** drag to reorder (the `SortableList` already used by `ProgrammePicker`). The row menu ⋯ holds Rename, Replace file and Remove.
- **Order and repeats:** shuffle is seeded and deterministic, so the guide and every cluster node agree. The repeat window works the same way as a channel's repeat-avoid setting.
- **Loudness match:** on by default. It normalizes each clip to the channel's level when the clip is converted, so the volume doesn't jump between ads.
- **Packs:** a reel exports in a pack along with its clips (§7), within the package size limits. Larger reels get a one-line warning in the export drawer.

### 3.5 Describe a channel

The first step of a new virtual channel (`TemplateGallery`) gains a prompt bar
above the template cards: **Describe a channel…**. Submitting opens the Describe
drawer:
- `size="xl"`;
- the top half is a live `WeekGrid` plus `Timeline` built from the streamed `channel_draft` results;
- the bottom half is `AgentPanel`.

This mirrors `DescribeDialog.tsx`. Examples:
- "A Saturday-morning cartoon network from the late 80s, with a creature feature at 11 pm."
- "Thursday-night film noir with Hitchcock in colour on Sundays."

The footer has Discard, **Open in editor** (a full channel drawer with the
draft), and **Create channel**. The agent can also propose a style; that appears
as a filmstrip under the grid with a **Use this style** chip.

### 3.6 Personal channels

**Creating one:** `TemplateGallery` gets a **Made for each viewer** card. It
creates a virtual channel with a `Personal` recipe. In that channel's drawer,
Programming is replaced by **Recipe**, and Schedule keeps the `WeekGrid`, now
with recipe-aware blocks.

```
│ Recipe                                             Preview as [Braedon ▾]   │
│                                                                              │
│  ▣ Continue watching          ━━━━━━━━●━━  more       [ ● ]                  │
│  ▣ Next episodes              ━━━━━━●━━━━             [ ● ]                  │
│  ▣ Picked for them            ━━━━━━━━━●━             [ ● ]                  │
│  ▣ From their watchlist       ━━━●━━━━━━━             [ ● ]                  │
│  ▣ Comfort rewatches          ━━●━━━━━━━━             [ ● ]                  │
│  ▣ New releases they'd like   ━━━━●━━━━━━             [   ]                  │
│  ▣ Your picks (collections)   [Choose…]               [   ]                  │
│                                                                              │
│  Pause when they leave  ⓘ                                     [ ● ]          │
│  Channel name   [{viewer} TV ▾]                                              │
│ ▸ More options  (repeat rules, max share per show, runtime limits at night)  │
```

**The recipe**
- Each ingredient card shows a still from the selected viewer's next pick.
- The slider sets its share.
- **Preview as** re-renders the hero timeline and the Style preview for that viewer: their name, their reasons ("Because you rated Annihilation 5★"), their watchlist trailer.
- Weekly blocks can name a lens: "Fridays 8 pm · Heist night · Picked for them, Crime". The block's `Name` shows on the viewer's guide.

**Pause when they leave** (on-demand mode): the channel's clock advances only
while that viewer is watching. When they come back they get the `welcome`
moment ("Welcome back. You were 31 minutes into Severance."), then they resume.

**Who gets one:** the channel's existing Viewers tab, unchanged. On the viewer
profile (owner rule: per-viewer settings live there), the Limits › Live TV
section gains one `SettingsRow`:
- **Personal channel** with a Switch, defaulting to the channel's audience;
- `MoreOptions` "Name" (override `{viewer} TV`).

Nothing else is added to the profile.

### 3.7 Packs: export and install

**Export:** from the channel drawer kebab (**Export as pack…**) or the style
drawer kebab. It opens a `Drawer size="md"`:

```
│ Export as pack                                                     │
│  Name      [Nocturne Late Night]     Version [1.0.0]               │
│  Author    [Braedon]                                               │
│  Includes                                                          │
│   ☑ Nocturne 7          channel · schedule, 4 blocks               │
│   ☑ Nocturne            style · 7 moments                           │
│   ☑ Assets              6 files · 3.1 MB                           │
│   ☐ Programming as smart rules instead of fixed titles  ⓘ          │
│  Never included: viewers, access, favourites, numbers, history.    │
│                                         [Cancel]  [Download pack]  │
```

**Install:** the existing plugin install flow (Plugins page, Marketplace, or the
Add › Channel pack shortcut that opens the same component). The install preview
renders the pack: live tiles of its styles, channel cards with tonight's guide
resolved on this server, and three plain status rows:
- "142 of 150 titles available here";
- "Numbers 7, 8 → 31, 32";
- "6 assets, 3.1 MB".

**Install** creates linked records and lands on Live TV with the new channels
highlighted (the Studio "install lands where it appears" rule).

**Pack records**
- Pack records show a `Badge` "From Nocturne Late Night 1.0".
- Programming, style and assets come from the pack and update with it.
- Number, on air, viewers and favourite stay the owner's.
- **Make it mine** (kebab) detaches a copy that the owner owns completely. The pack then stops updating that record.

---

The canonical JSON field names for `Channel.look`, `Channel.clock`, `Channel.personal`, `Channel.screens` and the viewer `personal` policy are in Appendix A, which supersedes §4.3 where they differ.

## 4. Server design

### 4.1 Package layout

| Package | Responsibility |
| --- | --- |
| `internal/onair` (new) | Styles and families (CRUD, validation, built-ins); the scene document model; bindings; the asset store; clip planning and cache; the render queue. Implements `core.ChannelGraphics`. |
| `internal/onair/raster` (new) | The pure-Go 2D renderer: scene document plus bindings plus t → `*image.RGBA`. It has no dependencies outside `x/image`, so it also compiles to WASM for the console (§6.3). |
| `internal/onair/encode` (new) | Pipes frames to the bundled ffmpeg and writes the master MP4 (and, in Phase 5, alpha overlays). |
| `internal/livetv` | Interstitials in `Schedule`, guide coalescing, the switcher and per-channel broadcast outputs (§4.5), personal rundowns and the on-demand cursor, a viewing log. |
| `internal/transcode` (additions) | The continuous encoder runner and the decoder runner (§4.5). |
| `internal/personalize` | `PersonalPicker` (picks with reasons, policy-filtered). |
| `internal/plugins` | The `data` runtime, `looks`/`virtualChannels`/`families`/`reels` contributions, pack export, link tables; the `onair_content` capability with `onairFeeds`/`onairSegments`, the feed fetch schedule and `onair_feed_values`. |
| `internal/agent`, `internal/agent/builtin` | The `/livetv/look` and `/livetv/describe` routes; the `look_draft`, `channel_draft`, `channel_summary` and write-proposal tools. |
| `internal/transcode` | An exported encode-slot API, and an overlay input on `Decision` (Phase 5). |
| `web/src/pages/livetv/onair/` (new) | The stage, the gallery, the style drawer, the Style tab, the recipe, the WASM renderer loader. |

### 4.2 Core contract (additive)

```go
// core/onair.go
type ClipRequest struct {
    LookID    string
    Moment    string            // ident, upnext, tonight, break, back, offair, welcome
    ChannelID string
    ViewerID  int64             // non-zero only for personal moments
    Bind      map[string]string // resolved slot values (titles, times, reasons)
    Art       map[string]string // slot → artwork reference (TMDB path or asset digest)
    Language  string
}
type ClipRef struct {
    Hash     string        // content address
    Duration time.Duration // a multiple of 4 s
    Path     string        // node-local master MP4 (1080p30 H.264 High, AAC 48 kHz stereo)
}
type ChannelGraphics interface {
    Duration(lookID, moment string) (time.Duration, bool) // from the template; no render, no I/O
    Clip(ctx context.Context, r ClipRequest) (ClipRef, bool) // cache lookup only; never renders
    Ensure(ctx context.Context, r ClipRequest)               // queue a render; returns immediately
}

type PersonalPick struct {
    Title  *Title
    Reason string // "Because you rated Annihilation 5★"
    Kind   string // continue, upnext, picked, watchlist, comfort, new
    Score  float64
}
type PersonalPicker interface {
    Picks(ctx context.Context, u *User, kinds map[string]float64, n int) ([]PersonalPick, error)
}
```

`livetv.Options` gains `Graphics core.ChannelGraphics`, `Picker
core.PersonalPicker`, `State core.State`, `Watchlist` (a narrow
`Items(ctx, u)`) and `Extras` (a narrow `List`/`Resolve`). Each is optional;
when one is nil, the features that need it are off.

### 4.3 Data model

Channel JSON fields (no migration needed, because channels are JSON blobs):

```go
type ChannelLook struct {
    LookID   string `json:"lookId"`
    Ident    bool   `json:"ident"`
    UpNext   bool   `json:"upNext"`
    Breaks   string `json:"breaks,omitempty"` // "", between, inside
    BreakEvery int  `json:"breakEvery,omitempty"` // minutes, 0 = per programme
    OffAir   bool   `json:"offAir"`
    Overlays string `json:"overlays,omitempty"` // "", whenTranscoding, always
    Tagline  string `json:"tagline,omitempty"`
    Accent   string `json:"accent,omitempty"`
    ReelID   string `json:"reelId,omitempty"`   // break reel; "" = look's break screen only
    BreakSeconds int `json:"breakSeconds,omitempty"` // target break length when a reel is set
}
type PersonalRecipe struct {
    Mix        map[string]int `json:"mix"`        // ingredient → share
    Collections []core.RatingKey `json:"collections,omitempty"`
    PauseAway  bool           `json:"pauseAway"`
    NameFormat string         `json:"nameFormat"` // "{viewer} TV"
    MaxPerShow int            `json:"maxPerShow,omitempty"`
}
// Channel gains: Style *ChannelLook; Personal *PersonalRecipe; Pack *PackLink (read-only)
```

Migrations:
- `onair` v1:
  - `onair_looks(id PK, data JSON, pack_id, updated_at)`;
  - `onair_families(id PK, data JSON, pack_id)` for pack families only (built-ins are embedded);
  - `onair_assets(digest PK, kind, name, mime, size, license, content BLOB, created_at)`. BLOBs live in the DB so every cluster node sees them; they are materialized to the node cache on first use;
  - `onair_copy(title_key, language, kind, text, created_at)`: cached loglines.
  - `onair_reels(id PK, data JSON, pack_id, updated_at)`: name, order, repeat window, loudness, clip order.
  - `onair_reel_clips(id PK, reel_id, name, duration_ms, source_digest, status, error, created_at)`. The original upload is kept as an asset BLOB; the converted segments are materialized to the node cache on first use, like clips (§4.6).
- `livetv` v5:
  - `livetv_viewing(channel_id, viewer_id, programme_id, moment, started_at, seconds, ended_by)`;
  - `livetv_personal(viewer_id, channel_id, day, rundown JSON, cursor_ms, built_at)`, registered with `RegisterViewerTable(..., ViewerMergeMove)`;
  - `livetv_pack_links(pack_id, kind, local_id, record_id)`.

### 4.4 Schedule and guide

- **Interstitials.** When `c.Style != nil` and a moment is on, `weeklySchedule` emits `Item{Key: 0, Slate: "upnext"|"break"|"back"|"tonight", DurationMS: Graphics.Duration(...)}` around real items. Lengths come from the template, never from a render, so `Schedule` stays pure.
  - `legacyCalendar` returns false when `Style` is set.
  - `emitCycle`'s rule that slates abort a cycle (`schedule.go:100`) is relaxed only for interstitials.
- **Placement, in order of preference:**
  1. **Gaps first.** With `Align` (hour or half hour), `fillGap` already makes dead air. Interstitials take that gap before filler or slate, so the guide doesn't move at all.
  2. **Inserted.** Without alignment, an interstitial adds a slot. Later programmes shift by its length. This happens only when the owner saves the channel. The save toast says "Guide times shift by up to 12 s per programme."
- **Guide coalescing.** `Guide()`, XMLTV and the Plex grid fold every interstitial into the preceding real programme's end time. Viewers never see 8-second programmes, and real programme IDs stay `<ch>-<startMs>` of the real start. Playout uses the uncoalesced list. Inserted mode changes guide times only, never by more than the interstitial's own length.
- **Off-air** keeps today's guide entry. Designed `offair` clips loop; analogue-static scenes composite a prepared foreground over the continuously running crisp Signal engine (§4.9).
- **Tune-in ident** is not a schedule entry. It is a session prefix (§4.5), so the guide is unaffected.

### 4.5 Playout: one continuous stream per channel

**Era pods:** every bumper and spot is an ordinary scheduled switcher input,
prepared by the existing background planner before its boundary. Programme
pieces resume through `Programme.Offset`, as clock-interrupted programmes do.
The broadcast guide coalesces resumed pieces and pod spans into the programme.
Pod boundaries preserve the unconditional 40 ms PCM anti-click ramp. This is
not a creative crossfade. `Moment.crossfade` requests a creative transition;
the continuous switcher owns visual and longer audio crossfades.


**Owner decision (2026-11-05).** Every virtual channel, and every IPTV channel with a can't-play screen enabled, plays as **one continuous stream** carrying every show, break, interstitial, ident and screen. There is no discontinuity, new playlist or MPD, retune or spinner at programme boundaries, on HLS, DASH and Plex for Mac's HTTP stream. This replaces the per-programme tracks, the three-track cap, clip tracks on the slate path and the per-session ident prefix described in earlier drafts.

**The channel output** (`livetv` "broadcast", one per channel × rendition × node):

```
schedule (wall clock) ─▶ input plan: programme · clip · screen · reel clip · plugin segment
                              │
 programme decoders ─────────┐│   (the current one, plus the next one prewarmed before the boundary)
 clip / screen frame rings ──┤▼
 reel / segment decoders ────┼─▶ Go switcher (frame sync, PCM, crossfade) ─raw─▶ encoder (rendition A) ─▶ segments ─┬─▶ viewer 1 playlist
                             │                                           └─raw─▶ encoder (rendition B) ─▶ segments ─┼─▶ viewer 2 playlist
 (IPTV: provider decoder) ───┘                                                    (only when a client needs it)     └─▶ viewer N …
```

- **Switcher.** One frame and its audio per output tick, paced to the wall clock. The input at each tick comes from the deterministic schedule: the programme at its offset, an interstitial clip, a reel clip, a plugin segment, or a screen. Programme changes land on the scheduled frame.
- **Programme decoders.**
  - A restartable ffmpeg decoder per input reads Ether's own playback input (`Playback.Prepare` / `InputURL`, as playout does today), starts at the programme's offset, and emits raw frames scaled to the channel's working size and frame rate, with PCM resampled to the channel layout.
  - The **next programme's decoder is prewarmed** from 20 s before the boundary: started, seeked to the programme start, and holding its first frames, so the cut lands on the exact frame.
  - Clips and screens are decoded once into in-memory frame and PCM rings.
- **Stalls.**
  - When the current input stops producing frames (a Usenet stall, a release that fails mid-play, a provider drop), the switcher holds the last frame for up to 1.5 s, then fills with the channel's can't-play screen and retries in the background (§4.9).
  - At a programme start whose release isn't playable, it uses `virtualCandidates` exactly as `tuneVirtual` does today, and fills with the screen while the next candidate prepares.
- **Encoders.**
  - One long-lived ffmpeg encoder per rendition, fed raw frames and PCM over loopback TCP (production is Windows).
  - Its codec, profile, level, resolution, frame rate, GOP and audio settings never change, and its timestamps are a monotonic frame clock.
  - Each encoder writes 4 s segments as TS for HLS and fMP4 for DASH, plus a continuous Matroska/TS byte stream when a Plex for Mac viewer is attached.
  - Hardware encode is used whenever `HWAccel` reports it.
- **Renditions.**
  - The channel starts with one rendition: 1080p30 H.264 High with AAC stereo when hardware encode is available, otherwise 720p30.
  - A second rendition (720p, or 480p when the first is 720p) starts only when a viewer's client needs it: a device ceiling, a bandwidth or remote-quality cap, or a Plex quality choice. The mapping goes through the existing `Decide` path. The rendition stops when its last viewer leaves.
  - Each extra rendition adds an encoder, not a decoder.
- **Fan-out.**
  - Every viewer of a non-personal channel reads the same rendition's segments. A viewer session is only a signed playlist view over the shared segment window (stream grant, access re-checks, stream-limit accounting as today).
  - Late joiners start at the live edge.
  - **Timeshift:** segments are kept on node-local disk for a DVR window, 30 minutes by default and set per channel under More options. A paused or rewound Plex client resumes within the window.
- **Lifecycle.**
  - The output starts on a channel's first viewer. The first frame is the channel's ident or screen, served from the ring immediately, while the programme decoder prepares. The first segment is ready within one segment duration (4 s); the target is first frame ≤ 1 s for a channel already running and ≤ 4 s cold.
  - The output stops after a **60 s idle grace** once the last viewer leaves.
  - `maintain()` prewarms releases as today for channels watched in the last 2 h.
- **Personal channels** are per viewer by nature. Each viewer's personal channel is its own output (its own switcher and encoder, one rendition), driven by that viewer's rundown (§4.7).
- **IPTV channels** are linear too. With a screen enabled and switching on, the provider decoder feeds a shared per-channel output, and the existing rule holds: one provider connection per channel, shared by all its viewers.
- **Cluster.** Outputs are node-local. Viewers on different nodes each attach to a node-local output for the channel. Every node computes the same schedule, so outputs on different nodes air the same programme at the same moment.

**Cost** (estimates; the slice gate measures them on the Mac lab and HYPERV03):

| | CPU | Memory | Disk |
| --- | --- | --- | --- |
| Shared channel, software x264 `veryfast`, 720p30 | ≈ 0.7–1.0 core per channel | ≈ 250 MB | DVR: ≈ 0.7 GB per 30 min at 3 Mb/s |
| Shared channel, software, 1080p30 | ≈ 1.5–2.2 cores per channel | ≈ 400 MB | ≈ 1.3 GB per 30 min at 6 Mb/s |
| Shared channel, hardware encode, 1080p30 | ≈ 0.3–0.5 core per channel, plus one GPU encode session | ≈ 350 MB | as above |
| Extra rendition | + the encode only (720p ≈ 0.4–0.6 core in software, ≈ 0.05 with hardware) | + ≈ 80 MB | + its DVR |
| Each additional viewer of a shared channel | ≈ 0.005 core | ≈ 1 MB | none |
| Personal channel, per viewer | the "shared channel" row above, per viewer | as above | DVR off by default |
| Today, for comparison | per-programme remux or encode **per viewer** | | |

The CPU figures include decode. Decode is the variable part: a 1080p H.264 release decodes in ≈ 0.1–0.2 core, but a 4K HEVC HDR release needs ≈ 1.5–3 cores plus tone mapping. Virtual channels therefore ask the resolver for releases at or below the channel's rendition (≤ 1080p SDR), using the existing quality profiles, and decode in hardware where available.

**Defaults.**
- Virtual channels always use the continuous output (owner decision).
- Renditions: one, sized by hardware availability; the second only on demand.
- DVR: 30 minutes. Idle grace: 60 s.
- Personal channels: 720p30 in software, 1080p30 with hardware encode.
- IPTV: §4.9 *When it applies*.

**What clients see.**
- **Plex:** the tune, decision, playlist and MPD keep their current shapes.
  - Playlists become one continuous sliding window: monotonic media sequence numbers, no discontinuity tags, `#EXT-X-PROGRAM-DATE-TIME` on every segment.
  - The tune response's media attributes now describe the channel rendition (H.264 / AAC at its size) instead of each programme's remux. That changes values, not shapes; it is a deliberate behaviour change for the owner's approval.
  - DASH is one Period for the life of the output. Plex for Mac's HTTP stream continues across programmes.
- **Jellyfin, Emby, M3U and Silo** get the same continuous stream through the sessions broker.

**Guide.** Unchanged from §4.4: interstitials are folded into the preceding programme, and real programme IDs are stable in gap mode.

### 4.6 Rendering

- **Job.** `onair.render(ClipRequest)`:
  1. resolve the template and tokens;
  2. bind the slots;
  3. hash `(renderer version, template, tokens, bindings, asset digests, encode profile)`;
  4. on a hit, return;
  5. otherwise render the frames and encode to `<data>/cache/onair/<hash>/`.
- **Frames.** Frames are a pure function of `t`, so the renderer draws them in parallel: `GOMAXPROCS/2` workers, niced, into a reorder buffer, then into `ffmpeg -f rawvideo -pix_fmt rgba -s 1920x1080 -r 30 -i - [-i sting] …`.
  - Output: `-c:v libx264 -profile:v high -level 4.0 -pix_fmt yuv420p -g 120 -keyint_min 120 -sc_threshold 0`, AAC 48 kHz stereo, `-movflags +faststart` into the master MP4.
  - Hardware encode is used when `HWAccel` reports one.
  - Silent clips get `anullsrc`, as pre-rolls do.
- **Slots.** Renders take the shared transcode slot limit. `acquireTranscodeSlot` is exported as `Manager.AcquireSlot(ctx, priority)`, so a viewer's transcode always preempts a background render.
- **When renders happen** (never on a request path):
  - **On style save:** each channel's `ident`, `offair` and `break`, and for personal styles a viewer-agnostic `welcome`.
  - **On reel upload:** each clip is converted once with the pre-roll pipeline (`prerolls/media.go` profile, plus loudness normalization to −23 LUFS when on, scaled to the live resolution, and segmented to 4 s TS and fMP4). It reuses the pre-roll limits: 3 minutes per clip, 500 MB per upload, plus a 5 GB reel budget set in **More options**.
  - **In `maintain()`** (every 30 s, per node): `upnext`, `back` and `tonight` for programmes in the next 3 h on channels watched in the last 2 h; the next 24 h for channels marked "Always ready"; personal moments for viewers active in the last 7 days.
- **Cache.** An LRU like `overlays/cache.go`, at `<data>/cache/onair`. The default budget is 2 GB, set in **More options**. Clips referenced by the next 3 h of any watched channel are pinned. A 6 s clip is about 3 MB at 1080p.
- **Cost guard.** Templates are validated with a cost estimate: at most 200 layers, 2,000 particles, a blur radius of 64, and 30 s. Owner edits that exceed it get an amber note on the Layers tab and are clamped.
- **Cluster.** Rendering is content-addressed and per node, so it needs no coordination. Each node renders what its own sessions need.

### 4.7 Personal rundowns

- The schedule isn't a pure function here, because viewer state changes. Each viewer's rundown is **materialized** for the guide horizon (48 h) into `livetv_personal`, built by `PersonalPicker` plus the recipe, the weekly blocks and repeat rules.
- **What's immutable:** the past, the now-airing programme, and anything within 30 min. Later slots are recomposed on state events (a finished episode, a watchlist change) at most once every 10 min per viewer. Then `bumpLineup` runs for that viewer only.
- **Every pick passes the safety checks** for that viewer: `Allowed(viewer, title)` (rating, blocked genres and keywords, share filter) and the not-interested list. Next-episode picks skip unreleased and already-watched episodes, as `genUpNext` does.
- **Pause when they leave.** The rundown's clock is `cursor_ms`, which advances only while the viewer has a session open on the channel. The guide shows the rundown from the cursor onward, anchored at now. Tuning in after more than 10 min away plays `welcome` first.
- **Lineup and name.** One channel ID; `AllowedChannels(user)` returns it with the formatted name ("Braedon TV") for that viewer only. Other viewers never see someone else's personal channel in their lineup.

### 4.9 Signal screens

**What renders the screens**
- **Live Signal engine.** Analogue static faithfully restores the owner-approved continuity demo (`cac0121d1`, `internal/onair/signal.StaticVideo`): neutral chroma, owner-selected detail 1.75 (3×3 destination-pixel grains from a base grain size of 6) changing at 15 Hz, native 30 Hz rolling bar, scanline modulation and rare tracking tears. Grains are written directly into the full negotiated frame: no low-resolution field, interpolation, Gaussian bloom or temporal EMA. The continuous channel clock drives the non-repeating picture, hiss and hum; there is no clip loop.
- **One static.** Cold-tune fill, Signal static screens, Nocturne off-air and every scene `noise` background use this generator. Prepared foregrounds composite over live static in the switcher. The console preview uses the same integer hash, grain grid, bar and tracking model at the native scene size. Designed cards retain their subtle film grain.
- **Other screens.** Modern card, bars, BRB and test card use prepared native-resolution plates with live phosphor effects. All are silent. Typography is prepared away from the frame clock.
- **Detail and efficiency (owner-approved 2026-11-05).** The owner approved the crisp static at detail 1.75 as the shipped default. Detail defaults to 1.75; all other picture and audio parameters retain the approved W18 defaults. Noise stays native-resolution and dynamic. Row modulation skips unchanged rows and uses a tight add loop only when the complete luma range proves clamps unnecessary; pixel comparisons against the unoptimised 1.75 generator are exact, and Engine.Render allocates zero times per frame. The temporary owner static lab is not shipped.
- **Bitrate.** The shared encoder retains its negotiated VBV ceiling and 20-frame x264 lookahead. Software grain tuning uses `aq-mode=2:aq-strength=0.8:psy-rd=1.0,0.15`; no blur or substituted texture hides entropy. Offline real-switcher samples record average/one-second peak bitrate and CPU at 1080p/720p.
- **Cache.** The crisp renderer version participates in the content hash, invalidating soft static masters. Live static never reads a looping master from that cache.

**Settings.** `config.LiveTV.Screens{OffAir, NoSignal string; StationID bool}` holds the owner default. `Channel.Screens{OffAir, NoSignal string}` is the per-channel override, where `""` follows the default. The style IDs are `card`, `static`, `bars`, `brb` and `testcard`.

**Off air** (virtual channels, scheduled gaps). The `offAir` slate plays the chosen style instead of the plain card. The guide is unchanged.

**Can't play** (IPTV, and virtual channels whose candidates all fail within the tune budget). This must be **seamless** (owner, 2026-11-05).
- **What the app must never see:** an `EXT-X-DISCONTINUITY`, a new playlist or MPD, a new session, a retune, or a buffering spinner.
- **What happens instead:** the stream keeps delivering segments the whole time. When the source stops, or never starts, the same stream's next segments carry the chosen screen. When the source returns, the same stream goes back to the real picture.

**Design: a continuous-output switcher** (chosen). Every IPTV session that can fall back runs:

```
provider ─▶ ffmpeg decoder ──raw frames + PCM──▶ ┌───────────────┐      ┌──────────────────────┐
             (restartable)                       │  Go switcher  │─────▶│ ffmpeg encoder       │─▶ HLS / DASH / HTTP
live Signal frames + PCM (non-repeating) ▶ │ (frame sync)  │ raw  │ (one per session,    │   (the same output
                                                 └───────────────┘      │  never restarted)    │    the whole time)
                                                                        └──────────────────────┘
```

- **The encoder** is started once per session with fixed output parameters: codec, profile and level, resolution, frame rate, pixel format, GOP, audio codec, layout and sample rate. They come from the session's negotiated contract, the same `slateInfo` / `Decide` path, so clients see exactly the shapes they get today. Its input is constant-rate raw video plus PCM, so its timestamps are a monotonic frame clock that can't jump.
  - Inputs reach ffmpeg over loopback TCP, because `ExtraFiles` pipes don't exist on Windows (production runs on Windows).
- **The decoder** turns the provider stream into raw frames already scaled to the output size and frame rate, and PCM resampled to the output layout (`-vf scale,fps`, `aresample=async=1`). It is restarted on every reconnect. It is the only process that holds the provider connection.
- **The switcher** (Go, in `internal/livetv`) is a broadcast-style frame synchroniser. It writes exactly one video frame per output tick and the matching audio samples, choosing the source frame when it's fresh. Ticks are paced to the wall clock.
  - **Stall detection.** If no decoded frame arrives for 1.5 s, or the decoder exits, or the provider closes or errors, the switcher repeats the last frame until the 1.5 s threshold, then switches to the screen. Audio crossfades over 40 ms. The output never gaps.
  - **Recovery.** A background retry (cooldown, tune gap, per-hour cap and provider-slot rules exactly as above) starts a new decoder. The first decoded frame is always a keyframe, because the decoder starts there. The switcher cuts back on the next tick, re-timed onto the continuous clock, with a 40 ms audio crossfade.
  - **Starting down.** A tune whose source is down from the start begins on the screen at the channel's last known contract (the cached probe from its last session or health sweep, otherwise 1080p30 H.264 / AAC stereo). It becomes the real feed later with the same continuity. If the feed arrives at a different size or rate, it is scaled into the fixed contract for the rest of the session.
- **The screens** come directly from the live Signal engine; prepared foreground plates are composited over the non-repeating generator, so filling costs no extra decoder.

**HLS, DASH and HTTP.**
- The encoder writes the session's output (HLS segments; fMP4 for Plex Web's DASH; a continuous Matroska/TS byte stream for Plex for Mac's HTTP path) from one uninterrupted stream.
- All three keep continuity with no reconnect, including Plex for Mac, which has no rollover today. A single endless output doesn't need any.

**Alternative ruled out: copy mode with a matching pre-encoded slate.**
- Splicing a slate into a stream-copied feed without a discontinuity requires the slate's H.264 SPS/PPS, timebase, GOP structure and AAC AudioSpecificConfig to be bit-identical to the provider's. RFC 8216 §4.3.2.3 requires `EXT-X-DISCONTINUITY` whenever encoding parameters change.
- Ether can probe the provider's parameters, but it can't reproduce a third-party encoder's SPS/PPS and VUI bit-exactly with x264.
- Providers also come back from an outage with different parameters: a restarted transcoder, a new resolution, or a backup input.
- Without the tag, real decoders (Apple VideoToolbox in particular) corrupt or stall on the SPS change. With it, the owner's requirement fails.
- Copy-mode splicing would therefore be seamless only in the special case where nothing changes, and that can't be guaranteed or detected in time. It is ruled out.

**Cost** (estimates to be measured as the slice gate). IPTV channels are linear, so the switcher output is **shared per channel**, like virtual channels (§4.5). The rows below are per channel, not per viewer, except the first:

| | CPU | Memory | Notes |
| --- | --- | --- | --- |
| Today, relay or remux | ≈ 0.02 core | ≈ 20 MB | No encode |
| Switcher, x264 `veryfast`, 720p30 | ≈ 0.5–0.7 core | ≈ 150 MB | Decode ≈ 0.1, encode ≈ 0.4–0.6, frame pump ≈ 0.05 (≈ 40 MB/s raw) |
| Switcher, x264 `veryfast`, 1080p30 | ≈ 1.2–1.7 cores | ≈ 250 MB | Frame pump ≈ 95 MB/s raw |
| Switcher, hardware encode (NVENC, QSV, VAAPI, VideoToolbox) | ≈ 0.15–0.25 core | ≈ 200 MB | Uses an encoder session on the GPU |
| Session already transcoding (`Channel.Transcode`, or the client needs it) | ≈ +0.1 core over today | ≈ +60 MB | The encode already exists; only the decoder split and the pump are added |

**When it applies (IPTV).** A shared output makes the cost per channel being watched, not per viewer. Software encoding still costs roughly 0.7–1 core per watched channel against ≈ 0.02 for relay, so IPTV uses the switcher only where a can't-play screen is enabled.
- The owner default **When a channel can't play** gets one more option, **Apply to**:
  - **Streams Ether already converts**: the default; it costs almost nothing.
  - **Every stream**: adds encoding per viewer. The ⓘ gives the measured cost and how many simultaneous viewers this server can encode (from the transcode benchmark).
- Hardware encode is used whenever `HWAccel` reports it.
- Sessions without a screen (owner default **Show an error**, or a relayed stream under **Streams Ether already converts**) keep today's relay path and today's error on failure.

**Virtual channels** use the same switcher for their whole stream (§4.5). Their stalls and failed programme starts fill with the can't-play screen the same way.

**What the Plex app sees.** With a screen enabled, a failed or dropped source never produces an error, a retune or a spinner. The picture goes to the screen within about 1.5 s of the source stopping, and back to the real feed about 1 s after the retry reconnects. The guide and Now Playing still show the scheduled programme.
- **No Plex response shapes change.** The tune response, decision, playlists and MPD are the same shapes as any successful tune today. The one deliberate behaviour change is that a tune that fails today with HTTP 503 now succeeds.
- Jellyfin, Emby and M3U clients see the same through the sessions broker.

**Lasting tests.**
- A fake source that stalls for 20 s and recovers produces a continuous segment sequence on HLS and DASH: no discontinuity tags, monotonic PTS and DTS, constant codec parameters, continuous media sequence numbers.
- A real-ffmpeg check decodes the whole captured output straight through (`ffmpeg -xerror -i … -f null -`) with no errors, timestamp warnings or parameter changes. It is skipped when ffmpeg isn't installed.

- **Diagnostics are unchanged.** Source health, cooldowns and failure logs record every failed attempt exactly as now, and Activity shows the session as "No signal, retrying".

### 4.8 AI

- **`look_draft`** (route `/livetv/look`).
  - Input: a prompt, plus a channel summary (top genres, decades, sample titles, block names).
  - Output: style tokens (family, palette roles, type pairing from installed faces, energy, textures, tagline, on-air words).
  - It is validated: palette contrast, known faces, a 40-character tagline. Then it is rendered into the stage.
  - One retry with the validation error, as `collections/generate.go` does.
- **`channel_draft`** (route `/livetv/describe`).
  - Prompt → `Channel` with blocks, sources (smart filters, collections, TMDB lists), timezone, alignment, and an optional style draft.
  - Previewed through the existing `/livetv/preview`.
- **Loglines and block names** are generated once per title and language into `onair_copy` when a clip is first planned, never at render time. Without a language model, the TMDB overview is used, trimmed to one sentence.
- **MCP coverage:** `livetv_look_draft` and `livetv_channel_draft` (read); `livetv_look_save`, `livetv_channel_look` and `livetv_channel_create` (write proposals); `livetv_pack_export` (returns a console link). The tool counts in `builtin_test.go` and `app/mcp_test.go` are bumped. Ask Ether suggestions for `/livetv` gain "Give Nocturne a late-night style".
- **Announcer voice** (Phase 5) needs a speech-synthesis client on the one Language model integration (`config.LLM.Speech*`). It is off by default and generated once per clip text.

---

## 5. Scene documents (templates)

Templates are data, so every part of them can be edited in the console and
shipped in packs, and none of it runs code.

```jsonc
{
  "v": 1,
  "moment": "upnext",
  "duration": 8,                       // seconds, multiple of 4
  "poster": 3.2,                       // still frame time for tiles
  "slots": ["next.title", "next.time", "next.meta", "next.logline", "next.backdrop"],
  "layers": [
    { "id": "bg", "type": "art", "src": "{next.backdrop}", "fit": "cover",
      "scale": { "keys": [[0, 1.06], [8, 1.13, "linear"]] }, "tint": "{accent}", "tintBlend": "multiply" },
    { "id": "scrim", "type": "rect", "w": 1920, "h": 1080,
      "fill": { "linear": [0, 0, 1440, 0], "stops": [[0, "{bg}@0.95"], [1, "{bg}@0"]] } },
    { "id": "kicker", "type": "text", "text": "{words.upNext}", "font": "{type.mono}", "size": 44,
      "x": 140, "y": 250, "fill": "{accent}", "track": 12, "behave": [{ "typewriter": { "start": 0.3, "step": 0.07 } }] },
    { "id": "title", "type": "text", "text": "{next.title}", "font": "{type.display}", "weight": 900,
      "size": 128, "maxWidth": 1150, "fit": "wrap", "maxLines": 2,
      "x": 135, "y": 440, "fill": "{text}", "reveal": { "wipe": "ltr", "from": 0.9, "to": 1.9, "ease": "inOut" } }
  ]
}
```

- **Layer types:** `rect`, `ellipse`, `path` (SVG path data), `text`, `image` (asset), `art` (programme or channel artwork), `logo`, `blob` (seeded organic shape), `particles` (seeded emitter), `gradient`, `noise`, `grid` (perspective), and `group` (transform, clip, opacity, repeat).
- **Properties:** a constant, keyframes `[[t, v, ease], …]`, or a token or slot reference (`{accent}`, `{next.title}`, with an optional `@alpha`).
  - Eases: `linear`, `in`, `out`, `inOut`, `back`, `elastic`, `spring`.
  - The energy token scales the overshoot and the amplitude of the `back`, `elastic` and `spring` eases, plus behaviours.
- **Behaviours:** `typewriter`, `flicker`, `wiggle`, `bounce`, `glint`, `chroma`, `drift`, `pop`, `squash`. Each has a few typed parameters and is seeded and deterministic.
- **Post effects:** `grain`, `scanlines`, `vignette` and `flicker`, driven by the style's texture tokens.
- **Blend modes:** `normal`, `multiply`, `screen`, `overlay`. **Filters:** Gaussian `blur` and `glow`.
- **Validation:** a JSON Schema at `schemas/onair-scene.v1.json` plus the §4.6 cost estimate. Unknown fields are rejected.
- **Fixed size:** documents are 1920×1080. Other resolutions are rendered by scaling.

The sample reel's five brands become the first five families: the same
designs, rebuilt as documents.

---

### 5.1 Broadcast overlay preparation

Bugs use title safe: 10% inset on every edge (192/108 at 1080p, 128/72 at 720p).
Medium occupies 288×100 reference pixels; small is 75%, large 130%. Wordmarks
use the display face, weight 600 and two pixels of tracking. Channel artwork
precedes style artwork. Monochrome uses alpha for transparent artwork, luminance
for opaque artwork (inverted for light backing), and a faint soft shadow.
The mask, raster and compact YUV planes are prepared once per channel/style/size;
frame ticks only alpha-blend compact bounds with a scalar fade.
Scene image `tintBlend: "silhouette"` colours the alpha mask; the TS preparer uses
the same alpha/luminance rule. Missing artwork falls back to a wordmark.
Break and style boundaries use 500ms smoothstep ramps. Credit fades use prepared
media chapters whose names include “credit”; without cues the bug stays visible.
Lower thirds stay within title safe with dark-to-transparent panels and a shared
fade envelope. Idents retain their family design with 400ms boundary fades.

## 6. Rendering engine

### 6.1 Primitives

`raster` implements exactly what §5 needs on `x/image/vector` and
`x/image/font/opentype`:
- **Fills:** paths (moveTo, lineTo, quad, cubic, arc), even-odd and non-zero fill, and strokes with joins and caps (stroked path → fill).
- **Gradients:** linear and radial, with premultiplied RGBA.
- **Compositing:** clipping by a path mask; the blend modes per pixel; separable Gaussian blur (three box passes); glow (blur plus screen).
- **Text:** shaping limited to kerning and tracking. Arabic and Hebrew right-to-left and CJK use the bundled Noto fallback, with no complex shaping in v1, and the docs say so.
- **Images:** decoded once per job and sampled bilinearly with transforms.

### 6.2 Fonts

Fonts are embedded with `go:embed` under `internal/onair/fonts`, all SIL OFL,
subset to Latin plus Latin Extended plus Greek and Cyrillic, about 4 MB in
total:
- Inter, Space Grotesk, Fraunces, Playfair Display;
- Bungee, Fredoka;
- Special Elite, Monoton, Orbitron, VT323;
- Archivo Black, IBM Plex Mono;
- Noto Sans.

Noto Sans CJK is downloaded on demand into `<data>/tools/fonts` the first time
a style renders CJK text. The console loads the same files through
`GET /ether/api/onair/fonts/{id}`. It is the first font loading in the console,
and it's scoped to the On Air screens. Licences are listed in
`THIRD_PARTY_NOTICES.md`.

### 6.3 One renderer, two places

`raster` compiles with `GOOS=js GOARCH=wasm` into `onair.wasm`, lazy-loaded
only by the On Air screens. The browser renders into an `ImageData` at
960×540, or 1920×1080 in fullscreen. The console and the server therefore share
one implementation and preview what airs exactly, with no second renderer to
drift.
- **Gate (Phase 0):** at least 45 fps at 960×540 for every built-in family in Chrome on an M-series Mac, and at least 30 fps on a mid-range Windows laptop.
- **If the gate fails:** fall back to a TypeScript Canvas2D port, with golden-frame parity tests against the Go renderer ([§13](#13-owner-decisions-needed), decision 4).

### 6.4 Performance gates

- **Server:** a 6 s 1080p30 clip renders in 6 s or less with 4 workers on the HYPERV03 production box. On the Mac lab: 1.5× real time or better.
- **No request path renders or reads a template.** `Duration()` is served from memory.
- **Tune with a cached ident:** first frame within 1 s, measured in the perf harness. Channels without a style don't regress against today.

---

## 7. Channel packs as plugins

- **Package:** the normal `.etherpkg` (`manifest.json` plus files, with a digest).
- **Runtime.** The new `ether.runtime.kind: "data"` has no entry or module, and nothing is executed. `Validate` allows it only when the manifest contributes nothing but `looks`, `families`, `reels` and `virtualChannels`.
- **Permissions.** None are required. The install permission sheet reads "Adds channels and styles. Runs no code."
- **Contributions:**

  ```jsonc
  "ether": {
    "protocol": 1, "kind": "extension", "runtime": { "kind": "data" },
    "capabilities": ["livetv_pack"],
    "contributes": {
      "looks":           [{ "id": "nocturne", "file": "looks/nocturne.json" }],
      "families":        [{ "id": "late-night-2", "file": "families/late-night-2.json" }],
      "reels":           [{ "id": "saturday-ads", "file": "reels/saturday-ads.json" }],
      "virtualChannels": [{ "id": "nocturne-7", "file": "channels/nocturne-7.json" }]
    }
  }
  ```
- **Assets:** stored as `assets/<sha256>.<ext>` and referenced by digest from the documents. They use the existing 64 MiB entry and 128 MiB total limits.
- **Portable programming.**
  - Items are exported as TMDB IDs plus media type, with season and episode where they apply. They're resolved on import, and unavailable titles are reported, never silently dropped.
  - Collections are exported as their smart rules (`core.Filter`). Hand-picked collections become fixed lists.
  - Viewers, policies, numbers, favourites, history and source credentials are never exported.
- **Install:** `PreviewInstall` renders the §3.7 preview. `Install` creates records through `livetv_pack_links`, like `plugin_collection_links`.
- **Updates:** when an update arrives, pack-owned fields are replaced and owner fields (number, enabled, viewers, favourite, chosen style) are kept.
- **Undo:** installs and updates go into the undo journal, as `collections.upsert` does.
- **Make it mine:** copies the record into an owner record with no link. Used styles, families and reels are copied with it.
- **Export:** `POST /ether/api/onair/packs/export` streams a `.etherpkg` built by a new `kit.PackData`, which shares digest code with `kit.Pack`. Packs share through the Marketplace by the existing route: a signed catalogue entry pointing at a GitHub release. **Share to Marketplace** in the export drawer is a later addition.
- **Families from packs** appear in the new-style chooser with the pack badge.
- **Dynamic content** (weather, headlines, scores) comes from code plugins, not packs. See §7a.

---

## 7a. Plugin content for channels

Era commercials are owner-imported reel clips, not plugin-provided executable
content. Packs can carry the additive break-pattern and moment asset references
as normal JSON; dynamic plugin segment scheduling remains independent.


User-authored plugins can feed live content into virtual channels. A weather
plugin, for example, can put a two-minute forecast on the :58 of every hour, in
the channel's own style. The code lives in Studio, where Ether's only code editor
is; the channel and the style stay no-code.

### What a plugin can contribute

A new capability `onair_content` is declared by `kind: extension` with a hosted
runtime. A plugin can contribute either or both of these:

1. **Feeds:** named data that any template can bind to, like built-in slots. `weather/now.temp`, `weather/now.icon`, `weather/today.summary`, `headlines/items[0].title`.
   - The plugin declares each field's type (text, number, image, list) and a sample value.
   - The binding picker ⌁ in the Layers tab lists them under the plugin's name, with their live values.
2. **Segments:** whole programme pieces, such as "Local forecast · 2 min" or "Top headlines · 1 min".
   - A segment is a scene document (§5), shipped as a file in the plugin package, plus the feed it binds to.
   - Its duration is fixed and declared in the manifest (a multiple of 4 s), so `Schedule` stays pure.
   - Segment templates are written against style tokens (`{accent}`, `{type.display}`, `{bg}`), so one weather plugin automatically matches Nocturne, Meridian and BLORP!.
   - Owners can restyle a segment per style in the Layers tab, like any built-in moment, and **Reset moment** returns it to the plugin's version.

```jsonc
"ether": {
  "kind": "extension", "runtime": { "kind": "hosted", "entry": "dist/index.js" },
  "capabilities": ["onair_content"],
  "permissions": { "network": ["api.open-meteo.com"] },
  "schedule": "15m",
  "settings": { "$ref": "settings.json" },          // e.g. location, units
  "contributes": {
    "onairFeeds":    [{ "id": "now", "fields": { "temp": "number", "icon": "image", "summary": "text" } }],
    "onairSegments": [{ "id": "forecast", "name": "Local forecast", "duration": 120,
                        "file": "segments/forecast.json", "feeds": ["now", "week"] }]
  }
}
```

### How data flows

Data never reaches a request path.
- **Fetching.** The plugin's `handle({op: "feed", id})` runs on the manifest `schedule` (at least 15 minutes) through the existing runtime, network allowlist and host services. It returns JSON only, validated against the declared field types.
  - Text is plain, with no markup.
  - Images come back as bytes (capped at 2 MB, decoded under bounds) or as references to the plugin's packaged assets.
  - Lists are capped at 50 items.
- **Storage.** The last good value of each feed is stored in `onair_feed_values(plugin, feed, data JSON, fetched_at)`, with a `maxAge` from the manifest.
- **Rendering.**
  - A segment's clip hash includes the feed version, so new data means a new render.
  - Segments are rendered close to air time: the `maintain()` loop renders a segment due in the next 15 minutes with the freshest data.
- **Stale or failed data.** If a fetch fails, the last good data is used while it's within `maxAge`. Past that, the segment is skipped like any missing clip and the slot falls back to filler, the reel or slate. Repeated failures go through `faillog`, and the plugin's health shows on the Plugins page as it does today.
- **Cluster.** Feeds are fetched by one node (`cluster: leader` in the manifest, already supported) and stored in the shared DB. Every node renders segments from the same stored values, so clips stay content-addressed and identical.
- **Per viewer** (later). v1 feeds are household-wide. Viewer-scoped feeds, such as "your local weather" on a personal channel, would need the existing `viewers.identity` permission and per-viewer values.

### Where it shows up in the console

- **Channel Schedule tab.** A block's programming source picker gains a **From plugins** group, for example "Local forecast · Weather · 2 min". Owners place a segment:
  - in a block (a "News at 6" block made of segments);
  - on a repeating clock slot, using new **On the clock** rows: "Local forecast at :58 every hour, 6 am to 11 pm";
  - inside breaks, through the Style tab's **During breaks** choices (Style's break screen, Clips from a reel, Plugin segments).
- **Style drawer.** Installed plugin segments appear in the moments filmstrip after the built-in moments, marked with the plugin's icon. They preview with the plugin's live feed values, or its sample values before the first fetch. Feed fields appear in the Layers binding picker.
- **Studio.** New starter templates **Channel segment** and **Data for channel graphics**. Studio's preview pane uses the same `Stage` component, so a builder sees their segment playing in a real channel's style on real data while they write it ("Make me a weather segment for Toronto" → a forecast playing in Nocturne's style within a minute). Install lands on the channel Schedule tab with the segment highlighted.
- **Plugins page.** The plugin's drawer lists where its segments air ("Nocturne 7, hourly at :58"). Uninstalling asks first and names those channels. Their slots fall back to filler.

### Safety and limits

- Plugins only ever return data and declarative documents. Ether does the drawing and the encoding, so a plugin can't stream arbitrary video into a virtual channel. Plugins that want to provide whole live streams already can, as a Live TV source (`livetv_channels`).
- Segment documents pass the same schema and cost validation as any template (§4.6).
- Feed text is length-capped per field and rendered through the same Fit rules, so a long headline can never overflow.
- Segments respect channel and viewer content policy like any other programme. A plugin segment is treated as unrated unless it declares a rating; a channel with a rating cap shows it only if the owner allows unrated segments on that channel.

## 8. HTTP API (owner)

All routes are registered with MCP metadata (or a written exclusion) and get
mock-server handlers in `web/src/mock/livetv.ts`.

| Route | Purpose |
| --- | --- |
| `GET/POST /onair/looks`, `GET/PUT/DELETE /onair/looks/{id}` | Style CRUD. `PUT` triggers background renders. |
| `GET /onair/families` | Built-in and pack families, with their documents. |
| `POST /onair/looks/{id}/duplicate`, `POST /onair/looks/{id}/detach` | Duplicate a style; Make it mine. |
| `GET /onair/bindings?channel&at&viewer&moment` | Resolved slot values for previews (titles, times, reasons, artwork URLs). |
| `POST /onair/clips/render` (`look, moment, channel, at, viewer`) | Renders one real clip on demand and returns an HLS URL: the **Render real clip** button. |
| `GET/POST/DELETE /onair/assets[/{digest}]` | Asset upload (multipart, with progress like pre-rolls), serving and delete. |
| `GET /onair/fonts/{id}` | Font files for the console. |
| `GET/POST /onair/reels`, `GET/PUT/DELETE /onair/reels/{id}` | Break reel CRUD. |
| `POST /onair/reels/{id}/clips`, `PUT/DELETE /onair/reels/{id}/clips/{clip}`, `GET …/clips/{clip}/preview` | Clip upload (with progress), rename or replace, delete, preview playback. |
| `GET /onair/renderer.wasm` | The WASM renderer, immutable-cached by version. |
| `POST /onair/packs/export` | Builds a pack and returns the download. |
| `GET /livetv/channels/{id}/rundown?viewer` | A personal channel's rundown, for the drawer preview. |
| `GET /livetv/stats?channel&days` | Viewing totals (Phase 5 impact). |

There's no new viewer route; viewers see results through their apps.

---

## 9. Console implementation notes

- **Files:** under `web/src/pages/livetv/onair/`:
  - `LooksGallery.tsx`, `LookDrawer.tsx`
  - `DesignPanel.tsx`, `LayersPanel.tsx`, `AssetsPanel.tsx`, `DescribeLook.tsx`
  - `Stage.tsx` (canvas, transport, filmstrip, hit testing), `Timeline.tsx`
  - `LookTab.tsx` (channel drawer), `RecipeTab.tsx`
  - `renderer.ts` (WASM loader with an `OffscreenCanvas` worker), `model.ts` (the `onair` API client).
- **Stage rendering** runs in a Web Worker with `OffscreenCanvas`, so scrubbing never blocks React. Tiles share one worker and render stills on idle, caching them by style hash in memory.
- **Drafts:** `useDraft(saved)` for the style. Every edit is a pure document patch, which gives undo/redo for free (a patch stack). `useUnsavedGuard` covers drawer close.
- **Motion and accessibility:** AppKit motion tokens for UI chrome. `prefers-reduced-motion` stops tile autoplay and keeps the stage paused at the poster frame until Play. Keyboard: space, ←/→, ⇧←/→ (1 s), K (keyframe), ⌘D (duplicate layer), Delete.
- **Navigation:**
  - Live TV's Go descriptor sections (`internal/app/extend_console.go` `consolePages()`) gain `looks`.
  - `settings-index.ts` and `sitemap.ts` gain Styles and Personal channel entries.
  - Ask Ether suggestions are added for `/livetv/looks`.
- **New AppKit pieces (AppKit first):** `ColorPicker` (popover), `PaletteEditor` (role swatches), and `Timeline` primitives if they're generic enough. They are upstreamed to `appkit-ui` and consumed from the release.
- **Review:** screenshots for every new surface at 1440×900, light and dark, plus 390 px, against the mock console. The tiles and the stage are also recorded as short videos for the owner. Owner screenshot approval comes **before** deep build (the Studio rule).

---

## 10. Phases

Each phase ships on its own and leaves Ether better than it was.

**Playout slices** (replacing the per-programme track work):

| Slice | Owner | Delivers |
| --- | --- | --- |
| P1 | W4 | `Schedule` interstitials (gap and insert), guide coalescing, `core.ChannelGraphics` contract. Kept from W4's first slice; its clip-track, track-budget and ident-prefix code is dropped before merge. |
| P2 | new worker | `internal/transcode`: the **continuous encoder runner** (raw video + PCM over loopback TCP; fixed contract; HLS TS + fMP4 DASH + continuous byte-stream outputs; renditions; hardware encode; slot accounting) and the **decoder runner** (playback or provider input → raw frames at a size, rate and offset, plus PCM; restartable; prewarm). Interface-first, so P3 builds against fakes. |
| P3 | W4 | `internal/livetv`: the **switcher** (wall-clock frame sync, schedule-driven input plan, prewarm at boundaries, stall fill, PCM crossfade, clip and screen rings, provider-slot retries) and the **broadcast** (per channel × rendition × node; fan-out to viewer playlists; live edge; DVR window; first-viewer start, idle stop; personal channels per viewer). |
| P4 | W4 (after P2 and P3) | Wiring into Plex tune / decision / HLS / DASH / HTTP and the sessions broker for Jellyfin, Emby, M3U and Silo. Deletes the per-programme track code. Continuity and real-decoder tests. Measured CPU, memory and disk per channel and per viewer on the Mac lab (and HYPERV03 through the coordinator). |

| Phase | Scope | Exit criteria |
| --- | --- | --- |
| **0 · Spike** (1 week) | `raster` with paths, gradients, text, blur and blend; one family as a document; Go render benchmark; WASM build in a throwaway stage. | §6.3/§6.4 gates measured on HYPERV03 and the Mac lab. Go/no-go on WASM ([§13](#13-owner-decisions-needed), decision 4). |
| **1 · Styles foundation** | `onair` package, built-in Clean family plus the five reel families, style CRUD, render cache, **continuous channel output** (§4.5: switcher, shared encoders, fan-out, renditions, DVR window), ident and off-air as switcher inputs, Styles sub-tab plus gallery, style drawer Design tab, channel Style tab (Ident, Off-air). | Looked off-air plays on HLS and DASH. Tune-to-first-frame ≤ 1 s with an ident. Owner screenshot approval. |
| **2 · Moments** | Interstitials in `Schedule` (gap and inserted), guide coalescing, `upnext`/`back`/`break`/`tonight`, **break reels** (upload, conversion, reel drawer, break contents in the Style tab), Layers tab, Assets tab, Describe a style, DASH clip encode. | Guide IDs stable in gap mode. 24 h soak on the lab with no stalls. Owner approval of the designer. |
| **2b · Signal screens** | Signal family (card, static, bars, brb, testcard) with synthesized audio; owner default and per-channel pickers; off-air styles; can't-play screens with a continuous-output switcher (decoder → Go frame sync → one long-lived encoder) so a stalled, failed or recovering source never interrupts the stream on HLS, DASH or HTTP; background retry inside the connection limit. | Provider outage simulated on the lab: Apple TV, Plex Web and Android show the screen with sound and then the real feed, with no discontinuity, spinner or reconnect; continuity and real-decoder tests green; the account limit never exceeded; per-session cost measured on the lab and HYPERV03. Owner approval of the pickers. |
| **3 · Personal channels** | `PersonalPicker`, recipe tab, rundowns, on-demand cursor, `welcome`, watchlist trailers in breaks, viewer profile row, per-viewer lineup naming. | Tested on two viewers on Apple TV and Plex Web, with policy-filter checks. |
| **4 · Packs and describe-a-channel** | `data` runtime, contributions, links, export drawer, install preview, Make it mine, `channel_draft` describer. | Export → import round-trip on a second server reproduces the channel and style. |
| **4b · Plugin content** | `onair_content` capability, feeds and segments contributions, `onair_feed_values`, segment placement (blocks, On the clock, breaks), binding picker, Studio templates and stage preview, one sample weather plugin built in Studio. | A Studio-built weather plugin airs hourly in two different styles on the lab, survives a feed outage without stalling, and uninstalls cleanly. |
| **5 · Extras** | Corner logo and lower thirds (alpha overlay through the transcode chain), offline black/silence break-point analysis enabling "Inside programmes", announcer voice, viewing impact on the style drawer. | Burn-in only when allowed; impact numbers are truthful. |

v0.3.4 already ships the plain off-air card, so Phase 1 starts from a working slate path.

---

## 11. Tests (lasting ones only, per the test budget)

- **Schedule determinism with interstitials:** two overlapping windows return the same programmes. Gap mode leaves real programme IDs unchanged. Coalesced guide equals the guide without a style in gap mode.
- **Renderer goldens:** one frame at 3 times for each built-in moment, compared within a perceptual tolerance. These run in Go CI.
- **WASM parity:** a single Go test that runs `raster` under the same inputs used by the console loader smoke test.
- **Pack round-trip:** export, install into a fresh store, and the records are equal apart from IDs, numbers and links (styles, channels and reels).
- **Reel breaks:** the selected clips are the same for overlapping windows, respect the repeat window, and never cut a clip.
- **Plugin feeds:** malformed or oversized feed output is rejected field by field, and a stale feed past `maxAge` skips its segment without affecting the schedule.
- **Personal policy:** no pick exceeds the viewer's rating cap or blocked genres, including trailer breaks.
- **Continuity:** a channel output across programme boundaries, interstitials, a stalled input and a recovery produces one continuous segment sequence on HLS and DASH (no discontinuity tags, monotonic PTS/DTS, constant codec parameters, contiguous media sequence), and a real ffmpeg decodes the captured output straight through.
- **Fan-out:** two viewers attached at different times to one channel output read identical segments for the same media sequence numbers.

Everything else is verified once by hand and recorded in the acceptance doc.

---

## 12. Out of scope

- **Splicing into IPTV streams.** It's third-party content, and its timing is unknown.
- **AI image or video generation.** Artwork comes from TMDB and owner assets.
- **Recording.** Ether has no DVR.
- **Mid-programme breaks before Phase 5.**
- **Clips on the Plex for Mac HTTP/Matroska path,** which has no programme rollover.
- **Generated commercials or any other AI-produced media.** Break reels play only what the owner uploads.

---

## 13. Owner decisions needed

1. **Protected Live TV page.** May I add a **Styles** sub-tab to the aside, a **Style** tab to the virtual channel drawer, and **Style** / **Channel pack** to the Add menu? Nothing else changes. (Recommended: yes.)
2. **Break placement default.**
   - Recommended: *gaps only*, so the guide never moves. Breaks then need the channel to use alignment; otherwise the Breaks control explains that.
   - Alternative: *insert* slots, so breaks work everywhere but guide times shift a few seconds per programme.
3. **Where packs install.**
   - Recommended: the plugin install flow only (Plugins page and Marketplace), with Live TV's Add › Channel pack opening that same drawer. It's one install path with two doors.
   - Alternative: Plugins only, with no Live TV door.
4. **Preview renderer.**
   - Recommended: compile the Go renderer to WASM (one renderer, exact parity), if Phase 0 meets the frame-rate gate.
   - Fallback: a TypeScript renderer kept in parity by golden tests.
5. **Personal channel name default:** "{viewer} TV", or the viewer's choice of name later through a viewer app?
6. **AppKit additions:** `ColorPicker` and `PaletteEditor` upstreamed to `appkit-ui` first. (Recommended: yes.)
7. **Font bundle:** about 4 MB added to the binary for 13 OFL families, with CJK downloaded on demand. (Recommended: yes.)
8. **Unrated plugin segments** on channels with a rating cap: hidden unless the owner allows them per channel (recommended), or always allowed?
9. **Can't-play screens:** tunes that fail today with HTTP 503 succeed, and the stream continues seamlessly on the chosen screen until the source returns (continuous-output switcher, §4.9). Plex response shapes don't change; only the outcome does. (Recommended: approve, keeping **Show an error** as an option.)
9a. **Where the switcher runs:** **Streams Ether already converts** by default (near-zero cost), with **Every stream** as an opt-in that adds about 0.5–0.7 core per 720p viewer in software, or about 0.2 with hardware encode. (Recommended.)
9b. **Virtual channels on the switcher:** decided by the owner on 2026-11-05: in v0.3.5, one continuous shared output per channel (§4.5).
9c. **Continuous-output defaults:** one rendition sized by hardware availability, a second only on demand, a 30-minute DVR window, a 60 s idle grace, and personal channels at 720p30 in software or 1080p30 with hardware encode. The tune response's media attributes describe the channel rendition. (Recommended.)
10. **Screen defaults:** Modern card for both situations. Colour bars and the test card are silent (owner decision after the lab review; no tone option).
11. **Release:** v0.3.5 (decided by the release coordinator, 2026-11-05).

---

## Appendix A. Canonical channel schema (2026-11-05)
This supersedes spec §4.3 where they differ. The JSON names are camelCase and
identical in Go (`internal/livetv`) and TypeScript (`web/src/pages/livetv/onair/model.ts`).

## Channel.style (`ChannelLook`, optional; null follows an explicitly configured default style; no default preserves existing behaviour)

| JSON | Type | Values / default |
| --- | --- | --- |
| `lookId` | string | style id |
| `ident` | bool | ident when someone tunes in |
| `upNext` | bool | up next before each programme |
| `placement` | string | `gap` (default) \| `insert` |
| `breaks` | string | `""` (off) \| `between` \| `inside` |
| `breakEvery` | int | minutes; 0 = once per programme |
| `breakContent` | string | `look` (default) \| `reel` \| `segments` |
| `reelId` | string | when `breakContent` = `reel` |
| `segmentIds` | string[] | plugin segment ids (`plugin:<id>/<segment>`) when `breakContent` = `segments` |
| `breakSeconds` | int | target break length: 30, 60, 120 or 180 |
| `offAir` | bool | off-air screen |
| `overlays` | string | `""` (off) \| `whenTranscoding` \| `always` |
| `allowUnratedSegments` | bool | default false; only shown on rating-capped channels |
| `tagline` | string | per-channel override |
| `accent` | string | per-channel override, `#rrggbb` |

## Channel.clock (`[]ClockSlot`, optional; plugin segments "on the clock")

`{ "segmentId": "plugin:weather/forecast", "minute": 58, "everyMinutes": 60, "from": 360, "until": 1380, "days": [] }`

`from` and `until` are minutes after midnight in the channel timezone. Empty `days` means every day (Go weekdays, 0 = Sunday).

## Channel.personal (`PersonalRecipe`, optional; present = personal channel)

| JSON | Type | Notes |
| --- | --- | --- |
| `ingredients` | `{kind, share, enabled, collections?}[]` | `kind`: `continue` \| `nextEpisodes` \| `picked` \| `watchlist` \| `comfort` \| `newReleases` \| `collections`; `share` 0–100; `collections`: rating keys, only for kind `collections`. The array order is the display order. |
| `pauseWhenAway` | bool | on-demand mode |
| `nameFormat` | string | default `{viewer} TV` |
| `repeatHours` | int | don't repeat a title within N hours |
| `maxShowShare` | int | percent of a day one show may take |
| `nightMaxMinutes` | int | 0 = no limit; caps programme runtime late at night |

## Channel.screens (`ChannelScreens`, optional; signal screens §3.4a)

`{ "offAir": "", "noSignal": "" }`. Each value is `""` (follow the owner default) or `card` \| `static` \| `bars` \| `brb` \| `testcard`. The owner default lives in `config.LiveTV.Screens{offAir, noSignal, stationId}`, where `noSignal` may also be `error` (today's behaviour).

## Viewer policy addition (`livetv_policies` JSON)

`personal`: `{ "enabled": bool | null, "name": string }`. `null` follows the channel's audience; `name` overrides `nameFormat` for this viewer.

## Appendix A addition: station bug settings

Optional `Style.bug` and `Channel.look.bug` share this sparse JSON object:

```json
{"position":"bottomRight","opacity":0.3,"style":"monochrome","size":"medium","source":"logo","fadeBreaks":true,"fadeCredits":false}
```

`position`: `topLeft|topRight|bottomLeft|bottomRight`; `opacity`: finite 0.15–0.60;
`style`: `monochrome|original`; `size`: `small|medium|large`;
`source`: `logo|wordmark`; fade fields: booleans. Absent fields resolve through
channel override → style → defaults above. Explicit `false` is preserved.
This is additive JSON. Existing records use the new defaults without migration
or legacy styling. `core.ClipRequest.Bug` carries overrides to background
preparation; no server configuration setting is added.

### W15 shipped hierarchy and patch contract (2026-11-05)

`config.LiveTV.defaultStyleId` is optional and empty by default. Style records
add optional `screens` with offAir, noSignal, stationId and applyTo. These are
JSON additions; no table migration. Live TV reads cached style screens and
resolves default → channel style → channel screen. No rendering occurs on that
path. Scene documents retain the §5 schema; manual layer patches change only
text, fit, size, opacity, x/y and hidden. Animation stays in family documents.

The protected-page addition in §13.1 is named Styles / Style. All unrelated
Live TV layout remains unchanged. Owner-facing terminology is independent of
stable API identifiers.
### Appendix A addendum: era channels (v0.3.5)

All additions are optional. Absent `Channel.look.breakPattern` retains existing
schedules, guides and Plex behaviour. No core/config schema changes are needed.

`Channel.look.breakPattern` (`BreakPattern`):

| JSON | Type | Constraint / meaning |
| --- | --- | --- |
| `preset` | string | `1980s`, `1990s`, `2000s`, `2010s`, `custom`; saved numeric values are authoritative |
| `seconds` | int | 0–maxSeconds; zero selects count target |
| `minSeconds`, `maxSeconds` | int | whole-pod bounds, 0–600 / 1–600, min ≤ max |
| `spots` | int | 0–maxSpots; zero selects seconds target |
| `minSpots`, `maxSpots` | int | 0–30 / 1–30, min ≤ max |
| `podsPerHalfHour` | int | 1–6; frequency ceiling |
| `inside` | bool | use saved programme break points when available |
| `noRepeatMinutes` | int | 0–1440; also never repeat a spot inside a pod |
| `stationId` | bool | add the style's ident after the back bumper |
| `bumperStyle` | string | `break-back` |
| `clockAlignment` | string | `none` or `half` (pod cannot cross half-hour) |

`Item.breakPointsMs`: optional sorted positive integer media offsets in
milliseconds, strictly before the programme duration. `Moment.assetDigest`:
optional SHA-256 reference to a video asset; `Moment.crossfade` is an optional
boolean request for a creative transition. Deletion remains guarded by style
usage. ReelClip and Asset add optional `source` and `attribution` strings.
Pending assets also expose `status`, `progress`, `error`; pending clips reuse
the existing status/progress/error fields, adding `queued` and `downloading`.
The `onair/3` migration adds durable `onair_imports` jobs and asset attribution
columns. Imports never fetch local files or ship any commercials with Ether.

Console integration components: `BreakPatternEditor` in channel Look › Breaks,
`LinkImport` in `ReelDrawer` and `AssetsPanel`, and `MomentVideoPicker` for the style
inspector's selected moment (props `look`, `moment`, `onChange`). All changes use
the owning drawer/page save bar except import actions, which queue immediately
like uploads.
