# Vela audio fallback MSE clock

Verified 2026-11-14 with FFmpeg 9.0.2 and installed headless Chrome 157 on macOS.
All media was generated locally and fetched through `Manager.StartHLS`,
`ServePlaylist`, and `ServeSegment`; no production or lab playback was used.

AAC audio-only HLS now supplies the inner MP4 muxer options:

```
movflags=+frag_discont+skip_sidx:use_editlist=0:avoid_negative_ts=make_non_negative
```

`use_editlist=0` removes dependency on browser edit-list support. It must be
paired with the inner timestamp policy: its automatic policy otherwise rebases
positive timestamps to zero. `make_non_negative` preserves positive timestamps
and shifts negative AAC priming at title zero to zero, avoiding a wrapped uint64
`tfdt`. Video muxing retains its existing options. Segments remain 72 AAC frames
at 48 kHz (1.536 seconds), with a shorter final fragment.

## Actual server box dumps

The fixture was a ten-second 48 kHz AC3 5.1 source, downmixed to AAC stereo at
192 kbps. Each row is a new HLS session at the requested title position.
The baseline runner used the base branch's exact options by stripping only the
new inner muxer options from the command. Both captures came through the same
server delivery methods. `mdhd.timescale=48000` and `tfdt.version=1` throughout.

| Capture | `moov/trak/edts/elst` payload (hex) | `tfdt` per fragment (ticks) |
| --- | --- | --- |
| Before, 0 s | `0000000000000001000000000000000000010000` | 18446744073709550592, 72704, 146432, 220160, 293888, 367616, 441344 |
| Before, 4 s | same | 190976, 264704, 338432, 412160 |
| Before, 6 s | same | 286976, 360704, 434432 |
| After, 0 s | absent (`edts` absent) | 0, 73728, 147456, 221184, 294912, 368640, 442368 |
| After, 4 s | absent (`edts` absent) | 190976, 264704, 338432, 412160 |
| After, 6 s | absent (`edts` absent) | 286976, 360704, 434432 |

The original standalone reproducer omitted the server's existing `frag_discont`
flag: it had an empty edit delaying zero-based fragments to the requested
position. The actual base server already produced absolute decode times at
4/6 seconds, but retained an edit list and wrapped the initial -1024-sample
priming timestamp at title zero. These captures distinguish those cases.

## Chrome MSE proof

The [standalone harness](vela-audio-mse-clock.html) appends each init and all
fragments into a fresh MediaSource on the same audio element, in order 0, 4, 6.
It asserts the SourceBuffer starts within one AAC frame of the requested title
position, starts playback at that position, then seeks a second forward and
asserts the clock continues advancing. No timestampOffset is set. The element
is muted for headless autoplay; this proves decode/playback clocks, not audible
quality or lip sync.

| Requested start | SourceBuffer range (s) | Clock after 300 ms (s) | Seek target → clock after 200 ms (s) |
| --- | --- | --- | --- |
| 0 | 0–10.021333 | 0.256040 | 1 → 1.156445 |
| 4 | 3.978666–9.999999 | 4.258498 | 5 → 5.156957 |
| 6 | 5.978666–9.999999 | 6.257514 | 7 → 7.157536 |

All three had `audio.error=null`. The extra 21.333 ms at zero is the shifted
encoder priming frame. At nonzero starts the first frame precedes the request
by one AAC frame.

To repeat the browser check, place server-produced fixture folders `after-0`,
`after-4`, and `after-6` beside the harness. Each contains `init.mp4`,
`index.m3u8`, and the playlist's `.m4s` resources. Serve on loopback, open the
harness in installed headless Chrome with `playwright-cli`, then evaluate
`async () => ({userAgent:navigator.userAgent,results:await window.runClock()})`.
Close that browser and loopback server afterward.

The existing `TestOriginalAudioOnlyRendition` now checks starts 0/4/6, absence of
`edts`, zero edit shift, the raw fragment clock within one AAC frame, the AAC
stereo codec, and every fragment's real/advertised duration ≤2 s.
`CGO_ENABLED=0 go build ./...`, touched-package vet, and the focused test pass.
A failed exploratory run with only `use_editlist=0` correctly caught the inner
muxer's positive timestamp rebasing before the final policy was selected.

## Shipping client follow-up

The Vela audio engineer (thread `thr_4s4q52buzh`) independently fed the captured
4/6-second bytes through `feedEtherAudio` and `OwnedNativeMediaAudioBackend`,
with the client timeline guard enabled. Generation 1 at 4 s qualified at
5.111089 s; after retirement, generation 2 at 6 s qualified at 6.427438 s.
A stale generation-1 stop returned false while generation 2 continued. The
second generation ended at 9.999999 s with an ended event and zero pending
appends. This was a local browser harness, not installed-app or video lip-sync
verification. The coordinator should verify the installed player on real
devices, including start/resume, regeneration/seek, track changes, title-zero
priming, end-of-title, and audio/video sync. Negotiation remains enabled because
the required server-byte Chrome MSE harness passed before this change was pushed.
