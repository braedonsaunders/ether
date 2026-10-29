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
 rsync -az --exclude .git --exclude node_modules --exclude web/node_modules --exclude dist/ffmpeg "$candidate/" "$host:$remote/candidate/"
else
 git -C "$root" archive "$candidate" | ssh "$host" "tar -xf - -C '$remote/candidate'"
fi
rsync -az --exclude .git --exclude node_modules --exclude web/node_modules --exclude dist/ffmpeg "$root/" "$host:$remote/driver/"
rsync -az "$root/cmd/plexparity/" "$host:$remote/harness/"
rsync -az "$root/internal/app/parity_runtime_default.go" "$root/internal/app/parity_runtime_fixture.go" "$root/internal/app/parity_source_default.go" "$root/internal/app/parity_source_fixture.go" "$host:$remote/harness/"
ssh -o BatchMode=yes "$host" bash -s -- "$remote" <<'REMOTE'
set -euo pipefail
root=$1
for tree in baseline candidate; do
 if (cd "$root/$tree" && git apply --check "$root/harness/baseline-seam.patch" >/dev/null 2>&1); then
  (cd "$root/$tree" && git apply "$root/harness/baseline-seam.patch")
 fi
 python3 - "$root/$tree" <<'PYCODE'
import pathlib, re, sys
root = pathlib.Path(sys.argv[1]) / "internal/app"
replacements = {
 "build_catalogue.go": [
  ('log.With("component", "tmdb"))', 'log.With("component", "tmdb"), parityTMDBOptions()...)'),
  ('a.catalogOptions()...)', 'append(a.catalogOptions(), parityCatalogueOptions()...)...)')],
 "build_personalization.go": [
  ('state.Options{Logger:', 'state.Options{Now: parityClock(), Logger:'),
  ('personalize.Options{\n', 'personalize.Options{\n        Now: parityClock(),\n'),
  ('a.closers = append(a.closers, a.personalize.Close)', 'if err := parityWarmPersonalization(ctx, a); err != nil { a.personalize.Close(); return err }\n\ta.closers = append(a.closers, a.personalize.Close)')],
 "build_identity.go": [('plextv.Options{Identity:', 'plextv.Options{BaseURL: parityPlexTVBaseURL(), Identity:'), ('auth.Options{\n', 'auth.Options{\n        Now: parityClock(),\n')],
 "build_sources.go": [('resolver.New(a.sources, a.media,', 'resolver.New(parityIndexer(a.sources), parityHealth(a.media),'), ('resolver.Options{SearchObserved:', 'resolver.Options{Now: parityClock(), SearchObserved:')],
 "build_playback.go": [('Engine:     a.media,', 'Engine:     parityEngine(a.media, dataDir),'), ('playback.Options{DataDir:', 'playback.Options{Now: parityClock(), DataDir:')],
 "livetv.go": [('livetv.Options{DB: a.db', 'livetv.Options{Now: parityClock(), DB: a.db')],
 "collections.go": [('return []collections.Option{', 'return append([]collections.Option{'), ('\n\t}\n}', '\n\t}, parityCollectionOptions()...)\n}')],
 "plex.go": [('deps.Config = a.cfg.Get', 'deps.Now = parityClock()\n\tdeps.Config = a.cfg.Get')],
}
for name, changes in replacements.items():
 path = root / name
 text = path.read_text()
 for old, new in changes:
  if new in text: continue
  if old.startswith('a.closers = append(a.closers, a.personalize.Close)') and 'parityWarmPersonalization(ctx, a)' in text: continue
  if old == 'auth.Options{\n' and re.search(r'auth\.Options\{\s*Now:\s*parityClock\(\)', text): continue
  if old == 'personalize.Options{\n' and re.search(r'personalize\.Options\{\s*Now:\s*parityClock\(\)', text): continue
  if text.count(old) != 1: raise SystemExit(f"cannot apply parity seam: {path}: expected exactly one {old!r}")
  text = text.replace(old, new, 1)
 path.write_text(text)
checks = {
 "build_identity.go": r'auth\.Options\{\s*Now:\s*parityClock\(\)',
 "build_personalization.go": r'personalize\.Options\{\s*Now:\s*parityClock\(\)',
}
for name, pattern in checks.items():
 if not re.search(pattern, (root / name).read_text()): raise SystemExit(f"missing clock seam in {name}")
PYCODE
 cp "$root/harness/parity_runtime_default.go" "$root/harness/parity_runtime_fixture.go" "$root/harness/parity_source_default.go" "$root/harness/parity_source_fixture.go" "$root/$tree/internal/app/"
done
mkdir -p "$root/baseline/cmd/parityseed" "$root/candidate/cmd/plexparity"
cp "$root/harness/seed.go" "$root/baseline/cmd/parityseed/seed.go"
cat > "$root/baseline/cmd/parityseed/main.go" <<'GO'
package main
import ("context"; "fmt"; "os")
func main() {if len(os.Args)!=2 {os.Exit(2)}; if err:=seed(context.Background(),os.Args[1]);err!=nil {fmt.Fprintln(os.Stderr,err);os.Exit(1)}}
GO
[ -x "$HOME/prism-test/.cache/ffmpeg/linux-amd64/ffmpeg" ] || { echo 'CI-pinned ffmpeg missing from remote cache' >&2; exit 1; }
docker run --rm --network none \
 -v "$root/baseline":/baseline -v "$root/candidate":/candidate -v "$root/driver":/driver -v "$root/work":/work \
 -v prism-gomod:/go/pkg/mod -v prism-gobuild:/root/.cache/go-build \
 -v "$HOME/prism-test/.cache/ffmpeg":/ffmpeg:ro \
 -e CGO_ENABLED=0 -e GOFLAGS=-buildvcs=false -e PATH=/ffmpeg/linux-amd64:/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
 prism-go:1.27.3 bash -c '
 set -e
 cd /baseline
 go build -p 4 -tags plexparity -o /work/baseline ./cmd/ether
 go build -p 4 -o /work/seed ./cmd/parityseed
 cd /candidate
 go build -p 4 -tags plexparity -o /work/candidate ./cmd/ether
 cd /driver
 go build -p 4 -o /work/parity ./cmd/plexparity
 /work/seed /work/seeddata
 ffmpeg -hide_banner -loglevel error -f lavfi -i color=c=blue:s=320x180:r=24 -f lavfi -i anullsrc=r=48000:cl=stereo -t 20 -c:v libx264 -pix_fmt yuv420p -c:a aac -movflags +faststart /work/seeddata/parity.mp4
 /work/parity run /work/baseline /work/candidate /work/seeddata
 '
REMOTE
