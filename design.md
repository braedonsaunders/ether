# Prism security design

This document records security boundaries and requirements for the Prism
candidate. It is not a completed security certification. See the
[architecture status](docs/prism.md) and [first-Prism upgrade
runbook](docs/acceptance/prism-upgrade.md) for implementation and deployment
limits. Final broker adoption requires another security and documentation pass.

## Threat model

The owner console, viewer portal, protocol listeners (Plex, Jellyfin, Emby,
Silo, Stremio, files and discovery), media handlers and cluster transport are
separate trust boundaries. Remote clients, forwarded headers, identifiers,
playlist inputs and paired devices are untrusted. A restricted viewer may be
malicious; a valid login does not authorize another viewer's library, device,
playback session or surface. External provider account credentials and database
access are privileged. Compromised cluster hosts and database administrators
are outside the capability-isolation guarantee.

### Surfaces and owner operations

Only the authenticated owner console performs claim, initial setup, membership
and listener configuration. Protocol setup/status responses describe a configured
server and must not become alternate claim or setup APIs. The public viewer
portal issues viewer credentials and approves devices; it does not grant owner
configuration authority. Local-owner installations require no Plex claim.
Owner-console network admission uses `identity.ClientNet`, including configured
trusted proxy networks, before console authentication. LAN admission alone is
not proof of ownership. Mutating console endpoints retain their authentication
and request-origin protections.

### Credentials, grants and cross-surface replay

Plex account credentials remain in the Plex authentication domain. Surface
credentials are random, hashed in persistent storage, expiring and revocable;
resolution binds the exact surface and checks current active membership. Device
credentials also bind the stored device rather than trusting a caller's device
header. A Jellyfin grant must fail on Emby, Silo, Plex and the console, even when
the same viewer is linked to each. Refresh credentials rotate and must not be
accepted as access tokens. Revocation, account deactivation and owner-role
changes must take effect from shared state rather than stale node caches.
A stable GUID, rating key, compact Silo file ID or handle digest is an identifier,
not authorization; resolve through viewer policy before returning metadata or
opening media. Namespace separation prevents cross-kind collisions.

### Quick Connect

The short displayed code is an approval locator, never a bearer credential.
The independent random polling secret is stored hashed. Pending approvals bind
surface and device, expire, and are atomically consumed once when issuing a
grant. Approval requires a signed-in viewer and may issue only that viewer's
identity. Initiation, approval, status/details and redemption are rate limited
before verification using shared database buckets for client IP and identity or
secret. Malformed requests and database failures fail closed; switching nodes,
handles or surface must not reset per-IP budgets. Keep polling and password
budgets separate so normal polling does not exhaust password login capacity.

### Proxy trust

`identity.ClientNet` is the common address/trust classifier. Only loopback and
explicit configured proxy peers may supply forwarding headers. Untrusted peers
cannot turn themselves into LAN clients with `Forwarded`, `X-Forwarded-For` or
`X-Real-IP`. Malformed chains fail closed; a public client cannot hide behind a
private prefix. Use the same result for rate-limit identity, LAN admission and
protocol remote policy. Direct-peer-only gates for isolated diagnostic sockets
are distinct from forwarded-client authorization. Reverse proxies must replace
client-supplied forwarding headers, and trusted CIDRs must be narrowly scoped.

### Media URLs and manifests

Never emit Plex account tokens, surface login grants, passwords or refresh
credentials in media URLs, redirects, HLS or DASH output. Only playback-scoped
capabilities or random session paths may authorize media. Bind those capabilities
to viewer, surface, device, active session and lifetime; stop, revoke and viewer
policy changes must deny later segment, subtitle, image and range requests.
A URL copied to another protocol must not become an account credential. Query
credentials accepted from third-party clients are not echoed into generated URLs.
Do not log authentication headers, approval secrets or credential-bearing URLs.
Playlist writers escape XML attributes, bound output size through validated
session plans, and generate segment names instead of interpolating arbitrary
client strings into HLS lines. Native fuzz corpora exercise parser determinism,
namespace/codec round trips, output framing and XML attribute preservation.

### Cluster and mixed-version operation

Shared database checks and conditional updates arbitrate grants, revocation,
Quick Connect consumption and rate limits across nodes. A node cannot infer
permission from another node's unverified session ID. Broker forwarding needs
authenticated cluster transport, current generation/serving-node checks and
stop/failover fencing. Check cancellation before publishing issued identities.
Reject unknown capability versions and namespaces rather than falling back to
account-token authorization. Schema migration and rollback must preserve hash,
surface, expiry and revocation semantics. Old writers may bypass newer quotas
or fail to mirror renewal: mixed-version quota and lifetime guarantees require
separate upgrade evidence and are not established by current-writer tests.

### Review and acceptance limits

Plex wire behavior remains byte-identical; restricted-viewer policy leak fixes
are the sole approved exception. Security defects requiring another Plex wire
change go to the root orchestrator before implementation. Fuzzing is bounded
and complements authorization regressions; it cannot prove absence of leaks.
The security audit receipt records concrete source paths, regression evidence,
remote stages and remaining mixed-version/lifecycle limitations. No production
or external account claim is part of this security validation.
