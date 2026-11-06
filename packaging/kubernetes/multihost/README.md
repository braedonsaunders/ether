# Isolated Kubernetes lab with an optional second physical host

This lab uses k3s-in-Docker, a dedicated Docker bridge (MTU1280), WireGuard
Flannel (measured MTU1200), pod CIDR10.65.0.0/16 and service CIDR10.66.0.0/16.
No host network namespace, host sysctls, service units, Docker daemon changes,
external accounts or real providers are needed. Existing kind labs are untouched.

The initial topology is one control-plane/server container plus two agent
containers on one physical host. Each node is capped at2GiB (6GiB aggregate).
This topology tests node/container failure, **not physical-host failure**.
PostgreSQL and generated NNTP/Newznab fixtures remain on the control node.
The chart runs two replicas of each singleton specialist role across agents;
PVCs retain each ordinal's identity. All server replicas share PostgreSQL.
A singleton control plane and PostgreSQL are not infrastructure HA. Stopping
that host cannot establish database availability.

## Provision and deploy

Use a private absolute state directory outside Git; set `umask077`. Write a
random shared join token to `$state/server/token` without printing it. Then:

```sh
packaging/kubernetes/multihost/node.sh server "$state/server" "$TAILSCALE_IP"
packaging/kubernetes/multihost/local-agents.sh "$state"
```

Save the server's `/etc/rancher/k3s/k3s.yaml` to `$state/kubeconfig`, mode0600;
change its API origin to `https://$TAILSCALE_IP:16443` and rename the context
`ether035-multihost`. Put pinned kubectl1.34+ and Helm3.19+ in `$state/bin`.
Python3/PyYAML, OpenSSL, Docker and the repository's Go toolchain are required.
Verify host synchronization (including offset/error bound) before using the
values' explicit1000ms clock assertion. The deployment intentionally refuses
unknown clock bounds.

One command builds a reviewed integration commit using niced pure Go, packages
the pinned FFmpeg bundle, imports it into all three owned local nodes, and
installs/updates the fixtures and chart:

```sh
packaging/kubernetes/multihost/heavy-io.sh \
  packaging/kubernetes/multihost/redeploy.sh "$state" "$INTEGRATION_SHA"
```

The chart uses OnDelete. That command updates templates without terminating
healthy processes. Drain and replace one pod at a time using
`packaging/kubernetes/rollout.sh ether035-ha POD` with the lab kubeconfig.
For initial staging of rejected/never-started pods only, the explicit
`role-loss.sh` helper performs UID-preconditioned abrupt replacement.

On the shared worker server, builds, large tests, FFmpeg analyses and lab
rollouts all use `/data/bb/thread-storage/heavy-io.lock`. The wrapper waits
while `/data/bb/thread-storage/batch-gate.active` exists, gives the batcher
priority, and records queue, run and total wall seconds. A race with a newly
started gate exits75; resubmit after the gate finishes.

For an already-built gated image, archive `packaging/helm` from the same full
revision and set `ETHER_LAB_CHART` to that archive's `packaging/helm/ether`.
Then `heavy-io.sh roll-gated.sh "$state" "$IMAGE_TAG"` imports the image and
drains/replaces each old ordinal, retaining identities and all database rows.
A failed drain stops the rollout. Verify all eight replacement pod image IDs,
authenticated boots and free placement capacity before handing off playback.

Lab PostgreSQL uses a512MiB memory-backed volume with fsync,
synchronous_commit and full_page_writes disabled. Its contents disappear if
the PG pod is replaced; retain a private dump before doing so. Migrating an
existing disk-backed fixture requires all Ether role pods to be drained and
stopped first. `migrate-pg-tmpfs.sh` refuses live roles or a repeat migration,
takes a complete private dump, retains the original PVC, restores every table
and sequence, and restores service routing only after verification. Invoke it
under `heavy-io.sh`; it never edits placement, ownership or fencing rows.

Expose ingress through `http://$TAILSCALE_IP:18035`, Host
`ether035-roles.test`. For seed setup, use a temporary loopback port-forward
straight to a server pod's32400 listener (owner network admission intentionally
rejects console setup through an untrusted proxy). Run:

```sh
KUBECONFIG="$state/kubeconfig" python3 packaging/kubernetes/seed-lab.py \
  --context ether035-multihost --base http://127.0.0.1:18040 \
  --credentials-dir "$state/playback-private" --kubectl "$state/bin/kubectl"
```

This seeds only generated fixture sources and title1000000000900001. Delegation
expires after15minutes; refresh before measurements. Keep all credential files,
grant URLs, media logs and evidence outside Git. Use the actual Plex
`decision -> start -> authored manifests -> segments -> stop` flow through
ingress. A bare Start baseline does not establish decision/start compatibility.

## Native worker access

Optionally write the first host Tailscale IP to `$state/external-private-ip`.
The Helm post-renderer exposes each role ordinal's private TLS origin through
its own NodePort. Run `relay.sh "$state" "$TAILSCALE_IP"` once. It creates a
64MiB capped container on the owned bridge with at most64 connections:

| Listener | Destination |
| --- | --- |
|15432|Lab PostgreSQL NodePort30432|
|18410,18411|Server ordinals0,1|
|18420,18421|Source ordinals0,1|
|18430,18431|Transcode ordinals0,1|
|18440,18441,18442|Delivery ordinals0,1; optional second-host ordinal2|

Private TLS remains end to end, including CA verification, actual IP SAN,
client certificates and cluster identity/boot checks. The relay holds no
credentials. Keep tailnet access restricted to authorized lab machines.
Transfer DB URL, cluster key and independent issuer/roots through authenticated
private file access; never paste them into commands or logs. Native workers
must measure their own PostgreSQL clock offset and round-trip error against
the configured bound before joining. Readiness alone does not prove this.

## Join another physical host

Provision only one2GiB agent there; no host networking/sysctl/systemd changes.
The host needs cgroupv2, Docker, in-kernel WireGuard, outbound API16443/tcp,
bidirectional51820/udp over Tailscale, and a1280-byte underlay. Native host-gw
is unsuitable across separate Docker bridges; published WireGuard endpoints
use `node-external-ip` and `flannel-external-ip` instead.

Copy the scripts and shared token privately to the second host, then run:

```sh
python3 pressure-gate.py --state "$remote_state" \
  ./join.sh "$remote_state" "$REMOTE_TAILSCALE_IP" "$SERVER_TAILSCALE_IP"
```

The guard samples `/proc/pressure/memory` every60seconds, requires full avg10<5
for30continuous minutes, and then starts one capped lab agent. It keeps checking
and stops that exact container if full avg10>10 or the guard exits. Preserve
`pressure.jsonl` as evidence. A failed clock gate remains a separate blocker.
Do not use a native GPU process to bypass either gate.

Import the exact integration image into that node before scheduling workloads;
`docker save` plus `docker exec -i NODE_ID ctr -n k8s.io images import -` requires
no registry or daemon changes. The second host receives only `ether.lab/delivery=true`; keep the general
`ether.lab/workload` label off it so source/server/transcode remain local.
Copy `physical-host.values.yaml` into the private state directory, label both
local agents `ether.lab/delivery=true`, then run install/redeploy. The overlay
adds one delivery ordinal and spreads only delivery pods across the three
agents. Other pools continue to spread across the two local agents. Existing
per-ordinal PVC affinity remains intact during replacement.
Keep the second host limited to one light role (prefer delivery), with no
transcode there. A third delivery ordinal can use fresh local storage there,
while the two existing local ordinals retain their PVCs. Verify actual placement
across `ether.lab/host` for a physical-host experiment; the default hostname spread alone does
not guarantee cross-host placement. Do not move the singleton PG/fixture onto
the host being lost.

## Reversible loss actions

Set PATH and KUBECONFIG to this lab explicitly. Node faults run a supplied
blocking verifier while active and restore on exit, including INT/TERM:

```sh
loss.sh stop      "$state/agent1" "$PEER_IP" VERIFIER ARGS...
loss.sh pause     "$state/agent1" "$PEER_IP" VERIFIER ARGS...
loss.sh partition "$remote_state" "$SERVER_TAILSCALE_IP" VERIFIER ARGS...
role-pause.sh ether035-mh-ether-source-0 VERIFIER ARGS...
role-loss.sh ether035-mh-ether-delivery-0
remote-loss.py --container-id "$CAPTURED_GPU_AGENT_ID" --fault pause -- VERIFIER ARGS...
```

`stop` checks running=false/PID0; `pause` freezes a whole Kubernetes node.
`partition` changes only INPUT/OUTPUT rules inside that node container's private
network namespace. It blocks the specified tailnet peer, never host traffic.
`remote-loss.py` uses an authenticated BB terminal on gpu-workstation, checks
that exact immutable ID's lab label/private namespace/2GiB+1CPU cap, runs the
local verifier during the remote fault, then captures running/unpaused restore
evidence. It has a180s fault bound and aborts/stops that node if host PSI full
avg10 exceeds10. Raw terminal and restore summaries stay in the invoking
thread's storage. Do not infer protocol continuity from successful restoration.
Run per-role pause on the Docker host that owns the selected node container.
Per-role pause freezes the immutable containerd task and its descendants,
without pausing co-located roles. Abrupt role delete uses gracePeriodSeconds0
with a Pod UID precondition; the StatefulSet recreates the pod. Select the actual
active source/encoder/controller from durable session routes immediately before
action; deleting an unused replica proves nothing. These helpers do not claim
fencing, retirement, client continuity or database HA.

## Teardown

First stop owned port-forwards/collectors and drain live sessions. Run
`teardown.sh "$state"` on each host for that host's captured container IDs.
It checks lab labels, removes only those containers, then the captured owned
bridge. Bound data and private credentials are retained for review; dispose of
those exact private state directories only when no longer needed. Never prune
Docker, restart WSL/Docker or remove another lab's resources.

## Isolated long fixture

For a continuous run longer than the original180s title, `long-fixture.py`
renders a separate600s generated MPEG4/AAC source, title900002/rating key
1000000000900002. It uses another Service/ConfigMap/indexer/provider/release,
Newznab ID and NNTP article namespace. The original900001 media is untouched.

```sh
python3 packaging/kubernetes/multihost/long-fixture.py "$PWD" "$REVIEWED_IMAGE" > "$state/long-fixture.yaml"
kubectl -n ether035-ha create configmap ether035-ha-long-source-code \
  --from-file=packaging/kubernetes/source-fixture.py
kubectl -n ether035-ha apply -f "$state/long-fixture.yaml"
kubectl -n ether035-ha rollout status deployment/ether035-ha-long-source --timeout=300s
python3 packaging/kubernetes/seed-lab.py --context ether035-multihost \
  --base http://127.0.0.1:18040 --long-fixture --credentials-dir "$state/playback-private"
```

Run with this lab's KUBECONFIG/PATH and a temporary direct owner port-forward.
Import the generator image on the control node immediately before applying the
manifest; an unused imported image may be collected before use. Seeding adds
the selected fixture while preserving the other known fixture. It refuses
nonfixture providers/indexers. `--refresh-only` still refreshes delegation
without source configuration changes. Use a separate reference for the long
clip; its bytes and duration differ from the180s title.
