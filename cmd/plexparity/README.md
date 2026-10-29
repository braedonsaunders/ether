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
