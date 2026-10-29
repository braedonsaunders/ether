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

## Go modules (direct dependencies)

| Module | Used for | Licence |
| --- | --- | --- |
| `modernc.org/sqlite` (+ its modernc.org support modules) | Pure-Go SQLite store | BSD-3-Clause (module); SQLite itself is public domain |
| `github.com/jackc/pgx/v5` (+ `github.com/jackc/pgpassfile`, `github.com/jackc/pgservicefile`, `github.com/jackc/puddle/v2`) | Pure-Go PostgreSQL driver (database/sql adapter) for clustered mode, `internal/store` | MIT (© Jack Christensen) |
| `github.com/kardianos/service` | OS service (systemd/launchd/Windows SCM), `internal/service` | Zlib-style (“as-is”, © Daniel Theophanes) |
| `github.com/ulikunitz/xz` | 7z header LZMA decoding, `internal/archive` | BSD-3-Clause |
| `golang.org/x/crypto` (`argon2`) | Local password hashing, `internal/identity` | BSD-3-Clause |
| `golang.org/x/text` | Legacy subtitle code pages, `internal/subtitles`; language names, `internal/collections` | BSD-3-Clause |
| `golang.org/x/image` | Collection poster rendering (scaling, WebP decoding, OpenType text), `internal/artwork` | BSD-3-Clause |
| Go fonts (`golang.org/x/image/font/gofont`: Go Bold, Go Medium), embedded | Title text on generated collection posters, `internal/artwork` | BSD-3-Clause-style (© 2016 Bigelow & Holmes Inc., distributed by the Go Authors; see `golang.org/x/image/font/gofont/ttfs/README`) |
| `github.com/dlclark/regexp2` | Bounded native custom-format expressions, `internal/resolver` | MIT (© Doug Clark) |
| `github.com/go-chi/chi/v5` | HTTP routing (wired by other packages) | MIT |
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
| `github.com/mattn/go-isatty`, `github.com/ncruces/go-strftime`, `github.com/remyoudompheng/bigfft`, `github.com/hashicorp/golang-lru/v2` | modernc/sqlite internals (indirect) | MIT / MIT / BSD-3-Clause / MPL-2.0 respectively |
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
| `@braedonsaunders/appkit-ui`, `@braedonsaunders/appkit-tokens`, `@braedonsaunders/appkit-dashboard` (primitives entry) | Design system: components, semantic tokens, dashboard tiles | AGPL-3.0-or-later |
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

## AltMount (reference only)

Ether was designed with [AltMount](https://github.com/javi11/altmount)
as a reference for Usenet-streaming concepts (provider pooling, NZB
segment indexing). Ether's code is its own.

> MIT License — Copyright (c) 2025 Javier Blanco

No AltMount code is vendored; the stream engine's tests use AltMount's
MIT-licensed WinRAR RAR5 fixture files (see below).

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

- Metadata and artwork come from **TMDB**. This product uses the TMDB
  API but is not endorsed or certified by TMDB.
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
