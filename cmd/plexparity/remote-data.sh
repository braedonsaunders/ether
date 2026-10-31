#!/usr/bin/env bash
# Frozen /data staging successor. Never stages into /home or an unreviewed path.
set -euo pipefail
if [ "$#" -ne 2 ]; then echo 'usage: plex-parity.sh BASELINE-REF CANDIDATE-REF-OR-WORKTREE' >&2; exit 2; fi
baseline=$1
candidate=$2
# Full milestone runs include both the ordinary and active-model worlds.
# Explicit focused diagnostics select their own world and remain incomplete.
both_models=${ETHER_PARITY_BOTH_MODELS:-}
if [ -z "$both_models" ]; then
 both_models=1
 for mode in ETHER_PARITY_ATTRIBUTION ETHER_PARITY_CROWD_DIAG ETHER_PARITY_NOTIFICATIONS_ONLY ETHER_PARITY_LIVETV_ONLY ETHER_PARITY_DOWNLOADS_ONLY ETHER_PARITY_TRICKPLAY_ONLY ETHER_PARITY_SUBTITLES_ONLY ETHER_PARITY_IPTV_ONLY ETHER_PARITY_POLICY_COUNTERFACTUAL ETHER_PARITY_TRAIN_RANKER ETHER_PARITY_RANKER_TRAIN_ONLY ETHER_PARITY_EXPIRY_ONLY ETHER_PARITY_CLUSTER_ONLY ETHER_PARITY_INFUSE_ONLY; do
  if [ "${!mode:-0}" != 0 ]; then both_models=0; break; fi
 done
fi
host=${PRISM_TEST_HOST:-braedon@100.123.220.120}
seconds=${PRISM_PARITY_TIMEOUT_SECONDS:-120}
allow_internal_pg=${PRISM_PARITY_ALLOW_INTERNAL_PG:-0}
[[ "$host" =~ ^[a-zA-Z0-9_.@-]+$ && "$host" != -* ]] || { echo 'invalid worker host' >&2; exit 2; }
[[ "$seconds" =~ ^[1-9][0-9]{0,3}$ ]] && ((seconds <= 3600)) || { echo 'timeout must be 1..3600 seconds' >&2; exit 2; }
case "$allow_internal_pg" in 0|1) ;; *) echo 'invalid internal PostgreSQL capability' >&2; exit 2;; esac
if [ "${ETHER_PARITY_CLUSTER_ONLY:-0}" = 1 ] && [ "$allow_internal_pg" != 1 ]; then echo 'cluster probe requires explicit internal PostgreSQL capability' >&2; exit 2; fi
root=$(git rev-parse --show-toplevel)
baseline=$(git -C "$root" rev-parse --verify "${baseline}^{commit}")
if [ "${ETHER_PARITY_POLICY_COUNTERFACTUAL:-0}" = 1 ] && [ "$baseline" != 24be28768e1dc6be99fbf73193444f6325314d38 ]; then
 echo 'counterfactual policy port is pinned to main 24be2876' >&2
 exit 1
fi
if [ ! -d "$candidate" ]; then candidate=$(git -C "$root" rev-parse --verify "${candidate}^{commit}"); fi
printf 'baseline=%s candidate=%s\n' "$baseline" "$candidate"
remote=$(ssh -o BatchMode=yes "$host" bash -s <<'CHECK'
set -euo pipefail
base=/data/prism-test
[ -d "$base" ] && [ "$(realpath -e "$base")" = "$base" ] || { echo 'missing or redirected /data/prism-test' >&2; exit 1; }
[ "$(stat -c %d "$base")" != "$(stat -c %d /home)" ] || { echo '/data staging shares the /home filesystem' >&2; exit 1; }
[ "$(stat -c %d "$base")" != "$(stat -c %d /)" ] || { echo '/data staging shares the root filesystem' >&2; exit 1; }
python3 - <<'PYDISK'
import os, sys
for path in ('/', '/data'):
    s = os.statvfs(path)
    free = s.f_bavail * s.f_frsize
    print(f'{path}: {free} available bytes', file=sys.stderr)
    if path == '/data' and free < 20 * 1024**3:
        sys.exit('insufficient /data headroom (20 GiB minimum)')
    if path == '/' and free < 1024**3:
        print('WARNING: root critically low; all stage/cache/temp writes must stay on /data', file=sys.stderr)
PYDISK
[ "$(docker info --format '{{.DockerRootDir}}')" = /data/docker ] || { echo 'Docker root is not /data/docker' >&2; exit 1; }
for volume in prism-gomod prism-gobuild; do
 mount=$(docker volume inspect --format '{{.Mountpoint}}' "$volume")
 [ "$mount" = "/data/docker/volumes/$volume/_data" ] || { echo "unapproved cache volume $volume" >&2; exit 1; }
done
for binary in ffmpeg ffprobe; do
 path="/home/braedon/prism-test/.cache/ffmpeg/linux-amd64/$binary"
 [ -x "$path" ] && [ "$(realpath -e "$path")" = "$path" ] || { echo "missing or redirected pinned $binary" >&2; exit 1; }
 version=$("$path" -version)
 case "$version" in "$binary version 8.1.3-Jellyfin"*) ;; *) echo "unexpected $binary version" >&2; exit 1;; esac
done
mktemp -d "$base/plexparity.XXXXXXXX"
CHECK
)
exclude=(--exclude .git --exclude node_modules --exclude web/node_modules --exclude /dist/ --exclude /ether --exclude '/*.test' --exclude '/coverage*' --exclude .scratch --exclude scripts/lab/lab.env --exclude .conform --exclude '.env*' --exclude prod.env --exclude '*.pem' --exclude '*.key')
case "$remote" in /data/prism-test/plexparity.*) ;; *) echo 'unexpected remote temporary path' >&2; exit 1;; esac
ssh -o BatchMode=yes "$host" bash -s -- "$remote" <<'VERIFY'
set -euo pipefail
stage=$1
[ -d "$stage" ] && [ "$(realpath -e "$stage")" = "$stage" ] && [ "$(stat -c %d "$stage")" = "$(stat -c %d /data)" ] && [ "$(stat -c %d "$stage")" != "$(stat -c %d /home)" ] && [ "$(stat -c %d "$stage")" != "$(stat -c %d /)" ] || { echo 'created stage is redirected or on the wrong filesystem' >&2; exit 1; }
VERIFY
printf 'Retained parity stage: %s:%s\n' "$host" "$remote" >&2
ssh "$host" "mkdir -p '$remote/baseline' '$remote/candidate' '$remote/harness' '$remote/driver' '$remote/work/tmp' '$remote/work/home'"
git -C "$root" archive "$baseline" | ssh "$host" "tar -xf - -C '$remote/baseline'"
if [ -d "$candidate" ]; then
 rsync -az "${exclude[@]}" "$candidate/" "$host:$remote/candidate/"
else
 git -C "$root" archive "$candidate" | ssh "$host" "tar -xf - -C '$remote/candidate'"
fi
rsync -az "${exclude[@]}" "$root/" "$host:$remote/driver/"
rsync -az "$root/cmd/plexparity/" "$host:$remote/harness/"
rsync -az "$root/internal/app/parity_runtime_default.go" "$root/internal/app/parity_runtime_fixture.go" "$root/internal/app/parity_source_default.go" "$root/internal/app/parity_source_fixture.go" "$root/internal/app/lab_media.go" "$host:$remote/harness/"
rsync -az "$root/internal/personalize/parity_wait_fixture.go" "$host:$remote/harness/"
rsync -az "$root/internal/livetv/parity_session_fixture.go" "$host:$remote/harness/"
rsync -az "$root/internal/transcode/parity_clock_fixture.go" "$host:$remote/harness/"
ssh -o BatchMode=yes "$host" bash -s -- "$remote" "${ETHER_PARITY_ATTRIBUTION:-0}" "${ETHER_PARITY_CROWD_DIAG:-0}" "${ETHER_PARITY_NOTIFICATIONS_ONLY:-0}" "${ETHER_PARITY_LIVETV_ONLY:-0}" "${ETHER_PARITY_DOWNLOADS_ONLY:-0}" "${ETHER_PARITY_TRICKPLAY_ONLY:-0}" "${ETHER_PARITY_SUBTITLES_ONLY:-0}" "${ETHER_PARITY_IPTV_ONLY:-0}" "${ETHER_PARITY_POLICY_COUNTERFACTUAL:-0}" "${ETHER_PARITY_TRAIN_RANKER:-0}" "${ETHER_PARITY_RANKER_TRAIN_ONLY:-0}" "$both_models" "${ETHER_PARITY_EXPIRY_ONLY:-0}" "${ETHER_PARITY_CLUSTER_ONLY:-0}" "${ETHER_PARITY_INFUSE_ONLY:-0}" "${ETHER_PARITY_EXPECT_EARLY_RESUME:-0}" "$seconds" "$allow_internal_pg" < "$root/cmd/plexparity/staged-run-data.sh"
