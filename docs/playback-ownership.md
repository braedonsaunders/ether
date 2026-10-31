# Playback authority and retirement ownership

Audit slice 2, based on candidate `82daa1c79`. This is an ownership contract
and gap inventory, not a claim that durable sticky playback is implemented.
The accompanying READ change integrates the reviewed `28f9de73` request seam.
It introduces no migration and changes no broker, encoder or resume ownership.

## Evidence boundaries

| Evidence | Disposition |
| --- | --- |
| `wip/fc-sticky-authority` `9eb78e75` | Unlanded prototype. Consumption ledger, pre-work epochs, atomic bundle and conservative payload markers illustrate required predicates; existing held writers, invalidation and GC are not completely fenced. |
| `wip/fc-sticky-resume` `26c5c23f` | Unlanded test draft distinguishing delivery from URL issuance. It does not make current resume writes delivery-authoritative. |
| READ `28f9de73` | Integrated request-producer evidence. Not a successful source-close outcome. |
| ENCODER `dde2dab7` | Unlanded isolated source/open budgets, owner tickets and outcome tracker. No actual manager/encoder wiring. Do not import a parallel retirement framework. |
| Flight transfer `59632a05` | Unlanded older admission/lease/migration design. Its startup tests do not prove the current common implementation is fenced. Preserve its transfer guarantees, not its obsolete schema. |
| Candidate Lazy Stream Bounds (`refs/review/slice-lazy-bounds`) | Current bounded binding placeholders/flight map and sessions/8 are already present. Do not reintroduce their predecessor tables or reserve migration 8 again. |

Current manager `playback.go` saves resume when Prepare publishes a session;
`failover.go` saves again when replacing a generation. These happen before
successful viewer delivery. `resume.go` held writes are keyed by user/title;
its NZB reference map and GC mutex are local to one store instance. This is not
a durable, cross-node sticky-authority contract. Receipt claims in
`internal/handles/capability` solve scoped atomic admission/claiming, not source
liveness or payload retention.

## One owner, distinct facts

The broker owns published session lifecycle and source plan/session counts.
The manager owns each preparation flight and its acquired source generations.
The encoder owns its input producers independently of an HLS segment request.
A protocol adapter translates IDs and invokes that ownership; it must not add
a second destructive stop path for an already-owned session.

Keys include protocol, viewer, broker-scoped client/device, item, session,
source revision and node incarnation where appropriate. Raw Plex client IDs
are not interchangeable with broker client IDs. A shared release/hash alone
does not identify the same acquired source or node-local payload domain.

Three facts must remain distinct:

1. A request's registered producers have exited and released generation refs.
2. Every acquired source, including prior and provisional generations, has
   closed successfully or transferred to a protected exact-source owner.
3. Every encoder/input producer has retired, with its own outcome.

READ proves only (1). In particular generation release might not close a
source with other references, and `generation.closeSource` discards Close's
error. A closed READ signal therefore cannot prove (2), even when it waited
for a synchronous Close call. A segment request's READ scope proves nothing
about an independent FFmpeg loopback request.

## Linearisation points

| Operation | Required point and failure disposition | Current implementation / remaining work |
| --- | --- | --- |
| Plan | Reserve bounded plan/flight capacity before external work. Flight owns the provisional resource independently of its first waiter. | Broker plan bounds and current lazy flight placeholders exist. Full durable source responsibility is pending. |
| Open | Transfer a plan claim to one accepted session under the owner gate and broker lock. Publish no grant without the exact source/revision attachment. | `transferPlanSourceLocked` orders local counts; durable receipt attachment/commit barrier is pending. |
| Failed Open | Remove only the failed plan's claim under the same owner gate. Close any acquired source only after its last owner leaves. An Open error does not prove no source was acquired. | Local plan lease tests protect existing/shared sources. Detached/late backend completion needs manager-owned close outcomes. |
| Cancellation | Cancellation stops accepting work; only producer exit releases its READ slot. Cancelled waiters never authorize destruction of a still-running shared flight. | READ Begin/Seal mutex orders registration. Slot survives stalled ReadAt and generation release. Durable cancelled-flight responsibility is pending. |
| Stop | Under the broker owner gate, detach exactly the matching session and seal new work. Manager deletion from sessions/byIdent and setting closed blocks session reuse; generation retire seals read admission. | Broker and manager have local linearisation points, not one cross-node durable outcome. Stop return, missing lookup and heartbeat expiry do not authorize payload reclamation. |
| Failover | Attach replacement responsibility before publishing the new generation; retire old input without dropping old read/Close responsibility. Failed preflight must never publish. | Manager generation refs already delay old Close. Durable ownership spanning all prior/late generations and encoder swaps remains pending. |
| Successful delivery | After a successful nonzero viewer Write, atomically commit the source-bound authority CAS, resume row, index and probe. Durable order is admission sequence, not clock or physical network arrival. A failed newer attempt does not supersede an older successful one. | Current save-on-Prepare/failover is insufficient. Loopback/probe/prefetch/HEAD/URL issuance are not delivery. Draft atomic bundle remains unlanded. |
| Renewal | Under the same capability shard/domain lock, CAS the exact still-live scope/revision/incarnation. Revalidate session/grant authority; never resurrect an expired or revoked claim. Commit typed record and header extension together. | Current lazy `renewActiveBinding` and common `RenewHeaderInTx` are the starting primitive. Header expiry alone is not retirement. |
| Cleanup | Reclaim only after confirmed exact-source transfer or successful source and encoder outcomes plus relevant READ drainage. Serialize with renewal and payload writers; bounded batches retain uncertain markers. | Existing TTL/header pruning and local GC cannot stand in for this outcome. Unknown commits, crashes, timeouts and Close errors retain protection. |

READ registration occurs before the copy producer starts and before generation
acquisition. Seal rejects future registration. The producer releases its tracker
only after generation release returns. Failed registration/acquisition releases
its admission slot. A cancelled context cannot start a tracked producer. At most
128 tracked copy producers exist per process across managers; a stuck read or
Close retains a slot. Full admission fails before starting a producer and does
not evict existing work. Existing untracked Plex/loopback callers retain their
admission and bytes; this is not a bound on all legacy producers or on arbitrary
callers of the public tracker. Chunk/read-ahead configuration remains separate.

## Broker seam agreement

Broker lead `thr_fc855ih3dk` owns broker.go, media.go, plex_lookup.go and Plex
wiring. The first delivery is only `core.WithReadRetirement` /
`ReadRetirementFrom`, `Begin`, `Seal`, and playback copy-producer integration.
The broker attaches one tracker to every serving request, seals it when that
handler stops creating producers, and waits on stop and supersede until those
producers release. The signal is request-producer evidence. It does not free a
protected source or capability lease by itself.

Lifecycle worker `thr_8g6zxzjbdu` calls existing Report/Stop; no additional Stop
helper belongs in timeline, terminate or unmount. A cancelled request or a
Report/Stop error is not proof that ownership was absent. A fallback must not
stop a replacement after partial broker teardown. Plex unmount detaches the
mount; runtime shutdown owns broker Close. Legacy transcode IDs and broker
session IDs require their existing explicit translation.

## Remaining durable integration using the common primitive

Do not land the sticky drafts or old flight migrations wholesale. The next
implementation must use common `AdmitHeadersInTx`, `LockHeaderScopeInTx`,
`ClaimHeaderInTx` / `ConsumeHeaderInTx`, and `RenewHeaderInTx` within the domain
transaction, with typed source bindings and bounded admission. Headers are
bookkeeping, not source authority. Typed state must protect resources after a
header expires; generic pruning must not cascade away a live/uncertain owner.

Reserve an idempotent exact-source transfer/barrier under the manager flight
lock, release that lock for database work, attach it to the target session before
publication, commit the durable relationship, then finalize under the flight
lock. Stop captures the barrier without waiting indefinitely. A definite rollback
can release only an unacquired reservation; an unknown commit retains both
protected sides. Duplicate cancel/transfer/cleanup must be idempotent.

All source opens must be covered before Engine.Open, including speculative
preflight and detached late completions. Track successful/error Close outcomes
through all generations and encoder inputs before releasing responsibility.
Budget uncertain outcomes, cleanup reservations and payload markers explicitly;
saturation refuses new acquisition rather than collecting protected resources.

Resume/outbox/pre-work/cachefill/prefetch/invalidation/retention/GC writers must
all participate in the same ordering and payload-domain guard. A reader-only
ledger cannot fence them. No timestamp comparison or normalization of Plex
responses can substitute for that integration.

Mixed-version safety requires the documented maintenance transition: stop all
old nodes and fence their DB credentials before activating authority writes or
GC. Startup must reject unsupported schema/authority versions. Application
advisory locks do not constrain old binaries or direct SQL. Add explicit old
startup/writer refusal tests against the final schema. No rolling-upgrade safety
is claimed. Any new sessions migration requires agreement with the broker lead
and migration acceptance; this READ delivery uses none.

## Validation boundary

READ stalled-read, stalled-Close, cancellation, replacement, concurrent release,
capacity and rejection tests exercise actual producer exit. HTTP parity compares
status, headers and body exactly for full, range, suffix, invalid range and HEAD
responses, without normalization. Existing source lease and Lazy Bounds tests
run on SQLite and PostgreSQL, using the current capability primitive.

These gates do not certify durable sticky authority, cross-node payload GC,
encoder retirement or actual Plex broker adoption. Those remain launch blockers
until their real consumers and failure outcomes are integrated and validated.

## Manager source outcome increment

The manager now reserves bounded backend responsibility before every actual
Engine.Open (initial, failover/preflight, warm-up and verification). Acquired
sources finish that responsibility only after Close returns; the first Close
error is retained and its admission charge is not reclaimed. Prior generations
remain included after failover. Probe-verification HTTP readers also retain
references until they exit. This replaces the earlier discarded-Close-error gap
for these actual manager opens; it does not implement encoder retirement.

`PrepareSurfaceSource` returns the shared flight's exact source outcome on
cancellation/failure, or the accepted session's outcome on success. Cancelling a
non-last waiter never seals a source needed by another waiter. Failed/unpublished
preparations seal their scope; acquired or late sources remain pending through
Close. The manager bounds backend opens/uncertain Close outcomes at1024 and
asynchronous cleanup tasks at128. Saturation refuses acquisition/detach first.

`RetireSource(id, expected)` validates and detaches one exact manager-private
lifetime in the same manager critical section used for reuse/publication. Cleanup
runs after unlocking in a bounded manager-owned task. Shutdown drains these
tasks for at most one second, reports unfinished cleanup, and retains their
source outcomes/accounting; it starts no extra waiter goroutine. The existing
synchronous Stop path retains its cleanup/event order. Duplicate or stale expected
lifetimes cannot detach a replacement. Callers must hold no broker owner gate.

There is deliberately no broker caller yet. An outcome is not an external owner
lease: a Prepare that linearized before detach may still return that lifetime.
A durable claim/transfer barrier is required before broker ownership can safely
cross a released owner gate. This API does not prove that barrier, last external
owner release, encoder completion, payload cleanup, or persistent crash recovery.

## Durable payload guard increment (playback/4)

Released `playback/2` is the cluster live-session registry and remains immutable.
Released main owns `playback/3` (`original_language` on `playback_resume`).
Payload ownership is `playback/4`. An earlier draft reused `playback/2` and
was rejected by upgrade/cluster gates. The delivered9503 fixture caught it.
Migration preflight refuses unversioned payload tables or reserved indexes on
both dialects, including a same-name incompatible index; it does not adopt an
unknown schema. Repair preserves existing delivered rows and then reruns the
payload migration.

A verified directory has a persisted opaque domain identity and at most128
fixed witness-slot files. Each live store holds an exclusive kernel witness
(flock on Unix, LockFileEx on Windows) for its exact domain/incarnation. Its
registry row and typed owners/deleting markers use RESTRICT references. Source
opens, resume writes and NZB reads reserve before touching the resource. Common
headers and RetirementOwnerClaim are committed atomically with typed owners;
expiry maintenance skips them. Expiry/heartbeat never releases ownership.

The retention anchor identifies only exact `(domain, payload hash, incarnation)`.
It grants no viewer authorization and does not identify the same backend source.
The first retain commits bounded common-header admission and its typed anchor.
Later retains always use the same context-aware gate, verify the held witness,
and increment per-use references. They can reuse a previously committed anchor
during a DB outage. A fresh unknown payload still requires durable admission.
Each actual Engine.Open/Close continues to have its own source-lifetime ticket.

Last release marks the anchor retiring before database work; racing retain either
wins before that transition or refuses until a fresh anchor can be issued. The
owner retirement and any required deleting marker commit in one transaction.
Only then is the file removed; only confirmed removal clears the marker.
Unknown commit/Close/deletion retains its witness and bounded charge. Duplicate
per-use releases are idempotent, and a released writer cannot replay through a
shared anchor retained by another source. Resume row, index and probe updates
now commit together behind the payload lock and exact writer-owner check.
Consumption epochs and delivery authority are still separate unfinished work.

Startup and a bounded pin-maintenance actor reconcile dead incarnations. They
must acquire the exact verified witness nonblocking before retiring any owners.
A live stalled Close, missing witness, changed content or changed filesystem
identity cannot authorize recovery. Batches retain deletion markers across
crash mid-commit and mid-delete. Remaining owners beyond one batch are revisited;
no timestamp declares a process dead. Unknown outcomes keep process roots so
ordinary Manager garbage collection cannot release a still-needed witness.
All128 slots held or unverifiable refuses a new manager, with no eviction.

Reclamation requires a verified local filesystem. Linux admits known local
ext/XFS/Btrfs/tmpfs/overlay/ZFS types; Darwin admits local APFS/HFS; Windows checks
the final opened directory path, drive type and NTFS/ReFS filesystem. SMB, NFS,
remote/unknown filesystems and unresolved identities fail closed and keep
existing ownership. Windows build/vet is required, but cross-compilation is not
native Windows runtime certification. No service or production host is contacted.

The mandatory first-Prism maintenance gate/runbook remains the old-writer fence.
These witnesses cannot constrain an old process that never implemented them;
all old processes must be stopped and credentials fenced before activation.
Broker exact-source transfer, external-owner claims, encoder retirement and
moving broker cleanup outside owner gates remain required before full adoption.

## Historical local observation follow-up

CurrentSource now copies a generation-owned observation validated before and
after actual source probe, outside manager/session locks. Unsupported identity
or replacement during probe clears identity and scalar facts. Report/Stop reads
perform no filesystem/source callback I/O; HDR pointers are defensive copies.
This is historical open evidence, not fresh authorization or proof that a local
path stayed immutable. Fresh PinSourceRevision and serve checks are unchanged.

## Staged encoder work accounting

The encoder increment reserves an exact session lifetime before startup and
output-directory creation. It has no broker caller and supplies no durable
transfer or sticky-consumption authority. Its source review and execution gates
are separate from the payload/3 and historical-observation receipts above.

Process-wide admission bounds 1,024 lifetimes, each with at most 64 outstanding
work tickets. Four fixed cleanup workers share a queue of 1,024 admitted
lifetimes. Sealing prevents new tickets and queues exact-object detachment;
workers do not wait for process exit. Starts, session-owned `Runner.Run`
preflights, run hints, collectors and reapers finish their own tickets only when
the corresponding operation actually returns. A run reserves its collector and
future Stop tickets before Start, including a process returned together with an
error. A stopped or superseded late start is reaped without publication.

`RetireEncoder` seals without taking manager/session locks. Legacy `Close` waits
on the exact outcome for ten seconds; successful completion still requires
process/reaper/collector completion before directory removal, session removal
and capacity release. A stalled Start, Run, Stop, collector, filesystem cleanup
or shutdown worker returns/retains pending responsibility. A confirmed cleanup
error is a terminal error and retains directory/lifetime/encoder charges. Another
manager in the same process skips protected output directories at startup.
Manager capacity uses scalar lifetime charges and never takes a session mutex
under the manager mutex. Shutdown has one shared deadline and no per-Close
WaitGroup waiter or goroutine per detached session.

`EncoderDone` closes only at a terminal result; `EncoderErr` is the explicit
`core.ErrEncoderPending` sentinel beforehand. A terminal error cannot later turn
into successful proof. Until the separately reviewed manager-issued input lease
is implemented, an actual input attempt terminates with
`core.ErrEncoderInputUnverified` after proven legacy work cleanup. That error
cannot authorize broker retirement, but it does not retain completed legacy
process/finalizer capacity. Missing optional input proof must never exhaust
ordinary successful start/close cycles. Source retirement proves neither encoder
work nor input drain. Native/global probe processes remain outside this increment.

Same-ID admission has its own exact-lifetime reservation before directory/start
work. A concurrent replacement seals even an unpublished prior reservation and
waits outside locks for confirmed legacy cleanup before claiming the ID. Timeout
or terminal cleanup error refuses replacement. Publication checks that exact ID
reservation together with the seal/manager state, so a late older start cannot
publish over its successor. The controlled test holds the first external Start,
lets the second seal it, then proves the process lifetimes never overlap.

## Staged verified encoder input leases

The optional `HLSRequest.InputLease` is excluded from JSON. Only trusted host
composition can pass the opaque object minted by
`PlaybackEncoderInputs.ReserveEncoderInput(ctx, id, exactSourceOutcome)`. The
broker mints one lease before `StartHLS` when playback implements that seam and
seals it if the encoder is not published. A live lease refuses `RetireSource`
and generation replacement. A manager validates the actual
private Session and SourceLifetime together when minting. At most 128 scopes
exist process-wide; no eviction or authority from a provider header is possible.
Unclaimed leases remain caller-owned if admission/validation fails before claim;
the caller must seal unused leases. A successfully claimed lease transfers its
input-drain responsibility to that exact encoder lifetime.

`ClaimInput(onDrained)` succeeds once, before seal. A second claim, claim after
seal or nil callback refuses without storing that caller's callback. The encoder
reserves its input ticket before claim; failed claims release it exactly once and
never seal another encoder's lease. Successful claims yield exactly one callback,
including a callback racing the claim return. Zero producers or producers already
released still require seal before drain. The callback may only perform bounded
accounting/requeue. It runs after the producer count is committed drained, the
manager registry entry/global scope slot are released and InputDone is closed,
outside scope, source, generation, manager and session locks.

`HeadersForInput` checks the supplied URL's exact manager loopback origin,
canonical issued generation path and optional sole `background=1` query. It
rejects provider/redirect URLs, foreign ports, userinfo, fragments, escaped paths
and unknown generations. Every use validates token membership against the
captured Session and SourceLifetime. Future failover generations may attach only
when issued by that manager to the same captured responsibility. A later current
session or a replacement with the same client identity cannot be substituted.
The permitted loopback route serves representations and never redirects; no
scope-bearing command is constructed for a provider or redirect locator.

The internal header is reserved separately from provider InputHeaders. Run and
picture/audio sample commands bind it independently before execution; it does
not disable audio matching by appearing to be provider metadata. Plain provider
headers retain their existing construction, selection and redaction. Raw
provider attempts to set the reserved control header refuse. Input values are
redacted before stderr truncation and in command/start/exit diagnostics; opaque
lease formatting hides the internal value. The lease grants no viewer, policy,
source-open or transfer authorization.

A loopback request with a supplied forged/sealed/mismatched scope fails closed.
Unscoped reads preserve legacy behavior. Request READ and input trackers may
coexist: both register before generation acquire, and both finish after release
returns. The global producer slot is returned before input callbacks. Source
Close success remains an independent SourceRetirement obligation.

To avoid a cycle, the encoder's actual-work count excludes its one retained input
ticket when deciding to seal. A bounded cleanup task calls SealInput outside all
lifecycle locks and returns immediately. A stalled producer occupies neither a
waiter goroutine nor a cleanup worker; its callback later releases that input
ticket and requeues cleanup. EncoderDone/charges wait for actual work AND verified
input drain. Missing optional proof remains ErrEncoderInputUnverified after
confirmed legacy cleanup, with no repeated-close capacity leak. An input proof
error or unknown actual work retains its cleanup/charge and never becomes nil.

Same-ID startup serialization is a **single Manager** guarantee. Independent
Managers do not share the ID map. The process-wide admission bound and skipping
currently protected output directories do not assert cross-manager process
serialization or durable transfer authority.

Scoped live-playlist input refuses before claim: the private VOD scope must never
be forwarded to child playlist/provider requests. Ordinary unscoped live playback
keeps its existing header and demuxer behavior. Scoped run and sample commands
retain the VOD demuxer restrictions, and only the exact nonredirecting loopback
representation route can receive the private value.

Scalar capacity follows decision changes between runs: noncopy decisions retain
their reservation even between processes, while copy transitions retain all
previous real-run charges until both watcher and reaper finish. Sealing a verified
input with prior encoding retains that charge while input drain is unproved.

## Broker transfer on the current primitive

Schema 9/10 lease and transfer tables are not revived. A flight claims
`retirement-owner` on the existing lazy-stream header and admits a second
`broker-transfer` header for the target side, in one transaction. Expiry
pruning skips that claim, and the schema 8 trigger still rejects deleting the
stream while its header exists, including an old process's bulk delete.
The lazy claim lasts for the in-flight response. A failed or unknown commit
does not delete either side and does not stop the prepared source.

Open records one `broker-transfer` header with the same claim. Stop, supersede,
and live stop drop it only after the backend outcome is successful:
`Live.Stop` returned nil, or `RetireSource` / `RetireEncoder` reached a nil
error. `ErrEncoderInputUnverified` and a source Close error keep the claim.
Read retirement still proves only that the request's producers exited.
Stop does not wait on the database to drop the claim. There is no sessions
migration in this increment.
