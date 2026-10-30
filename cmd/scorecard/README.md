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
Ether has one title, selected-source verification must be performed per session,
and the stubbed TMDB responses do not reproduce the complete offline parity fixture.
Do not treat its results as fairness-certified without checking catalogue,
viewer state, selected file, media bytes and client requests on both sides.

Setup hashes and probes the files once, before the handlers listen, and caches
probed MediaInfo in the private manifest. Variant lookup reads this prepared
metadata; it does not probe during a measured request. On the loopback listener,
`GET /__scorecard/sessions` lists each active playback's session ID, resolved
release ID, source ID and expected SHA-256. `GET /__scorecard/verify?id=SESSION`
reads the actual selected MediaSource and returns its SHA-256 and `match`;
run this outside the timed interval. For opaque versions, obtain a handle from the authenticated item's
`MediaSources` response, then query the fixture-only loopback endpoint
`GET /__scorecard/offer?handle=HANDLE` to correlate it with the seeded item,
release and expected SHA-256. An offer mapping by itself is not authorization
or proof that a handle was offered to the current viewer; only use handles from
the authenticated response. The broker session must bind that handle to the
same viewer, device, protocol and item. A comparison is valid only if that
selected release and source agree and the actual verified hash matches the
reference file. Diagnostics contain no credentials or file paths.
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

When each quantile has enough tail observations (n·p·(1−p) >= 10), the
collector emits approximate 95% binomial order-statistic intervals for
successful-response and first-byte percentiles. These intervals assume
independent samples and can understate uncertainty for correlated requests;
point-estimate winners are not significance tests. Missing intervals mean the
sample count is insufficient, not zero uncertainty.
The lab drivers attempt logout for every successfully authenticated session;
a logout failure is recorded without credentials and exits nonzero. Earlier
reports created before this check do not prove their reference grants were
revoked. The offline lifecycle test is `python3 -B
cmd/scorecard/lab_cleanup_test.py`.

`scorecard acceptance -input FILE` evaluates predeclared recovery, growth and
cleanup evidence. Recovery requires three consecutive verified successes from
distinct clients within 30 seconds, a continuously observed five-minute
follow-up, and at least 211 distinct matched trials before comparing p95 with
approximate order-statistic intervals. Overlap is inconclusive unless the full
interval of differences falls within a predeclared positive tolerance. RSS
checks require a complete two-hour series with at most 30-second gaps and apply
the predeclared final-window drift/slope bounds. Cleanup requires continuous
five-minute observation and exposes failure or incomplete coverage. Metadata
must match for host, fixed limits, media, profile, trace and quality; errors,
work accounting, cleanup and timing gaps disqualify a recovery winner. This
is an assertion evaluator, not a workload driver; inputs and manifest hashes
must be independently collected and verified. It cannot authenticate their
provenance or establish overall efficiency/robustness acceptance by itself.

### Isolated mixed workload (experimental)

`scorecard workload -config /private/path/workload.json` reads an operator-prepared
JSON configuration and emits redacted JSON evidence. Only use it after the
coordinator reserves isolated staging resources; a `reservation_id` is a label,
not proof of a reservation. The configuration supplies loopback `reference` and
`ether` targets with distinct matched cgroup limits, a pinned `ffmpeg` path and
`ffmpeg_sha256`, `manifest_sha256`, `cache_state`, `concurrency`, `repetitions`,
`timeout_ms`, named `flows`, and a scheduled `trace` of client/flow/`at_ms`.
Target variables and captured JSON values may contain credentials: protect the
configuration and never put credentials in client or flow names. Steps declare
kind, HTTP method, path or captured URL variable, expected status and response
byte limit; cleanup steps are attempted after errors and reported on failure.
Media steps require the same declared delivered-byte SHA-256 on both targets and
expected decoded duration. FFmpeg decodes the downloaded bytes before a stream
minute can be counted. This currently supports identical bounded media responses,
not distinct HLS encodes, independently verified source selection, injected
provider faults, or full video-quality assessment. The manifest hash and cache
state are declarations and require independent verification. A passing workload
alone does not establish comparative superiority.

The acceptance input also accepts `ttff_observations` and `performance_pairs`.
A verified, completed unresolved first-frame observation must be at most 5,000
ms (4,000 ms is the stretch target); a cached one must be at most 1,000 ms.
These are per-observation hard thresholds, not statistical certification.
Record resolve, probe, decision, first-byte and, for HLS, first-segment durations
when measured; absent phases remain incomplete and must not be summed as if
independent. Provide an independently established cache state and source match.
Matched, successful, quality-verified pairs for transcode start, seek to picture,
resume, subtitle/audio switch, browse latency and CPU per stream-minute fail
when Ether exceeds the reference in that observation. A single passing pair is
not proof of overall parity: collect repetitions and assess uncertainty.

For disposable TV fixtures, optional `-silo 127.0.0.1:18098`,
`-webdav 127.0.0.1:18099`, and `-stremio 127.0.0.1:18100` mount the real
registered handlers, backed by the same fixture catalogue and broker. These
extra listeners accept loopback IPs only; a separately managed recording proxy
can provide controlled LAN access. The private fixture data directory contains
`scorecard-app-credentials.json` (mode 0600), with distinct WebDAV, Stremio and M3U
viewer grants. It is a secret: do not commit, log, or include it in captures.
Silo uses the seeded local viewer login. Stremio clients may require HTTPS and
app-origin configuration not provided by this fixture. A mounted handler does
not establish that end-to-end playback is certified. Plex is deliberately not mounted.

`python3 -B cmd/scorecard/sync-state.py` accepts private JSON on stdin with
`reference_url`, `target_url`, `reference_user`, `reference_password`,
`target_user`, `target_password`, and optional `apply: true`. The target must
be a disposable loopback fixture. It reads the reference viewer's movie and
episode UserData, matches TMDB movie IDs and show/season/episode IDs, and
rejects missing or conflicting duplicate-version state before changing anything.
A dry run is the default. Apply uses the target's authenticated played,
favorite, and playback-stop APIs and verifies the resulting state, surfacing
errors and logout failures. Resume seeding can open real fixture playback and
must occur before timed runs. An unsupported state (for example a reference
resume position the target refuses to retain), incompatible viewers, or
ambiguous matching is a failure, not evidence of alignment; a failed apply may
have made partial changes, so recreate or inspect the disposable fixture before
rerunning. Supply credentials only over private stdin; never copy or print the
reference lab credential file. Each invocation aligns one reference viewer to
one target viewer; multiple viewers need separately provisioned fixture users.

`-dlna 127.0.0.1:18101` additionally mounts the real DLNA handler with the
fixture's disposable viewer selected and SSDP discovery disabled. A recording
proxy must enforce LAN-only access and block `/__scorecard/` diagnostics; the
upstream sees the proxy as loopback. HTTP proxying alone does not provide SSDP
multicast discovery, so discovery and end-to-end DLNA playback require a separate
isolated LAN test.

`-m3u 127.0.0.1:18102` mounts the registered M3U/XMLTV surface using its
separate viewer grant. Channel and guide coverage require explicitly seeded
Live TV sources; an empty playlist only proves the handler and grant work.

When duplicate reference versions have different state, an explicit
`selected_reference_variants` map from movie TMDB ID to lowercase SHA-256
and `verified_manifest` path selects one reference item whose source path
matches the independently verified manifest. The chosen hashes are included
in the result. This is a per-version comparison only; it does not align the
other reference versions to Ether's title-wide state.

The loopback-only fixture diagnostic `/__scorecard/traces` reports resolver
candidate rejection categories and, for scorecard sources, fixture SHA-256.
It omits release names, paths, arbitrary errors and client credentials. The
trace ring is process-wide; isolate requests before associating a trace with a
specific test. Recording proxies must deny every `/__scorecard/*` route.

The loopback-only `/__scorecard/detail-offer?handle=HANDLE` joins an
unexpired, surface-specific detail handle to its fixture item, release,
source revision and expected SHA-256. The caller must obtain the handle from
an authenticated detail response and independently verify the actual opened
source; this diagnostic is not an authorization check and does not prove the
requesting viewer or device owns the handle. Recording proxies must block it.
