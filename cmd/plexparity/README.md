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

The current corpus exercises the real personalization engine but does not yet
seed a trained, active learned-reranker model. Active-model reranking parity
remains unverified until a real API-trained fixture passes its normal gate and
the full differential replay.

Intentional difference `42ef28ac`: viewers who saved scoped language or
quality overrides have those preferences applied to Plex playback selection.
The no-override path is covered by `TestNoScopedOverridesPreserveSelection`
and `TestScopedQualityOnlyTightensPlexRequest`. This is recorded for
attribution only: no response is allowlisted without a verified exact pair.

The complete v0.2.45 corpus covers every registered Plex GET route, including
real offline IPTV resource, relay and DASH media. A zero-unapproved result with
exit 2 means a future inventory route is uncovered and is not a parity verdict.
