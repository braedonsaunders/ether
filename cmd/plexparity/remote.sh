#!/usr/bin/env bash
# Preliminary offline differential probe. It cannot yet establish parity.
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
root=$(git rev-parse --show-toplevel)
baseline=$(git -C "$root" rev-parse --verify "${baseline}^{commit}")
if [ "${ETHER_PARITY_POLICY_COUNTERFACTUAL:-0}" = 1 ] && [ "$baseline" != 24be28768e1dc6be99fbf73193444f6325314d38 ]; then
 echo 'counterfactual policy port is pinned to main 24be2876' >&2
 exit 1
fi
if [ ! -d "$candidate" ]; then candidate=$(git -C "$root" rev-parse --verify "${candidate}^{commit}"); fi
printf 'baseline=%s candidate=%s\n' "$baseline" "$candidate"
remote=$(ssh -o BatchMode=yes "$host" 'mkdir -p ~/prism-test && mktemp -d ~/prism-test/plexparity.XXXXXXXX')
exclude=(--exclude .git --exclude node_modules --exclude web/node_modules --exclude dist/ffmpeg --exclude scripts/lab/lab.env --exclude .conform --exclude '.env*' --exclude prod.env --exclude '*.pem' --exclude '*.key')
case "$remote" in /home/*/prism-test/plexparity.*) ;; *) echo 'unexpected remote temporary path' >&2; exit 1;; esac
cleanup() {
 ssh -o BatchMode=yes "$host" bash -s -- "$remote" <<'CLEAN'
set -euo pipefail
root=$1
docker run --rm --network none -v "$root":/cleanup prism-go:1.27.3 chown -R "$(id -u):$(id -g)" /cleanup
rm -rf -- "$root"
CLEAN
}
trap cleanup EXIT
ssh "$host" "mkdir -p '$remote/baseline' '$remote/candidate' '$remote/harness' '$remote/driver' '$remote/work'"
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
rsync -az "$root/internal/playback/parity_warm_settle.go" "$root/internal/plex/parity_warm_route.go" "$host:$remote/harness/"
ssh -o BatchMode=yes "$host" bash -s -- "$remote" "${ETHER_PARITY_ATTRIBUTION:-0}" "${ETHER_PARITY_CROWD_DIAG:-0}" "${ETHER_PARITY_NOTIFICATIONS_ONLY:-0}" "${ETHER_PARITY_LIVETV_ONLY:-0}" "${ETHER_PARITY_DOWNLOADS_ONLY:-0}" "${ETHER_PARITY_TRICKPLAY_ONLY:-0}" "${ETHER_PARITY_SUBTITLES_ONLY:-0}" "${ETHER_PARITY_IPTV_ONLY:-0}" "${ETHER_PARITY_POLICY_COUNTERFACTUAL:-0}" "${ETHER_PARITY_TRAIN_RANKER:-0}" "${ETHER_PARITY_RANKER_TRAIN_ONLY:-0}" "$both_models" "${ETHER_PARITY_EXPIRY_ONLY:-0}" "${ETHER_PARITY_CLUSTER_ONLY:-0}" "${ETHER_PARITY_INFUSE_ONLY:-0}" "${ETHER_PARITY_EXPECT_EARLY_RESUME:-0}" < "$root/cmd/plexparity/staged-run.sh"
