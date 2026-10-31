# Head-to-head scorecard

`cmd/scorecard/h2h.py` compares a Jellyfin or Emby reference with an Ether
process. Credentials stay on stdin. The machine-readable result records
nearest-rank p50/p95 for browse routes and a single play-to-first-media-byte
span for direct play, seek, resume, and HLS transcode. A latency winner is
recorded only when both sides succeed and return the same `TotalRecordCount`.
Playback is scored only when both sides open the same SHA-256 and the same
delivery method.

Exact aggregates are in [prism-scorecard.json](prism-scorecard.json).

## Matched run

The timed Ether process served the lab media on bb-worker-server with the
same 2 CPU and 6GB cap as `prism-jellyfin` and `prism-emby`. It listened on
loopback `18296` (Jellyfin) and `18297` (Emby). `prism-jf-lab-zbv` was not
restarted. Samples were 21 at concurrency 1, with 3 playback repeats.

That process was built from `e3792dab4` plus the library-refresh fix in this
change. Prism moved to `1a25b7273` before the commit, and these numbers were
not taken again after that fast-forward. The shared lab containers were
returned to unlimited CPU and memory after the run. Docker cannot clear a
memory limit in place, so the restore recreated `prism-jellyfin` and
`prism-emby` from their existing Compose files and volumes. Nothing was
reseeded.

Catalogue counts are still not aligned. Jellyfin lists three Big Buck Bunny
editions; this Ether build keeps one movie title. Recursive `/Items` still
omits seasons. Both of those belong to the Jellyfin library work and are not
in this change. Episode lists are the same length (40).

| Type | Jellyfin | Emby | Ether |
| --- | ---: | ---: | ---: |
| Movie | 6 | 4 | 4 |
| Series | 1 | 1 | 1 |
| Season | 2 | 2 | 0 |
| Episode | 40 | 40 | 40 |

On `prism-jf-lab-zbv`, before this fix, a recursive episode list stayed near
16s and a combined Movie+Episode+Season+Series list stayed near 30s. The time
was waiting on a metadata read that failed once per row. The scorecard seed
was browsable but not a fresh detail, failed refreshes were not remembered,
and show, season, and episode rows were loaded from SQLite again for every
image grant. This change remembers a failed refresh for one minute, stores
the scorecard seed as a detail, and caches those rows the same way movies
were already cached.

After that, episode p50/p95 was 458/1716 ms against Jellyfin 36/50 ms, and
510/2893 ms against Emby 4.9/7.0 ms. The steady repeat on that server was
about half a second. That is still a loss. Home views are faster on Ether.
Jellyfin movie cardinality does not match, so that route has no latency
winner. Emby movies match at 4 and Ether is slower (56/74 ms versus 2.2/3.3 ms).

Direct, seek, resume, and HLS all returned media bytes on both protocols.
Jellyfin direct p50 was 46 ms on Ether and 63 ms on the reference. Jellyfin
HLS p50 was 677 ms on Ether and 4229 ms on the reference. Emby HLS p50 was
922 ms on Ether and 1835 ms on the reference. Jellyfin seek p50 was 1285 ms
on Ether and 55 ms on the reference. Emby direct p50 was 1984 ms on Ether
and 15 ms on the reference. Those seek and Emby direct gaps are still losses.
The harness follows Ether's same-host playback redirect and does not apply
the Emby prefix twice; the earlier all-error playback result was that
measurement bug.

An earlier `tvdemo-ether` run showed the same episode list near 14s and
direct, seek, and resume failing. That was a defect in the fixture's
metadata path, not a number to set aside.
