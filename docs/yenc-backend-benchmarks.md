# Ether yEnc backend scenarios

Measured 2026-11-07, Apple M4, 10 logical CPUs, go version go1.27.3 darwin/arm64.
Decoder implementation: `eb272ee65ba3c2d0c5c4b5bf21467650061abca6`. Platform: `macOS-27.2-arm64-arm-64bit-Mach-O`.
Broad matrix: three 300ms samples per case; medians reported. Serial GOMAXPROCS=1; concurrency GOMAXPROCS=8.
Representative serial, buffer and bare-package results use five 1-second confirmation samples when present; the broad matrix uses three 300 ms samples.
Random 384/768 KiB payloads, 128/256-byte lines, CRC enabled. All backend runs were sequential.
The Ether cases include BodyReader, framing, limits and CRC; the source is an in-memory NNTP response, not a live socket.

**Units:** MB/s means decoded decimal megabytes per second. CPU time is process user + system time across all threads, including GC.
**100% busy CPU = one occupied core.** Divide by the machine's logical CPU count for whole-machine percentage.
CPU ms/GiB measures work per decoded byte; lower is better. Busy CPU alone does not measure efficiency.

**Variability:** 11 cases had a greater than 20% max/min throughput spread. This is a shared machine; interpret small differences cautiously.
Ranges below show the actual minimum–maximum samples, not confidence intervals. CPU cost is reported separately from wall throughput.

## Build combinations

| Scenario | CGO_ENABLED | GOEXPERIMENT | Selected decoder with fresh output |
|---|---:|---|---|
| Go SIMD | 0 | `simd` | RapidYenc Go (NEON) |
| C, experiment off | 1 | `(off)` | RapidYenc C (NEON) |
| SWAR | 0 | `(off)` | Ether SWAR |
| C, experiment on | 1 | `simd` | RapidYenc C (NEON) |

Caller-supplied exact/headroom buffers always select SWAR in the current integration, in every build combination.
Both cgo configurations selected the same C NEON decoder here. Their timing differences do not establish a different backend or a causal benefit from enabling the Go SIMD experiment.

## Representative CPU and throughput

768 KiB articles, 128-byte lines, fresh output; selected decoder.

| Scenario | Median MB/s | MB/s range | CPU µs/article | CPU ms/GiB | Allocations/article |
|---|---:|---:|---:|---:|---:|
| Go SIMD | 3,975 | 3,845–4,017 | 193.9 | 264.7 | 35 |
| C, experiment off | 3,510 | 3,391–3,533 | 220.5 | 301.1 | 35 |
| SWAR | 2,308 | 2,256–2,356 | 336.5 | 459.4 | 22 |
| C, experiment on | 3,517 | 3,268–3,540 | 220.4 | 300.9 | 35 |

## Fresh-output sizes and line lengths

Broad matrix (three 300 ms samples). Each cell is median MB/s and throughput ratio against forced SWAR **in the same build**. Repeated SWAR controls expose run variation.

| Article / line length | Go SIMD | C, experiment off | SWAR | C, experiment on |
|---|---:|---:|---:|---:|
| 384 KiB / 128 | 3,807 (1.68×) | 3,256 (1.41×) | 2,367 (1.03×) | 3,242 (1.43×) |
| 768 KiB / 128 | 4,041 (1.90×) | 3,522 (1.68×) | 2,277 (0.99×) | 3,479 (1.65×) |
| 384 KiB / 256 | 3,984 (1.51×) | 3,421 (1.33×) | 2,566 (0.99×) | 3,380 (1.31×) |
| 768 KiB / 256 | 4,244 (1.72×) | 3,626 (1.50×) | 2,603 (0.98×) | 3,609 (1.56×) |

## Output-buffer scenarios

768 KiB / 128-byte lines, Go SIMD build. These distinguish allocations and actual decoder selection; all four builds were measured in the raw files.

| Buffer | Actual decoder | MB/s | CPU ms/GiB | B/article | Allocations/article |
|---|---|---:|---:|---:|---:|
| fresh | RapidYenc Go (NEON) | 3,975 | 264.7 | 796,159 | 35 |
| exact | Ether SWAR | 2,353 | 456.4 | 1,088 | 21 |
| headroom | Ether SWAR | 2,269 | 473.2 | 1,088 | 21 |

`exact` has capacity equal to payload size; `headroom` adds 64 bytes. Both reuse the same caller-owned buffer on each read.

## Measured saturated concurrency: fresh output

768 KiB / 128-byte lines; fixed independent readers continuously decode. Each cell is aggregate MB/s / process CPU%.
Goroutine launch/join and runtime/GC are included; fixture/reader setup is excluded. These are not rate-limited playback measurements.

| Readers | Go SIMD | C, experiment off | SWAR | C, experiment on |
|---:|---:|---:|---:|---:|
| 1 | 4,279 / 120% | 3,753 / 117% | 2,427 / 111% | 3,665 / 116% |
| 4 | 11,433 / 387% | 10,225 / 388% | 6,966 / 363% | 8,941 / 387% |
| 8 | 12,539 / 540% | 12,742 / 568% | 8,732 / 594% | 13,194 / 581% |
| 16 | 15,955 / 613% | 14,600 / 626% | 9,780 / 676% | 14,479 / 624% |

## Measured saturated concurrency: exact output

768 KiB / 128-byte lines; fixed independent readers continuously decode. Each cell is aggregate MB/s / process CPU%.
Goroutine launch/join and runtime/GC are included; fixture/reader setup is excluded. These are not rate-limited playback measurements.

| Readers | Go SIMD | C, experiment off | SWAR | C, experiment on |
|---:|---:|---:|---:|---:|
| 1 | 2,350 / 100% | 2,381 / 100% | 2,593 / 100% | 2,391 / 100% |
| 4 | 6,950 / 371% | 8,350 / 373% | 8,850 / 372% | 8,228 / 372% |
| 8 | 10,651 / 702% | 11,159 / 730% | 11,251 / 702% | 10,809 / 688% |
| 16 | 12,112 / 749% | 11,546 / 744% | 12,861 / 771% | 11,288 / 719% |

## Bare-package diagnostic

768 KiB / 128-byte lines. Public RapidYenc decoder reused between complete responses; fresh output, or reusable output with 64 bytes of headroom.
This removes Ether's BodyReader and extra guard/metadata checks. It is a diagnostic comparison, **not a production-safe replacement**.

| Backend | Fresh MB/s | Fresh CPU ms/GiB | Reused MB/s | Reused CPU ms/GiB |
|---|---:|---:|---:|---:|
| Go SIMD | 5,511 | 189.4 | 6,626 | 162.1 |
| C, experiment off | 4,638 | 227.2 | 5,428 | 198.1 |
| C, experiment on | 4,654 | 225.7 | 5,420 | 198.5 |

## Projected decoder CPU for multiple reads

**Estimates, not rate-limited measurements.** Use serial 768 KiB / 128-byte / fresh-output CPU cost above.
Each independent, uncached reader decodes its payload once. Rates are decoded payload **Mb/s**, not MB/s or NNTP wire rates.

`CPU% = 100 × readers × (Mb/s per reader × 1e6 / 8) × CPU ns/article / article bytes / 1e9`

| Readers | Mb/s per reader | Go SIMD | C, experiment off | SWAR | C, experiment on |
|---:|---:|---:|---:|---:|---:|
| 1 | 25 | 0.08% | 0.09% | 0.13% | 0.09% |
| 4 | 25 | 0.31% | 0.35% | 0.53% | 0.35% |
| 8 | 25 | 0.62% | 0.70% | 1.07% | 0.70% |
| 16 | 25 | 1.23% | 1.40% | 2.14% | 1.40% |
| 1 | 100 | 0.31% | 0.35% | 0.53% | 0.35% |
| 4 | 100 | 1.23% | 1.40% | 2.14% | 1.40% |
| 8 | 100 | 2.47% | 2.80% | 4.28% | 2.80% |
| 16 | 100 | 4.93% | 5.61% | 8.56% | 5.60% |

At a fixed aggregate bitrate, additional readers do not automatically multiply CPU: use the total decoded rate.
These estimates exclude TLS, sockets, disk, application work and transcoding. Cache hits can avoid decoding; retries, prefetch and read amplification can add it.
Linear serial-cost projections do not capture the higher per-byte cost sometimes seen under saturated concurrency. They are useful below saturation, not an achievable-throughput guarantee.

## Reproduce and inspect

CPU metrics are implemented for macOS/Linux. A compatible C/C++ toolchain is required for cgo configurations.

```sh
bash scripts/bench_yenc_backends.sh /tmp/ether-yenc-results
```

The runner saves raw samples/metadata and calls the standard-library-only report script. Benchmarks do not change production decoder code.

- Go SIMD: [serial](yenc-backend-benchmarks/go-simd-serial.txt), [concurrent](yenc-backend-benchmarks/go-simd-concurrent.txt), [selection](yenc-backend-benchmarks/go-simd-selection.txt), [build environment](yenc-backend-benchmarks/go-simd-environment.json), [selected source files](yenc-backend-benchmarks/go-simd-files.txt), [bare package](yenc-backend-benchmarks/go-simd-direct.txt), [serial confirmation](yenc-backend-benchmarks/go-simd-serial-confirmation.txt), [bare-package confirmation](yenc-backend-benchmarks/go-simd-direct-confirmation.txt).
- C, experiment off: [serial](yenc-backend-benchmarks/cgo-no-simd-serial.txt), [concurrent](yenc-backend-benchmarks/cgo-no-simd-concurrent.txt), [selection](yenc-backend-benchmarks/cgo-no-simd-selection.txt), [build environment](yenc-backend-benchmarks/cgo-no-simd-environment.json), [selected source files](yenc-backend-benchmarks/cgo-no-simd-files.txt), [bare package](yenc-backend-benchmarks/cgo-no-simd-direct.txt), [serial confirmation](yenc-backend-benchmarks/cgo-no-simd-serial-confirmation.txt), [bare-package confirmation](yenc-backend-benchmarks/cgo-no-simd-direct-confirmation.txt).
- SWAR: [serial](yenc-backend-benchmarks/swar-serial.txt), [concurrent](yenc-backend-benchmarks/swar-concurrent.txt), [selection](yenc-backend-benchmarks/swar-selection.txt), [build environment](yenc-backend-benchmarks/swar-environment.json), [selected source files](yenc-backend-benchmarks/swar-files.txt), [serial confirmation](yenc-backend-benchmarks/swar-serial-confirmation.txt).
- C, experiment on: [serial](yenc-backend-benchmarks/cgo-with-simd-serial.txt), [concurrent](yenc-backend-benchmarks/cgo-with-simd-concurrent.txt), [selection](yenc-backend-benchmarks/cgo-with-simd-selection.txt), [build environment](yenc-backend-benchmarks/cgo-with-simd-environment.json), [selected source files](yenc-backend-benchmarks/cgo-with-simd-files.txt), [bare package](yenc-backend-benchmarks/cgo-with-simd-direct.txt), [serial confirmation](yenc-backend-benchmarks/cgo-with-simd-serial-confirmation.txt), [bare-package confirmation](yenc-backend-benchmarks/cgo-with-simd-direct-confirmation.txt).

[Buffer-reuse confirmation](yenc-backend-benchmarks/go-simd-buffer-confirmation.txt).
