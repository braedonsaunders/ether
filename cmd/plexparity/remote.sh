#!/usr/bin/env bash
# Preliminary offline differential probe. It cannot yet establish parity.
set -euo pipefail
if [ "$#" -ne 2 ]; then echo 'usage: plex-parity.sh BASELINE-REF CANDIDATE-REF-OR-WORKTREE' >&2; exit 2; fi
baseline=$1
candidate=$2
host=${PRISM_TEST_HOST:-braedon@100.123.220.120}
root=$(git rev-parse --show-toplevel)
baseline=$(git -C "$root" rev-parse --verify "${baseline}^{commit}")
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
rsync -az "$root/internal/app/parity_runtime_default.go" "$root/internal/app/parity_runtime_fixture.go" "$root/internal/app/parity_source_default.go" "$root/internal/app/parity_source_fixture.go" "$host:$remote/harness/"
rsync -az "$root/internal/personalize/parity_wait_fixture.go" "$host:$remote/harness/"
ssh -o BatchMode=yes "$host" bash -s -- "$remote" "${ETHER_PARITY_ATTRIBUTION:-0}" "${ETHER_PARITY_CROWD_DIAG:-0}" < "$root/cmd/plexparity/staged-run.sh"
