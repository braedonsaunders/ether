# Plex parity harness

Run `cmd/plexparity/remote.sh BASELINE CANDIDATE` from the worktree. The
candidate may be a Git revision or a worktree path. The remote runner builds
both trees with test-only seams and runs the real components against copied,
offline fixture data. Exit 0 means no unapproved differences and complete
coverage; 1 means a difference or an invalid fixture; 2 means no unapproved
differences but incomplete route coverage. CI fails on either 1 or 2.

`c2057bfe` restricted Home backfill: not exercised by fixture; covered by
experience unit tests. Do not allowlist a difference without an exact, verified
response pair and its commit attribution.

Live DASH manifests use a fixed test-only clock in both staged binaries. This
checks serialization under identical times; it does not test wall-clock
progression. Production builds retain the normal live clock and its tests.

The default corpus exercises the real personalization engine. The opt-in
`ETHER_PARITY_TRAIN_RANKER=1` fixture uses deterministic synthetic viewer
activity through the real APIs to train an active learned reranker under the
unchanged production holdout gate. Its self-comparison passed; the cross-tree
result remains unverified while its restricted-viewer differences are audited.
This fixture is synthetic evidence, not real-user or device evidence.

Intentional difference `42ef28ac`: viewers who saved scoped language or
quality overrides have those preferences applied to Plex playback selection.
The no-override path is covered by `TestNoScopedOverridesPreserveSelection`
and `TestScopedQualityOnlyTightensPlexRequest`. This is recorded for
attribution only: no response is allowlisted without a verified exact pair.

Intentional scope fix `19bdfcd1`: `/hubs/home/recentlyAdded` no longer
widens unsupported types, comma-separated types, or explicit section filters
into mixed results. Read-only PMS probes returned empty for the affected
requests. Unscoped mixed Home responses remain supported. Exact before/after
pairs must be recorded before any difference is approved.

The complete v0.2.45 corpus covers every registered Plex GET route, including
real offline IPTV resource, relay and DASH media. A zero-unapproved result with
exit 2 means a future inventory route is uncovered and is not a parity verdict.
