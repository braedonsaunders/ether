# Fenced playback composition

Opt-in runtime composition; the application owns listeners, membership, admission
budgets, certificate lifecycle and scheduling. No public Plex route changes.

Construct NewVerifier, placement.New(db, options, verifier), NewRuntime(Options),
then verifier.Bind(runtime), once before serving. Options requires live Guard,
exact Self membership boot, trusted Endpoint, Eligible (new work, rejects drain),
MemberRole (existing work, permits drain), explicit concurrent capacity and clock skew bound. HistoryCapacity (default4096)
is an independent boot-retained tombstone quota, bounded by primitive authority
limits. Capacity=1 does not limit a boot to one completed playback. SourceAccess
must be constructed with matching history limits. History exhaustion fails closed;
quiescent runtime rotation is still required after that explicit quota. No TTL
eviction manufactures source or encoder retirement. Long-lived safe compaction
requires extending nodeauthority/SourceAccess retirement-floor APIs and remains a
production acceptance gate.
Source roles supply Manager, SourceAccess, ActiveViewer and SourcePolicy. The latter
rechecks current title/parent/library/rating/release policy using local facts.
Worker roles supply the real core.OwnedTranscoder (CPU or GPU manager). There is no
alternate encoder or provider implementation inside this package.

Private Handler registers SourcePath, nodesource.Path and WorkerPath where those
roles exist, and DeliveryPath. Host supplies verified mTLS listener. NewRuntime
does not change placement targets/pools or start cluster membership. Application
must reconcile SetPool/SetTarget and only advertise protocol compatibility once
all configured roles/listeners/policy are ready. Run Tick on a bounded control
executor. Run Cleanup separately; SourceAccess.Reconcile can block in backend
Close with no context contract. Cleanup cannot promise a bounded join. Close seals
admission and closes owned loopback listeners; it never proves retirement.
Pending(ctx) counts unproven local lifetimes, input/encoder work and reservations.
Drain timeout is a failed drain, not permission to delete placement records.

PrepareSource invokes a concrete authenticated source-owner control RPC with an
immutable operation, viewer, title, release selection and client capabilities.
It calls the real Manager.PrepareSurfaceSource; only opaque PlayID, immutable
revision, release ID and media facts cross the wire. RetirePreparedSource retires
pre-reservation preparations. Uncertain preparation retries use the identical
operation. StartPreparedHLS reserves durable capacity, binds the exact source at
its owner, and assigns only the stable handle to the worker. Worker resolves that
handle through the authenticated source RPC, then Go nodesource mTLS and nodeinput
loopback input feed the existing encoder. FFmpeg receives no cluster credentials
or provider addresses. Source grants bind the entire durable fence and viewer,
revision, controller/target identities and boots. Raw source expiry minus declared
skew must cover the execution deadline.

The neutral broker's DistributedHLS hook currently starts from its locally
prepared exact source. It preserves local optional transcoder interfaces. Remote
source preparation has a real exported API and offline three-role fixture, but
neutral remote plan/session ownership adaptation is still required before a
server-only frontend can use it through ordinary client admission. Unsupported
requests stay local only before distributed ownership. An allocated session never
falls back to a second encoder on transport loss.

Tick retries unknown reservation outcomes with the same immutable operation.
Failures after reservation retire exact source responsibility and encoder token;
a start transport failure retains Session. Successful source renewal followed by
unaccepted worker renewal retires the generation; no historical source validation
is replayed. Successful retirement marks terminal local generation and durable
placement floor. New preparation may use the same public ID after both proofs.
Indexed route lookup uses one session row, independent of catalogue size.
ForwardMedia performs one mTLS ownership hop to the owning broker; grant and live
viewer/media policy remain checked there. HEAD/Range/init/cache representation
headers and streaming backpressure pass through; redirects/private addresses and
peer credential headers do not. Scoped SessionAccess delivery is deliberately
not forwarded by the broker yet; public grant and protocol-scoped delivery are.

## Remaining enablement gates

The initial coordinator gate accepts fixed HLS only. The remote protocol worker's
explicit ABR/DASH/HTTP options must be negotiated with worker capability facts
before those requests route remotely; subtitles, probe, source swap, audio filters,
enhancement, external streams, live input and keyframe metadata remain rejected.
The broker's ABR and specialized session controls need their own explicit remote
hooks; they must not disappear behind a base HLSSession adapter.

Fleet target selection currently chooses one eligible durable transcode target;
codec/hardware capability matching, saturated-target alternatives and scheduling
fairness remain to be integrated. The existing atomic placement reservation is
the only admission authority. Source-only broker planning, durable coordinator
restart reconstruction and safe continuity recovery remain gated. A node's lease
expiry, absent membership or network silence never authorizes another encoder.

Recovery seam: retain stable public ID/URL and exact generation ownership. On a
supported deterministic failure first obtain encoder retirement and independent
source retirement, or an explicitly trusted external host-fencing attestation.
Then reopen the exact revision at an independently decodable keyframe, allocate
strictly increasing output sequence/timestamps, and publish a continuity-aware
manifest through stable delivery. Source affinity must preserve the same bytes
or exact revision reacquisition must prove identity. Required client buffer is at
least detection + retirement/fencing + source reopen + keyframe/encoder restart +
manifest publication + transfer margin; these intervals must be measured in the
node-kill/Kubernetes lab. No finite buffer hides an arbitrary partition without a
bounded external fencing service. This increment does not implement that
continuity allocator or claim invisible failover, FFmpeg/GPU or Kubernetes proof.

Focused offline fixture exercises real three-peer mTLS, remote source preparation,
Go source bytes through nodeinput, an instrumented OwnedTranscoder, segment Range
forwarding, source renewal/worker acceptance replay and exact source+encoder
retirement. It also verifies no-target retryability and unknown committed Reserve
reconciliation without a second encoder. Application/coordinator owns full gates,
PG14 matrix extension and real encoder/deployment acceptance.

## Specialist broker source adapter

NewRemotePlayback explicitly implements the neutral broker's core.Playback,
SurfaceScopedPlayback, OwnedSourcePlayback, revision pin and source snapshot
contracts. It is injected only into the specialist server graph. The existing
full-server/Plex Manager remains unchanged. Its Estimate and source selector are
explicit local metadata/membership dependencies; it constructs no provider or
encoder. DecisionTranscoder supplies only the trusted decision function; actual
encoding remains owned by DistributedHLS.

Ordinary Plan prepares the exact source owner through concrete authenticated RPC,
returns only media facts and a trusted in-process source revision facade, and
retains an exact remote lifetime even after an uncertain response. Tick reconciles
that same immutable preparation operation before retirement. Remote Session map
presence is metadata, not fresh authorization. Pin/CurrentSource call authenticated
prepare-status and source owner policy; Range additionally checks current policy
at source admission. PreparedBytesPath proxies normal Manager Range/HEAD/cache
semantics through verified Go mTLS with no whole-stream client timeout. Sources
are pinned to their captured revision; source failover is unavailable until the
explicit recovery/continuity contracts are connected. No forced remux decision
hides direct playback capability gaps. BindClientSession records the public ID's
controller for indexed delivery, with generation zero before encoder reservation.

The broker's optional EncoderRetirement lookup is independent of Session serving
availability and retains a token through pending reservation/start and stopped
executions. It closes only after exact terminal reconciliation, including a fresh
proof that a failed reservation had no side effect. Stop/expired lease/network
errors never close this token on their own.

## Same-owner reopen attestation

ReopenSource and Runtime.VerifyReopen provide a concrete same-owner implementation
of playbackcontinuity.SourceReopenVerifier. This is limited to the original exact
owner node AND boot. The original authenticated preparation must remain available;
its exact source lifetime and producers must have retired successfully, and the
old complete placement fence must already be durably retired. Reopen uses the
original authorized exact release through the real Manager, then requires fresh
source revision AND size equality. This relies on the existing resolver/source
immutable revision contract; equality does not invent a new byte hash guarantee.
Cross-host and cross-boot equivalence are unsupported.

The bounded source-owner registry returns an opaque attestation tied to the
immutable reopen operation, original binding, original owner boot, old/new PlayID,
revision and size. Verification rechecks current source policy, exact new route,
and complete current placement authority. The first verified new fence binds
permanently; another generation cannot reuse it. Historical successful verification
is never current execution authority after retirement. A lost response retains the
same operation; it must not prompt an alternate source/encoder start.

This provider does not validate final delivery buffer retention, measured recovery
bounds, independent restart or real muxed audio/video clocks. Those independent
EvidenceVerifier methods and a surviving delivery buffer remain required before
playbackcontinuity can drive an enabled recovery. Source-host loss still needs a
separate authoritative immutable reopen provider plus real external fencing.

## Concrete retirement evidence and atomic publication

Pre-reservation target diagnostics return SelectionError with only aggregate
eligible/exhausted/unsupported/unavailable counts. ErrNoEligibleTarget means no
configured live candidate passed membership/role admission. ErrTargetUnavailable
means at least one candidate had an unresolved pool or authenticated capability
check; it is not saturation. ErrUnsupported means a candidate declined the exact
request or negotiated capabilities. ErrCapacity is reserved for known exhausted
advisory pools (all eligible candidates) or atomic placement admission. These
observations never authorize retries after reservation, release, or retirement.

Runtime now constructs placement.History with the configured cluster and itself as
an independent re-fetching ReceiptVerifier. Read-only EvidencePath permits exact
current controller proof reads while its lease remains active; a different live
server may read only after the exact old authority is revoked/expired. It checks
the complete current ledger fence and the actual intended worker/source boot on
every observation. No encoder or source start, renewal or output occurs there.

Worker.TerminalEvidence supplies the retained actual immutable Start hash,
initial/accepted lease CAS, explicit negative admission barrier when applicable,
and separately domain-bound joined encoder/local-input proof. Source evidence
uses actual captured SourceHandle size, exact revision/PlayID/owner boot/fence,
sealed authority+manager acquisition barriers, exact SourceRetirement Done/nil,
zero tracked producers, and joined source verification/acquisition/issuance calls.
Those callbacks register before invoking external authority and run outside the
runtime mutex. Future source calls cannot cross the sealed registry admission.
Bounded expired credential-index eviction only removes rejected credentials; it
never proves source/encoder retirement or releases placement capacity.

Complete independently authenticated proofs use RetireWithReceipt atomically with
claim release. An unknown write result retains the local execution token. The next
Tick first checks the exact immutable receipt, so a committed-but-lost response can
settle without contacting nodes again, including after a replacement. A generic
retired status, absent record, source-only lease extension or enum value cannot
manufacture a receipt. Unsupported worker proof keeps payloads; ordinary Retire
still uses its original verifier and cannot authorize history compaction. No map
or generation-floor eviction is enabled by this change. Pre-placement random
operation histories still require their own durable replay barrier before reuse.

Focused composition checks include independent proof/digest binding, accepted
renewal preservation, a deliberately unjoined source callback, tampered source
proof rejection, atomic receipt insert failure with capacity retained, and a
committed write whose local acknowledgement is lost while both remote endpoints
are unavailable. Historical receipts remain exact and readable after replacement;
they never restore current execution authority.

## Preparation retirement across source boots

The source publishes exact immutable preparation receipts before PendingDetails
can acknowledge quiescence. The key is configured cluster, original source node
and boot, original controller node and boot, and a domain-separated operation
hash. Receipts bind the original request hash and an opaque identity assigned to
the actual retained source lifetime. Media revision/size are captured from the
actual source when available. A preparation that fails before returning media
can still prove its exact owned lifetime Done+nil; missing media facts are not
invented, and this receipt cannot serve as a placement/source-media/reopen proof.
An authentic no-source barrier is explicit and sealed, never inferred from a
missing operation or expired lease.

Publication seals preparation replay/use admission, joins preparation and policy,
activity, Range, binding, issuance, Verify and Acquire callbacks, and requires the
actual lifetime Done+nil plus associated source producers drained. The source
marks bound handles retiring before revocation; late binds must finish and revoke
before publication. Policy/backend callbacks and SQL run outside the runtime
mutex. Failed/unknown writes retain the same payload and pending barrier until a
fresh exact immutable read confirms publication. Source Tick and PendingDetails
perform this publication; source rollout must use this implementation before it
can preserve proof for controllers that outlive the source boot.

RetirePreparedSource and the read-only PreparedSourceRetired first read the exact
receipt for Self as controller. Unknown is not proof. Exact historical source
membership need not remain live after a confirmed receipt, so a source-first
replacement can complete the controller's retained lifetime without pretending to
be the old boot. No wildcard boot seal, retroactive receipt, TTL or map compaction
exists. The old 982 lab process predates these records and cannot be cleared by
this implementation without independent, valid original-boot fencing evidence.

The receipt admission barrier follows Manager's native source identity: blank and
Plex surfaces coincide; different real surfaces stay independent. A nonexact
stale preferred release can normalize to the automatic variant or join its
original picked flight, so differing nonexact preferences can share one lifetime
and are excluded while a predecessor operation remains unreceipted. Exact-release
variants remain distinct. Registration/exclusion is atomic before Manager source
acquisition; retries of the same immutable operation retain their own identity.
Retained receipts are bounded per source boot by HistoryCapacity independently
of concurrent encoder capacity. There is no rolling history release policy yet.

PendingDetails reports numeric barrier categories, including PreparationReceipts
and SourceCallbacks. The total is not a count of distinct sessions; a source,
its placement and its encoder can independently require proof. No raw operation,
viewer identity, grant, provider URL or private address is exposed by that DTO.

## Private initial admission and retained Start commitment

Each attempted remote start captures a detached initial request after the actual
placement reservation and authenticated source bind, before invoking the remote
adapter. Its private execution state binds the full placement fence, owning
controller node/boot, target node/boot, authenticated source node/boot, PlayID,
viewer and exact prepared revision. Route has no separate delivery-node identity;
none is invented. No input URL, headers, grant or input-lifetime actor is retained
in this snapshot. Every mutable request fact is detached, and the adapter receives
another independent copy. The initial request/decision never comes from live
Observe telemetry. Unset protocol/duration remain default instructions rather
than fabricated backend facts.

Session.StartCommitment returns only the exact retained Start identity and SHA256
of the actual immutable marshaled bytes installed before its RPC attempt. It
works on uncertain/stopped tokens independently of Session availability and does
not infer sending, acceptance, source verification or retirement. Runtime captures
it immediately when the owned token returns, outside its mutex, and binds it only
to the matching initial route-derived identity. Mismatch/error retains the exact
token and reservation for conservative retirement; it never authorizes fallback.

Migration v3 persists one immutable sanitized admission and actual Start hash per
cluster/session/generation before any Start RPC bytes. AdapterOptions.CommitStart
runs synchronously outside adapter locks with a context bounded to five seconds.
The outbound Start comes only from installed Session.start bytes. Nil preserves
default adapter behavior. SQL errors are unknown outcomes; only an independent
identical read permits sending. Conflicts, timeout without exact reconciliation,
and full history retain source, placement and owned-token responsibilities.
Records are bounded to 256 KiB and a durable cluster-wide HistoryCapacity,
including every controller boot and retired generation. All coordinators must
configure the same quota. Restart never replenishes it; exhaustion fails closed
for new records while exact retries remain possible. No expiry deletion,
eviction or compaction is provided. Historical
route JSON stays unchanged. The transaction's PostgreSQL route read is not
row-locked with route replacement: a racing replacement may leave an inert old
historical row. Immutable generation keys prevent overwrite, and fresh placement
and route checks after exact write readback gate sending. Worker authorization
still independently fences every execution; a historical row is never authority.
The synthetic write lock is SQLite-only and constant-false, with no history scan.
Explicit fields retain complete keyframes and slate
text omitted by ordinary request JSON. Raw grants, URLs, headers and InputLease
actors never persist. The reservation operation is retained only as a hash.

The package-private Runtime.loadRecoveryAdmission is a concrete historical reader
for a current authenticated viewer and current server boot. It validates exact
current placement/operation/route, original source owner boot/PlayID/revision and
fresh source-owner viewer policy through a read-only authenticated control call.
The source must retain the original bound lifetime. Route/fence are checked again
after policy callbacks, which run outside locks. Missing binding or unavailable
policy fails closed. The reader returns a detached original snapshot and hash;
it installs no execution, starts no encoder, issues no grants, renews no old boot
and supplies no retirement evidence. Historical admission alone cannot resume a
rebooted source or controller. Pre-install failures have no attempted Start and
are not claimed as recovered plans.

The focused offline DB/mTLS invariant reconstructs a new-boot Runtime while the
worker holds received Start before returning a response, loads the original
snapshot/hash, and checks immutable conflicts, unknown writes, bounded history,
stale generation exclusion, current policy and callback mutation. It uses no real
encoder. This adds no public observation API, output observation, successful
splice, client-buffer evidence or recovery enablement. Later receipt consumers
still need reviewed actual source-index, complete packaged-picture/sample ends,
final transformed bytes, current authority and fencing contracts.

## Private exact-operation gate storage prerequisite

Migration v4 reserves one private operation gate atomically with each NEW
immutable admission, under the same cluster-wide durable HistoryCapacity.
Every original boot and retired/replaced generation remains charged. There is
one accepted-attempt nonce/worker-boot slot, one arm association (at most 16 KiB),
and one irreversible seal with closed reasons and bounded actor identity.
Admission digest v1 covers the exact sanitized original record, including the
explicit keyframes/slate, entire Route, operation hash and exact Start identity
and hash. No independent attempt history, expiry, eviction or compaction exists.
Exact historical retries, reads and sealing remain usable at capacity.

Package-private storage methods serialize reservation, claim, seal and arm on the
same component-owned transaction lock on SQLite and PostgreSQL. Ambiguous writes
require independent exact readback. Typed historical results distinguish unknown,
absent/unsupported, unclaimed, claimed and both sealed states. A missing gate on a
legacy admission is unsupported/unknown; retries never backfill one or declare
that operation never started. A matching sealed retry returns history only.
Conflicts deny replacement of immutable bindings, slots or seal metadata.

These helpers provide bookkeeping only. They neither authenticate Worker-owned
accepted handles nor authorize an actor to seal, acknowledge live arming, issue
execution permits, or prove retirement. No mutation method is wired into actual
Start, replay, renewal or ongoing authorization. Those behaviors remain unchanged.
A separate reviewed slice must join actual Worker-owned handle authentication,
provisional acceptance, trusted seal authorization and observer-owned live arming
before any gate can become an execution veto or participate in host evidence.
An arm payload/digest cannot authenticate itself, restore lost kernel handles or
release placement/source obligations. There is no exported gate API, generic
proof callback, host-proof consumer or production recovery enablement here.
