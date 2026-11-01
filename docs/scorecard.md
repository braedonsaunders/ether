# Head-to-head scorecard

`cmd/scorecard/h2h.py` compares a Jellyfin or Emby reference with an Ether
process. Credentials stay on stdin. The machine-readable result records
nearest-rank p50/p95 for browse routes and a single play-to-first-media-byte
span for direct play, seek, resume, and HLS transcode. A latency winner is
recorded only when both sides succeed and return the same `TotalRecordCount`.
Playback is scored only when both sides open the same SHA-256 and the same
delivery method.

Exact aggregates are in [scorecard.json](scorecard.json).

## Matched run

The timed Ether process served the lab media on bb-worker-server with the
same 2 CPU and 6GB cap as the Jellyfin and Emby lab containers. It listened on
loopback `18296` (Jellyfin) and `18297` (Emby). The extra Jellyfin lab container was not
restarted. Samples were 21 at concurrency 1, with 3 playback repeats.

That process was built from `e3792dab4` plus the library-refresh fix in this
change. The multi-app branch moved to `1a25b7273` before the commit, and these numbers were
not taken again after that fast-forward. The shared lab containers were
returned to unlimited CPU and memory after the run. Docker cannot clear a
memory limit in place, so the restore recreated the Jellyfin and Emby lab containers
from their existing Compose files and volumes. Nothing was
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

On the Jellyfin lab host, before this fix, a recursive episode list stayed near
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

## Playback

Matched run from the playback-latency worker at `bdd06c108`. Ether and each
lab container had 2 CPU. Each figure is nearest-rank p50/p95 over 20 samples.
The span is PlaybackInfo plus the first media byte.

| Route | Ether Jellyfin | Lab Jellyfin | Ether Emby | Lab Emby |
| --- | ---: | ---: | ---: | ---: |
| Direct | 7.68 / 23.39 | 38.63 / 57.66 | 4.28 / 9.69 | 5.28 / 7.21 |
| Seek | 4.78 / 6.22 | 40.57 / 56.71 | 4.34 / 6.77 | 5.42 / 7.14 |
| Resume | 4.84 / 8.36 | 42.18 / 57.30 | 4.13 / 13.83 | 5.47 / 7.33 |

Ether's median beats both labs on direct, seek, and resume. Ether's p95
beats lab Jellyfin on all three and beats lab Emby on seek. On the shared
host, Ether's Emby p95 still trails lab Emby on direct (9.69 ms versus
7.21 ms) and on resume (13.83 ms versus 7.33 ms).

An earlier `tvdemo-ether` run showed the same episode list near 14s and
direct, seek, and resume failing. That was a defect in the fixture's
metadata path, not a number to set aside.
