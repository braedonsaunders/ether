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
Playback begins when the startup buffer reaches the target (default 30s).
Thereafter buffer consumption uses monotonic elapsed time, including blocked
requests and exponential retry sleeps. The bounded window includes startup;
choose a window longer than startup plus the desired steady-state observation.
Terminal request failure continues consuming the buffer until the window ends,
so an outage longer than the remaining buffer is measured as a stall.

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

Force-delete pins the inspected pod UID in a Kubernetes DeleteOptions precondition;
it cannot delete a replacement by reusing its name. Other actions (`pause`,
`partition`, `container-kill`, `host-stop`) require `lossArgv` and `restoreArgv`
arrays. Use the multihost adapter with an immutable target ID. Both commands
have 20s deadlines; restore runs even after player failure. No shell expansion
is performed. Restore must be idempotent after a partially failed loss action.
A pause lasts through the observation window, so failover must recover while
the original worker remains unavailable. The runner reaps its player on exit.

For `--matrix`, add `targets` keyed by controller/source/transcode/delivery,
with each target's `pod` and fault adapter, plus the required `jellyfin` client
configuration described below. The runner covers each Plex role ×
force-delete/pause × early/steady (16 runs) and two Jellyfin loss runs. Early injects 2s after the
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

The same client also accepts `-protocol emby -prefix /emby`, declares an Emby
client identity and authenticates against that independent MediaBrowser surface.
Jellyfin and Emby use separate credentials/grants, references and output
artifacts; neither result is inferred from the other surface's success.
