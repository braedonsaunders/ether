# HTTP latency measurement (experimental)

This opt-in tool measures full response time for configured GET requests. It is
only one component of the Prism scorecard; it does not establish fixture parity,
profile compatibility, streaming startup, seek, features, or device experience.
It makes `samples` total requests to each server at each specified concurrency,
with a 30-second timeout. Non-2xx responses and transport errors are counted;
any errors make the comparison inconclusive. p50 and p95 use nearest rank among
successful responses. Reference is measured before Ether, so repeat and reverse
order externally when assessing cache or time effects. A mixed result means
one server won p50 and the other p95. No statistical significance is implied.

Set `SCORECARD_CONFIG` to a private JSON file outside the repository, or pass
`-config`. Supply endpoint URLs and authentication headers separately for each
server. Output contains names and aggregate measurements, never URLs or headers.
Use only read-only endpoints; a GET endpoint can still have side effects. Verify
that the servers, viewers, media and operations are equivalent before comparison.

```json
{
  "samples": 100,
  "concurrency": [1, 10, 50],
  "scenarios": [{
    "name": "home",
    "reference": {"url": "http://reference/Users/example/Views", "headers": {}},
    "ether": {"url": "http://ether/Users/example/Views", "headers": {}}
  }]
}
```

Run `go run ./cmd/scorecard > report.json`. Keep the configuration private; it
may contain credentials. This tool does not authenticate or discover IDs.

## Experimental local-file fixture

The `scorecard` build tag adds a disposable local-file resolver and engine and
`scorecard serve`. It must not be used in a production build. Build with
`CGO_ENABLED=0 go build -tags scorecard ./cmd/scorecard`. Provide a fresh data
directory containing only `scorecard-files.json`, and a JSON reference Items
listing with ProviderIds, Path, media metadata and UserData. The source maps
movie TMDB IDs and show TMDB/season/episode to files; distinct hashes yield
distinct release IDs. The loader checks every hash before accepting requests.
The listing, manifest and directory are disposable; never point it at a real
Ether data directory. Set `SCORECARD_USER` and `SCORECARD_PASSWORD` for a test
viewer. `serve -data DIR -items ITEMS.json` listens on loopback ports supplied
with `-jellyfin` and `-emby`; bind both to loopback in a lab. It does not start
the normal background worker lifecycle, and does not publish a Plex identity.

`prepare-manifest.py` accepts a reference listing, a host media directory,
ffprobe and the reference container name. It compares each host hash with the
hash calculated inside the reference container before writing the manifest.
Mount the same media at `/media:ro` in the scorecard container. Verify every
reference separately and retain the resulting manifest as evidence.

This remains an experimental harness: state copied from the listing is limited
to played/progress, reference versions can have independent state even though
Ether has one title, broker version selection is not yet verified, and the
stubbed TMDB responses do not reproduce the complete offline parity fixture.
Do not treat its results as fairness-certified without checking catalogue,
viewer state, selected file, media bytes and client requests on both sides.

Setup hashes and probes the files once, before the handlers listen, and caches
probed MediaInfo in the private manifest. Variant lookup reads this prepared
metadata; it does not probe during a measured request. On the loopback listener,
`GET /__scorecard/sessions` lists each active playback's session ID, resolved
release ID, source ID and expected SHA-256. `GET /__scorecard/verify?id=SESSION`
reads the actual selected MediaSource and returns its SHA-256 and `match`;
run this outside the timed interval. A comparison is valid only if the requested
variant ID, selected release and source IDs agree and the verified hash matches
the reference file. Diagnostics contain no credentials or file paths.
`GET /__scorecard/broker` links the protocol PlaySessionId to `playback_id`
and reports the broker plan decision and stream format. When a transcoder is
active, it also reports the actual transcoder decision and copy flags. Treat
an absent active transcoder decision as unknown for an encoding claim; the plan
alone does not prove what ran. `client_id` and `item_id` in the session listing
allow independent correlation. For a Jellyfin DeviceId `X`, the broker client
ID is `8:jellyfinX`.
`GET /__scorecard/events` returns only allowlisted playback/transcoder failure
messages and fixed error categories from the in-memory log ring. It omits raw
attributes, credentials, media paths and arbitrary error text. Events are
process-wide; isolate a request in time before using them as evidence.

For a route-level feature inventory, run `scorecard inventory -protocol
jellyfin -spec PATH` (or `emby`). The command verifies the input against the
repository's pinned OpenAPI SHA-256, mounts the protocol router with a fake
identity, and reports every spec operation with exact method/template matching.
A missing registration is a review candidate: alternate routes, wildcard
handlers, licensing and feature applicability can change the conclusion. A
registered route does not establish complete behavior.

Recorded MediaBrowser text GETs can also drive a scenario: set `profile_file`
to a JSON profile under `internal/conform/profiles`, and set `flow` and `probe`
to the recorded names. In this mode each target `url` is a base URL and each
`variables` object supplies its own ID and device substitutions; `headers` can
supply the authenticated token. Missing variables, binary, fixture-only and
non-GET probes are rejected. Response content and state still need separate
alignment checks before timings can be interpreted as fair comparisons.

Each measurement also reports first-response-byte p50/p95 (from Go's HTTP trace)
and a winner for each percentile. Set the same positive `read_limit` on both
targets to stop after at most that many body bytes; `completion_scope` records
this explicitly. Use an explicit Range header when testing a bounded stream.
First byte is an HTTP response measurement, not decoded first frame; a first HLS
segment requires a separately configured segment request after negotiation.

For full JSON response probes, set `spec_file` to the pinned OpenAPI JSON,
`protocol` to `jellyfin` or `emby`, and each target's `spec_path` to the
corresponding path in that spec (profile-based requests fill this automatically).
Every successful response is checked against the documented status, content
type and schema, including fields marked required; `schema_checked` and
`schema_errors` distinguish checks from untested responses. Results with schema
errors have an inconclusive latency winner. Validation occurs after the timed
read and may affect throughput. It is a spec check, not evidence that every
client's strict decoder has run; schemas may leave client-required fields
optional. Bounded streaming responses cannot use this mode, and JSON responses
larger than 16 MiB count as schema errors.
Undocumented method/path combinations are counted separately as
`schema_uncovered`; they are not reported as required-field failures. Either
schema failures or uncovered operations make the latency winner inconclusive.
`lab-profile-browse.py` is an opt-in lab driver that reads credentials only
from stdin and refuses to time resume/next-up responses whose visible item
projections differ. It can compare views by collection type despite different
labels; those timings still require the stated workload caveats.
For fixture-only session failures, `/__scorecard/events` may also report a
`scorecard session stage failure` with stage `plan`, `open`, or `start_stream`
and a fixed error category. An absent event does not identify the failure;
only sentinel errors are classified, and arbitrary error text is not exposed.

For bounded open-loop tests set `rate_per_second` to the offered arrival rate,
`repetitions` to at least two, and `timeout_ms` explicitly. Requests retain their
intended schedule even when all workers are busy; queue p50/p95/p99 and
`all_scheduled_*` include scheduler delay and failed/timed-out work. The
successful-response p50/p95/p99 and first-byte percentiles exclude failed HTTP
requests; error and timeout counts and valid-success throughput are reported
alongside them, and any error makes the winner inconclusive. Repetitions
alternate reference-first and Ether-first. `cache_state` is an explicit label,
not an automatic cache flush. With no rate, the tool uses closed-loop load and
cannot measure omitted arrivals; queue values are not meaningful. This is a
bounded request generator, not yet a mixed-playback or resource-efficiency
soak harness, and it does not establish statistical significance by itself.

For preliminary cgroup-v2 resource evidence, set an absolute `cgroup` path on
both targets. The run rejects different or unlimited CPU/memory limits and
different effective CPU sets. Before and after each target run it samples
hierarchical CPU and block I/O counters, cgroup memory, and best-effort process
RSS/FD/thread/child counts. It reports CPU-seconds per valid successful HTTP
operation when the counters are available. Run the generator outside both
server cgroups and use fresh, isolated cgroups; otherwise its own work or other
jobs contaminate the measurement. An HTTP success is not proof of correct
media quality or an SDK-valid response. Counter regressions and incomplete
process snapshots make the latency winner inconclusive.

For growth and peak observation use `scorecard resources -cgroup /absolute/path
-interval 1s -samples 7201` in a separately coordinated run. This emits a
machine-readable cgroup-v2 time series; interrupted runs have `complete=false`.
Memory.current includes cache, RSS sums can double-count shared pages, and
memory.peak may predate the run unless the cgroup is fresh. GPU, per-container
network I/O and Go goroutine counts are explicitly unmeasured. FFmpeg children
are included only if they remain in the measured cgroup. The tool does not yet
measure CPU per verified stream-minute, perform mixed churn/failure injection,
or control cold caches; no extended soak should run without aligned fixtures,
resource isolation and the coordinator's reservation.
