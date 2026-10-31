# Availability monitoring

Ether observes recent releases and reuses successful resolver health checks. It
never removes catalogue/search results because evidence is missing. Movie dates
and episode air dates still use the catalogue's existing release eligibility;
quality, CAM acceptance, identities, blocklists and episode targeting use the
resolver's existing policies rather than a second rules engine.

## Owner controls

Sources → Availability provides monitoring, selective validation, participating
feeds, debrid participation, preference for checked recommendations and optional
newly available shelves. Monitoring and validation start disabled. Presentation
preferences start enabled and can benefit from ordinary playback observations.

Default daily background allowances (UTC, shared across nodes):

- 100 feed API requests, and 100 validation API requests.
- 20 candidate checks and 20 descriptor grabs (NZB or torrent).
- 512 MiB of conservative data reservations. NNTP BODY reservations use the
  provider's maximum article size, so actual transferred bytes can be much lower.
- Preserve 80% of each indexer's configured daily cap for foreground work.

An allowance of zero disables that work. Foreground requests still have their
normal account limits. Every feed page, provider API request and source read goes
through admission before network work. Ordinary playback does not use the
background allowance. Shared worker contexts retain the admission hook.

## Discovery and evidence

Newznab/Torznab recent-release pages use an empty `t=search` request, following the
incremental model documented by [Radarr](https://wiki.servarr.com/radarr/faq#how-does-radarr-work).
One page describes up to 100 releases. Polling defaults to 30 minutes. The first
sync starts at the head; later syncs look for overlap with the previous head,
with at most three pages. Failed requests do not advance the cursor. Missing
overlap records a visible coverage gap rather than triggering unlimited catch-up.
Search remains the fallback for older and missed releases. Built-in sites without
a recent-feed contract remain available for ordinary search.

Matching scans local title identities and resolves episodes locally; it does not
call TMDB for every feed item. Conflicting identifiers and ambiguous title matches
are rejected. A season pack only contributes known, already-aired episodes, and
its health check is targeted to the chosen episode using the existing resolver.

The queue prioritizes popular titles, checks one candidate per title per pass,
and defers extra candidates when the title already has fresh successful evidence.
It never searches the entire catalogue in the background. Playback attempts can
reuse discovered descriptors before normal search, with a five-second shortlist
budget. Viewer/device policy is always reapplied on that path.

Debrid cache lookups are batched and capability-dependent. Background validation
can inspect existing account items and validate a bounded HTTPS range; it never
starts an uncached download or adds torrents. Unsupported cache APIs leave
availability unknown. A debrid account by itself is not a global discovery feed;
recent torrent discovery requires a configured Torznab source. Usenet, debrid,
both, and no playback provider are supported configurations.

Successful evidence is source-, title-, policy- and configuration-specific. It
expires after six hours. Configuration changes invalidate old positives. A source
failure never creates a title-wide negative. Timeout, exhausted allowance and
unsupported checks remain unknown. Normal resolver health-cache hits do not renew
evidence timestamps. Sampled health is useful evidence, not a guarantee of all
bytes or all clients.

Discovery filtering captures one isolated settings value, policy fingerprint,
source list and clock reading per operation. Nested episode and retained-release
reads, pagination exclusions, recommendation priority and newly available shelves
reuse those inputs. The next operation sees settings and source-generation
changes; existing observation invalidation still prevents stale memo writes.
`config.Store.Get` retains its deep-copy isolation. Background admission remains
live so owner pauses and allowance changes take effect during background work.
Resolver candidate checks still apply their existing policy; this snapshot does
not bypass access, identity or quality checks or promise to remove all Home delay.

Only fresh checked movies move ahead within popular/trending/recommendation
shelves; no items are removed. Explicit chronology and Continue Watching retain
their order. Newly available shelves mean first successfully checked by this
server, not necessarily a film's worldwide premiere.

## Operations

The existing cluster leader runs one scheduler; cursors, observations and atomic
usage counters are durable in the shared database. Hot settings are respected by
admission. Checks pause while local playback is active. Each cycle is bounded,
cancellation propagates, transient outcomes back off, and descriptors expire
after 30 days with a hard retention cap of 30,000 observations. Signed release
locators are sealed using the config store; owner status never exposes them.

Tests use fake feeds/providers and local HTTP fixtures. They cover admission,
account reserve, midnight reset, restart/overlap/gaps, identity conflicts,
source/policy invalidation, stale evidence, disabled-provider behavior, diverse
candidate selection, resolver reuse, and budget propagation across shared workers.
