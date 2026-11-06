# W25: bundled FFmpeg and process lifetime audit

Test host: bb-worker-server, Linux x86_64. Baseline: `14911c13da4080d6d1d7feba8a9cf8e56749b483`.
Toolchain: `/data/bb/toolchains/go1.27.3/bin/go`. Jobs run with `nice -n 10`, Go package concurrency 2.

## Reproduction

`scripts/fetch-ffmpeg.sh linux-amd64` downloaded and checksum-verified the exact
`packaging/ffmpeg.lock.json` artifact: Jellyfin 8.1.3-1, archive SHA256
`b86dc023c64a5a9a410e6e3a938a970460214fae78f31c1553c9f88f1c289b6a`.
Binary version: `8.1.3-Jellyfin`.

The unmodified baseline's complete `TestIntegrationMatroskaOpenGOPCopyTimeline`
passed twice (98.49 s and 105.45 s), including short-gop, short-gop-same-params,
long-gop and long-gop-mid, each with swap, same-release and seek. The hardened
helpers also passed the matrix (96.71 s). No bundled-build hang was observed.
A monitor checked the second baseline run for ffprobe processes older than ten
seconds; none qualified for stack/input capture. There is consequently no hung
stack or evidence of an upstream Jellyfin bug to report.

The W11 BtbN 8.1 hang remains unexplained by this reproduction. The implicated
`videoPackets` helper already selects `v:0`, uses `-show_entries packet=...`,
and reads a finite local file without `-read_intervals`. `os/exec.Output` drains
stdout and stderr concurrently. A missing stream selection, read-interval seek,
or an undrained pipe is not established as its cause. No media/probe arguments
or Plex output contracts were changed speculatively.

## Confirmed defects and changes

Direct media-tool callers outside transcode bypassed its guardian: context
cancellation killed only the direct child, leaving descendants and inherited
pipes alive. Several also had no intrinsic deadline. `internal/mediaproc` now
owns those commands, imposes deadlines, kills Unix process groups or Windows
kill-on-close jobs, and bounds post-exit pipe waits to two seconds. Windows
processes start suspended, join their job, then resume; a child cannot escape
before assignment. The existing platform ownership code was moved into this
package and reused by transcode's guardian; no second platform implementation
was retained. Linux Pdeathsig remains confined to the guardian's locked OS
thread; ordinary scheduler threads must not own a parent-death signal.

The reel progress scanner could stop on an oversized/invalid line and then
wait for an encoder still writing progress to its unread pipe. Its error path
now closes the read end, cancels the tree and waits before returning the scanner
error. Normal progress and media arguments are unchanged.

Finite probes get 30 s, background conversions get 15 min, and existing tighter
caller deadlines win. Session/streaming encoders retain their progress, idle and
request cancellation mechanisms with a final 24 h context ceiling. Embedded
subtitle extraction now has 15 min; paced subtitle streaming has 24 h.

`mediaproc.Output` drains both streams, retains at most 64 MiB stdout / 64 KiB
stderr, and returns an error if successful stdout would be truncated.
`CombinedOutput` drains both streams retaining 64 KiB diagnostics. Streaming
callers retain their explicit pipe/writer contracts. The guardian still has its
existing four-second pipe wait bound.

## Invocation audit

G = Unix process group / Windows job, including descendant cleanup after normal
exit. D = concurrently drained, B = bounded retention, S = streaming to consumer.
The table covers every production media invocation found under `internal` and
`cmd`, including calls through the injectable transcode Runner. Non-media
commands (service launch, git, build, mount, notifications, yt-dlp, GPU telemetry)
are outside this ffmpeg/ffprobe audit.

| Call site | Timeout before → after | Tree before → after | stdout / stderr before → after |
|---|---|---|---|
| transcode/probe.go `probeOnceMode` | 20/60 s → same | G → G | D JSON / D → same |
| transcode/aac_pce.go | 3 s → same | G → G | D packet JSON / discard → same |
| transcode/discover.go `toolVersion` | 10 s → same | G → G | D / D → same |
| transcode/hwaccel.go feature discovery | 15 s → same | G → G | D / D → same |
| transcode/hwaccel.go | 15/20 s → same | G → G | discard / D → same |
| transcode/hardsub.go | 8 s → same | G → G | D raw frame / discard → same |
| transcode/bif_thumbs.go | frameTimeout → same | G → G | D frame / B tail → same |
| transcode/slate.go | 15 s → same | G → G | discard / D → same |
| transcode/audio_match.go | analysis budget → same | G → G | B samples / discard → same |
| transcode/picture_crop.go | 5 s → same | G → G | B samples / discard → same |
| transcode/manager.go `extractSubtitle` | caller only → 15 min | G → G | S / B tail → same |
| transcode/manager.go `streamSubtitle` | caller only → 24 h | G → G | S / B tail → same |
| transcode/session_encoder.go | session idle/stall/close → same + 24 h | G → G | D progress / B tail → same |
| transcode/http_session.go | HTTP cancel → same + 24 h | G → G | S (pipe closed on stop) / B tail → same |
| transcode/continuous_process.go | caller/manager cancel → same + 24 h | G → G | D progress or raw sockets / B tail → same |
| transcode/rtx_session.go | session stop → same + 24 h | G → G | D progress / B tail → same |
| transcode/download_watchdog.go | progress watchdog/caller → same + 24 h | G → G | S / D → same |
| speech/wav.go `ExtractWAV` | caller only → 15 min | direct only → G | D combined → B combined |
| prerolls/media.go `run` (convert and thumbnail) | caller only → 15 min | direct only → G | discard / B 4 KiB → same |
| onair/encode/encode.go `WithOptions` | caller only → 15 min | direct only → G | discard / D; streamed stdin → same |
| onair/reels.go `convertClip` | caller only → 15 min | direct only → G | progress pipe / discard → D progress; cancel on scanner error |
| onair/reels.go `silenceEdges` | caller only → 15 min | direct only → G | discard / D → same |
| onair/moment_video.go | caller only → 15 min | direct only → G | discard / discard → same |
| subtitles/timing_speech.go `decodeWindow` | caller only → 30 s | direct only → G | D PCM / bounded exec errors → B output/errors; stdin read budget retained |
| spike/probe.go | 30 s → same | direct only → G | D JSON / bounded exec errors → B output/errors |
| transcodebench/verify.go `verifyOutput` | 30 s → same | direct only → G | D JSON / D → same |
| transcodebench/verify.go `ffprobeVersion` | 10 s → same | direct only → G | D / bounded exec errors → B output/errors |
| app/scorecard_serve.go local-file probe | caller only → 30 s | direct only → G | D JSON / bounded exec errors → B output/errors |
| cmd/onair-samples/main.go decoder | caller only → 15 min | direct only → G | raw pipe / os.Stderr → same; explicit cleanup cancels tree |
| cmd/onair-samples/main.go audio | caller only → 15 min | direct only → G | D combined → B combined |
| cmd/onair-samples/main.go metadata | caller only → 30 s | direct only → G | D JSON / bounded exec errors → B output/errors |
| cmd/onair-samples/stream.go remux | outer 90 s → same + 15 min cap | direct only → G | D combined → B combined |
| cmd/transcodechaos/main.go fixture command | outer 20 min → min(outer,15 min) | direct only → G | D combined → B combined |
| cmd/transcodechaos/timeline.go | caller only → 30 s | direct only → G | D JSON / bounded exec errors → B output/errors |
| cmd/scorecard/workload.go decodeDuration | 2 min → same | direct only → G | D progress / bounded exec errors → B output/errors |
| media fixture helpers under internal and cmd (transcode, app, livetv, playback, plex, sessions, subtitles, onair/encode, scorecard) | mixed; some unlimited → 30 s open-GOP probes / 2 min others | direct only → G | D output/errors → B output/errors |

The runner's hard lifetime ceiling also bounds any injected production call that
lacks a more specific timeout. Existing input/protocol allowlists are unchanged.
No `internal/core` or `internal/config` changes. No spec divergence or UI work.

## Lasting lifecycle test

`internal/mediaproc/command_test.go` runs a fake ffprobe that starts a child,
shares its stdout/stderr, then sleeps indefinitely. Both a 500 ms deadline and
explicit cancellation must return within three seconds. Linux checks `/proc`
for child termination (a killed zombie awaiting host-init reaping is inactive);
Darwin checks PID disappearance; Windows waits on the child process handle.
The child's heartbeat must also stop. Existing guardian lifecycle tests remain.

## macOS reproduction commands

Run from the integration checkout (use `darwin-amd64` for Intel):

```sh
scratch=$(mktemp -d)
FFMPEG_OUT="$scratch/bundle" nice -n 10 sh scripts/fetch-ffmpeg.sh darwin-arm64
PATH="$scratch/bundle/darwin-arm64:$PATH" nice -n 10 go test -p 2 ./internal/transcode \
  -run TestIntegrationMatroskaOpenGOPCopyTimeline -count=1 -timeout=4m -v
PATH="$scratch/bundle/darwin-arm64:$PATH" nice -n 10 go test -p 2 ./internal/transcode \
  -count=1 -timeout=12m
rm -rf "$scratch"
```

The script checks the platform's locked archive checksum before staging binaries.
The tests discover ffmpeg and ffprobe through PATH and log the selected paths.

## Validation results

All Go commands use the pinned toolchain, `nice -n 10`, `-p 2` where applicable,
and `GOCACHE=/tmp/ether-w25/gocache` because the default cache is read-only in this
sandbox. The scratch cache and downloaded artifact are removed after delivery.

- Baseline exact-bundle open-GOP matrix: PASS, 98.502 s; monitored repeat PASS,
  105.45 s. All seeks passed; no probe exceeded the ten-second capture threshold.
- Hardened exact-bundle open-GOP matrix: PASS, 96.721 s.
- Exact-bundle `go test -p 2 ./internal/transcode -count=1 -timeout=12m`: PASS,
  381.562 s. No upstream hang reproduced in the full package.
- `go test -p 2 ./internal/mediaproc ./internal/onair -count=1 -timeout=2m`: PASS
  (1.176 s and 3.428 s). Deadline and explicit-cancel child cleanup both passed.
- Focused tests for speech, prerolls, onair/encode, subtitles, spike,
  transcodebench, cmd/onair-samples, cmd/transcodechaos and cmd/scorecard: PASS.
- Guardian cancellation / Linux parent-death focused check: PASS.
- Fixture-only changes in app, livetv, playback, plex and sessions:
  `go test -p 2 <packages> -run '^$'`: PASS (compile only, no broad test run).
- Plex fixture compilation with `-tags transcodechaos -run '^$'`: PASS.
- `go vet -p 2` on every touched package: PASS.
- `go build -p 2 ./...`: PASS.
- `CGO_ENABLED=0 go build -p 2 -o <scratch>/ether ./cmd/ether`: PASS.
- Windows amd64 pure-Go build of mediaproc/transcode: PASS. Shared lifecycle
  tests cross-compile for Windows amd64 and Darwin arm64: PASS. Runtime tests on
  those hosts remain the orchestrator's responsibility.
- `git diff --check`: PASS.

The W11 BtbN observation cannot be assigned an upstream root cause without its
hung input/process. This audit closes the confirmed Ether lifetime defects;
it does not assert that every FFmpeg 8.1 distribution is free of demuxer bugs.
