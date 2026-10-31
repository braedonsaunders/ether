#!/usr/bin/env bash
# Run the Prism soak on bb-worker-server. Caps: 2 CPUs, 4 GiB, 256 pids.
# Mounts the lab media read-only. Does not start, stop or exec into
# prism-jellyfin, prism-emby, prism-silo or prism-jf-lab-zbv.
set -euo pipefail
root=$(git rev-parse --show-toplevel)
host=${PRISM_TEST_HOST:-braedon@100.123.220.120}
profile=${1:-${SOAK_PROFILE:-shakedown}}
manifest_src=${SOAK_MANIFEST:-/Users/braedonsaunders/.bb/thread-storage/prism-briefs/scorecard-qsb/verified-files.json}
media_src=${SOAK_MEDIA:-/home/braedon/prism-reference-lab/media}
ffmpeg_root=${PRISM_FFMPEG_ROOT:-/home/braedon/prism-test/.cache/ffmpeg}
test_root=${PRISM_TEST_ROOT:-/data/prism-test}
build=$(git -C "$root" rev-parse --short=12 HEAD)
name=prism-soak-${build}
case "$name" in
  prism-jellyfin|prism-emby|prism-silo|prism-jf-lab-zbv) echo "refusing reserved name" >&2; exit 2;;
esac
case "$profile" in shakedown|sustained|diagnose) ;; *) echo "bad profile" >&2; exit 2;; esac
seconds=3000
if [[ "$profile" == sustained ]]; then seconds=9000; fi
if [[ "$profile" == diagnose ]]; then seconds=900; fi
diagnose=0
if [[ "$profile" == diagnose ]]; then diagnose=1; profile=shakedown; fi
slot_dir=/Users/braedonsaunders/.bb/thread-storage/prism-briefs/slots
mkdir -p "$slot_dir"
while :; do
  for i in $(seq 1 "${PRISM_REMOTE_SLOTS:-8}"); do
    lock="$slot_dir/slot$i"
    if [[ -f "$lock/pid" ]] && ! kill -0 "$(cat "$lock/pid" 2>/dev/null)" 2>/dev/null; then
      rm -f "$lock/pid"; rmdir "$lock" 2>/dev/null || true
    fi
    if mkdir "$lock" 2>/dev/null; then
      echo $$ > "$lock/pid"
      trap 'rm -f "$lock/pid"; rmdir "$lock" 2>/dev/null || true' EXIT INT TERM
      echo "prism-soak: slot $i" >&2
      break 2
    fi
  done
  sleep 10
done
stage=$(ssh -o BatchMode=yes "$host" "bash -s -- $(printf '%q ' "$test_root" "$name" "$media_src" "$ffmpeg_root")" <<'REMOTE'
set -euo pipefail
base=$1; name=$2; media=$3; ffmpeg=$4
[[ $(realpath -m "$base") == "$base" ]] || exit 2
[[ -d "$media" && -x "$ffmpeg/linux-amd64/ffmpeg" && -x "$ffmpeg/linux-amd64/ffprobe" ]] || { echo "media or ffmpeg cache missing" >&2; exit 2; }
docker image inspect prism-go:1.27.3 >/dev/null
if docker container inspect "$name" >/dev/null 2>&1; then echo "container name already reserved" >&2; exit 2; fi
umask 077
mkdir -p "$base"
run=$(mktemp -d "$base/$name.XXXXXXXX")
mkdir "$run/src" "$run/data" "$run/tmp" "$run/home"
printf '%s\n' "$run"
REMOTE
)
[[ "$stage" == "$test_root/$name."* && "$stage" != *$'\n'* ]] || { echo "bad stage" >&2; exit 2; }
filtered=$(mktemp)
python3 - "$manifest_src" "$filtered" "$profile" <<'PY'
import json, sys
rows = json.load(open(sys.argv[1]))
profile = sys.argv[3]
if profile == "sustained":
    chosen = rows
else:
    chosen = [row for row in rows if any(part in row["path"] for part in ("Elephants Dream", "Sintel", "S01E01.mp4", "S02E01.mp4"))]
if len(chosen) < 3:
    raise SystemExit("manifest subset is too small")
json.dump(chosen, open(sys.argv[2], "w"))
print(f"manifest files {len(chosen)}", file=sys.stderr)
PY
scp -q "$filtered" "$host:$stage/manifest.json"
rm -f "$filtered"
rsync -az --omit-dir-times --no-perms \
  --exclude .git --exclude node_modules --exclude 'web/node_modules' \
  --exclude 'dist/ffmpeg' --exclude '.scratch' \
  --exclude 'scripts/lab/lab.env' --exclude 'scripts/lab/.conform' --exclude '.env' --exclude '.env.*' \
  --exclude '*.pem' --exclude '*.key' --exclude 'prod.env' \
  "$root/" "$host:$stage/src/"
cname=$(basename "$stage")
flag=
if [[ "$diagnose" == 1 ]]; then flag=-diagnose; fi
set +e
ssh -o BatchMode=yes "$host" "bash -s -- $(printf '%q ' "$stage" "$cname" "$media_src" "$ffmpeg_root" "$profile" "$build" "$seconds" "$flag")" <<'REMOTE'
set -euo pipefail
run=$1; name=$2; media=$3; ffmpeg=$4; profile=$5; build=$6; seconds=$7; flag=$8
case "$name" in prism-jellyfin|prism-emby|prism-silo|prism-jf-lab-zbv) exit 2;; esac
if docker container inspect "$name" >/dev/null 2>&1; then echo "container name already reserved" >&2; exit 2; fi
cleanup() {
  case "$name" in prism-jellyfin|prism-emby|prism-silo|prism-jf-lab-zbv) return;; esac
  if docker container inspect "$name" >/dev/null 2>&1; then docker stop -t 8 "$name" >/dev/null || true; fi
}
trap cleanup EXIT
set +e
# --init reaps the encoder after its guardian is SIGKILLed. Without it the
# soak process is pid 1 and that zombie stays in the process tree.
timeout --signal=TERM --kill-after=20s "$seconds" \
docker run --pull=never --init --name "$name" --network=none \
  --cpus=2 --memory=4g --pids-limit=256 --log-driver=none --read-only \
  --mount "type=bind,src=$run/src,dst=/src,readonly" -w /src \
  --mount "type=bind,src=$media,dst=/media,readonly" \
  --mount "type=bind,src=$run/manifest.json,dst=/manifest.json,readonly" \
  --mount "type=bind,src=$run/data,dst=/soak" \
  --mount "type=bind,src=$run/home,dst=/root" \
  --mount "type=bind,src=$ffmpeg,dst=/opt/ffmpeg,readonly" \
  --tmpfs /tmp:rw,exec,nosuid,size=1g,mode=1777 \
  --mount type=volume,src=prism-gomod,dst=/go/pkg/mod,readonly \
  --mount type=volume,src=prism-gobuild,dst=/root/.cache/go-build \
  -e GOTOOLCHAIN=local -e GOPROXY=off -e GOSUMDB=off -e GOMAXPROCS=2 -e CGO_ENABLED=0 \
  -e GOFLAGS=-buildvcs=false -e GOCACHE=/root/.cache/go-build \
  -e SCORECARD_FFMPEG=/opt/ffmpeg/linux-amd64/ffmpeg \
  -e SCORECARD_FFPROBE=/opt/ffmpeg/linux-amd64/ffprobe \
  -e PATH=/opt/ffmpeg/linux-amd64:/usr/local/go/bin:/usr/bin:/bin \
  prism-go:1.27.3 \
  sh -c 'set -eu
    if [ "${3:-}" != -diagnose ]; then
      go test -tags "scorecard" -count=1 -timeout 600s -run TestScorecardFaultOutageAndTelemetry ./internal/app/
      go test -count=1 -timeout 120s -run "TestEvaluateSoak|TestRunSoak|TestSynthesize|TestProcessTree|TestNetDev" ./cmd/scorecard/ ./internal/hoststat/
    fi
    go build -tags "scorecard" -o /tmp/soakscore ./cmd/scorecard
    exec /tmp/soakscore soak -profile "$1" -data /soak -manifest /manifest.json -out /soak/report.json -build "$2" $3
  ' sh "$profile" "$build" "$flag"
status=$?
set -e
trap - EXIT
cleanup
printf '%s\n' "$status" > "$run/exit-code"
exit "$status"
REMOTE
remote_status=$?
set -e
echo "stage $host:$stage" >&2
if [[ "$remote_status" != 0 ]]; then exit "$remote_status"; fi
ssh -o BatchMode=yes "$host" "python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))[\"status\"])' $(printf '%q' "$stage/data/report.json")" || true
