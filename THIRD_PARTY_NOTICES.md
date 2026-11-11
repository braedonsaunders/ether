# Third-party notices

Ether is GPLv3 (see [LICENSE](LICENSE)). The components below are used
under their own licences, all of which are GPL-compatible or (FFmpeg)
GPL itself. Full licence texts ship with each upstream project; for Go
modules the exact versions are pinned in [go.mod](go.mod)/[go.sum](go.sum)
and their `LICENSE` files are in the module sources.

## Bundled binary: FFmpeg

Installers and the Docker image bundle `ffmpeg` and `ffprobe` from the
**jellyfin-ffmpeg** portable builds (pinned in
[`packaging/ffmpeg.lock.json`](packaging/ffmpeg.lock.json), currently
`8.1.3-1`), which are GPL builds of [FFmpeg](https://ffmpeg.org/).

- FFmpeg is © the FFmpeg developers, licensed under the GNU General
  Public License v2-or-later (the bundled build enables GPL components
  such as libx264/libx265, so the combined binary is GPL).
- Build scripts and corresponding source:
  <https://github.com/jellyfin/jellyfin-ffmpeg> and
  <https://ffmpeg.org/download.html>. This notice plus the Ether source
  repository constitute the source offer for these binaries.
- Ether is GPLv3, so distributing a GPL FFmpeg alongside it is intended
  and compatible. If you build Ether yourself you may equally point it
  at your system's ffmpeg (`transcode.ffmpegPath` in the config).

## RapidYenc

`internal/usenet` imports [mnightingale/rapidyenc](https://github.com/mnightingale/rapidyenc),
pinned in `go.mod`/`go.sum` to commit `6657b7ca854e05728fa4a67f29ebc90ecd304428`
(2026-10-08). Supported pure-Go SIMD builds use its public `NewDecoder` API
behind Ether's bounded NNTP input adapter. Ether does not select upstream's C
backend. CGO-enabled builds, Windows ARM64, unsupported targets/CPUs and
explicit caller buffers use Ether's SWAR decoder. The RapidYenc response path
requires `CGO_ENABLED=0 GOEXPERIMENT=simd` and has no C/C++ toolchain dependency.
The dependency is MIT-licensed:

MIT License

Copyright (c) 2023 Michael Nightingale

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.

## Go modules (direct dependencies)

| Module | Used for | Licence |
| --- | --- | --- |
| `github.com/SherClockHolmes/webpush-go` | Web Push notification delivery | MIT (© Ethan Holmes) |
| `github.com/coder/websocket` | Viewer event and playback-control WebSockets | ISC (© Coder) |
| `github.com/golang-jwt/jwt/v5` | VAPID token signing via webpush-go (indirect, shipped) | MIT (© Dave Grijalva and golang-jwt maintainers) |
| `modernc.org/sqlite` (+ its modernc.org support modules) | Pure-Go SQLite store | BSD-3-Clause (module); SQLite itself is public domain |
| `github.com/jackc/pgx/v5` (+ `github.com/jackc/pgpassfile`, `github.com/jackc/pgservicefile`, `github.com/jackc/puddle/v2`) | Pure-Go PostgreSQL driver (database/sql adapter) for clustered mode, `internal/store` | MIT (© Jack Christensen) |
| `github.com/kardianos/service` | OS service (systemd/launchd/Windows SCM), `internal/service` | Zlib-style (“as-is”, © Daniel Theophanes) |
| `github.com/ulikunitz/xz` | 7z header LZMA decoding, `internal/archive` | BSD-3-Clause |
| `github.com/mnightingale/rapidyenc` | Pure-Go SIMD Usenet yEnc article decoding, `internal/usenet` | MIT (© Michael Nightingale) |
| `golang.org/x/crypto` (`argon2`) | Local password hashing, `internal/identity` | BSD-3-Clause |
| `golang.org/x/text` | Legacy subtitle code pages, `internal/subtitles`; language names, `internal/collections` | BSD-3-Clause |
| `golang.org/x/image` | Collection poster rendering (scaling, WebP decoding, OpenType text), `internal/artwork` | BSD-3-Clause |
| `github.com/srwiley/oksvg` and `github.com/srwiley/rasterx` | Bounded pure-Go SVG rasterization, `internal/artwork` | BSD-3-Clause |
| Go fonts (`golang.org/x/image/font/gofont`: Go Bold, Go Medium), embedded | Title text on generated collection posters, `internal/artwork` | BSD-3-Clause-style (© 2016 Bigelow & Holmes Inc., distributed by the Go Authors; see `golang.org/x/image/font/gofont/ttfs/README`) |
| `github.com/dlclark/regexp2` | Bounded native custom-format expressions, `internal/resolver` | MIT (© Doug Clark) |
| `github.com/skip2/go-qrcode` | Pure-Go QR code generation for device connection instructions | MIT (© Tom Harwood) |
| `github.com/go-chi/chi/v5` | HTTP routing (wired by other packages) | MIT |
| `github.com/tetratelabs/wazero` | Pure-Go WebAssembly runtime for hosted plugins, `internal/plugins/wasm` (pinned to v1.9.0: `fastschema/qjs` v0.0.6 is incompatible with wazero v1.12 function-signature checking) | Apache-2.0 |
| `github.com/fastschema/qjs` | JavaScript for hosted plugins: QuickJS-NG compiled to WebAssembly (its embedded `qjs.wasm`, ~1 MiB), run on wazero, no cgo, `internal/plugins/wasm`; locally patched for isolation, see `third_party/qjs/SANDBOX.md` | MIT (© Nguyen Ngoc Phuong and contributors); the embedded QuickJS-NG engine is MIT (© Fabrice Bellard and the QuickJS-NG contributors) |
| `github.com/evanw/esbuild` | TypeScript-to-JavaScript compilation for hosted plugins (Go API), `internal/plugins/wasm` | MIT (© Evan Wallace) |
| `github.com/PuerkitoBio/goquery` | HTML/XML selection for built-in indexer definitions, `internal/cardigann` | BSD-3-Clause (© Martin Angers and contributors) |
| `github.com/andybalholm/cascadia` | CSS selector engine used with goquery, `internal/cardigann` | BSD-2-Clause (© Andy Balholm) |
| `golang.org/x/net` (`html`) | HTML parsing for built-in indexer definitions, `internal/cardigann` | BSD-3-Clause |
| `go.yaml.in/yaml/v3` | Reading owner-supplied indexer definitions, `internal/cardigann` | MIT and Apache-2.0 (© Canonical Ltd. and the YAML maintainers) |
| `github.com/shirou/gopsutil/v4` | CPU, memory, network and process load on the Overview, `internal/sysload` | BSD-3-Clause (© WAKAYAMA Shirou and contributors) |
| `github.com/ebitengine/purego`, `github.com/tklauser/go-sysconf` (+ `numcpus`), `github.com/yusufpapurcu/wmi`, `github.com/go-ole/go-ole`, `github.com/lufia/plan9stats`, `github.com/power-devops/perfstat` | gopsutil internals: macOS system calls without cgo, Linux sysconf, Windows WMI (indirect) | Apache-2.0 / BSD-3-Clause / MIT / MIT / BSD-3-Clause / MIT respectively |
| `fyne.io/systray` | Tray/menu-bar icon, `cmd/ether-tray` | Apache-2.0 |
| `github.com/godbus/dbus/v5` | Linux StatusNotifier (systray transport) | BSD-2-Clause |
| `github.com/google/uuid` | IDs (via modernc / components) | BSD-3-Clause |
| `github.com/dustin/go-humanize` | Human-readable sizes (indirect) | MIT |
| `github.com/mattn/go-isatty`, `github.com/ncruces/go-strftime`, `github.com/remyoudompheng/bigfft` | modernc/sqlite internals (indirect) | MIT / MIT / BSD-3-Clause respectively |
| `golang.org/x/sync` | pgx internals (indirect) | BSD-3-Clause |
| `golang.org/x/sys` (and other `golang.org/x/*` in the build graph) | Platform primitives (indirect) | BSD-3-Clause |

Regenerate the ground truth any time with `go list -m all` and each module's `LICENSE` file.

## Go fonts (embedded in the binary)

Generated collection posters (`internal/artwork`) set titles in Go Bold
and Go Medium from `golang.org/x/image/font/gofont`, compiled into the
Ether binary.

> These fonts were created by the Bigelow & Holmes foundry specifically
> for the Go project. Copyright (c) 2016 Bigelow & Holmes Inc.. All
> rights reserved.
>
> Redistribution and use in source and binary forms, with or without
> modification, are permitted provided that the following conditions are
> met:
>
> - Redistributions of source code must retain the above copyright
>   notice, this list of conditions and the following disclaimer.
> - Redistributions in binary form must reproduce the above copyright
>   notice, this list of conditions and the following disclaimer in the
>   documentation and/or other materials provided with the distribution.
> - Neither the name of Google Inc. nor the names of its contributors may
>   be used to endorse or promote products derived from this software
>   without specific prior written permission.
>
> DISCLAIMER: THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND
> CONTRIBUTORS "AS IS" AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING,
> BUT NOT LIMITED TO, THE IMPLIED WARRANTIES OF MERCHANTABILITY AND
> FITNESS FOR A PARTICULAR PURPOSE ARE DISCLAIMED. IN NO EVENT SHALL THE
> COPYRIGHT OWNER OR CONTRIBUTORS BE LIABLE FOR ANY DIRECT, INDIRECT,
> INCIDENTAL, SPECIAL, EXEMPLARY, OR CONSEQUENTIAL DAMAGES (INCLUDING, BUT
> NOT LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR SERVICES; LOSS OF
> USE, DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER CAUSED AND ON
> ANY THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY, OR TORT
> (INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE OF
> THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.

## Owner web UI (npm packages bundled into `internal/admin/dist`)

The `/ether/` owner UI (`web/`) is built with Vite and embedded in the
binary. The bundle includes these packages (exact versions are pinned in
[`web/pnpm-lock.yaml`](web/pnpm-lock.yaml)):

| Package | Used for | Licence |
| --- | --- | --- |
| `@braedonsaunders/appkit-ui`, `@braedonsaunders/appkit-tokens`, `@braedonsaunders/appkit-dashboard` (primitives entry), `@braedonsaunders/appkit-ai`, `@braedonsaunders/appkit-i18n`, `@braedonsaunders/appkit-viewspec` | Design system: components, semantic tokens, dashboard tiles | AGPL-3.0-or-later |
| `@codemirror/lang-javascript`, `@codemirror/lang-json`, `@codemirror/autocomplete`, `@codemirror/language`, `@codemirror/lint`, `@codemirror/merge`, `@codemirror/state`, `@codemirror/view`, `@lezer/highlight`, `@uiw/react-codemirror` | Plugin source editor and syntax highlighting | MIT |
| `@monaco-editor/react`, `monaco-editor`, `@typescript/vfs` | Plugin editor and TypeScript language support | MIT |
| `@valtown/codemirror-ts` | TypeScript editor integration | ISC |
| `react-markdown`, `remark-gfm` | Markdown rendering | MIT |
| `react`, `react-dom`, `react-router`, `react-router-dom` | UI runtime and routing | MIT |
| `@dnd-kit/core`, `@dnd-kit/sortable`, `@dnd-kit/utilities` | Accessible drag-to-reorder | MIT |
| `lucide-react` | Icons | ISC |
| `hls.js` | Owner Live TV preview player (loaded on demand) | Apache-2.0 |
| `framer-motion` (+ `motion-dom`, `motion-utils`) | AppKit interaction motion | MIT |
| `clsx`, `tailwind-merge` | Class composition (via AppKit) | MIT |
| `class-variance-authority` | Component variants (via AppKit) | Apache-2.0 |
| `tailwindcss` (build-time; generated CSS ships) | Utility CSS | MIT |

AppKit is AGPL-3.0-or-later. GPLv3 section 13 expressly permits combining
GPLv3 code with AGPLv3 code; the AppKit parts keep their AGPL terms,
including the obligation to offer source to users interacting with them
over a network. Ether's complete source, including `web/`, is public at
the repository above, which satisfies that offer.

## AltMount

Ether was designed with [AltMount](https://github.com/javi11/altmount)
as a reference for Usenet-streaming concepts (provider pooling, NZB
segment indexing).

> MIT License — Copyright (c) 2025 Javier Blanco

Disc-image playback adapts AltMount code (contributed by its author); each
adapted file keeps the copyright line above in a header comment:

- `internal/iso/fs.go`, `bluray.go`, `mpls.go`, `volume.go` (and their
  tests): ISO 9660/UDF reading and Blu-ray main-feature playlist selection,
  from AltMount `internal/importer/archive/iso/`. Ether bounds the
  filesystem walk and playlist reads against damaged or hostile images.
- `internal/iso/disc.go`: multi-disc grouping by volume label, from AltMount
  `internal/importer/archive/iso_expansion.go`.
- `internal/stream/tsremux.go`: MPEG-TS timestamp rewriting for a
  continuous Blu-ray main-feature timeline, from AltMount
  `internal/nzbfilesystem/tsremux.go`.

The stream engine's tests also use AltMount's MIT-licensed WinRAR RAR5
fixture files (see below). The MIT License text follows those fixtures.

## Stream engine components

## github.com/ulikunitz/xz

- Used by: `internal/archive` (LZMA/LZMA2 decoding of encoded 7z headers)
- Licence: BSD-3-Clause
- URL: https://github.com/ulikunitz/xz
- Copyright (c) 2014-2022 Ulrich Kunitz. All rights reserved.

### AltMount test fixtures

- Used by: `internal/archive/testdata/altmount/` (test data only, not shipped):
  `archive.rar`, `archive.part01.rar`, `archive.part02.rar`, `compressed.rar`,
  `encrypted.rar`, `solid.rar`, `multi.part1.rar`, `multi.part2.rar`,
  `multi.part3.rar`, `payload_a.bin`, `payload_b.bin`
- Source: AltMount `internal/importer/testdata/`
- Licence: MIT
- URL: https://github.com/kipsilabs/altmount
- Copyright (c) 2025 Javier Blanco

MIT License text:

> Permission is hereby granted, free of charge, to any person obtaining a copy
> of this software and associated documentation files (the "Software"), to deal
> in the Software without restriction, including without limitation the rights
> to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
> copies of the Software, and to permit persons to whom the Software is
> furnished to do so, subject to the following conditions:
>
> The above copyright notice and this permission notice shall be included in all
> copies or substantial portions of the Software.
>
> THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
> IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
> FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
> AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
> LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
> OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
> SOFTWARE.

## Data and API attributions

### Jellyfin Web

Ether serves the official, **unmodified** Jellyfin Web client at `/web` on the
Jellyfin port. Nothing is bundled: on first use Ether downloads the pinned
release artifact from `repo.jellyfin.org`
(`jellyfin-web_12.4+deb13_all.deb`, version 12.4, verified by SHA-256) and
extracts it under the data directory. Source:
https://github.com/jellyfin/jellyfin-web/tree/v12.4. The package's own
`COPYRIGHT` notice is served at `/web/COPYRIGHT`.

Jellyfin Web is licensed under the GNU General Public License, version 2
(GPL-2.0). Ether distributes it unmodified and dynamically; it is a separate
program from Ether and its complete licence text travels with the downloaded
package.

- Metadata and artwork come from **TMDB**. This product uses the TMDB
  API but is not endorsed or certified by TMDB.
- Content-rating labels in `internal/contentrating` are the per-country
  certification lists TMDB publishes (`/certification/movie/list`,
  `/certification/tv/list`); their age levels follow each classification
  board's published definitions. Jellyfin's rating files
  (GPL-2.0, github.com/jellyfin/jellyfin) were consulted as a cross-check
  only; no Jellyfin data or code is copied.
- Watch sync (optional): **Trakt**. Subtitles (optional):
  **OpenSubtitles**. Both are off until the owner configures accounts.
- Collection sources (optional): public **Trakt**, **Letterboxd** and
  **MDBList** lists, fetched only when the owner adds one to a
  collection, identified, rate-limited and cached.

### Encrypted archive password derivation

`internal/archive/rar_crypto.go` adapts password derivation from
[`nwaples/rardecode`](https://github.com/nwaples/rardecode) revision
`ad22ba91a2ab0d3ff6bca20cd978b33ee78faab1` (BSD-2-Clause).
`internal/archive/sevenzip_crypto.go` adapts AES properties and password
derivation from [`bodgit/sevenzip`](https://github.com/bodgit/sevenzip) revision
`bc7ebbd2043c386e3bab1bd04a64a62062890fc4` (BSD-3-Clause).
Ether adds bounded, cancellable derivation and integration with its random-access
archive readers. No AIOStreams source code is incorporated into these components.
The full upstream notices follow and are also retained beside the adapted files.

Copyright (c) 2015, Nicholas Waples
All rights reserved.

Redistribution and use in source and binary forms, with or without
modification, are permitted provided that the following conditions are met:

* Redistributions of source code must retain the above copyright notice, this
  list of conditions and the following disclaimer.

* Redistributions in binary form must reproduce the above copyright notice,
  this list of conditions and the following disclaimer in the documentation
  and/or other materials provided with the distribution.

THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS "AS IS"
AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT LIMITED TO, THE
IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR A PARTICULAR PURPOSE ARE
DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT HOLDER OR CONTRIBUTORS BE LIABLE
FOR ANY DIRECT, INDIRECT, INCIDENTAL, SPECIAL, EXEMPLARY, OR CONSEQUENTIAL
DAMAGES (INCLUDING, BUT NOT LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR
SERVICES; LOSS OF USE, DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER
CAUSED AND ON ANY THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY,
OR TORT (INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE
OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.
BSD 3-Clause License

Copyright (c) 2020, Matt Dainty
All rights reserved.

Redistribution and use in source and binary forms, with or without
modification, are permitted provided that the following conditions are met:

* Redistributions of source code must retain the above copyright notice, this
  list of conditions and the following disclaimer.

* Redistributions in binary form must reproduce the above copyright notice,
  this list of conditions and the following disclaimer in the documentation
  and/or other materials provided with the distribution.

* Neither the name of the copyright holder nor the names of its
  contributors may be used to endorse or promote products derived from
  this software without specific prior written permission.

THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS "AS IS"
AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT LIMITED TO, THE
IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR A PARTICULAR PURPOSE ARE
DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT HOLDER OR CONTRIBUTORS BE LIABLE
FOR ANY DIRECT, INDIRECT, INCIDENTAL, SPECIAL, EXEMPLARY, OR CONSEQUENTIAL
DAMAGES (INCLUDING, BUT NOT LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR
SERVICES; LOSS OF USE, DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER
CAUSED AND ON ANY THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY,
OR TORT (INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE
OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.

### Encrypted RAR interoperability fixtures

The unmodified `rar5-psw.rar`, `rar5-hpsw.rar`, and `rar3-comment-hpsw.rar`
fixtures in `internal/archive/testdata/rar-encrypted/` come from
[`markokr/rarfile`](https://github.com/markokr/rarfile), revision
`d2f7df6fc843dae356fd6b0a85971dc36fd6e757`, under the following ISC license.
The remaining generated fixtures are Ether-owned synthetic test data.


Copyright (c) 2005-2026 Marko Kreen <markokr@gmail.com>

Permission to use, copy, modify, and/or distribute this software for any
purpose with or without fee is hereby granted, provided that the above
copyright notice and this permission notice appear in all copies.

THE SOFTWARE IS PROVIDED "AS IS" AND THE AUTHOR DISCLAIMS ALL WARRANTIES
WITH REGARD TO THIS SOFTWARE INCLUDING ALL IMPLIED WARRANTIES OF
MERCHANTABILITY AND FITNESS. IN NO EVENT SHALL THE AUTHOR BE LIABLE FOR
ANY SPECIAL, DIRECT, INDIRECT, OR CONSEQUENTIAL DAMAGES OR ANY DAMAGES
WHATSOEVER RESULTING FROM LOSS OF USE, DATA OR PROFITS, WHETHER IN AN
ACTION OF CONTRACT, NEGLIGENCE OR OTHER TORTIOUS ACTION, ARISING OUT OF
OR IN CONNECTION WITH THE USE OR PERFORMANCE OF THIS SOFTWARE.

## Conformance API descriptions

Jellyfin OpenAPI 12.4 is published at `https://repo.jellyfin.org/files/openapi/stable/`;
Emby.SDK publishes OpenAPI documents from `MediaBrowser/Emby.SDK`. The upstream
redistribution license for these generated descriptions was not explicit in the
reviewed sources, so Ether does not vendor them. Fetch current Jellyfin 12.4 and
pinned Emby.SDK snapshots with `internal/conform/specs/fetch.sh` when needed.
Silo reference route names/shapes in the conformance notes are independently
recorded observations, not copied AGPL source.

## On Air embedded Google Fonts

Sources: https://github.com/google/fonts (pinned revision in
`internal/onair/fonts/data/revision.txt`). Rebuild with
`tools/subset-onair-fonts.py` and fonttools 4.68.0. The modified faces keep
Latin, Latin Extended, combining marks, Greek, Cyrillic and punctuation
available in their sources; Noto Sans remains the glyph fallback. Hinting
is removed for the on-air renderer, which uses unhinted text. Common shaping,
kerning, ligatures and combining-mark features are retained.

Inter, Space Grotesk, Fraunces, Playfair Display, Bungee, Fredoka, Orbitron,
VT323, IBM Plex Mono and Noto Sans are licensed under SIL Open Font License
1.1. Complete original copyright and licence texts are embedded alongside
the TTFs in `internal/onair/fonts/data/*-OFL.txt`. Special Elite is licensed
under Apache License 2.0; its complete licence is in
`internal/onair/fonts/data/specialelite-LICENSE.txt`.

All modified primary/PostScript font names are renamed EtherAir01 through
EtherAir11 (mapping in the subset script), respecting the Reserved Font
Names **Playfair Display**, **Orbitron** and **Plex**. Stable API IDs and
filenames identify the source family; original licence metadata is retained.
Archivo Black is also bundled under SIL Open Font License 1.1 for the Grunge 90
family, with its complete licence in `internal/onair/fonts/data/archivoblack-OFL.txt`.
It is subset with the same script and renamed EtherAir12, retaining its
original licence metadata. Monoton is not bundled.
