# Plugin content kinds

A content plugin adds a whole **library** to Ether — audiobooks,
comics, music, books, photos, field recordings, anything flat or
hierarchical — with no core changes. The owner meets each kind you
declare as a library beside Movies and TV Shows, in the console and
in every client app you publish it to.

This page is the contract. The worked example is
[`sdk/examples/kind-library`](../sdk/examples/kind-library); the
Studio "A new library" template generates the same skeleton from a
description.

## Declare a kind

In the manifest's `ether` block: `kind: "content"`, at least one of
the `catalog_provider` / `kind_resolver` capabilities (plus optional
`matcher` and `frontend_adapter`), and one entry per kind in
`contributes.mediaKinds`:

```jsonc
{
  "key": "recordings",              // ^[a-z][a-z0-9-]{0,31}$, unique in the plugin
  "label": "Field recordings",
  "library": { "name": "Field Recordings" },   // default name; the owner can rename
  "levels": [                       // 1–5 levels, first is the root
    { "key": "collection", "label": "Collection" },
    { "key": "album", "label": "Album", "container": true },
    { "key": "track", "label": "Track", "playable": true }
  ],
  "fields": [                       // validated per item; indexed ones can facet
    { "key": "genre", "type": "string", "label": "Genre", "indexed": true },
    { "key": "durationSec", "type": "duration", "label": "Duration" }
  ],
  "present": { /* see "Publishing" */ },
  "browse": { "sorts": ["title", "index"], "facets": ["genre"], "search": true, "liveSearch": true },
  "sync": { "mode": "pull", "refresh": "1h" },
  "cache": { "maxBytes": 2147483648 },        // optional; per-library blob cache
  "match": { "thresholdDefault": 0.8 }        // optional
}
```

Rules that will bite if ignored: at least one level must be
`playable`; facet fields must be `indexed`; field keys may not reuse
envelope names (`id`, `title`, …); declarations are **additive across
versions** — removing or retyping a field/level is refused at upgrade
unless the owner agrees to replace the library's contents.

## The item envelope

Your provider returns `KindItem`s: your own `id` (letters, digits,
`. _ ~ -`, ≤128 chars — Ether namespaces it, you never see the stored
identity), `level`, `parentId`, `index` within the parent, `title`,
optional `summary/year/released/rating`, `playable`, counts, `art`
(`poster/backdrop/thumb/logo` plus declared roles), `fields` (checked
against your declaration; unknown keys are dropped, wrong types
reject the item), `hints` (an opaque JSON object ≤16 KiB, stored and
handed back to your resolver verbatim — put ids and locators there,
never secrets), and an `etag` for cheap re-syncs.

Caps: title 500 characters, summary 20k, each field 4k, 16 art roles.

## Routes Ether calls

Kind and setup routes are `GET`/`POST` with JSON in and out, through the
normal plugin transport. Frontend-adapter traffic keeps whatever method and
query the client sent:

| Route | When | Notes |
|---|---|---|
| `/ether/v1/kinds/{kind}/browse` | Pull sync walks your catalogue | One level per call (`parentId`, `level`, `cursor`, `limit`). Return `nextCursor` or the full set up to `limit`; never more than `limit` items. |
| `/ether/v1/kinds/{kind}/changes` | Pull sync refresh | Optional delta feed `{upserts, deletes, nextCursor, complete}`. Answer 404/501 and Ether re-walks and diffs by `etag` instead. |
| `/ether/v1/kinds/{kind}/items/{id}` | Conformance and detail refresh | One full item. |
| `/ether/v1/kinds/{kind}/search` | Client searches, only when `browse.liveSearch` is on | Same page shape as browse with `query` set. Short budget; see below. |
| `/ether/v1/kinds/{kind}/resolve` | A viewer presses play | `{libraryKey, item, partIndex?, client?}` → ordered `sources`. Never called during browse. |
| `/ether/v1/kinds/{kind}/match` | Resolve-time matching, console re-match | `{item, candidates}` → `{matches:[{candidateRef, score 0..1, reasons}]}` best first. |
| `/ether/v1/setup/{step}/test` | The setup wizard | `{step, fields}` → `{ok, detail, fields?}`. Secrets arrive via `SecretGet`, never in this body. |
| `/ether/v1/frontend/{key}/…` | Your frontend adapter's traffic | Any method, and the original query string (repeated keys flatten to the first value). Ether authenticated the viewer first; you get `X-Ether-Viewer` headers, never a credential. |

Clients are **always served from Ether's store** — browse, search
and detail never call you live, so nothing scales with your catalogue
size. You are called at sync time and at play time.

Sync modes: **pull** (Ether walks `browse`, then polls `changes` on
your `refresh`, minimum 15m) or **push** (you call the `KindUpsert`
host service; refused for pull kinds — one writer per kind). A pull
walk that would delete more than half the library aborts and raises
owner attention instead of wiping it.

### Live search

With `browse.liveSearch: true`, a client search on your library also
calls your `/search` route with a short budget (about two seconds).
Results are validated, **upserted into the store like synced items**
(stubs are fine — playable leaves resolve at play time), and merged
with local results. On timeout or error the viewer gets local
results in time and your late answer still lands in the store for
the next search. Declare it only if your source can answer a search
quickly; the Studio template has it on with the explanation beside
the toggle. Pair it with `browse.search: true` so earlier live
results stay findable locally.

## Resolving bytes

`resolve` returns ordered `PlayableSource`s; Ether tries them in
order. Patterns:

- **file** — a hosted resolver path inside an owner-picked folder; requires
  `folders: read` and live containment checks when resolving and opening.
- **range** — a URL Ether reads with HTTP Range (demoted to download
  when the host cannot range and the size fits the cache).
- **download** — fetched whole into the per-library blob cache
  (SHA-256 checked when you provide one), then served locally.
- **direct** — the client surface is handed the URL (only where the
  surface supports it).
- **container** — one member of a zip/RAR/7z, possibly multi-volume:
  `{volumes:[{url,size}], member}` or `memberIndex`. Passwords come
  from a named settings secret (`passwordSecret`), never from your
  response. Ether caches the extracted member.
- **release** — hand Ether a magnet/infohash/NZB and its existing
  debrid/Usenet/torrent pipeline opens it. For torrents, set
  `release.fileIndex` (zero-based index in the provider's complete file list),
  `release.candidate.fileIndex`, or `release.file` (exact path or unique
  basename) to open audio, books, comics, ROMs or other non-video files.
  A top-level `fileIndex` overrides the candidate's index. If both index and
  `file` are supplied they must identify the same file. Empty files,
  invalid indices and missing or ambiguous names fail without fallback.
  Without either selector the existing video picker is used. In Go, indices
  are `*int`: a pointer to zero selects the first file; nil leaves selection
  automatic. Indices refer to the provider's ordering, not its file IDs;
  prefer a path when the provider's ordering is unknown. Explicit selectors
  on an NZB release are rejected; use a container source for archive members.
  `fileHint` retains its existing meaning as an archive member hint.

`ResolveSources({kind: "indexer_search", query, category})` accepts a
comma-separated list of Newznab category IDs (`"3000"`, `"7030"`) or standard
names: `console`, `movies`/`movie`, `audio`/`music`, `pc`, `tv`, `adult`,
`books`, `comics`, `other`. It sends a generic search with those categories
and the query unchanged, rather than assuming a movie or TV request.
Invalid categories are rejected. Stremio/addon sources do not support this
filter and reject scoped searches. Omit `category` for the default media
search. Results and acquired files are cached separately by category and
file selection. The wire shapes are documented in
[`plugin-sources.v1.json`](../schemas/plugin-sources.v1.json).

URLs may carry `{itemIndex}`, `{pageNumber}` (= itemIndex + 1),
`{byteStart}`, `{byteEnd}` and `{byteRange}` placeholders; Ether
substitutes integers only. Resolver `headers` may use
`{secret:name}` placeholders, substituted host-side — never put a
raw secret in a response (responses are logged). Returning zero
sources means "not available right now" and is cached briefly.

With `permissions.sources: "search"` (or `"acquire"` to open debrid
content) your resolver can ask Ether to find bytes through the
owner's indexers and debrid via the `ResolveSources` host service,
rank the candidates yourself (or in `/match`), and return a
`release` source.

Watch state and progress are keyed per playable leaf. For paged
media, store progress against the leaf the viewer is on (its index
is the natural unit); containers themselves carry no progress.

## Publishing (`present`)

Each library publishes per protocol; the owner can override your
defaults per library afterwards.

- `plex: {base: "movie"|"show"|"artist"|"photo"}` — root→show/artist,
  middle→season/album, leaf→episode/track. More than three levels
  are flattened with indexes kept (the Studio preview shows this).
- `mediaBrowser: {collectionType, itemTypes}` — Jellyfin/Emby.
  Item types: `Movie, Series, Season, Episode, MusicAlbum, Audio,
  Book, PhotoAlbum, Photo`, mapped per level; unmapped levels use
  the collection's natural container.
- `stremio: {type}`, `xtream: {category}`, `silo: {type}`.
- `adapterOnly: true` — no generic mapping fits; ship a
  `frontend_adapter` and serve the protocol yourself. Embed
  `/ether/v1/kind-media/{itemId}` stream URLs for bytes (the SDKs'
  `StreamURL`/`stream_url`/`streamUrl` helper) instead of proxying
  media through the adapter.

A library the owner cannot see is invisible to that viewer on every
surface; you never check viewer permissions yourself.

## Matching

Matching is yours; persistence and the console are Ether's. A manual
pin from the item drawer always wins, then your best score at or
above the kind's threshold (manifest default, owner-editable), then
an inline match during resolve. Give plain-language `reasons` — the
owner reads them.

## Host services

Beyond the usual services, kind plugins get (capability-checked on
every call): `KindUpsert` / `KindDelete` (push kinds), `KindItems`
(read your own library back), `ResolveSources`, and
`CachePut` / `CacheOpen` for the per-library blob cache
(content-addressed `blob:<sha256>` refs; you never name paths).
Default cache quota is 2 GiB per library under a shared cap of
min(10 GiB, 5% of the cache disk); your `cache.maxBytes` and the
owner can override it.

## Setup

Declare `contributes.setup.steps` (any plugin kind): each step lists
settings fields and an optional test route. The console walks the
steps in a drawer; a content plugin's libraries stay hidden and sync
waits until required steps pass (the owner may skip, and sees a
setup banner instead).

## SDKs

Same contract in all three, pinned by the shared fixtures in
`sdk/conformance/kinds` (each SDK's tests replay them):

- **Go** (`sdk/go`): `Plugin.OnCatalog / OnResolve / OnMatch /
  OnFrontend / OnSetupTest`, the `CatalogProvider`, `KindResolver`,
  `KindMatcher`, `FrontendAdapter` interfaces, host kind methods on
  `Host`, and `sdk/go/kit` helpers — `scrape` (polite host-HTTP
  fetching), `archivex` (zip/cbz/epub members), `metaparse` (EPUB
  OPF, ComicInfo, ID3, FLAC), `feedparse` (RSS/Atom/JSON Feed →
  items), `hints` (typed hints/fields).
- **Python** (`sdk/python`): `CatalogProvider`, `KindResolver`,
  `KindMatcher`, `FrontendAdapter`, `SetupTester` base classes with
  `on_catalog` etc., `ctx.ether.kinds` / `.cache`, `parse_feed` and
  `archive_list` helpers.
- **TypeScript** (`sdk/typescript`): `onCatalog / onResolve /
  onMatch / onFrontend / onSetupTest`, `ctx.ether.kinds` / `.cache`,
  `parseFeed`, `archiveList`, `scrapeText/Links`.

Content plugins are usually best as `remote` (scraping wants a real
process); `hosted` works for pure-API providers — the example and
the Studio template are hosted. In hosted JS, read settings with
`ether.settings.get()` and kind services via `ether.kinds`,
`ether.sources`, `ether.cache`.

## Checking your work

- The conformance kit (`kit.CheckKindProvider`, run by Studio and
  `ether plugin check`) walks your provider offline: pagination must
  terminate, pages must fit the limit, detail must round-trip, a
  declared live search must answer, resolver sources must use known
  patterns and placeholders.
- Studio's kind preview browses your draft, shows the hierarchy and
  the per-protocol projection, and dry-runs the resolver on a real
  leaf (template expansion included) before anything is installed.
- Start from the Studio "A new library" template or copy
  `sdk/examples/kind-library`; its test is the executable version of
  this page.


### Granted local files (hosted runtime)

A resolver can return a source such as
`{"id":"local","pattern":"file","path":"/picked/library/Game.nds","mime":"application/octet-stream"}`.
This requires `folders: read` and an owner-picked, non-revoked folder grant.
Ether uses the same `validateLocalFile` and `openGrantedLocalFile` path as
Stremio `ether.file`: it rejects `..`, resolves symlinks, checks containment at
resolve time and again at open time, and opens through `os.OpenRoot`.
Remote/sidecar resolvers cannot serve host files with this pattern.

### File identification without file contents

Hosted plugins can call `ether.folders.hash(root, path)` (TypeScript SDK:
`host.folders.hash`; Go SDK: `FoldersHash`). `path` is relative to the picked
root. The service requires `folders: read` and the same live folder grant as
`folders.stat`. It refuses traversal, symlink escapes and non-regular files.
It returns `{size, modTime, crc32, md5, sha1}`: Unix seconds for `modTime`,
lowercase hexadecimal digests (CRC32 IEEE, 8 digits; MD5, 32; SHA1, 40).
No file contents cross the plugin boundary. All three hashes are computed in
one streaming pass with fixed memory. `hash` stays synchronous for files up to
256 MiB; larger files must use `hashStart(root, path)` (Go: `FoldersHashStart`).
This explicitly starts a background job and returns `{id, state, bytes}`.
Poll `hashPoll(id)` (`FoldersHashPoll`) for bytes processed and state: `running`,
`done`, `canceled`, or `error`. A done job includes `result` with the hash object;
an error includes `error`. `hashCancel(id)` (`FoldersHashCancel`) requests cancellation;
poll until terminal to observe completion. Start and poll do not return file bytes.

There is no file-size ceiling for jobs. At most one runs per plugin and two
across the process; a busy start returns an error to retry later. Starting the
same active root/path returns its existing job ID. Readers yield for 2 ms after
each 1 MiB chunk. Jobs survive the initiating call, have a 24-hour deadline,
and recheck grants and cancellation every chunk. Files changing during hashing
abort without caching. Revocation or plugin disable stops ongoing reads.

Successful digests persist in a host-owned database cache indexed by plugin,
resolved path, size and nanosecond modification time. Unchanged files reuse the
cached digest after permission and containment checks. Cache rows are removed
when the plugin is uninstalled. Only explicitly requested files are hashed.
Job status is process-local, and each plugin's terminal status is retained until
its next start; after a restart, start again to reuse any completed cached result.
SDK sidecars cannot use these hosted folder services.

### Item availability

An upsert can carry
`"unavailable":{"code":"no_verified_source","detail":"No source matched the DAT record"}`.
The code must be nonempty and at most 128 bytes; detail is optional and at most
2000 bytes. The item remains visible in browse/counts so owners can inspect the
coverage gap. Ether exposes the reason in the item drawer, marks the item
nonplayable, and never calls its resolver or opens a cached resolution while
this declaration is present. The declaration also travels in frontend item
details. The next upsert without `unavailable` clears it, even with the same
etag. This is independent of the level's `playable` capability; clearing the
reason does not grant playback to a nonplayable level.

### Large downloads and ZIP members (v0.3.11)

Hosted and remote clients can keep large data in the host's existing library
blob cache, without copying the complete file into guest memory:

```ts
const cached = await ctx.ether.http.fetchToCache!({
  url: "https://datasets.example/platform.zip",
  libraryKey: "pk-community-example-library-games", // this plugin's own library
  name: "platform.zip",
});
const members = await ctx.ether.archive!.list(cached.blob);
const member = members.find(m => m.name === "games/example.json")!;
const chunk = await ctx.ether.archive!.open(cached.blob, member.index, {
  offset: 0, length: Math.min(member.size, 1048576),
}); // Uint8Array; request another range for the next piece
```

`http.fetchToCache({url, headers?, libraryKey, name})` performs a GET, streams
up to the owner download ceiling (8 GiB by default), further bounded by the library cache quota, into the plugin's own library cache, and returns
`{blob, size, crc32, md5, sha1, status}`. SHA-256 is the blob reference.
Digests cover the stored response bytes in one pass. Compare the digest with
trusted catalogue metadata before serving a verified download. Non-2xx responses,
network failures, size overflow and quota failures return errors; they do not
publish a truncated blob. Redirects recheck the live network grant and the
existing SSRF policy. The existing content capability, network host declaration
and library cache quota apply; no new permission is needed. The operation is
synchronous and bounded by the current call's deadline; there is no background
prefetch. Cache eviction still applies to returned refs.

`cache.read(blob, {offset, length})` reads at most 1 MiB from a cached blob.
`archive.list(blob)` returns `[{index, name, size}]`, reading the ZIP directory
without loading all compressed bytes. `archive.open(blob, memberIndex,
{offset, length})` returns at most 1 MiB from the **uncompressed** member.
Offsets are nonnegative integers; lengths are 1..1048576. A read at EOF returns
empty bytes and reads crossing EOF return the available suffix. Raw host calls
return `{bodyBase64, size}`; SDK clients decode this into bytes. Every call opens
only the calling plugin's cache namespace and closes the reader afterward.
The Go `RemoteHostClient` provides `FetchToCache`, `CacheRead`, `ArchiveList` and
`ArchiveOpen` through the optional `BlobHost` interface, with `DataRange` options.

ZIP sizes and member counts use owner global/per-plugin ceilings: defaults are
8 GiB compressed archive/member, 32 GiB total expansion and 65,535 members.
The central directory is bounded to 64 MiB (1024 bytes per member name), with a
1000:1 maximum expansion ratio per member. ZIP64 is supported; split archives
remain unsupported. Store and Deflate are supported. Unsafe paths, special files, encryption,
Split archives are rejected. Nothing is extracted to a filesystem.
Deflate ranges inflate and discard up to the requested offset on each call;
prefer large sequential chunks, and avoid repeatedly rescanning a large member.
Range reads do not independently verify the full member CRC; use trusted digests
when integrity matters. Local owner-granted paths are not archive inputs: first
use a plugin-owned cached blob. Gzip files are not ZIPs and are unsupported here.
An HTTP `Content-Encoding: gzip` response may be transparently decoded by the
HTTP client; its digests then describe the decoded bytes. A `.gz` file is not
automatically decompressed by this archive service.

Direct `http.fetch` still limits the raw response to 10 MiB. Its JSON result
allowance now includes encoding expansion (binary base64 or escaped text), so the
4 MiB general host-result cap no longer blocks that documented limit. This is
an I/O ceiling, not a guarantee of guest heap space: the default guest has 32 MiB
and the owner ceiling defaults to 2 GiB (adjustable up to wasm32 4 GiB), with temporary JSON strings and parsed objects
using additional memory. Use fetch-to-cache and ranged reads for large inputs;
do not concatenate all chunks into one guest buffer. Other host results remain
capped at 4 MiB.

### Owner-started multi-call work and resource requests

A page action returning `{job: "build-catalogue"}` starts a background job.
The job returns `continueInMs`, an opaque JSON `continuation`, and a `progress`
string to continue; Ether forwards the token in the next POST body. `done: true`
finishes, `failed: "reason"` fails, and an absent `continueInMs` finishes too.
Jobs use the owner's time ceiling (12h by default), a 1,000,000-call sanity bound,
a minimum 100ms pause, OS background CPU/IO priority, and cooperative pauses
between calls and host operations during playback, Live TV or high host load.
They are cancellable with visible progress. Persist checkpoints in KV for
recovery on a later owner action after restart; Ether does not resume eagerly.

`ether.runtime.limits` requests memory, time, compute, download and archive
resources within owner-controlled global/per-plugin ceilings. Defaults: 2 GiB
memory ceiling (up to 4 GiB; omitted heap request stays 32 MiB), 12h, effectively
unlimited background work units, 8 GiB download/member, 32 GiB total expansion,
and 65,535 members. The library cache quota further bounds downloads. The owner
sees requested versus granted values before install and in the drawer, and can
raise or lower ceilings under Plugins → Settings or for one plugin. Saving
stops affected jobs so the new ceilings apply immediately.

Viewer browse/search/playback calls retain tight budgets: `/stream/`, `/catalog/`,
`/meta/`, `/subtitles/`, `/ether/open`, `/ether/metadata/`, `/ether/subtitles/`,
`/ether/markers`, and `/ether/v1/kinds/` use at most 10,000 units/30s plus tighter
route/caller deadlines. Ranking budgets stay unchanged. Administrative setup,
actions, tools, events and jobs use owner grants. Health/configuration/view/options
keep short protocol deadlines. See the SDK README for exact fields and APIs.
