# Finite Linux host access probe

This standalone helper measures whether a future external observer can open
pidfds, PID namespace descriptors and real cgroup-v2 references for one live
outer namespace init. It does not sign, attest retirement, revoke admission,
signal processes, talk to Docker, or release capacity. All descriptors close on
return. Even `accessComplete:true` is only access feasibility: no armed evidence
survives this invocation. No production provider or Runtime callback is added.

The launcher supplies a bounded JSON object on stdin and closes stdin:

```json
{"version":1,"hostBoot":"exact-host-kernel-boot","daemonID":"exact-daemon-id","containerID":"64-lowercase-hex-container-id","containerStart":"exact-runtime-start-timestamp","pid":123,"cgroup":"/exact/outer/cgroup"}
```

The illustrative container ID must be replaced with the actual full hex ID.
Fields are mandatory; duplicate/unknown keys, trailing documents, oversized
input, root/unclean cgroup paths, and nonpositive/init-host PIDs are rejected.
Runtime identities are launcher assertions recorded for correlation, **not
independently authenticated Docker observations**. No bearer tokens, argv,
environment or process memory is read. Procfs and cgroup paths are deployment
configuration, never request-provided paths inside the JSON binding.

The executable accepts only `-proc-root` (default `/host/proc`) and
`-cgroup-root` (default `/host/cgroup`). Exit 0 means completed live access;
1 means denied/unknown/inconsistent access with a staged diagnostic; 2 means
invalid input/output failure. It reads the kernel boot, requires the observer's
PID numbering to agree with supplied procfs, and brackets retained task/pidfd,
namespace and cgroup access with identity and live-task rechecks. cgroup paths
are opened with openat2 beneath the supplied hierarchy, without symlinks or
crossing mounts. A dead/missing task or removed cgroup fails; timeout/absence
never becomes death evidence. Namespace ancestry inspection may fail under
capability-free Docker; that is an expected useful measurement, not permission
to add capabilities or weaken seccomp.

## Deployment requires separate exact authorization

Lab owner controls create/start/remove of a unique observer ID, using a reviewed
static binary hash, an already-present immutable image digest and exact paths.
Proposed configuration: host PID/cgroup namespaces, network none, read-only root
and proc/cgroup bind mounts, cap-drop ALL, no-new-privileges, restart=no, bounded
memory/CPU/PIDs, no Docker socket, no key, no services. Capture the create ID,
attach stdin/stdout, wait for this finite probe to exit, remove only that owned
ID. No image build, key generation or container launch is authorized by this
document. Host proc bind mounts must refer to the Docker daemon host, not the
launcher's private proc mount. No broad cleanup or automatic privilege retry.

The proposed future lab provider assumes trusted Docker/kubelet/admin with no
checkpoint/restore, old-key injection, migration/escape or DB rollback. This is
not proof against a hostile privileged workload. Actual attestation still needs
an authenticated alive binding of full cluster/fence/Start hash/application
boot to CRI/Pod/Node/task identities, retained task and namespace death plus
hierarchical populated=0, exact runtime stop reinspection, a durable exact
operation admission seal independent of time, and a separately pinned external
signing identity/journal. Cgroup removal before observing empty is unknown.
Source lifetime proof is independent. The current old982 HOLD has no prearmed
proof and cannot be repaired by this probe.

Offline checks (only with the lab's exclusive test slot):
`CGO_ENABLED=0 GOMAXPROCS=2 go test -p 1 ./internal/hostfenceprobe ./cmd/ether-host-fence-probe`
They test input ambiguity, identity parsing and filesystem substitution, not
host acceptance. References: [pidfd_open](https://man7.org/linux/man-pages/man2/pidfd_open.2.html),
[namespace access](https://man7.org/linux/man-pages/man7/namespaces.7.html),
[cgroup v2](https://docs.kernel.org/admin-guide/cgroup-v2.html).
