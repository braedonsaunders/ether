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

Full runs exercise both the ordinary fixture and a second fixture that uses
deterministic synthetic viewer activity through the real APIs to train an
active learned reranker under the unchanged production holdout gate. Its
self-comparison passed; an integrated passing cross-tree replay remains
outstanding after exact difference review. This is synthetic evidence, not
real-user or device evidence. Focused diagnostic modes run only their named
fixture and return incomplete coverage.

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

Owner-approved resume improvement: a nonzero position is retained at 60
seconds or 5% of duration, whichever comes first, until the existing watched
threshold. This shared-state change affects `/:/timeline`,
`/library/metadata/{{movieKey}}`, and `/hubs/continueWatching` for those
positions. The earlier PMS threshold was not established: the Guest probe
stopped at authorization. This note does not allowlist any response; exact
reviewed before/after pairs are still required.

Intentional track selection: a viewer with no audio list no longer follows the
file's default flag ahead of a language they can use. The order is the
viewer's trackprefs audio languages (their row, otherwise user 0, which the
setup wizard writes), then the catalog language (`en-US` matches English),
then the title's original language, then the default flag. PreferOriginal
still ranks the original language first and is separate from that chain.
The same order picks subtitles. With subtitle mode left off, a forced
subtitle in the viewer's language is selected only when the chosen audio
is not one of those languages. This note does not allowlist any response.

Intentional playback change `1d4dee05c` (PR #198, merge `e7a3bf2dd`): when
every candidate's health check times out, playback starts the first pooled
candidate in the resolver's own order (preferred release, then primary, then
downgrades already sorted by quality) instead of returning an error. Failover
stays armed: other candidates without a confirmed failure remain alternates.
A provider that cannot answer, and any confirmed failure, still fails. The
unverified winner is not cached as verified or available. This note does not
allowlist any response.

The complete v0.2.45 corpus covers every registered Plex GET route, including
real offline IPTV resource, relay and DASH media. A zero-unapproved result with
exit 2 means a future inventory route is uncovered and is not a parity verdict.
