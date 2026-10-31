#!/usr/bin/env bash
# CI setup may download Go modules and pinned ffmpeg; the parity run itself
# uses --network none and refuses to fall back to network access.
set -euo pipefail
if [ "$#" -ne 2 ]; then echo 'usage: ci.sh BASELINE-COMMIT CANDIDATE-COMMIT' >&2; exit 2; fi
root=$(git rev-parse --show-toplevel)
baseline=$(git rev-parse --verify "$1^{commit}")
candidate=$(git rev-parse --verify "$2^{commit}")
printf 'baseline=%s candidate=%s\n' "$baseline" "$candidate"
mkdir -p "$HOME/prism-test/.cache/ffmpeg"
dir=$(mktemp -d "$HOME/prism-test/plexparity.ci.XXXXXXXX")
cleanup() {
 docker run --rm --network none -v "$dir":/cleanup prism-go:1.27.3 chown -R "$(id -u):$(id -g)" /cleanup
 rm -rf -- "$dir"
}
trap cleanup EXIT
mkdir -p "$dir/baseline" "$dir/candidate" "$dir/driver" "$dir/harness" "$dir/work"
git archive "$baseline" | tar -xf - -C "$dir/baseline"
git archive "$candidate" | tar -xf - -C "$dir/candidate"
rsync -a --exclude .git --exclude node_modules --exclude dist/ffmpeg --exclude scripts/lab/lab.env --exclude .conform --exclude '.env*' --exclude prod.env --exclude '*.pem' --exclude '*.key' "$root/" "$dir/driver/"
rsync -a "$root/cmd/plexparity/" "$dir/harness/"
cp "$root/internal/app/parity_runtime_default.go" "$root/internal/app/parity_runtime_fixture.go" "$root/internal/app/parity_source_default.go" "$root/internal/app/parity_source_fixture.go" "$root/internal/app/lab_media.go" "$dir/harness/"
cp "$root/internal/personalize/parity_wait_fixture.go" "$dir/harness/"
cp "$root/internal/livetv/parity_session_fixture.go" "$dir/harness/"
cp "$root/internal/transcode/parity_clock_fixture.go" "$dir/harness/"
bash "$root/scripts/fetch-ffmpeg.sh" linux-amd64
mkdir -p "$HOME/prism-test/.cache/ffmpeg/linux-amd64"
cp -a "$root/dist/ffmpeg/linux-amd64/." "$HOME/prism-test/.cache/ffmpeg/linux-amd64/"
printf 'FROM golang:1.27.3\nRUN apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq xz-utils unzip >/dev/null && rm -rf /var/lib/apt/lists/*\n' | docker build -q -t prism-go:1.27.3 - >/dev/null
# Download dependencies before isolating the actual build and execution.
for tree in baseline candidate driver; do
 docker run --rm -v "$dir/$tree":/src -w /src -v prism-gomod:/go/pkg/mod -v prism-gobuild:/root/.cache/go-build -e CGO_ENABLED=0 prism-go:1.27.3 go mod download
done
# Run both the ordinary world and the independently trained active-ranker
# world. The actual comparisons remain inside the offline container.
bash "$root/cmd/plexparity/staged-run.sh" "$dir" 0 0 0 0 0 0 0 0 0 0 0 1
