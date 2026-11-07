# Client-observed HLS loss measurements

Build with `CGO_ENABLED=0 go build -o /tools/hlsplay ./cmd/hlsplay`. The simulator uses the lab viewer's
`X-Plex-Token`, Plex universal decision and start endpoints, the returned master
and variant URLs, and timeline/stop calls. URLs and tokens are omitted from the
reports. Supply a token file rather than a command-line secret. Output must be a
new directory outside git. Redirects and cross-origin media are refused.

```sh
hlsplay -url https://ether.lab.example -token-file /private/viewer-token \
  -rating-key 1000000000900001 -output /private/evidence/reference \
  -window 90s -buffer 30s -ffmpeg /tools/ffmpeg -ffprobe /tools/ffprobe
hlsplay -url https://ether.lab.example -token-file /private/viewer-token \
  -rating-key 1000000000900001 -output /private/evidence/baseline \
  -reference /private/evidence/reference/report.json
```

The first capture supplies reference AUs; it exits nonzero with
`continuity-unverified` because no independent reference was supplied. Repeat
with that reference to establish a clean baseline. Match media, codec profile,
segment grid and offset. Reference segments are matched to the fetched index
window. A reference must contain that whole window. Never use a run as its own
reference to certify recovery.

`events.jsonl` is flushed continuously, including HTTP attempts and every
received segment. `report.json` and `report.md` contain hashes, byte counts,
latency, retries, buffer samples, stalls and shared `internal/splicecheck`
results. Status zero means a transport failure before an HTTP response.
First-byte latency is measured with `httptrace`, not response completion.
The CLI also writes mode0600 `http-*.private.json` and `.body` snapshots for
playlists and failed requests, capped at64KiB per body. Credential headers are
redacted; private URLs and playlist bodies can contain read capabilities.
`-private-http=false` disables these snapshots. Public reports omit URLs.
Playback begins when the startup buffer reaches the target (default 30s).
Thereafter buffer consumption uses monotonic elapsed time, including blocked
requests and exponential retry sleeps. The bounded window includes startup;
choose a window longer than startup plus the desired steady-state observation.
Terminal request failure continues consuming the buffer until the window ends,
so an outage longer than the remaining buffer is measured as a stall.
An unrecoverable failure before startup returns immediately because playback
has no forward buffer to consume.
No further media requests are sent after retry exhaustion. That stall is the
configured client's outcome; it does not establish how long the server remained
unavailable after the last attempt.

The current player supports clear MPEG-TS HLS. Encrypted, discontinuous,
byte-range and fMP4 playlists fail explicitly. The shared checker also supports
fMP4; adding that representation to the player is a separate capability.

## Loss runner

Use only an isolated lab prepared with generated media and redundant capacity.
`run.py` accepts any kubeconfig and ingress URL. Run:

```sh
python3 packaging/kubernetes/losstest/run.py --config /private/loss.json \
  --output /private/evidence/transcode-loss
```

Example private configuration (paths and target pod are lab-specific):

```json
{
  "url": "https://ether.lab.example",
  "tokenFile": "/private/viewer-token",
  "ratingKey": "1000000000900001",
  "hlsplay": "/tools/hlsplay",
  "ffmpeg": "/tools/ffmpeg",
  "ffprobe": "/tools/ffprobe",
  "reference": "/private/evidence/reference/report.json",
  "kubeconfig": "/private/kubeconfig",
  "namespace": "ether-lab",
  "pod": "ether-transcode-0",
  "role": "transcode",
  "action": "force-delete",
  "phase": "steady",
  "windowSeconds": 90,
  "bufferSeconds": 30
}
```

Optional `tries` (default 8), `requestTimeoutSeconds` (5) and `backoffSeconds`
(0.2, exponential with a 16× cap) configure the client retry policy. The runner
records that policy. `control` reports the final Stop status and all failed
lifecycle requests separately. Overall acceptance requires every lifecycle request to succeed and an actual
terminal Stop with a 2xx response. Periodic progress failures set
`allRequestsPassed=false` even when final Stop succeeds. `stopPassed` reports
Stop independently. Media continuity alone cannot certify session cleanup.

`fault-dispatch.json` records the session, local time before action dispatch and
immutable target inventory. A trusted post-check can use this time with a
replacement production event on the same host clock to establish a conservative
interruption upper bound; client receipt time is still reported separately.

The runner samples its own Linux host's load average and CPU, I/O and memory
pressure into `host-load.jsonl` at a nominal one-second interval. Each row has
its capture time and read duration. Missing `/proc` counters are null. Sampling
pauses during bounded fault-adapter calls; these readings describe the runner
host and do not reconstruct earlier load or remote worker load.

Force-delete pins the inspected pod UID in a Kubernetes DeleteOptions precondition;
it cannot delete a replacement by reusing its name. Other actions (`pause`,
`partition`, `container-kill`, `host-stop`) require `lossArgv` and `restoreArgv`
arrays. Use the multihost adapter with an immutable target ID. Both commands
have 20s deadlines; restore runs even after player failure. No shell expansion
is performed. Restore must be idempotent after a partially failed loss action.
A pause lasts through the observation window, so failover must recover while
the original worker remains unavailable. The runner reaps its player on exit.

For `--matrix`, add `targets` keyed by controller/source/transcode/delivery,
with each target's `pod`, `lossArgv` and `restoreArgv`. The runner covers each
role × force-delete/pause × early/steady (16 runs). Early injects 2s after the
first segment; steady injects 10s after startup buffering completes. Targets
must have spare capacity and the same shared fixture. The runner captures the
current UID independently for each run. Actions that do not occur before the
player exits fail acceptance. No-loss (`action: "none"`) is also supported.

## Honest server interruption evidence

Client receipt latency cannot prove when a replacement encoder produced data.
An optional trusted observer writes `producerEvidence`, a JSON array with
`session`, `generation`, `segmentIndex`, `producedAt` (UTC RFC3339). Supply its
path to the runner; hlsplay consumes the generation mapping before invoking the
shared checker. The observer must scope rows to the unique session and match
the exact output artifact. The runner reports kill-to-first-production in a
new generation only when both pre-loss and replacement production are present.
Otherwise the server metric is null/unverified. Transcode loss cannot pass
without that metric. Controller/source/delivery recovery need not replace a
healthy encoder, so their server metric may remain unverified; client
continuity acceptance still requires every checker predicate.

`clientFirstPostLossReceiptSeconds` is explicitly separate. It may describe a
cached segment and is never interpreted as replacement evidence. This harness
measures playback; it does not itself certify authority or stale-writer fencing.

## Generated-media local lab

The opt-in integration lab runs a single-node Ether Plex handler with a real
transcode manager, generated H.264/AAC media, and local catalog/auth fixtures.
It contacts no providers and creates no account/server claims. FFmpeg is paced
so fault injection cannot occur after all output has already been generated.
The fault kills only the exact process returned by this lab's encoder runner.

```sh
CGO_ENABLED=0 GOMAXPROCS=4 \
HLSPLAY_LAB_OUTPUT=/private/evidence/local-lab \
HLSPLAY_FFMPEG=/tools/ffmpeg HLSPLAY_FFPROBE=/tools/ffprobe \
go test -p 2 -tags integration ./internal/plex -run '^TestHLSPlayerLab$' -count=1 -v
```

This is real-handler local playback evidence, with fixture catalog/auth; it is
not a full-daemon, Kubernetes, ingress or node-recovery certification. The
release lab must run the loss matrix against deployed roles for those claims.

To validate the CLI and Python runner together with that same generated fixture,
set `HLSPLAY_LAB_BINARY` to the built `hlsplay` absolute path,
`HLSPLAY_LAB_DRIVER` to this directory's `run.py` absolute path, and
`HLSPLAY_LAB_REFERENCE` to the first lab's `reference/report.json`. Choose a new
`HLSPLAY_LAB_OUTPUT` and run only `TestHLSPlayerDriverLab` with the same integration
tag and tool paths. That probe starts another real-handler local node and
requires the outer no-loss `report.json` to pass.

For replicated labs, optional `targetArgv` resolves the session's active role at
injection time. Each argv element may contain `{session}`. The command emits a
JSON object with `pod` and optionally `uid`, `lossArgv`, `restoreArgv`. Force-delete
re-inspects that pod, verifies the selected UID if provided, and pins the delete
to it. This avoids injecting into an idle replica selected before playback.
Pause adapters can return commands scoped to the selected container. The lookup
and its result are retained in the run report.

An adapter that owns restoration with a trap can use `lossWrapperArgv` instead
of separate loss/restore commands. The runner keeps that process alive through
the observation window and gives it piped stdin. Its verifier writes the path
substituted for `{readyFile}` only after the fault is active, then blocks on
stdin. Closing stdin on completion lets the verifier exit and the adapter
restore its exact container. The runner waits for restoration and reaps the
adapter; premature adapter exit fails the run.

## Jellyfin client

Use `-protocol jellyfin -prefix /jellyfin` against an enabled MediaBrowser
surface. Supply either its device-bound token with `-token-file` and
`-device-id`, or `-credentials-file` pointing to private JSON with `username`
and `password`. The latter performs `Users/AuthenticateByName` with a fresh
device identity; passwords and granted tokens are never printed or persisted.
The client verifies `Users/Me`, posts PlaybackInfo with an HLS H264/AAC TS
profile, fetches the authored master and variant, and posts Playing,
Playing/Progress and Playing/Stopped with the actual PlaySessionId and ticks.
It shares the buffer, retry, segment ledger and continuity checker with Plex.
The matrix requires a `jellyfin` object containing that surface's URL/auth and
independent no-loss reference. It appends Jellyfin controller force-delete and
transcode force-delete cases to the 16 Plex cases. A disabled or unrouted
Jellyfin surface is an unmet prerequisite, never a successful skipped case.

`TestHLSJellyfinLab` (integration tag) validates login, real MediaBrowser
handlers, real broker, encoder and media grants with fixture catalog/auth.
Set the same FFmpeg/FFprobe/output variables as the Plex lab and additionally
`HLSPLAY_LAB_INPUT` to the generated MP4. It captures its own independent
reference before requiring clean no-loss playback and HTTP204 progress replies.

For admission diagnosis, Plex supports `-max-video-bitrate`,
`-video-resolution`, `-direct-stream` and `-direct-play`. The explicit
`-skip-decision` comparison is always labeled `diagnostic-only` when media
plays; it can never pass acceptance. Decision followed by start remains the
primary Plex flow.

Wrapper restoration has a separate `restoreTimeoutSeconds` bound (default150,
maximum180) so an exact-container adapter can verify an absent old task and a
ready replacement after kubelet recovery. This does not authorize targeting the
replacement or infer ownership retirement. Simple command adapters retain the
20-second command bound.

The player freezes the first authored variant before any fault, saves it as
`initial-variant.private.m3u8` (may contain grants; mode0600), and records its
SHA256 and global title-time boundaries in `InitialTimeline`. The checker uses
an independent reference's frozen timeline when supplied, otherwise this initial
plan. A replacement playlist never supplies expected boundaries. This preserves
short FastStart opening slots while detecting a replacement that changes them.

For an explicit cross-replica control probe, `-decision-origin`, `-start-origin`
and `-stop-origin` pin only those Plex control requests to supplied lab origins
(for example pod port-forwards B/A/B). Media and authored playlist resolution
keep the shared ingress origin. Redirects remain refused; origin pins cannot
contain credentials, queries, fragments or path prefixes. Reports mark
`ControlOriginsPinned` so this diagnostic placement is distinguishable from
ordinary ingress load balancing.

A coordinated run may supply a unique `session`. Clock-based CPU cases can
use `steadySeconds` (default10) after startup buffering. Native hardware loss
uses the observed-state gate below; elapsed time cannot establish that its
encoder is alive or that output remains to be regenerated.

## Encoder-state loss and independent recovery proof

`phase: "state"` requires `faultTriggerArgv`. After playback begins, the runner
polls this command (default once per second); `{session}` is substituted in argv.
It must return JSON with boolean `ready` and observed evidence. Polls are saved
in `trigger-events.jsonl`; the ready observation is retained in the report.
The native owner validates a LIVE HLS/NVENC process, at least two actual ahead
throttle restarts, matching node/boot/session placement, fresh state and less
than half the title produced. Elapsed time alone never triggers this case.

The kill-only wrapper rechecks those predicates immediately before killing its
retained Ether process. With `requireFaultReadyJSON: true`, its ready file must
contain JSON with `verified: true` and the exact `session`. Include final native
produced cutoff *after* the job is empty. It then blocks on stdin through the
CPU tail and verifies cleanup on EOF; it never restarts the native worker.

Optional `postCheckArgv` runs a bounded independent observer after playback and
restoration. Placeholders: `{session}`, `{playerReport}`, `{faultReadyFile}`,
`{runDir}`. JSON must contain `passed: true` and the exact `session`; otherwise
it fails the run. Native acceptance must show fetched segments after the FINAL
native-produced cutoff belonging to a new CPU execution generation, plus the
retained pre-kill NVENC/restart proof and all shared continuity checks. Serving
already-encoded buffered output does not satisfy this condition.

A server production observer may report `serverInterruptionUpperBoundSeconds`
and `serverInterruptionUpperBoundVerified`. This is separate from exact
production interruption and client receipt latency, and must be below the
configured buffer. For example, kill-dispatch to the first verified CPU segment
ready on the same measured host clock is a conservative upper bound; do not
substitute a startup/RPC acknowledgement or a fabricated production timestamp.

The same client also accepts `-protocol emby -prefix /emby`, declares an Emby
client identity and authenticates against that independent MediaBrowser surface.
Jellyfin and Emby use separate credentials/grants, references and output
artifacts; neither result is inferred from the other surface's success.

Native conditional fault dispatch uses `native_fault.arm`: bind the fresh trigger
observation and startup identity, retain the accepted or rejected observation,
and enqueue a single request without a second encoder-state network read. The
native supervisor must still perform its fresh retained-handle, restart-boundary
and authored-timeline checks immediately before killing the owned process. An
armed request is not proof that a fault occurred; acceptance requires the final
native fault/cleanup proof and new-generation client tail.

## Loss at an early authored segment boundary

A ramp case at index 1, 2 or 3 needs a request barrier because normal 30-second
prefetch passes those indices before playback starts. Set `phase: "boundary"`,
`pauseBeforeIndex: N`, `role: "transcode"`, `action: "force-delete"`, an independent
same-profile `reference`, and `barrierFaultProofArgv`. The runner starts hlsplay
with a fresh `segment-barrier` directory. After receiving exactly indices
`0..N-1`, the player atomically publishes `ready.json` with the actual admitted
session, N and the initial playlist digest. It sends no request for N until
release. The monotonic playback clock and buffer model continue normally; a
barrier before startup contributes to startup latency, and a barrier during
playback can produce a stall. Neither buffer size nor segment grid changes.

The trusted `targetArgv` selector must also return `barrierNodeInventory` with
`kubernetesNode`, `kubernetesNodeUID` and the immutable `ownedNodeContainerID`.
The selector verifies the owned lab-container label and matches its name to the
Kubernetes node. Before deletion the runner rechecks that node UID and binds
this inventory to the inspected original pod in `fault-dispatch.json`.

The ordinary UID-preconditioned deletion then runs. Kubernetes
object deletion alone does not release the barrier. `barrierFaultProofArgv` is a
bounded, read-only lab adapter, with `{session}`, `{index}` and `{runDir}` replaced
as argv values. It must verify that the original execution's exact container task
is absent on the original owned node and emit JSON:

```json
{
  "session": "actual-admitted-session",
  "index": 2,
  "oldPodUID": "original-pod-uid",
  "oldContainerIDs": ["containerd://original-ether-container-id"],
  "ownedNodeContainerID": "verified-original-lab-node-container-id",
  "kubernetesNode": "original-kubernetes-node-name",
  "kubernetesNodeUID": "original-kubernetes-node-uid",
  "originalTasksAbsent": true
}
```

The runner compares the proof to both original node identities, pod UID and
Ether container IDs in `fault-dispatch.json`, plus the request barrier session
and index. A shorter contiguous event snapshot remains pending and is re-read
after the marker; gaps, excess indices and changed bindings fail closed. It preserves this proof and atomically writes a session/index/playlist-
bound `release.json` containing its SHA256. The player verifies the proof digest
before requesting N. Missing, stale or mismatched evidence fails closed; timeout
or failed task retirement leaves N unrequested. The normal post-check must still
prove generation advance, floor equal to the delivered prefix, resume at N,
replacement production and full client continuity. A barrier release is not a
recovery acceptance result.

Capture the actual new-runtime decision/admission and initial authored timeline
before choosing any reference. Shared FastStart can ramp H264 output from MPEG4
input; old constant-duration references cannot qualify a new default profile.
Retain a fresh no-fault reference per client/title/profile, including ordinary
cleanup and delivery reruns. Use the initial timeline retained before the fault,
never a replacement playlist, for the shared splice checker.


Before launching any player or fault selector, the loss runner qualifies the
independent reference: successful playback, all six intrinsic continuity checks,
clean lifecycle requests and a successful terminal Stop are mandatory. Missing
frame/AU comparisons are expected for the first no-fault reference and do not
replace those intrinsic checks. A failed reference produces a failed run report
without starting the player or injecting a fault; shell command sequencing cannot
bypass this guard.

Cleanup policy `root-20261107-v1` is prospective. Its 15-second deadline is
anchored at the later of the successful terminal Stop response and successful
fault restoration; delete and no-fault runs use the Stop response. `run.py`
retains restoration in `fault-restoration.json` and the same event values in
`post-check-context.json`. The postcheck command budget remains 20 seconds.

An existing observer may call `cleanup_observer.InlineCleanup.step()` while the
player is running, including its deferred media analysis. The injected backend
provides the policy, bounded read-only lab callbacks, clock, and process inventory
(`cleanup_probe.py` supplies these adapters for the existing lab metadata shape).
Retain the exact target UID, Kubernetes node, Ether container ID and owned node
container before dispatch, including source node/boot for source faults. The
collector compares these identities with the dispatch record and keeps them when
retired placement metadata no longer names a target.

Acceptance requires one completed observation within the deadline showing the
exact session retired with zero current/displaced claims and zero owned FFmpeg
processes. A missing target, incomplete process inventory, late collector, or
only a late zero is invalid measurement, never a cleanup pass. A database query
started before the deadline cannot prove a nonzero state at the deadline merely
because it completes later. Historical reports retain their original policy and
verdicts. `cleanup_probe.py --backend PATH --role-proof PATH` combines this proof
with an existing role postcheck without discarding its session/interruption
fields; it does not perform faults or mutate cluster state.
