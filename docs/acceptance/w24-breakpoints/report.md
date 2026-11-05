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

Release key combines core source revision (title, release, file name/size or local file identity), NZB payload SHA-256 and exact duration. No viewer/channel in the cache key. Empty completed results persist too; incomplete/failed decoding never persists. Version 1 permits future threshold changes to invalidate old analyses.

Background publication uses compare-and-swap on saved channel JSON, invalidates existing guide caches and signals lineup changes. It traverses nested materialized sources, blocks and pins, honoring complete break-policy overrides. Even/between/disabled sections are excluded; non-null empty breaks disables inheritance. No source opening, detector or per-library metadata enrichment runs on requests. Existing schedule logic absorbs pods within the programme/block slot.

Saved Item JSON adds `breakPoints: [{atMs,source,confidence}]`, alongside `breakPointsMs`, and `breakReleaseId`/`breakRevision`. W22 consumes the timeline JSON. Programme emission clears source bindings for non-natural policies. Natural playback selects the planned release under normal channel-group accounting and pins its revision. If that exact source is unavailable or has changed, the decoder reports source unavailability rather than substituting unanalysed content into those act slices.

## Offline labelled evidence

Generated 3-minute FFV1/PCM fixture: white video, a one-second black + silent interval at 70–71 seconds, and a black interval with continuing tone at 130–131 seconds. Unit signal fixture also includes a five-second black + silent run as a trap. Chapter fixture has one labelled act, a generic chapter and credits.

These are tiny synthetic fixtures, **not real-episode accuracy estimates**. Timing tolerance for the accepted act is ±0.5 seconds. Recall below is relative to the fixture's one known true act.

| Signal | TP | FP | FN | Precision | Recall |
| --- | ---: | ---: | ---: | ---: | ---: |
| Explicit chapters (separate chapter fixture) | 1 | 0 | 0 | 100% | 100% |
| Short black alone (video fixture) | 1 | 1 | 0 | 50% | 100% |
| Silence alone (video fixture) | 1 | 0 | 0 | 100% | 100% |
| Scene cuts alone (grouped by black interval) | 1 | 1 | 0 | 50% | 100% |
| Black + silence, scene corroboration | 1 | 0 | 0 | 100% | 100% |

Synthetic timeline: [synthetic-timeline.json](synthetic-timeline.json). Stills: [before](synthetic-69s.png), [at break](synthetic-70.5s.png), [after](synthetic-72s.png). Black-with-audio candidate is rejected. Persist/reuse, source revision isolation, empty-result reuse, K-best spacing, background-only request invariant and nested complete override inheritance have lasting tests.

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
