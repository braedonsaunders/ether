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

The v0.2.45 full self-run leaves 3 GET routes uncovered. All are
implemented features; none should be exempted as intentionally unsupported:

- Three live-TV reads: DASH part, IPTV relay and IPTV resource. These need an
  offline IPTV source fixture.

A zero-unapproved result with exit 2 covers only the exercised corpus; it is
not a full parity verdict. The CI warning must become a failure once coverage
is complete.
