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
   wait for it. It also writes `audit.md` and `results.json`.

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

## The CI gate: library scale

`TestPerformanceGate` (`internal/app/perf_gate_test.go`) runs in its own CI
job. `internal/perfharness/fixture.go` builds the same synthetic household
twice:

- once over a library of 50k catalogue rows;
- once over a library ten times larger: 250,000 movies, 10,000 shows,
  40,000 seasons and 240,000 episodes, 540k rows in all. Three viewers each
  have 20,000 history rows.

On each library it starts a real App and replays one client per protocol
(Plex, Infuse on Jellyfin, Emby, Silo, Stremio, the console). Then it
compares every endpoint across the two libraries.

| an endpoint fails when, on the 10× library | threshold |
|---|---|
| rows read grow | more than 2× + 200 |
| statements grow | more than 1.5× + 20 |
| database time grows | more than 5× + 100 ms |
| warm p50 | over 2 s |

An indexed path does about the same work on both libraries. A path that
walks the catalogue does ten times the work, so the gate does not depend on
the CI machine's speed. Endpoints that scaled when the gate was introduced
are listed in `internal/app/testdata/perf_gate_debt.txt`. The gate fails on
any endpoint that scales and is not listed, and reports listed endpoints
that stayed flat so they can be removed; the list only shrinks.

```sh
ETHER_PERF_GATE=1 go test ./internal/app -run TestPerformanceGate -v -timeout 60m
ETHER_PERF_GATE=quick go test ./internal/app -run TestPerformanceGate -v   # a 5k/50k pair, not ratcheted
```

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

### Caches

- The catalogue keeps generation-stamped in-memory caches (titles, counts,
  browse pages, grandchildren, facets). Every catalogue write advances
  `writeGen`, so a cached page can never outlive the data it was built from.
- Identical concurrent work is collapsed with `internal/flight`, the
  singleflight used by TMDB reads, catalogue trends and stats.
- Per-viewer caches must key on the viewer and on the viewer-policy
  generation (`viewer_policy_epoch`), so a cached response is never served
  across viewers or after a policy change.

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
