# RapidYenc NNTP integration — issue #346

Validated 2026-11-07 on Apple M4, macOS ARM64, Go 1.27.3.
Branch: `perf/yenc-pure-go-simd`; PR base:
`c9297236e827b41b5a4833af9899fb2b10ad0067` (`release/0.3.5`).
The production decoder last changed in `d928b5b91`; `eb272ee65` adds the
repeatable mutation matrix, differential fuzz target and CI coverage.

The [backend benchmark report](yenc-backend-benchmarks.md) records the current
sizes, line lengths, output buffers, reader counts, process CPU cost and raw
samples. It replaces the measurements from before the bulk-feed adapter.

## Implementation and compatibility

`internal/usenet` calls the public decoder in pinned
`github.com/mnightingale/rapidyenc v0.0.0-20261008131718-6657b7ca854e` at the
NNTP BODY response boundary. Synchronous and pipelined requests share this
path. `internal/yenc` retains its portable implementation and explicit output
buffers still use SWAR, preserving caller buffer ownership and reuse.

The guard validates `=ybegin` and `=ypart` before forwarding allocation hints.
It then forwards whole buffered lines, preserving dot-stuffing and the real
NNTP terminator. `BodyReader` retains connection framing, deadlines, draining
and following-response isolation. LF-only articles use the line path with
CRLF normalization. Output hints and wire bytes remain bounded; the last
8 KiB are retained for trailer parsing. Header/trailer sizes, part number,
offset and bytes produced are cross-checked against RapidYenc metadata.
`yenc.VerifyChecksum` applies Ether's size, CRC and SkipCRC rules.

Payload errors can be rejected after decoding instead of before forwarding.
The old report's claims about counting every escape and rejecting payload
errors early no longer describe this implementation. An upstream Go parser
panic is recovered as `ErrMalformed`, and that decoder is discarded rather
than pooled. Completed decoder state is cleared before pooling so media output
is not retained.

Without a usable CRC, corrupt input can be accepted differently by the two
parsers. Differential tests permit one-sided acceptance in that case; they do
not establish identical malformed-input semantics. They compare results when
both accept, check connection alignment on errors, and the deterministic
mutation test also checks accepted CRC-protected data against the original.

## Backend selection

| Build | Fresh-output decoder |
|---|---|
| CGO off, SIMD experiment enabled, supported AMD64/ARM64 SIMD kernel | RapidYenc Go SIMD |
| CGO on, desktop macOS/Linux AMD64 or ARM64, or Windows AMD64 | RapidYenc C backend |
| Neither supported backend, or generic selected kernel | Ether SWAR |

Android/iOS are excluded from the C path, as are unsupported cgo targets such
as Windows ARM64. C is selected even when the Go SIMD experiment is enabled.
Caller-provided exact/headroom output buffers select SWAR in every build.
The shipped server configuration remains `CGO_ENABLED=0 GOEXPERIMENT=simd`.

## Current checks

| Check | Result |
|---|---|
| Usenet, yEnc and stream suites: CGO off / experiment off | Pass |
| Same suites: CGO off / SIMD | Pass |
| Same suites: CGO on / experiment off | Pass |
| Same suites: CGO on / SIMD | Pass |
| Race: CGO off / SIMD, macOS ARM64 | Pass |
| Race: CGO on, experiment off and on | Pass in both builds |
| Deterministic mutation matrix | Six seeds × 30,000 mutations per RapidYenc build |
| Differential fuzzing: native Go SIMD and C | 100,000 executions each, pass |
| AMD64 under Rosetta: Go SIMD AVX2, C SSSE3, AVX2-disabled generic fallback | Pass |
| Backend build-tag matrix | 15 cases, including Android/iOS and unsupported cgo targets, pass |
| Full-module SIMD vet; affected-package C vet | Pass |
| Shipped server cross-builds: Linux, macOS, Windows × AMD64/ARM64 | All six pass |
| `goreleaser check` | Pass |
| Full module, SIMD and portable | Failures outside the decoder; details below |

The local Go 1.27.3 toolchain supports CGO-disabled race builds on **macOS**.
This was verified with the Go SIMD path selected, rather than assuming a
CGO-enabled race run covers it. Linux still reports `go: -race requires cgo`;
therefore CI runs this SIMD race check on macOS. C race runs instrument the
Go adapter, not the bundled native archive. No native archive sanitizer claim
is made.

CI now runs 100,000-execution differential fuzz campaigns for Go SIMD and C on
Linux, the six deterministic mutation seeds in the existing backend tests,
and the pure-Go SIMD race suite on macOS. The local results above do not imply
that a new hosted CI run has finished.

[Machine-readable results](yenc-validation-results/summary.json),
[Go SIMD fuzz output](yenc-validation-results/fuzz-count-go-simd.log),
[C fuzz output](yenc-validation-results/fuzz-count-cgo.log), and
[Go SIMD race output](yenc-validation-results/go-simd-race.log) are committed
alongside the other affected-suite, race, vet, build and Rosetta logs.

## Full-module failures

Both current full-module runs completed. SIMD: six failed tests in five
packages; portable: eight failed tests in six packages. Six failures are shared:

| Package | Test | Observed failure |
|---|---|---|
| `cmd/plexparity` | `TestInventoryReportsUncoveredRoutes` | Plex GET routes absent from coverage inventory |
| `internal/frontend` | `TestFrontendImports` | Forbidden `internal/core` import |
| `internal/spike` | `TestPlexIntegration` | Playback timeline offset/duration zero |
| `internal/state/prismcompat` | `TestPrismStateAndPlexwatchSchema/sqlite_from_state2` | Migration `state/8` missing `state_history` |
| `internal/transcode` | `TestIntegrationMatroskaContractPillarboxesTenBitSDR` | Pixel values differ from expected values |
| `internal/transcode` | `TestIntegrationBurnedTextSubtitleRenders` | Subtitle cue not detected in rendered frame |

Two additional failures appeared in the portable full run:

- `cmd/plexparity/TestSeed`: empty live-TV fixture guide. In three isolated
  attempts **per build**, both SIMD and portable had one pass and two failures.
- `internal/store/TestWriteGateHandsOverPromptly`: a short write took 99.68 ms,
  exceeding its 90 ms threshold. Ten isolated attempts per build all passed.

These packages are unchanged from the PR base. The seven earlier documented
failures were reproduced across the current full/isolated runs; the store
failure is an additional timing-sensitive observation. Passing isolated reruns
do not erase the recorded full-suite failure. **The full suite is not green.**

See [SIMD failure excerpts](yenc-validation-results/full-go-simd-failures.txt),
[portable failure excerpts](yenc-validation-results/full-swar-failures.txt), and
the isolated recheck entries/logs in the results directory. Local transcoding
checks used FFmpeg 9.0.1 from PATH.

## Reproduce

Run the validation commands before benchmarks so workloads do not overlap:

```sh
for task_cgo in 0 1; do
  for task_exp in '' simd; do
    CGO_ENABLED="$task_cgo" GOEXPERIMENT="$task_exp" \
      go test -count=1 -timeout=5m ./internal/usenet ./internal/yenc ./internal/stream
  done
done
# macOS: pure-Go SIMD race coverage
CGO_ENABLED=0 GOEXPERIMENT=simd go test -race -count=1 -timeout=8m ./internal/usenet ./internal/yenc ./internal/stream
for task_exp in '' simd; do
  CGO_ENABLED=1 GOEXPERIMENT="$task_exp" \
    go test -race -count=1 -timeout=8m ./internal/usenet ./internal/yenc ./internal/stream
done
CGO_ENABLED=0 GOEXPERIMENT=simd go test ./internal/usenet -run '^$' -fuzz '^FuzzRapidDifferential$' -fuzztime=100000x -parallel=2 -timeout=3m
CGO_ENABLED=1 GOEXPERIMENT= go test ./internal/usenet -run '^$' -fuzz '^FuzzRapidDifferential$' -fuzztime=100000x -parallel=2 -timeout=3m
CGO_ENABLED=0 GOEXPERIMENT=simd go test -json -count=1 -timeout=10m ./...
CGO_ENABLED=0 GOEXPERIMENT= go test -json -count=1 -timeout=10m ./...
CGO_ENABLED=0 GOEXPERIMENT=simd go vet ./...
CGO_ENABLED=1 GOEXPERIMENT= go vet ./internal/usenet ./internal/yenc ./internal/stream
for target_os in linux darwin windows; do
  for target_arch in amd64 arm64; do
    CGO_ENABLED=0 GOEXPERIMENT=simd GOOS="$target_os" GOARCH="$target_arch" \
      go build -trimpath -o "/tmp/ether-$target_os-$target_arch" ./cmd/ether
  done
done
goreleaser check
bash scripts/bench_yenc_backends.sh
```

The benchmark runner now records and verifies `go env` and selected Go source
files for every build. This prevents the previous ad hoc zsh loop's unsplit
settings (for example `CGO_ENABLED='0 simd'`) from silently defaulting to CGO
and producing incorrectly labeled rows. The previous published four-build table
was incorrectly labeled; the new report supersedes it.

Docker image assembly remains unverified because the local Docker daemon is
stopped. Native Windows installer execution and installer signing/notarization
were not performed. Cross-builds and configuration validation do not establish
those packaging results.
