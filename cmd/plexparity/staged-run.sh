#!/usr/bin/env bash
set -euo pipefail
root=$1
attribution=${2:-0}
case "$attribution" in 0|1) ;; *) echo 'invalid attribution mode' >&2; exit 1;; esac
for tree in baseline candidate; do
 if (cd "$root/$tree" && git apply --check "$root/harness/baseline-seam.patch" >/dev/null 2>&1); then
  (cd "$root/$tree" && git apply "$root/harness/baseline-seam.patch")
 fi
 python3 - "$root/$tree" <<'PYCODE'
import pathlib, re, sys
root = pathlib.Path(sys.argv[1]) / "internal/app"
replacements = {
 "app.go": [('	background.start(func() { a.cluster.RunAsLeader(ctx, "catalogue sync", a.catalog.Run) })\n\tbackground.start(func() { a.cluster.RunAsLeader(ctx, "trending charts", a.catalog.RunTrends) })', '\tif parityRunCatalogueWorkers() {\n\tbackground.start(func() { a.cluster.RunAsLeader(ctx, "catalogue sync", a.catalog.Run) })\n\tbackground.start(func() { a.cluster.RunAsLeader(ctx, "trending charts", a.catalog.RunTrends) })\n\t}'),
 ('\tif a.clustered() {\n\t\tbackground.start(func() { a.runRegistrarAsLeader(ctx) })\n\t\tbackground.start(func() { a.registrar.RunPortMapping(ctx) })\n\t} else if err := a.registrar.Start(ctx); err != nil {\n\t\ta.log.Warn("plex.tv registrar", "err", err)\n\t}', '\tif parityRunRegistrar() {\n\t\tif a.clustered() {\n\t\t\tbackground.start(func() { a.runRegistrarAsLeader(ctx) })\n\t\t\tbackground.start(func() { a.registrar.RunPortMapping(ctx) })\n\t\t} else if err := a.registrar.Start(ctx); err != nil {\n\t\t\ta.log.Warn("plex.tv registrar", "err", err)\n\t\t}\n\t}'),
 ('\tbackground.start(func() { a.cluster.RunAsLeader(ctx, "watchlist sync", a.watchlist.Run) })', '\tif parityRunCatalogueWorkers() {\n\t\tbackground.start(func() { a.cluster.RunAsLeader(ctx, "watchlist sync", a.watchlist.Run) })\n\t}')],
 "build_catalogue.go": [
  ('log.With("component", "tmdb"))', 'log.With("component", "tmdb"), parityTMDBOptions()...)'),
  ('a.catalogOptions()...)', 'append(a.catalogOptions(), parityCatalogueOptions()...)...)'),
  ('a.closers = append(a.closers, a.tmdb.Close)', 'parityConfigureTMDB(a.tmdb)\n\ta.closers = append(a.closers, a.tmdb.Close)')],
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
  if name == 'app.go' and 'catalogue sync' in old and 'if parityRunCatalogueWorkers()' in text: continue
  if name == 'app.go' and 'a.registrar.Start(ctx)' in old and 'if parityRunRegistrar()' in text: continue
  if name == 'app.go' and 'watchlist sync' in old and re.search(r'if parityRunCatalogueWorkers\(\)\s*\{\s*background\.start\(func\(\) \{ a\.cluster\.RunAsLeader\(ctx, "watchlist sync"', text): continue
  if old.startswith('a.closers = append(a.closers, a.tmdb.Close)') and 'parityConfigureTMDB(a.tmdb)' in text: continue
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
 -e ETHER_PARITY_ATTRIBUTION="$attribution" -e CGO_ENABLED=0 -e GOFLAGS=-buildvcs=false -e PATH=/ffmpeg/linux-amd64:/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
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
