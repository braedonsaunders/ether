# Worker-local encoder input

Unwired pure Go composition only. No application, placement, configuration, or
public routes change. The Go `nodesource.Client` holds verified mTLS credentials
and opaque rotating source bearer. FFmpeg gets only the loopback capability URL
and local `X-Ether-Encoder-Input` scope header (which also activates the existing
encoder command-log redaction). No caller URL, source/provider header, pathname,
or cluster credential reaches FFmpeg.

Host composition:

1. Construct `New(Options)` with the verified `*nodesource.Client`, lifetime
   admission capacity, concurrent request/connection limit, and request timeout.
   Construction binds TCP4 `127.0.0.1:0`; host runs `Serve(ctx)` and owns `Close`.
2. Resolve a stable `Start.Grant` handle through authoritative placement/source
   policy. Call `Admit(ctx, start, grant, exactSize)`. It opens its own Reader and
   checks HEAD size. Scope must match controller/worker boot, session, positive
   generation without overflow; full stable-node IDs, viewer and revision remain
   bound by nodesource authority/Peer. Boot equality alone is not authorization.
   Grant expiry must cover start under the host's conservative clock policy;
   timestamp comparison here is not a cross-host clock guarantee.
3. Return `remotetranscode.Source{Gateway: input}`. No ordinary URL/Lease may be
   mixed in. Return a nonnil input even on admission/Open error so the worker can
   seal and retain its responsibility. The resolver must never mint an unused
   second lease. `GatewayInput` validates exact Identity and stable handle; worker
   independently validates literal-loopback URL shape and retains leases on errors.
4. Encoder claims once and obtains scope headers only for the exact URL. Local
   HTTP additionally checks exact listener Host/origin/path, scope header, and
   rejects duplicate/ambiguous headers/URL forms. Only HEAD/GET, one bounded byte
   range (including suffix/open end), 206 and 416 are supported. No redirects.
5. Drive `Reconcile` during activity and drain, even after cancellation/expiry.
   `SealInput` atomically seals admissions and cancels Reader requests without
   waiting. Producer registration and seal share a mutex. HTTP handlers, Open,
   explicit renewal, and Reader work are registered before starting. A blocked
   writer stays pending until it exits (write/request timeout bounds network
   writers); Close closes listener and connections. No read-wrapper goroutines,
   renewal loops or polling goroutines are created here.

Every reserved admission, including failed opens, consumes capacity for this
Gateway lifetime. Retained tombstones are never evicted to manufacture proof.
Reconcile releases Reader ownership before InputDone/callback publication; callback
runs once outside locks. `Accounting.Pending=0` means no retained producers, not
that callbacks have finished. Callback is immutable after seal and must perform
only bounded accounting. `InputErr` is pending until local proof, then nil.

**Local proof only:** InputDone/InputErr and Reader.Done never attest remote source
retirement. Remote source handler holds and durable placement capacity need source
owner sealing/producer/SourceRetirement proof independently. The offline fixture
explicitly keeps a delayed remote producer after gateway input has drained.

`Input.Renew(ctx, exactRenewal, authority)` calls authority-backed `Reader.Renew`,
which verifies unchanged complete Scope, revision, size and HEAD identity before
rotating the bearer. The Reader, URL, stable handle and InputLease stay identical.
Returned `RenewalEvidence` echoes the exact request, identity, handle, Scope, size,
SourceUntil and lease for composing `ValidateRenewal`. Placement must be separately
verified using its authoritative clock and skew policy. Successful source extension
can outlive rejected worker acceptance or stop and is never evidence of worker
CAS acceptance. Successful source-operation replays fail closed: historical evidence
is never returned as current source authority. Reconcile the worker's accepted
history after uncertainty; if the source operation succeeded but worker acceptance
did not, retire the generation. Source-validation replay support is unavailable.
Only one renewal validates at a time; contention returns an error without waiting.
Operation/version and preceding deadline must match source history exactly; lower
or changed same-operation payloads fail before authority work. A failed attempt
can retry its exact payload using fresh authority. Commit rechecks the captured
Reader, history and seal after external validation. No raw expiry setter/source swap/
encoder start is available. Parent owns schedule, worker renewal and reconciliation.

`Input` optionally implements `core.RecoverySourceReader` on the same claimed
lease. `RecoverySourceIdentity` returns the authenticated Scope revision and
immutable Reader size, crosschecked against admission. This cached identity is
not a fresh authority check. `ReadRecoveryAt` requires that exact revision and a
complete, positive range of at most 64 KiB within the source. Unknown/empty
identity or size, unclaimed/sealed inputs, overflow, cancellation and mismatches
fail closed. Each read shares the HTTP request limit and registers with the
existing tracker under the sealing mutex before invoking the actual Reader.
The request deadline is capped by RequestTimeout and composed with Input lifetime
cancellation. Reads run synchronously and Reconcile joins their registered work
before InputDone. Errors return zero bytes with ErrDenied or standard context
cancellation/deadline errors; callers must discard the buffer on error.

This capability supplies only same-source bytes for the separately bounded,
strict nonfragmented AAC-LC MP4 index. It does not attest previously retained
access units/output, authorize a second encoder, prove source-host equivalence,
or enable application/transport recovery. Ordinary inputs remain unchanged.

Limitations: host still supplies authority, stable-handle lookup, placement and
source-owner proof, certificate lifecycle, clock policy and durable recovery. Bounds
apply within one gateway boot; capacity exhaustion requires host lifecycle handling
without dropping unresolved ownership. Arbitrary custom trusted-gateway interface
implementations are the host's trust responsibility, not wire input. No actual
FFmpeg/GPU/Plex execution or deployment is claimed by the offline fixture.
