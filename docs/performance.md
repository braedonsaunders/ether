# Performance

Ether's catalogue is the whole TMDB catalogue. A home server has hundreds of
thousands of catalogue rows, years of watch history and several live TV
guides. Plex, Jellyfin and Emby were built for libraries a person filed by
hand. Ether has to stay fast at a size those servers never meet. This
document covers how Ether is built for that, the budgets every endpoint has
to meet, how they are measured, and the rules for new code.

## The rule

**No request path may scale with the size of the library.** A request reads
what it renders, plus an index seek. It never walks the catalogue, the
history or a guide. It never waits on a provider (TMDB, plex.tv, an indexer,
a debrid service) or a live probe.

Everything else in this document is how Ether keeps that rule, and how the
build catches code that breaks it.

## Budgets

Every client request has a class. Each class has a p95 budget for warm
requests, cold requests (the first after a start) and loaded requests
(concurrent viewers, or a busy catalogue writer running beside them).

| class | what | warm p95 | cold | loaded p95 |
|---|---|---:|---:|---:|
| static | identity, system info, capabilities | 50 ms | 500 ms | 150 ms |
| signin | password sign-in through a protocol | 100 ms | 500 ms | 300 ms |
| home | home screens, hubs, continue watching, next up | 150 ms | 1 s | 450 ms |
| browse | library listings, section pages, catalogs | 150 ms | 1 s | 450 ms |
| detail | one title, its seasons and episodes | 150 ms | 1 s | 450 ms |
| search | search as you type | 150 ms | 1 s | 450 ms |
| guide | live TV guide grid and hubs | 150 ms | 1 s | 450 ms |
| console | owner console pages | 250 ms | 1.5 s | 750 ms |
| playback | playback setup (reported, not gated: it needs providers) | 1 s | 3 s | 3 s |

In production, every request also carries a database budget. A request that
spends more than 500 ms in the database, runs more than 300 statements, or
takes more than 2 s of wall time (media and event streams excepted) is
logged as `request over performance budget`, with its route shape, timings
and three most expensive statements. Each route shape is logged at most once
a minute. These lines are how a slow path shows up in an owner's support
bundle before it shows up as a hang on a television.

## How requests are measured

`store.QueryStats` records the database work done for one unit of work:
statements, rows read back into Go, time inside the driver, and time spent
waiting to begin a transaction (SQLite's write lock or a PostgreSQL `BEGIN`).
Both drivers record into the stats carried by the statement's context:
SQLite through the prepared-statement cache in `internal/store/stmt_cache.go`
and PostgreSQL through `internal/store/pgdriver.go`. Nested measurements
record into the outer ones as well.

`internal/app/perf_observe.go` gives every request on every surface its own
`QueryStats`. That covers Plex, Jellyfin, Emby, Silo, Stremio, WebDAV, M3U,
DLNA and the owner console. Two environment variables expose more:

- `ETHER_PERF_TRACE=<file>` writes one JSON line per request: surface, path,
  status, bytes, duration and database work, broken down per statement with
  the `file:line` that issued it.
- `ETHER_PPROF_ADDR=127.0.0.1:<port>` serves Go's profiler on a loopback
  address. Non-loopback addresses are refused.

## The harness: `etherperf`

`go run ./cmd/etherperf real -source <data dir>` measures a real Ether on
real data:

1. It copies the data directory: an online SQLite backup of `ether.db` (the
   source may be a running server), `config.json`, `.credentials-key`,
   `node.json` and `certs/`. It never copies caches, logs or backups.
2. It sanitizes the copy. It removes Usenet, debrid and indexer providers,
   disables LLM, OpenSubtitles, Trakt, Simkl and poster services, turns off
   updates, UPnP and remote access, and moves every listener to loopback ports
   from `-port` (default 32990: Plex and the console on the base port,
   Jellyfin +1, Emby +2, Silo +3, pprof +9).
3. It mints credentials into the copy. It gives the active viewers a known
   password, a Plex token and a Stremio grant, so every client signs in
   through its real sign-in route.
4. It starts a real `ether` with outbound network blocked. On macOS that is
   `sandbox-exec` with loopback-only networking. On Linux it is a fresh
   network namespace (`unshare`).
5. It replays real client request sequences, taken from the conformance
   captures and the parity corpus:
   - Plex for Apple TV and Plex Web: home hubs, section pages, deep pages,
     detail, children, search and the live TV guide grid;
   - Infuse against Jellyfin (Infuse's exact field list), Swiftfin,
     Emby Theater and Infuse against Emby;
   - Silo for Apple TV;
   - Stremio's catalogs and metadata;
   - the owner console.
6. It runs the sequences in phases:
   - cold: the owner, straight after the server starts;
   - first: each other viewer once, with that viewer's caches cold;
   - warm: several rounds;
   - concurrent: N viewers at once;
   - busy writer: warm rounds while a second connection commits
     catalogue-sized write transactions back to back, the way a catalogue
     sync does.
7. It writes `report.md`, which checks every endpoint against its budget and
   shows the server's own statement count, rows read, database time and lock
   wait for it. Allocation counts and bytes are process-wide deltas while the
   request runs and can include background work. The harness enables this
   sampling explicitly; ordinary traces leave it off to avoid perturbing the
   scale gate. It also writes `audit.md` and `results.json` (including response
   hashes for comparisons).

To replay specific Plex metadata or postplay paths, `-plex-cases` accepts a
private JSON file containing an array such as
`[{"name":"example detail","path":"/library/metadata/123"}]`.
Only numeric metadata and postplay paths are accepted; metadata cases use
the detail client's usual include flags. Keep files containing real keys in
private scratch storage.

`audit.md` is the query audit. It runs `EXPLAIN QUERY PLAN` on every
statement those requests executed, then ranks the statements by the request
time spent in them. It flags:

- full table scans and full index scans of large tables;
- temporary B-trees for `ORDER BY`, `GROUP BY` and `DISTINCT`;
- automatic indexes;
- correlated subqueries;
- N+1 patterns (the same statement twenty or more times in one request);
- statements that read thousands of rows per execution.

Each entry carries the `file:line` that issued it.

```sh
go run ./cmd/etherperf real -source ~/path/to/data -out /tmp/etherperf-run
go run ./cmd/etherperf real -source ~/path/to/data -clients plex-appletv,infuse-jellyfin -rounds 3
go run ./cmd/etherperf audit -trace /tmp/etherperf-run/trace.jsonl -db /tmp/etherperf-run/data/ether.db
```

Run it against a copy of a real library, never against a live server's
directory. Use `-strict` to exit 1 when a gated endpoint misses its budget.

`-keep-providers` keeps the copy's Usenet, debrid and indexer settings
(outbound stays blocked), so availability filtering runs as it does in
production. Two production inputs stay out of reach offline: Plex Pass
comes from plex.tv, so Plex Live TV (and Home's On Now row) is off in the
sandbox, and TMDB is unreachable, so reads never fetch or store what the
mirror lacks. Measuring either needs a throwaway build or a TMDB-only
forward proxy (`HTTPS_PROXY`) that refuses every other host.

## Configuration matrix

Run the matrix on an isolated local App with the fixture cache on the scratch
volume. Set the Music source to a local checkout of the hosted Music plugin.
The gate loads that checkout with network permission revoked, seeds artists,
albums and tracks, and reads the Subsonic method list from its dispatcher.
A full or Subsonic-selected run fails before replay if the checkout is missing
or the adapter cannot be installed and mounted.
For `ETHER_PERF_GATE_MUSIC_SOURCE`, CI uses `braedonsaunders/ether-plugin-music`
at commit `f1b147485a022aa28e96b1410e0065aacc32a1b3`. Use that revision to
reproduce the CI matrix.

```sh
mkdir -p "$HOME/.scratch/tmp/ether-perf"
ETHER_PERF_GATE=1 \
ETHER_PERF_GATE_FIXTURES="$HOME/.scratch/tmp/ether-perf" \
ETHER_PERF_GATE_MUSIC_SOURCE="/path/to/ether-plugin-music" \
ETHER_PERF_GATE_REPORT="$PWD/perf-matrix-report.md" \
go test ./internal/app -run '^TestPerformanceGate$' -count=1 -v -timeout=45m
```

The cache holds a day-stamped SQLite base fixture. Each case gets a separate
copy in the same directory, so APFS can clone it and replay mutations cannot
leak into another case. The fixture holds 35,000 movies, 15,000 shows, 45,000
seasons and 270,000 episodes, plus plugin items, 240 owner collections, and
viewer watch history. Live cases add 300 channels, seven days of guide data,
and a synthetic sports feed and overlay. The matrix covers release modes for
movies and shows, ordered visibility rules, upcoming titles, viewer policies,
source configuration, and selected combinations. Console and plugin-host
scenarios use owner credentials in all cases.

The matrix checks statements, result rows and writes for each tagged response.
Queries and rows have class ceilings with tighter scenario bounds where
specified. GET and HEAD carry a zero-write budget with route-specific
exceptions for last-seen updates and capability credentials the response
publishes. Streaming scenarios use a one-minute observation window. Result rows are rows returned
by the driver. Use the scale comparison and the SQL plan audit to investigate
database work that returns few rows.

The Markdown report updates after every case. It lists failed or skipped
scenarios, registered routes missing successful scenarios, exemptions with
their reasons, the worst measured sample per route, and every over-budget
route and configuration. It uses route patterns
and strips SQL string literals. Coverage gaps and budget violations fail the
test. The run has a 40-minute replay deadline and a three-minute limit per case.
Ordinary requests have an eight-second replay window. The report retains
traced database work through cancellation and marks incomplete requests as
coverage gaps. For local triage,
`ETHER_PERF_GATE_MATRIX_CASES`, `ETHER_PERF_GATE_CLIENTS` and
`ETHER_PERF_GATE_STEPS` accept comma-separated filters. Cases and clients
match exact names. Step filters match a substring, ignoring case. Client
filters scope route inventory checks to the selected clients’ surfaces.
Step filters still report skipped registrations on those surfaces. Plex
registrations that accept multiple methods through one handler are reported
once as `*`. Leaf routes with distinct GET and HEAD registrations still need
both requests. Add a successful workflow with fixture IDs discovered from
earlier responses, or record the specific fixture prerequisite in
`internal/app/perf_matrix_routes_test.go`. To use PostgreSQL, provide a disposable
server through `ETHER_PERF_GATE_PG`. Each case gets a temporary schema.

## Library-scale comparison

`TestPerformanceGate` (`internal/app/perf_gate_test.go`) also supports a
two-scale comparison. `internal/perfharness/fixture.go` builds the same synthetic household
twice:

- once over a library of 54k catalogue rows;
- once over a library ten times larger: 250,000 movies, 10,000 shows,
  40,000 seasons and 240,000 episodes, 540k rows in all.

Both carry the same household (three viewers with 20,000 history rows
each, drawn from a taste of 8,000 movies and 300 series, three playlists
of 40, 600 and 3,000 items and 55 favourites), so only the library
differs. Titles are made-up words, one in fifty with a common English
word, so substring search matches about as sparsely as on a real
catalogue. Every title carries TMDB-shaped detail in long-tail
distributions (`LongTailShape` in `internal/perfharness/longtail.go`,
fitted to a real mirror): credits (cast capped at 30, as the catalogue
keeps it, plus directors, writers and crew; people dealt by preferential
attachment, so most have one credit and a few have hundreds), keywords,
genres, countries, studios, alternative titles, crowd recommendations,
franchises and home-release checks. The rows go in with the catalogue's
triggers set aside, and everything those triggers keep (search indexes,
person names, genre pairs, show dates, episode counts, library tallies)
is rebuilt from them; `TestGenerateMatchesLiveTriggers` proves the result
is what a load with every trigger on writes. The App runs with the
background WAL checkpointer, as `Run` does.

On each library it starts a real App and replays one client per protocol
(Plex for Apple TV and a second Apple TV, Plex Web, Infuse on Jellyfin,
Emby, Silo, Stremio, the console) and Apple TV's playback writes (play
queues, timeline, progress, scrobble, rating). The owner replays cold and
twice warm; a second viewer replays once with that viewer's caches cold
(the `first` phase), and the report shows the owner's cold work beside the
second viewer's first pass. Then it compares every endpoint across the two
libraries, the second viewer's first pass on its own.

| an endpoint fails when, on the 10× library | threshold |
|---|---|
| rows read grow (steps other than listings) | more than 2× + 200 (cold and first passes: 2× + 2000) |
| a listing reads more than its titles render | any request: rows > Σ titles rendered × that type's most detail rows in the fixture + 200 |
| statements grow (every pass) | more than 1.5× + 20 |
| database time grows (warm median only) | more than 5× + 100 ms |
| warm p50 | over 2 s |

A listing step (browse class) whose response names its titles (Plex
`Metadata`, Jellyfin and Emby `Items`, Silo `items`, Stremio `metas`) is
held to what it renders rather than to the rows ratio: a page deep in a
popularity order is famous titles on the large library and obscure ones on
the small, and famous titles carry far more detail rows. The per-type cap
is computed from the generated library (`perfharness.DetailCaps`: the most
genre, keyword, alternative title, studio, country, credit and
home-release rows any title of the type has, plus two rows per season for
a series and ten for the title itself). A walk of the library breaks the
bound at once; `TestPerfGateCatchesAPageWalk` proves it by injecting one.
Cold and first passes are compared by statements and rows only: one cold
sample on a shared runner is no measure of database time. The gate freezes
the catalogue cache's wall-clock minute (`Catalog.FreezeCacheMinute`), so a
replay crossing a minute counts the same statements.

An indexed path does about the same work on both libraries. A path that
walks the catalogue does ten times the work, so the gate does not depend on
the CI machine's speed. Endpoints that scaled when the gate was introduced
are listed in `internal/app/testdata/perf_gate_debt.txt`. The gate fails on
any endpoint that scales and is not listed, and reports listed endpoints
that stayed flat so they can be removed; the list only shrinks.

```sh
ETHER_PERF_GATE=scale go test ./internal/app -run TestPerformanceGate -v -timeout 60m
ETHER_PERF_GATE=quick go test ./internal/app -run TestPerformanceGate -v   # a 5k/54k pair, not ratcheted
ETHER_PERF_GATE=xlarge go test ./internal/app -run TestPerformanceGate -v -timeout 290m   # 128k/1.28M titles
```

The `xlarge` pair is a full TMDB mirror, the design target: 1,278,768
dated, non-adult titles (1,073,442 movies and 205,326 series), every series
with its seasons and the episodes of the 300 series a household opened
(the catalogue stores episodes when a season is opened), 1.69M catalogue
rows in all, against a tenth of it. It takes too long for every push, so
`.github/workflows/perf-nightly.yml` runs it nightly on main and on demand
(`workflow_dispatch`, any ref). It uses the same debt file and thresholds.

The nightly workflow runs the same pair on PostgreSQL too
(`ETHER_PERF_GATE_PG=postgres://…`): each library is copied into a schema
of its own as `ether cluster init` does, analysed, and replayed with the
same steps and thresholds.

On every push the PostgreSQL CI job also runs `TestPostgresHotPlans`
(`internal/app/perf_pg_plans_test.go`, about a minute): it replays Home,
Continue Watching and On Deck, sorted and filtered section pages, search
and detail against a 2k-movie library on PostgreSQL 14, and explains every
read those requests ran as a generic plan, twice: with the statistics
ANALYZE leaves (a catalogue small next to a heavy viewer's history, where
PostgreSQL likes to hash a whole catalogue subquery), and with every
catalogue table's planner statistics scaled to a 1.3M-title mirror and the
watch tables' to a large household (`pg_class.reltuples`). It fails on a
hashed subplan over a catalogue table in either state, and at mirror scale
on a sequential scan of a catalogue or watch table or a bitmap heap scan
without an index condition. Writes, and statements PostgreSQL cannot
prepare without parameter types, are listed as not checked.

`go run ./cmd/etherperf fixture -scale large -out DIR` (also `quick`,
`small`, `xsmall`, `xlarge`) writes a gate library as a data directory
that `etherperf real -source DIR` replays.

To work on one endpoint, narrow and keep the run:

| variable | effect |
|---|---|
| `ETHER_PERF_GATE_CLIENTS=plex-appletv,console` | replay only these clients |
| `ETHER_PERF_GATE_STEPS=/hubs,movies all` | replay only steps whose names contain these |
| `ETHER_PERF_GATE_KEEP=/tmp/gate` | keep each scale's trace and database for `etherperf audit` |
| `ETHER_PERF_TRACE_DEPTH=3` | record three callers per traced statement, to find an N+1's caller |
| `ETHER_PERF_GATE_CPUPROFILE=/tmp/replay.out` | CPU profile of the large replay alone |
| `ETHER_PERF_GATE_FIXTURES=/tmp/fixtures` | keep each generated library there and clone it on the next run instead of generating it again |

## Architecture

### Storage

- **SQLite** (standalone) runs WAL with `synchronous=NORMAL`, a 64 MB page
  cache, a 256 MB memory map, in-memory temp storage and `busy_timeout`.
  `wal_autocheckpoint` is off: `RunCheckpointer` checkpoints in the
  background, so a commit never pays for the fsync. `PRAGMA optimize` runs at
  open and daily, which keeps planner statistics fresh. Prepared statements
  are cached per connection; preparing happens outside the cache lock, so one
  slow statement never serializes others. Idle connections are kept, so a
  request never re-reads the schema.
- **One writer at a time, in Go.** SQLite has a single writer. Left to
  SQLite, connections that want to write at once poll the lock with growing
  sleeps; a short write behind a busy writer (a catalogue sync committing
  back to back) could wait the full 10 s busy timeout and fail.
  `internal/store/write_gate.go` makes this process's connections take turns
  through a one-slot gate instead: a write transaction holds it from BEGIN
  to COMMIT, an autocommit write for one statement, and the next writer runs
  the moment it is released. Reads, read-only transactions and WAL
  checkpoints never wait on it.
- **PostgreSQL** (cluster) uses the same SQL, rebound to `$n` by the
  `ether-pgx` driver, with `ANALYZE` in daily maintenance. Both engines are
  first-class: request stats, the harness trace and the gate thresholds are
  engine-neutral.

### Provider calls never block a page

- `catalog.Get` serves stored metadata and refreshes it behind the read
  (`internal/catalog/hydrate.go`). A title whose details are stored but stale
  (a Returning Series goes stale every hour) is returned at once and
  refreshed in a deduplicated background slot, at most 16 at once. A title
  with no details yet waits at most 2 s while its fetch carries on detached,
  so the result still lands in the mirror for the next read.
- A request has 1.5 s to wait on TMDB in all
  (`catalog.WithReadWaitBudget`, set for every request). A page that touches
  thirty titles with no details waits at most that long, not 2 s per title.
- The TMDB client has a circuit breaker (`internal/tmdb/breaker.go`). After
  three consecutive unreachable attempts (or exhausted 429/5xx ladders),
  reads fail fast for 30 s instead of paying a 0.5 + 1 + 2 s retry ladder
  per title. An outage therefore opens the breaker within the first
  request's retries.

On the lab data, cold Plex home routes took 35–87 s before these two changes.
Each page waited on TMDB retries title by title. After the changes they take
0.1–5 s, and the remaining time is Ether's own work, which the audit ranks.

### Keep the WAL checkpointed

Every page a read fetches is first looked up in the WAL index, unless the
WAL was fully checkpointed when its transaction began. On macOS and Windows,
modernc's 16-bit atomic loads (which that lookup uses) take one global
mutex, so a WAL the checkpointer cannot drain turns concurrent reads into a
queue. The gate measured this: without a checkpointer its large replay took
4 to 14 minutes, and with one about a minute. Long scans that pin an old
snapshot keep a checkpoint from finishing, which is another reason they
stay off requests. Background readers of the whole catalogue (the
collections index snapshot, the title search index) read 4000-key ranges in
separate short statements instead. The integrity check and a backup's
`VACUUM INTO` must read one snapshot, so they announce themselves
(`store.BeginLongRead`), and bulk writers (the catalogue sync) call
`store.WaitWALBudget` before each transaction: it waits, cancellably, for
those reads to end and for the checkpoint to drain the WAL below
`journal_size_limit`. Waiting is not a failure; only a wait longer than 15
minutes returns `store.WALBlockedError`, which names the reader when it is
ours.

### Bulk catalogue writes

The catalogue sync and detail fills write through `upsertItems`: many titles
per transaction (the sync sizes its batches to about 500 ms of write-lock
hold, `nextSyncBatch`; detail writes group-commit, `detail_writer.go`).
Within a transaction:

- a title's rating, votes and popularity wobble is written only when it
  matters (`rating_drift.go`);
- facets and credits are read once and written once per table for the
  whole batch, only the difference (`join_batch.go`);
- new titles are folded into the title search index, and new credits into
  person names and their search index, in a statement or two instead of
  one trigger per row (`fts_fold.go`). The triggers check a marker row
  (`catalog_fts_bulk`) that exists only inside such a transaction, so every
  other writer keeps them. `TestFTSFoldMatchesTrigger` and
  `TestPersonNamesFoldMatchesTrigger` hold the folds equal to the triggers.

### The viewer-policy epoch

`viewer_policy_epoch` fences authorization snapshots: a fence passes only
if nothing that could turn an allow into a deny was written since the
snapshot. It moves for exactly those writes (`internal/viewerpolicy`):

- always: rating, adult, type, browse membership, release, episode
  placement, collection and the Plex share label on `catalog_titles`; a
  title or credit removed; home-release outcomes; viewer, policy and
  membership tables;
- only while a safety policy or library rule reads them
  (`viewer_policy_inputs`): genres and keywords (per value, plus a title's
  set becoming empty or non-empty), studios, countries, language, year,
  runtime, rating, votes and popularity;
- never: a new title or credit (they can only add), credit details,
  alternative titles, collection artwork, raw home-release dates.

PostgreSQL advances it at most once per transaction. On SQLite a trigger
fires per changed row, but a transaction still writes the epoch page once.
Because detail fills no longer move it, the epoch is a usable cache key:
model builds share catalogue browse results between viewers who browse
under the owner's rules with no rating cap, keyed by it.

### Search

- **Titles.** An FTS5 trigram index (`catalog_titles_fts`, plus
  `catalog_alt_titles_fts` for alternative titles) answers the substring
  match a `LIKE '%q%'` scan used to. A query of three or more characters
  with no LIKE metacharacters becomes a lookup of its trigrams; shorter or
  unusual queries keep the plain `LIKE`. On PostgreSQL, a `pg_trgm` GIN
  index serves the same `ILIKE` predicate when the server has the
  extension. A test proves the index and the scan return the same rows.
- **People.** `catalog_person_names` holds one row per credited person,
  kept in step with `catalog_credits` by triggers, with an FTS5 word-prefix
  index. A people search takes the 500 most credited matching people, orders
  them as before (exact name first, then by name), and keeps each person
  whose most popular credited titles the viewer may see. It never walks
  every credited title.
- **Bounded windows.** `/Items?searchTerm=` (Infuse, Swiftfin, the Emby
  apps) is a search, not a library walk filtered by name. An experience
  search reads a candidate window that grows with the requested page: at
  least 100 candidates, at most 1000. TMDB's own search widens results
  beyond the mirror. It runs detached and is waited for at most 1 s, or
  150 ms when the mirror already has enough matches; late results are
  already in the mirror for the next keystroke.

### Browse totals

Library exclusion rules compile to key sets (`t.key NOT IN (SELECT key …)`),
so each set is built once and probed by rowid, never two correlated
subqueries per title. A covering index on
`(type, browsable, adult, released_ts)` lets a library-wide total read that
one narrow index. With the owner's country exclusion on the lab data, the
totals behind every listing fell from 0.4–2 s to 20–30 ms.

### Series by date added

A series' date added is its latest aired episode, so sorting series by it
dated every series. Newest-first pages (Recently Added rows and addedAt
listings) walk `catalog_titles_episode_recent` (aired episodes, newest
first) instead, meeting each series at its date added; series with no
aired episode are dated by their premiere, and only those that premiered
on or after the page's last date can reach the page.

### Recursive episode listings

A recursive, episodes-only listing (Apple TV clients ask for every
episode, 400 at a time, with MediaSources) reads stored, aired episodes in
key order and checks each one's series with the same visibility rules as
the library walk; a page end is remembered for the next page. It applies
when that order is the sorted order and the episode set is the
catalogue's. Before, it refreshed every series' seasons on the request.

### Send what the client asked for

Clients ask for less, and the reference servers answer less; Ether does the
same, and builds less.

- **Plex.** `excludeFields=` and PMS's allow-listed `excludeElements=`
  (Media, Part, Genre, Country, Guid, Rating, Director, Writer, Role,
  Producer) apply to every library container except a transcode decision;
  Media leaves `mediaCount`, Part leaves `Media@partCount`. Plex for Apple TV
  and Plex Web send `excludeFields=summary` on hubs and section pages. A
  light list row whose Media is excluded does not build it, nor read what it
  is drawn from (`internal/plex/response_shaping.go`; a test proves the
  bytes match building it and leaving it out). On Deck and Continue
  Watching are bounded by PMS's defaults: 40 rows (`OnDeckLimit`), watched
  within 16 weeks (`OnDeckWindow`), new season premieres exempt
  (`OnDeckIncludePremieres`).
- **Jellyfin and Emby.** List rows carry the fields `Fields=` asks for.
  Lists called without `Limit` answer the reference server's default
  (Latest 20; movie recommendations five categories of eight; Similar 50),
  and Ether's own bounded page where the reference has none.

### Caches

- The catalogue keeps generation-stamped in-memory caches (titles, counts,
  browse pages, grandchildren, facets). Every catalogue write advances
  `writeGen`, so a cached page can never outlive the data it was built from.
  Sync markers in `catalog_meta` do not advance it.
- The catalogue writes continuously, so a total of 10k or more and a facet
  are served from the last count while they recount, at most every 30 s.
  Callers that use only a page's items (personalization) pass
  `BrowseQuery.NoTotal` and skip the count.
- The collections index snapshots the whole catalogue. Its freshness check
  and rebuilds run behind the read, an unchanged write generation vouches
  for it, and rebuilds are spaced by ten times the last one's duration.
- An extracted embedded subtitle track is cached per release and track, so
  a seek, a retry or the next session does not run ffmpeg over the file
  again.
- Plex Web's soft subtitles are streamed as ASS from the player's position
  instead of after a whole-release read: the cues arrive as the source is
  read, the first minute at full speed and then at 1.5x real time as a
  background read, with at most four such readers at once. A track already
  cached whole is answered from the cache.
- Identical concurrent work is collapsed with `internal/flight`, the
  singleflight used by TMDB reads, catalogue trends and stats.
- Per-viewer caches must key on the viewer and on the viewer-policy
  generation (`viewer_policy_epoch`), so a cached response is never served
  across viewers or after a policy change.
- Plex Home (`/hubs`, `/hubs/promoted`, `/hubs/sections/{id}`) is served
  from each viewer's cached screen (`internal/plex/home_cache.go`). The key
  is the viewer and the client's effective rendering profile, never the
  client identifier or session. A request reads one revision statement
  (`experience.ReadHomeRevision`: the viewer's latest watch-state write,
  playlists, watchlist, favorites, exclusions and the epoch). The viewer's
  revision, effective policy, the owner's settings and the Home layout are
  hard inputs: a change is rebuilt on the request. For a restricted viewer
  the epoch is hard too. For an unrestricted viewer the epoch, catalogue
  writes, the daypart and age refresh the screen behind the response. Home builds never wait on TMDB; what the mirror
  lacks is fetched behind the build and the catalogue write refreshes the
  screen.
- Work a read starts but does not wait for (refreshes behind the read,
  verdicts and prewarms) runs without the request's query stats
  (`store.DetachQueryStats`): it is not the request's statements, writes
  or lock waits.

## Results

Measured with `etherperf` on a copy of the Mac lab data: an 866 MB
`ether.db` with 264k catalogue rows, 51k history rows and 122k guide
programmes. Outbound network was blocked, and the Mac shared its CPU with
about ten other agents (load average 20–40), so warm numbers are noisy.

| request (real client sequence) | before | after |
|---|---:|---:|
| Plex /hubs/promoted, cold | 58–87 s | 1.9 s |
| Plex /hubs, /hubs/continueWatching, /library/onDeck, cold | 35–90 s | 0.13–0.2 s |
| Plex /hubs/sections/shows, cold | 61–89 s | 1.5 s |
| Plex /library/metadata/{movie}, cold | 12–22 s | 0.7 s |
| Plex /hubs/search?query=star, cold | 3.7–7.3 s | 0.08 s |
| Infuse /Items?searchTerm=star, cold | 10.7–26 s | 0.12–0.29 s |
| Jellyfin /Persons?searchTerm=star | 12.0 s, unrelated people | 0.13–0.3 s, matches only |
| Emby /Users/{id}/Items?SearchTerm | 7.6 s | 0.05–0.17 s |
| Swiftfin cross-library Recently Added | 20 s timeout | about 0.25 s once settled |
| Movie and show browse totals behind every listing | 0.4–2 s (11 s under load) | 20–30 ms |
| A short write behind a busy writer (store test) | waited the full 10 s busy timeout, then failed | at most one transaction |

The gate's first full run (54k against 540k synthetic rows) listed 50
endpoints that scaled with the library. Four remain in
`internal/app/testdata/perf_gate_debt.txt`: the Plex show listing's
per-series leaf counts, and three substring searches whose match counts
grow with the library.

| at 540k rows (gate) | before | after |
|---|---:|---:|
| Recursive episodes, 400 per page, with MediaSources | 270k statements, 10.3 s DB, 14.4 s | 600 statements, 14 ms DB, 54 ms |
| Silo /api/v2/catalog page | about 2,500 statements | one stateless signing pass |
| Series by date added, newest first | 0.6–0.9 s and a full count | bounded index walk |
| Console collections, layout, viewer home | past 15 s | flat, under 2 ms |
| Plex per-title section check (#203) | 60 µs, 205 allocations | 27 ns, 1 allocation |

## Rules for new code

1. A request path never scales with the library, the history or a guide.
   Page in SQL with `LIMIT` on an index, and prefer keyset (seek) pages for
   deep listings. Count with an index or a maintained counter.
2. Hydrate in batches. A listing loads its page's user data, artwork,
   resume points, ratings and facets in one query per kind (`IN` over the
   page's keys, chunked to 200), never one query per row.
3. Never call a provider, probe media or resolve releases on a request path.
   Read what the mirror has; if something must be fetched, fetch it behind
   the read (`catalog.hydrate`) and bound any wait.
4. Do not write on a read path. A write waits for SQLite's single writer,
   and the catalogue sync can hold it. Record sightings and impressions
   asynchronously, or batch them.
5. Cache per viewer with generation stamps: key on the viewer and the
   data generation, and test that a restricted viewer never receives another
   viewer's cached bytes.
6. Every new query on a hot path must have an index that serves its `WHERE`
   and `ORDER BY`. Check it with `EXPLAIN QUERY PLAN` (`etherperf audit` does
   this for you) on SQLite, and with `EXPLAIN` on PostgreSQL.
7. Add the endpoint to the replayed sequences (`internal/perfharness`), so
   the gate covers it.


## Xtream listings (v0.3.6)

IPTV apps download a whole library in one `get_vod_streams` or `get_series`
response. Ether keeps those listings encoded on disk
(`internal/frontend/kit/listcache`, under `<data>/cache/frontends/xtream`,
capped at 1 GiB, emptied at start), so a list request streams a file with an
`ETag`, `Last-Modified`, range support and a gzip copy instead of reading,
authorizing, signing and encoding the library again.

- **Exact policy.** A listing fills a slot (list kind, category, media origin,
  viewer). Its stamp is the experience's `ExportPolicy` for each listed view
  (the active viewer's restrictions, hidden titles, blocked facts, the plex.tv
  share predicate, the viewer's catalogue rules) and the host's config,
  account, safety, hidden-title and image-revocation generations. A listing is
  never served to a request with another stamp.
- **Bounded staleness.** A catalogue write retires the listing at once when the
  viewer's policy judges titles on their facts (a rating cap, share filter,
  blocked genres or keywords, section limits, own or household catalogue
  rules). Otherwise the listing is served for up to 30 minutes after it was
  read and rebuilt behind the response after a write, or once it is 2 minutes
  old.
- **Never slower than encoding.** With no servable listing a request follows the
  build as it writes, getting bytes as soon as each page is encoded. Signing in
  (`player_api`) and listing categories start the build ahead of the list. A
  slot is not rebuilt speculatively again until its last speculative build was
  fetched.
- **Artwork.** Poster URLs are content tags whose claims live ten minutes in
  memory. A listing keeps the page grants it signed, and serving it re-arms them
  for the requesting credential (`ImagePageRenewer`) at most every 4 minutes.
  Serve still rechecks credential, revocation and artwork policy.
- **Build.** Pages are read in key order, signed by up to four concurrent signers
  (signing is the slowest stage) and encoded in order.

Performance gate, large tier (249k movies, 128 MB listing), development Mac
under load from other jobs, `GOMAXPROCS=3`: building the movie listing takes
8.0–9.0 s (it was 10.5–11 s with one signer). A served `get_vod_streams`
answers in about 350 ms warm (2 statements), and `get_series` in about 14 ms.
Before this, every request encoded the library itself and hit the gate's 15 s
client cap on CI.

Reproduce with `ETHER_PERF_GATE=1 ETHER_PERF_GATE_CLIENTS=xtream go test ./internal/app -run 'TestPerformanceGate$'`.
The semantics are covered by `TestXtreamListingNeverServesStaleVisibility` and
`TestListingLifecycle`.
