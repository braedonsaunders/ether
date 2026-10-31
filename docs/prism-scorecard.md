# Head-to-head scorecard

`cmd/scorecard/h2h.py` compares a Jellyfin or Emby reference with an Ether
process. Credentials stay on stdin. The machine-readable result records
nearest-rank p50/p95 for browse routes and a single play-to-first-media-byte
span for direct play, seek, resume, and HLS transcode. A latency winner is
recorded only when both sides succeed and return the same `TotalRecordCount`.
Playback is scored only when both sides open the same SHA-256 and the same
delivery method.

## Provisional run (not a pass or a defect list)

The matched-library Ether target was not available yet. This run used the
already-running `tvdemo-ether` fixture on bb-worker-server, which mounts the
lab media and the full item listing. It is not `origin/prism` `0aaae04c8`, and
it is not the shared lab target being built for the Jellyfin conformance work.
No reference container was restarted or reseeded, and this run did not start
a second Ether. Reference and Ether containers were both uncapped for CPU and
memory. The worker load average was about 21 on 16 cores, so the times below
include host contention.

Exact aggregates are in [prism-scorecard.json](prism-scorecard.json).

Catalogue counts were not fully aligned:

| Type | Jellyfin | Emby | Ether fixture |
| --- | ---: | ---: | ---: |
| Movie | 6 | 4 | 4 |
| Series | 1 | 1 | 1 |
| Season | 2 | 2 | 0 |
| Episode | 40 | 40 | 40 |

Jellyfin lists the three Big Buck Bunny versions as separate movies. The Ether
fixture keeps one movie title. Season rows are absent on the fixture. Episode
lists are the same length (40).

On that episode list, at concurrency 1 and 21 samples, Jellyfin p50/p95 was
51/80 ms and Ether was 13950/15024 ms. Emby was 5.6/9.1 ms against the same
Ether times. Emby movie lists also matched at 4 items: Emby 2.7/4.3 ms, Ether
299/307 ms. Direct, seek, and resume reads succeeded on the references and
failed on the fixture (3/3). Jellyfin HLS startup was faster on the fixture
(342 ms p50 versus 1673 ms) and Emby HLS startup failed on the fixture.

These timings are evidence about `tvdemo-ether` only. They are not losses
attributed to `0aaae04c8`. The next run has to use the shared current-prism
target, with seasons present and the same episode and movie workload, before
any of these gaps become a code fix.
