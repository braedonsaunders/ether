# Plex parity harness

Run `cmd/plexparity/remote.sh BASELINE CANDIDATE` from the worktree. The
candidate may be a Git revision or a worktree path. The remote runner builds
both trees with test-only seams and runs the real components against copied,
offline fixture data. Exit 0 means no unapproved differences and complete
coverage; 1 means a difference or an invalid fixture; 2 means no unapproved
differences but incomplete route coverage. CI currently warns on exit 2.

`c2057bfe` restricted Home backfill: not exercised by fixture; covered by
experience unit tests. Do not allowlist a difference without an exact, verified
response pair and its commit attribution.

Intentional difference `42ef28ac`: viewers who saved scoped language or
quality overrides have those preferences applied to Plex playback selection.
The no-override path is covered by `TestNoScopedOverridesPreserveSelection`
and `TestScopedQualityOnlyTightensPlexRequest`. This is recorded for
attribution only: no response is allowlisted without a verified exact pair.

The v0.2.45 notification-inclusive self-run leaves 18 GET routes uncovered. All are
implemented features; none should be exempted as intentionally unsupported:

- Five download-queue reads: queue, items, item, decision and media. These
  need an enabled queue with a completed real download item.
- Five subtitle and trickplay reads: subtitle search, external subtitle stream,
  transcode subtitle, BIF and individual frame. These need actual subtitle
  and generated trickplay data.
- Eight live-TV reads: session, playlist, DASH part, program segment, IPTV
  relay, IPTV resource, subscription and grab operation. The IPTV paths need
  an offline IPTV source fixture.

A zero-unapproved result with exit 2 covers only the exercised corpus; it is
not a full parity verdict. The CI warning must become a failure once coverage
is complete.
