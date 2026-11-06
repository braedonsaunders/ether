# Style Studio (v0.3.5)

The owner rejected the Looks editor. It was a skin editor over the On Air scene
engine:
- Presets were family documents embedded in the binary, and the designer could
  not recreate them.
- Layers hit-tested a hard-coded 3-box table.
- Each role had exactly one fixed document.
- Assets were an inconsistent list with no preview.

This spec replaces it. Owner bar: fun, rewarding, addictive and world-class for
people who want their own live TV through Plex. It must also be powerful
enough that **every shipped template can be rebuilt from scratch in the
editor.**

Orchestrator: thr_hvnvprbg42. Base branch: `feat/035-style-studio`. It is then
merged by thr_cp7svgfc62 into `feat/035-virtual-channels` and on to
`release/0.3.5`.

## 1. Model

The contract lives in `web/src/pages/livetv/onair/studio-model.ts`. The Go
mirror goes in `internal/onair/model.go`. The record keeps the `/onair/looks`
API path and the `onair_looks` table. The UI says "Style" everywhere.

- **Style** = brand kit + role durations + scenes + controls + channel use (+
  the existing `bug` and `screens`).
- **Scenes are owned.** A style holds complete scene documents. Families become
  **templates**. Creating from a template copies every document into the style.
  - Render code never reads `families.Get` for a style.
  - `Look.family`, `Look.moments`, `Look.layers`, `Look.documents`, the
    `brand.sting/volume/seed/words` fields and `applyLayers` are deleted.
    v0.3.5 is unreleased: no compatibility readers, no fallbacks, no
    migrations of old rows. The orchestrator recreates the lab and decades
    styles through the new API.
- **Roles.** Each role has one duration: a multiple of 4 from 4 to 30 s, so
  schedule layout stays deterministic. A role may hold any number of scenes.
  - Playout picks among enabled scenes by weight. The pick is deterministic per
    `hash(styleID, channelID, slate start)`.
  - livetv adds `Bind["slate.start"]` (unix seconds of the interstitial's
    start) to the clip request. This is additive.
  - A role with no enabled scene does not air.
- **Words are baked.** Template instantiation resolves `{words.*}` into
  literal layer text. Copy is edited on the layer, in the owner's language.
- **Tokens.** These are available to every scene:
  - `{bg} {accent} {text} {glow}`
  - `{type.display} {type.body}`
  - `{energy}` and `{textures.<name>}` (0 or the texture amount)
  - `{brand.logo}` and `{words.tagline}` (= brand.tagline)
  - `{control.<id>}` for each StyleControl
  - the live data slots: `channel.name`, `next.title`, `next.time`,
    `next.poster`, `now.title`, `viewer.name` and so on.
  - The hard-coded `INTERMISSION`/`Back in a moment` token hacks and the
    `ident-envelope` opacity hack in `service.plan` go. Templates carry that
    in their documents.
- **Sound.** Each scene has up to 4 `SceneSound`s, mixed into the master by
  ffmpeg: offset, trim, volume and fades, with a limiter on the mix. The
  engine's `audio` generators stay available.
- **Video.**
  - `SceneVideo.mode=background`: ffmpeg overlays the rasterized RGBA frames
    on the scaled video.
  - `mode=replace`: today's bumper path.
  - The channel's `momentAssets` override (W22) still wins over the style.
- **Scene layer additions** (scene v1, additive; Go `scene.Layer`, the TS
  `Layer` type, raster and the Canvas renderer):
  - `name` (display name)
  - `hidden` (not rendered)
  - `locked` (editor only)
  - `start`/`end` seconds (the layer is invisible outside them; 0/omitted =
    whole scene)
  - Keep Go/TS golden parity.
- **Validation.** `POST /onair/scenes/validate` and every save run
  `scene.Decode` plus the cost limits. The error says which layer and why, in
  plain words.

## 2. Experience

The Style drawer is full size (W23 sets it to `2xl` so expand/collapse works).
Three depths share one stage.

1. **Remix** (default tab):
   - The brand kit: logo upload or monogram, 4 colours with harmony
     suggestions, display/body fonts as live specimens, tagline, energy,
     texture chips.
   - Then the style's exposed **controls**, grouped.
   - Then "Describe a change" (AI).
   - Changing anything updates every scene at once. A grid of every role's
     scene plays live on the stage.
2. **Design.** A real canvas editor on the selected scene:
   - **Stage:** click to select, with true bounds from the renderer and
     handles to move, resize and rotate. Shift constrains, arrows nudge.
     Snapping to edges/centres/other layers, title-safe and action-safe
     guides, zoom.
   - **Insert bar:** Text, Shape (rect/ellipse/line/star/polygon), Pen (click
     and drag to draw paths), Image/Video (Library picker), Live data (a
     chip-picker of slots), Effects (gradient, grid, particles, noise, glow).
   - **Layers:** human names, groups, drag reorder, hide/lock, duplicate,
     delete, rename inline, multi-select.
   - **Inspector by type:** fill/stroke/text/font/size/tracking/align/fit,
     blend, blur/glow and reveal. Any property can bind to a brand token or
     live data, or be **exposed as a control** with one click.
   - **Timeline** under the stage:
     - a ruler and playhead scrub;
     - one bar per layer (drag the ends to set `start`/`end`);
     - keyframe diamonds per animated property (add, move, delete, easing);
     - one-click **animation presets** (In: Fade, Pop, Slide, Typewriter, Wipe,
       Glitch; Loop: Float, Pulse, Flicker, Glint, Wiggle; Out: Fade, Drop,
       Wipe) that write ordinary keyframes and behaviours;
     - **sound lanes**: drag to offset, trim the ends, fade handles, volume,
       waveform.
   - **Scene strip** along the bottom: every scene, grouped by role, as live
     thumbnails.
     - Add from blank, from another template's scene, or with AI.
     - Duplicate, rename, enable, set the rotation weight.
     - Set the role duration with a slider snapped to 4 s.
3. **Share:** export the style as a pack (scenes plus the referenced assets
   with licences), import a pack, and duplicate.

The rest of the experience:

- **Library** is a Live TV sub-tab (`/livetv/library`), global, replacing the
  Assets panel.
  - **Grid:** one uniform responsive thumbnail grid. Video plays on hover,
    audio shows a waveform, fonts show an "Aa" specimen, images show a cover
    crop.
  - **Find and add:** kind filter chips, search, sort, upload by dropping
    anywhere (fonts ask for a licence), and URL import (the existing
    `LinkImport`).
  - **Asset drawer** (a record, so a drawer): player or viewer, a waveform
    with trim to save as a new asset, rename, replace, licence/attribution,
    used-by links, delete with W22's in-use guard.
  - **Picker:** the same grid opens as a picker drawer from the editor.
- **Template gallery.**
  - Large live tiles rendered with the owner's real channel names and
    upcoming posters, playing on hover.
  - "Start from blank".
  - Templates with era/mood tags: Clean, Kids, Late night, Cinema, Retro,
    Personal, and the four decades (80s/90s/00s/10s).
- **Play the sequence.** The preview plays ident → programme stand-in → break
  out → (reel) → break in → up next on the chosen channel and time, with
  sound.
- **Remove the plumbing.**
  - No "Rendering N of 7": scene thumbnails carry a small "Getting ready for
    TV" state.
  - No "New shapes".
  - No hard-coded channels or viewers: the preview uses the owner's real
    channels and viewers.
- Every template must open in Design with named, grouped layers (rename
  `Title-weight0`… into groups like "Chrome title") and expose 3–8 sensible
  controls.

## 3. Engineering rules

- `CLAUDE.md`, the test budget, i18n (`t()`), AppKit components, and the
  drawer/inline rules (records in drawers, never convert existing paradigms).
  These are the rules most often missed.
- Run quick package tests only. The orchestrator runs the batch gate.
- Workers wrap all copy in `t()` and run `pnpm i18n sync`. One translator
  batch runs at the end, so don't translate locales yourself.
- Go/TS raster parity: update only the goldens you intentionally change.
- Stay in your files (§4). If you need something outside them, message the
  orchestrator: `bb thread tell thr_hvnvprbg42 '…'`.

## 4. Workstreams and file ownership

| WS | Branch | Owns |
| --- | --- | --- |
| S1 Engine & model | `feat/035-studio-engine` | `internal/onair/**` except `families/documents/*.json` and the library handlers in `assets.go`; `internal/onair/scene`, `raster`, `encode`; `scene-renderer.ts` (+ test, worker); `model.ts` (swap `Look` to `Style`); the additive `slate.start` in `internal/livetv/onair.go`; `/onair/templates`, `/onair/scenes/validate`, the save/render path, sounds, background video, rotation |
| S2 Canvas editor | `feat/035-studio-canvas` | `LookDrawer.tsx`, `Stage.tsx`, `LayersPanel.tsx`, new `editor/` (`Canvas`, `Handles`, `InsertBar`, `Inspector`, `SceneStrip`, `scene-bounds.ts`, `scene-edit.ts` pure doc ops); delete `DesignPanel.tsx`'s Design role |
| S3 Timeline, animation, sound | `feat/035-studio-timeline` | new `editor/Timeline.tsx`, `editor/animation-presets.ts`, `editor/keyframes.ts`, `editor/SoundLane.tsx`, `editor/VideoPicker.tsx` (replaces `MomentVideoPicker.tsx`), the preview audio mixer `editor/preview-audio.ts` |
| S4 Library | `feat/035-studio-library` | new `web/src/pages/livetv/library/**`, the `/livetv/library` route + sub-tab entry, `AssetsPanel.tsx` (delete), `LinkImport.tsx`; Go: the library handlers (info/thumb/waveform/PATCH/replace) in a new `internal/onair/library.go` + `assets.go` |
| S5 Remix, templates, AI, packs | `feat/035-studio-remix` | `LooksGallery.tsx`, new `editor/RemixPanel.tsx`, `DescribeLook.tsx`, `generate.go`, the pack export/import (`internal/onair/pack.go`), `families/documents/*.json` (naming, grouping, controls, decades), `editor/SequencePreview.tsx` |

Interfaces between workstreams:
- S2 renders S3's `<Timeline>` and the preview-audio hook in slots it leaves
  in `LookDrawer`.
- S2 calls S4's `<LibraryPicker kind onPick>`, exported from
  `library/LibraryPicker.tsx`.
- S2 renders S5's `<RemixPanel>` and `<SequencePreview>`.
- Pure document edits go through S2's `editor/scene-edit.ts`. S3 adds its
  keyframe ops in `editor/keyframes.ts`.
- If a component you depend on isn't merged yet, write against the props
  stated here and stub it locally in your own file. Never edit a file another
  stream owns.

### Component props (fixed)

```ts
// S3 editor/Timeline.tsx
export function Timeline(p: { scene: StyleScene; duration: number; time: number;
  playing: boolean; onTime(t: number): void; selected: string[];
  onSelect(ids: string[]): void; onChange(scene: StyleScene): void }): JSX.Element;
// S3 editor/preview-audio.ts: plays the scene's sounds in sync with the stage clock
export function usePreviewAudio(scene: StyleScene | undefined, time: number,
  playing: boolean, muted: boolean): void;
// S3 editor/VideoPicker.tsx
export function VideoPicker(p: { scene: StyleScene; onChange(scene: StyleScene): void }): JSX.Element;
// S4 library/LibraryPicker.tsx: a drawer with the Library grid in picking mode
export function LibraryPicker(p: { open: boolean; kinds: AssetKind[];
  onPick(asset: LibraryAsset): void; onClose(): void }): JSX.Element;
// S5 editor/RemixPanel.tsx
export function RemixPanel(p: { style: Style; onChange(style: Style): void }): JSX.Element;
// S5 editor/SequencePreview.tsx
export function SequencePreview(p: { style: Style; channelId: string; at: string }): JSX.Element;
// S5 LooksGallery.tsx: creating from a template calls studio.createStyle(template.style)
// S2 editor/scene-edit.ts: pure, immutable ops on Scene/StyleScene:
//   findLayer, updateLayer(id, patch), insertLayer(layer, parentId?, index?),
//   removeLayers(ids), moveLayer(id, parentId, index), groupLayers(ids),
//   ungroup(id), duplicateLayers(ids), newLayerId(scene, base)
// S2 editor/scene-bounds.ts
export function layerBounds(doc: Scene, b: SceneBindings, t: number):
  { id: string; x: number; y: number; w: number; h: number; rotate: number }[]; // 1920x1080 space
```
