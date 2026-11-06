# W24 natural act breaks

Implementation branch: `feat/035-breakpoints`. Core addition: the existing W17 `SetupOwnerID` reserved-principal constant is copied additively into `internal/core/types.go` as the prerequisite for W17 identity fix `150dbd1c0` (locally `44699249bf`). No config changes. Original implementation commit: `3dec30bb18`; subsequent commit adds W22 policy inheritance and evidence.

## Detection and selection

Own signal combination and spacing code in `internal/markers/breaks.go`; ffmpeg performs decoding and measures its standard black/silence/scene signals. No Comskip code or dependency.

1. Native probe chapters first: explicit Act N, Act Break, Commercial Break or Part N labels, confidence 0.99. Generic chapters, intro and credits are excluded. A chapter offset must be at least 60 seconds from either edge.
2. Decode one video/audio stream: 160×90, 10 fps filter input, one codec/filter thread, Unix nice 10. Black must cover 98% of the pixels below 8% luminance for 0.3–3 seconds. Audio below −45 dB for at least 0.2 seconds must overlap at least 80% of that run. Combined confidence is 0.91; a scene event (scdet threshold 8) within 0.6 seconds raises it to 0.96. A cut alone or silence alone never inserts a break. Break offset is the black run midpoint. Admission threshold is 0.90.
3. Ordered dynamic programming chooses at most K points, maximizing confidence with a small preference towards balanced act lengths, subject to 60-second edge protection and minimum spacing max(3 minutes, half ideal act length). K is the era pods-per-half-hour count times ceil(programme duration / 30 minutes). Detection never generates even points.

Station-logo recognition is deferred (optional signal). Existing blackdetect captures fade-to-black candidates; scdet corroborates nearby visual transitions. There is no trained logo or semantic scene model.

## Background and persistence

One sequential worker per Live TV service/node. Runs at startup and then each minute; schedules next two hours; does not analyze ongoing items or programmes starting within one minute. Candidates are ordered by nearest air time across channels, deduplicated by episode, and capped at 64 admissions per pass. Each attempt has a deadline of ten minutes or one minute before air time, whichever is sooner. Successful title admission sleeps six hours, failed admission fifteen minutes. Retries are pruned. In-flight work cancels on service shutdown.

Playback prepares a dedicated source through the existing engine (debrid ranges, Usenet or configured local source); marks resolution speculative and uses the existing authenticated loopback input with `?background=1`, which reads at prefetch priority and excludes bytes from viewer statistics. No source credentials are stored in worker code or artifacts. Source revision pins prevent failover while analyzing. Dedicated prepared sources are retired after each attempt, including cache hits and errors.

Migration component `markers_breaks/1` registers:

```sql
CREATE TABLE markers_breaks_cache (
  release_key TEXT NOT NULL,
  version INTEGER NOT NULL,
  duration_ms INTEGER NOT NULL,
  points TEXT NOT NULL,
  analyzed_at INTEGER NOT NULL,
  PRIMARY KEY (release_key, version)
);
```

Release key combines core source revision (title, release, file name/size or local file identity), NZB payload SHA-256 and exact duration. No viewer/channel in the cache key. Empty completed results persist too; incomplete/failed decoding never persists. Version 2 invalidates results from the old path that accepted an early EOF with ffmpeg exit 0. Future threshold or coverage changes must bump the version.

Background publication uses compare-and-swap on saved channel JSON, invalidates existing guide caches and signals lineup changes. It traverses nested materialized sources, blocks and pins, honoring complete break-policy overrides. Even/between/disabled sections are excluded; non-null empty breaks disables inheritance. No source opening, detector or per-library metadata enrichment runs on requests. Existing schedule logic absorbs pods within the programme/block slot.

Saved Item JSON adds `breakPoints: [{atMs,source,confidence}]`, alongside `breakPointsMs`, and `breakReleaseId`/`breakRevision`. W22 consumes the timeline JSON. Programme emission clears source bindings for non-natural policies. Natural playback selects the planned release under normal channel-group accounting and pins its revision. If that exact source is unavailable or has changed, the decoder reports source unavailability rather than substituting unanalysed content into those act slices.

## Offline labelled evidence

Generated 200-second FFV1/PCM fixture: white video, a one-second black + silent interval at 70–71 seconds, and a black interval with continuing tone at 130–131 seconds. Unit signal fixture also includes a five-second black + silent run as a trap. Chapter fixture has one labelled act, a generic chapter and credits.

These are tiny synthetic fixtures, **not real-episode accuracy estimates**. Timing tolerance for the accepted act is ±0.5 seconds. Recall below is relative to the fixture's one known true act.

| Signal | TP | FP | FN | Precision | Recall |
| --- | ---: | ---: | ---: | ---: | ---: |
| Explicit chapters (separate chapter fixture) | 1 | 0 | 0 | 100% | 100% |
| Short black alone (video fixture) | 1 | 1 | 0 | 50% | 100% |
| Silence alone (video fixture) | 1 | 0 | 0 | 100% | 100% |
| Scene cuts alone (grouped by black interval) | 1 | 1 | 0 | 50% | 100% |
| Black + silence, scene corroboration | 1 | 0 | 0 | 100% | 100% |

Synthetic timeline: [synthetic-timeline.json](synthetic-timeline.json). Synthetic stills are review artifacts in thread storage, not repository media. Recreate them with the opt-in integration test and its external artifact directory. Black-with-audio candidate is rejected. Persist/reuse, source revision isolation, empty-result reuse, K-best spacing, background-only request invariant and nested complete override inheritance have lasting tests.

Measured FFV1 fixture decoder cost on this shared worker: **7.50 CPU seconds per video hour; 3.17 wall seconds per video hour**, excluding fixture generation and still extraction. This is low-resolution synthetic data, not an HD/HEVC benchmark. Full release decode and network cost remain dependent on configured source/codec.

Verified temporary BtbN LGPL static ffmpeg SHA-256: `2d8567d356373eb794221ec1d44a46610f7212a81d68375c8ea962db41be308a`. Binary/archive removed after validation. Production uses Ether's configured ffmpeg.

## Real-source validation handoff

The orchestrator was notified that branch `feat/035-breakpoints` is testable and requested to deploy it to the decades instance, where owner credentials remain. Real Cheers/Golden Girls/Friends/Frasier labels, precision/recall, natural-break stills and CHROMA 80's playback are pending that access. No real-source accuracy or successful real-ad demo is claimed by these synthetic results.

After deployment, enable channel `look.breakPattern.inside` and `breakPlacement: natural`, ensure upcoming episodes exist within two hours, then inspect channel/guide Item JSON for breakPoints and source binding. Obtain stills through that instance's existing configured source; do not copy source credentials to this worker. Compare every detected point ±1 second against manually labelled acts, including missed acts. Report per-show and pooled TP/FP/FN, and compute precision TP/(TP+FP), recall TP/(TP+FN). A new threshold must bump breakVersion before re-analysis.

## Checks

All Go commands are niced. `GOCACHE=/tmp/gocache` because sandbox denies the default cache; dependency download required escalation to populate three missing modules in the normal module cache.

- `go build -p 2 ./...`: passed.
- `CGO_ENABLED=0 go build -p 2 -o /tmp/w24-ether ./cmd/ether`: passed.
- `go vet -p 2 ./internal/markers ./internal/livetv ./internal/playback ./internal/app`: passed.
- `go test -count=1 -p 2 ./internal/markers ./internal/livetv`: passed (before final override changes; full livetv package 59 seconds).
- Focused updated invariants: `go test -count=1 -p 2 ./internal/livetv ./internal/playback -run 'Test(Break|NaturalBreak|Schedule|Channel|Pod)'`: passed.
- Generated-media integration: `ETHER_BREAK_TEST_FFMPEG=<verified-binary> ETHER_BREAK_TEST_ARTIFACTS=<artifact-dir> go test -count=1 -p 2 -v ./internal/markers -run TestBreakSyntheticDecode`: passed.

App wiring additions: `internal/app/livetv.go` supplies analyzer DB, configured ffmpeg and source factory; new `livetv_breaks.go` prepares, revision-pins, probes tail chapters and retires the dedicated source. No web, translations, embedded distribution or provider configuration changed.

Opt-in diagnostics: `app.Options.BreakEvidenceObserver` wires `markers.BreakAnalyzer.OnEvidence(ctx, BreakInput, BreakEvidence)`. The synchronous callback runs while the source lease is pinned/open, receives the catalogue Key, and can collect stills through the existing background loopback input in a local validation harness. It must neither close nor retain the lease. BreakEvidence exports raw chapter bounds/label-match flags, black/silence intervals, scene times with actual ffmpeg scdet percent scores, accepted points, cache/complete/decode flags and CPU/wall milliseconds. It contains no URL, source grant or media; BreakInput URL/Info/Close are excluded from JSON too. Default is nil, no public endpoint, no new auth/config. Cache hits explicitly indicate unavailable historical decode evidence. [Raw synthetic candidates](synthetic-candidates.json) demonstrate measured scene scores and the rejected black-with-audio interval.

W17 owner-identity correction carries only the admitted numeric owner ID into each ordered background task and re-resolves the real owner at execution. Setup stand-in admission is accepted only while setup remains open and unclaimed; request contexts are not retained. Focused identity-transition and context-detachment tests pass.

## Bandwidth policy follow-up (held until the quality run completes)

Default full pre-air scans are limited to **1 GiB/day** and **256 MiB/release**, with a conservative reservation of **2 × playable-file size + 8 MiB**. Unknown sizes fail closed. Files above **1 GiB** require explicit large-file opt-in as well as adequate release/day limits. Reservations are atomic in the shared store, so cluster nodes cannot multiply the allowance. Failed attempts remain charged; no refund can enable a retry storm. An over-reservation meter cancels decoding; sampled enforcement can overshoot by one short source-read interval. Chapters, cached full results and cached verified partial observations bypass full-scan admission. Container metadata probes still have small bounded head/tail costs.

Owner-adjustable `liveTV` JSON fields (no UI/limit prompts):

| Field | Zero/default | Other values |
| --- | --- | --- |
| `breakScanDailyBytes` | 1073741824 | Negative disables full scans |
| `breakScanReleaseBytes` | 268435456 | Negative disables full scans |
| `breakScanLargeReleaseBytes` | 1073741824 | Positive large-file threshold |
| `breakScanAllowLarge` | false | Explicit opt-in above threshold |

The source factory retains the normal 1080p selection caps. No alternate smaller encode is scanned just to infer another release's offsets. All used offsets remain pinned to their analyzed release and revision.

The on-air decoder samples its existing unmodified limited-range Y plane (at most 2304 pixels/frame) and signed PCM envelope before overlays. No extra decode, HTTP source request or media buffer is created. Short black + RMS silence observations use the existing 0.91 combined threshold; no unmeasured scene confidence is added. Seeks discard unfinished runs; a cut must visibly finish before publication. Only verified positive points from partial playings persist; partial/empty viewings never masquerade as complete scans. Persistence/publication runs in the background, with a bounded queue and bounded in-memory pending map. First airings without evidence retain between-programme pods; even mode still uses only the explicitly selected even policy.

Additional schemas: `markers_breaks/2` creates `markers_breaks_budget(day PRIMARY KEY,reserved_bytes,read_bytes)`; `/3` creates `markers_breaks_observed(release_key,version,duration_ms,points,updated_at), PRIMARY KEY(release_key,version)`. Partial observations merge under a store transaction and remain separate from full-scan records. `BreakEvidence.partial` identifies reused partial facts. Byte evidence includes `reservedBytes`, `readBytes`, and `readBytesKnown`; an unknown meter is never a measured zero. Read bytes count logical source bytes delivered by Ether's existing background loopback path, not raw provider-wire overhead or cache misses. The 2× reservation accounts conservatively for mux/probe/range overhead.

| Analysis mode | Additional source bytes per video hour |
| --- | --- |
| Existing-decoder opportunistic sampling | **0**, by construction |
| Cached result | **0** decode bytes; bounded metadata lookup/probe |
| Full scan under new defaults | Metered `readBytes × 3600000 / durationMs`; bounded by admission/reservation, real measurement pending |
| W17 4.55 GB remux quality run | Full source read; deliberately diagnostic, denied by default policy. Exact normalized bytes/hour pending duration/read evidence |

New lasting tests cover remux rejection, daily reservation exhaustion, chapter bypass, partial observation reuse, dark-with-audio rejection, seek gaps and unfinished fades. Config changes are exactly the four additive fields above. Further app wiring shares a single release-key helper between pre-air and live observation and attaches a byte counter to the leased background source. Playback adds one background byte counter per source generation; viewer accounting is unchanged.

Snapshot activation adds optional `Item.breakAvailableAfterMs`: freshly learned points apply only to programme starts after this saved boundary. Existing known-point airings defer replacement, and new facts do not insert cuts into the programme currently being watched. Full and partial caches are merged so later interrupted viewings cannot erase earlier verified acts. Tests cover stable current-airing metadata and later-airing reuse.

Opportunistic sampler microbenchmark on the Linux Ryzen 7 2700X worker (720p Y, stereo 48 kHz PCM, 30 fps): **5264 ns/frame, 0 B/frame, 0 allocations/frame**, approximately **0.57 core-seconds per video hour** for sampling. This excludes the already-required playout decoder and uses zero additional source bytes. Command: `nice -n 10 go test ./internal/markers -run '^$' -bench BenchmarkBreakTap -benchtime=200ms -count=1`.


Evidence media live only in thread storage. W17 copied synthetic stills to `/Users/braedonsaunders/.bb/thread-storage/thr_cp7svgfc62/v035/dev/pass/breakpoints/synthetic/`; local worker copies and a full report are at `/data/bb/thread-storage/thr_cv85pewpp2/w24-breakpoints/`. Only text reports and JSON measurements remain in the repository.


W17 runtime fixes integrated locally: bounded prepared-source ownership holds prevent idle reaping during an analysis job (`ff91dbc55`, locally `e30709fc18`), and a newly admitted setup owner invalidates stale retry backoff (`1d91476ee`, locally `53f3052273`). Normal producer reads still do not refresh abandoned viewer sessions; explicit Stop, cancellation, deadline, owner conversion and shutdown retain their revocation behavior. Focused tests pass. Both full and partial caches include the detector version for future threshold invalidation.


## Provider safety and decode optimization (final held set)

Account coordination is **in process**, keyed by the configured provider-account record ID. There are no coordination directories, credential hashes, or new cluster infrastructure. Admission is conservatively idle-only: any existing live source blocks new preparation; each account permits one analysis job and one analysis range. A live reader immediately cancels that account's in-flight background range and suppresses further background reads for its active window. API backoff and media 429/Retry-After are honored across all files on that account. Repeated 429s use exponential delays (30-second minimum, up to 30 minutes, never shortening a longer Retry-After) with bounded jitter. Starts are paced at least 500 ms apart. Existing cluster account-backoff broadcasts are reused; cross-node background concurrency leasing remains a follow-up because the existing update lease is a singleton upgrade primitive, not a reusable keyed account lease. Other independent installs are outside this product coordination scope, as directed.

Held prepared analysis sources use **8 MiB** playback buffers and a two-buffer queue, continuing through `ReadAtPrefetchContext` priority via a named `AnalysisReaderAt` capability. Ordinary viewer, subtitle and probe transport behavior is unchanged. Analysis roles survive detached prefetch work. Automatic duplicate index/trailer fills are skipped for the dedicated analysis source; its explicit bounded chapter probe remains. Source/loopback producers drain before result publication, so late errors cannot become successful full-cache entries. Expected consumer cancellation is excluded from provider-error counts.

`BreakEvidence` now reports provider request count, actual HTTP body bytes, errors and known/unknown status, including source preparation. Logical loopback-delivered bytes remain separately metered. A lasting engine test proves an 8 MiB analysis read produces **one** provider range request and exactly 8 MiB of body-byte accounting. Tests also verify per-account job/range exclusion, viewer cancellation, pacing, cross-file 429 suppression and exponential Retry-After behavior.

Coverage is proved with ffmpeg `progress=end`, decoded timestamps, and video frame count. Long passes must reach the expected end within two seconds; short confirmation windows within 250 ms. A full confirmation also requires the expected 10 fps output count within that tolerance. A successful process exit alone is insufficient. Any source/loopback failure makes the result Partial; verified earlier finished breaks can enter the separate partial cache, and a dark run ending at truncated EOF is excluded. The reproduced 129-second EOF against an expected 1497.455-second episode leaves **zero full-cache rows** and marks Partial. Cache version 2 excludes all old successes, including the invalid empty S1E5 record.

Pre-air decode uses the configured, already-probed hardware backend where available (VideoToolbox, VAAPI, QSV or NVDEC), with software fallback. Software uses one low-priority decoder/filter thread and grayscale decode flags; supported MPEG-2 decoding uses its low-resolution mode. The first filter scales to 160×90 and discards chroma with explicit limited-range luma normalization. No detection threshold changed.

For seekable MKV/MP4 inter-frame video, a first pass computes an 8 kHz mono, 20 ms RMS envelope and sparse keyframe luma. RMS intervals propose conservative windows padded by 1.5 seconds; keyframes do not gate acceptance. Windows merge, then use full-rate decode with the same black/silence/scene confirmation rules. Silence-rich material falls back to one full confirmation pass to avoid a seek storm; all-intra/nonseekable media use a single luma pass. The first pass still reads the entire muxed source: this optimization reduces decode work, **not the mux's first-pass byte requirement**. Candidate seeks can add bytes; budgets bound that cost. Batching reduces request count. Live sampling is the path with zero extra source bytes.

Offline cost/conformance comparison on the worker (200-second fixtures, software backend, same accepted break within 30 ms and dark-with-audio trap rejected):

| Fixture | Original CPU/h | Optimized CPU/h | Original wall/h | Optimized wall/h |
| --- | ---: | ---: | ---: | ---: |
| Tiny FFV1/PCM | 6.52 s | 5.92 s | 2.79 s | 2.56 s |
| 720p H.264/PCM, generated moving pattern | 239.35 s | 19.85 s | 195.28 s | 10.44 s |

The HD fixture improved CPU by **91.7%** and wall by **94.7%** in the latest run; an earlier run measured 87.3%/91.6%. These are local generated fixtures, not a claim about real remux/GPU performance. Fixture encoded sizes were 3,652,239 and 52,385,591 bytes. They use local files, so provider requests/bytes are not applicable (meter unknown). Raw `scan-cost.json` is in `/data/bb/thread-storage/thr_cv85pewpp2/w24-breakpoints/`; no fixture recordings or images are committed.

W17's valid pre-optimization Cheers S1E4 scan: 1499.915 s video, 421.662 CPU seconds and 576.966 wall seconds, approximately **16.87 CPU-minutes and 23.08 wall-minutes per video hour**. Source size was approximately 4.55 GB, roughly **10.92 GB of encoded source per video hour** for a complete first mux pass; exact provider request/body counters were unavailable in that old binary. Raw detected points contain 188950 ms at confidence 0.91. The 653.4–656.7 s black interval was 3.3 s and was excluded by the unchanged maximum. Human labels, full per-signal precision/recall and the final real-ad playback demonstration remain pending resumed safe validation; no threshold was tuned from unreviewed candidates.

Core additions are additive: W17's `SetupOwnerID` constant, two optional analysis reader/stats interfaces, and an internal boolean background-analysis context marker. It carries no caller identity or request values into detached work. Config additions remain exactly the four LiveTV byte-policy fields already documented. App wiring additionally supplies hardware inventory, provider account admission, error/drain hooks and provider statistics to the analyzer. No UI or protected style files changed.
