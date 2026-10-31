#!/usr/bin/env bash
set -euo pipefail
root=$1
attribution=${2:-0}
crowd_diag=${3:-0}
notifications_only=${4:-0}
livetv_only=${5:-0}
downloads_only=${6:-0}
trickplay_only=${7:-0}
subtitles_only=${8:-0}
iptv_only=${9:-0}
counterfactual=${10:-0}
train_ranker=${11:-0}
ranker_train_only=${12:-0}
both_models=${13:-0}
expiry_only=${14:-0}
cluster_only=${15:-0}
infuse_only=${16:-0}
expect_early_resume=${17:-0}
case "$attribution" in 0|1) ;; *) echo 'invalid attribution mode' >&2; exit 1;; esac
case "$crowd_diag" in 0|1) ;; *) echo 'invalid crowd diagnostic mode' >&2; exit 1;; esac
case "$notifications_only" in 0|1) ;; *) echo 'invalid notification-only mode' >&2; exit 1;; esac
case "$livetv_only" in 0|1) ;; *) echo 'invalid live TV-only mode' >&2; exit 1;; esac
case "$downloads_only" in 0|1) ;; *) echo 'invalid downloads-only mode' >&2; exit 1;; esac
case "$trickplay_only" in 0|1) ;; *) echo 'invalid trickplay-only mode' >&2; exit 1;; esac
case "$subtitles_only" in 0|1) ;; *) echo 'invalid subtitles-only mode' >&2; exit 1;; esac
case "$iptv_only" in 0|1) ;; *) echo 'invalid IPTV-only mode' >&2; exit 1;; esac
case "$counterfactual" in 0|1) ;; *) echo 'invalid counterfactual mode' >&2; exit 1;; esac
case "$train_ranker" in 0|1) ;; *) echo 'invalid ranker training mode' >&2; exit 1;; esac
case "$ranker_train_only" in 0|1|2) ;; *) echo 'invalid ranker training-only mode' >&2; exit 1;; esac
case "$both_models" in 0|1) ;; *) echo 'invalid both-models mode' >&2; exit 1;; esac
case "$expiry_only" in 0|1) ;; *) echo 'invalid expiry-only mode' >&2; exit 1;; esac
case "$cluster_only" in 0|1) ;; *) echo 'invalid cluster-only mode' >&2; exit 1;; esac
case "$infuse_only" in 0|1) ;; *) echo 'invalid Infuse-only mode' >&2; exit 1;; esac
case "$expect_early_resume" in 0|1) ;; *) echo 'invalid early-resume assertion mode' >&2; exit 1;; esac
if [ "$ranker_train_only" != 0 ] && [ "$train_ranker" != 1 ]; then echo 'training-only mode requires ranker training' >&2; exit 1; fi
if [ "$both_models" = 1 ] && { [ "$train_ranker" != 0 ] || [ "$ranker_train_only" != 0 ]; }; then echo 'both-models mode must start from the ordinary fixture' >&2; exit 1; fi
if [ "$cluster_only" = 1 ] && { [ "$expiry_only" != 0 ] || [ "$both_models" != 0 ] || [ "$train_ranker" != 0 ]; }; then echo 'cluster-only mode requires the ordinary fixture' >&2; exit 1; fi
if [ "$infuse_only" = 1 ] && { [ "$cluster_only" != 0 ] || [ "$expiry_only" != 0 ] || [ "$both_models" != 0 ] || [ "$train_ranker" != 0 ]; }; then echo 'Infuse-only mode requires the ordinary fixture' >&2; exit 1; fi
if [ "$expect_early_resume" = 1 ] && [ "$infuse_only" != 1 ]; then echo 'early-resume assertion requires Infuse-only mode' >&2; exit 1; fi
# Explicit baseline patches. Each is behaviour-neutral and applied only when
# that checkout does not already contain it.
#   baseline-seam.patch         determinism seams (clock, fixtures) for older trees
#   baseline-diagnostics.patch  8862fd78d encoder nil-deref; removes the panic only
#   baseline-warm-retain.patch  nil hook so fixture probes survive the 32-entry cap
for tree in baseline candidate; do
 if (cd "$root/$tree" && git apply --check "$root/harness/baseline-seam.patch" >/dev/null 2>&1); then
  (cd "$root/$tree" && git apply "$root/harness/baseline-seam.patch")
 fi
 if [ "$tree" = baseline ]; then
  if (cd "$root/$tree" && git apply --check "$root/harness/baseline-diagnostics.patch" >/dev/null 2>&1); then
   (cd "$root/$tree" && git apply "$root/harness/baseline-diagnostics.patch")
  elif ! grep -q 'r.devices\[dev\] != nil' "$root/$tree/internal/diagnostics/recorder.go"; then
   echo 'baseline diagnostics patch did not apply' >&2
   exit 1
  fi
  if (cd "$root/$tree" && git apply --check "$root/harness/baseline-warm-retain.patch" >/dev/null 2>&1); then
   (cd "$root/$tree" && git apply "$root/harness/baseline-warm-retain.patch")
  elif ! grep -q 'parityRetainWarm' "$root/$tree/internal/playback/preresolve.go"; then
   echo 'baseline warm-retain patch did not apply' >&2
   exit 1
  fi
 fi
 python3 - "$root/$tree" "$crowd_diag" <<'PYCODE'
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
  ('subtitles.Options{\n', 'subtitles.Options{\n        BaseURL: paritySubtitleBaseURL(),\n        Now: parityClock(),\n'),
  ('personalize.Options{\n', 'personalize.Options{\n        Now: parityClock(),\n'),
  ('a.closers = append(a.closers, a.personalize.Close)', 'if err := parityWarmPersonalization(ctx, a); err != nil { a.personalize.Close(); return err }\n\ta.closers = append(a.closers, a.personalize.Close)')],
 "build_identity.go": [('plextv.Options{Identity:', 'plextv.Options{BaseURL: parityPlexTVBaseURL(), Identity:'), ('auth.Options{\n', 'auth.Options{\n        Now: parityClock(),\n')],
 "build_sources.go": [('resolver.New(a.sources, a.media,', 'resolver.New(parityIndexer(a.sources, dataDir), parityHealth(a.media, dataDir),'), ('resolver.Options{SearchObserved:', 'resolver.Options{Now: parityClock(), SearchObserved:')],
 "build_playback.go": [('Engine:     a.media,', 'Engine:     parityEngine(a.media, dataDir),'), ('playback.Options{DataDir:', 'playback.Options{Now: parityClock(), DataDir:')],
 "livetv.go": [('livetv.Options{DB: a.db', 'livetv.Options{Now: parityClock(), DB: a.db')],
 "collections.go": [('return []collections.Option{', 'return append([]collections.Option{'), ('\n\t}\n}', '\n\t}, parityCollectionOptions()...)\n}')],
 "plex.go": [('deps.Config = a.cfg.Get', 'deps.Now = parityClock()\n\tdeps.Config = a.cfg.Get')],
}
for name, changes in replacements.items():
 path = root / name
 text = path.read_text()
 if name == 'build_sources.go':
  text = text.replace('resolver.New(parityIndexer(a.sources), parityHealth(a.media),', 'resolver.New(parityIndexer(a.sources, dataDir), parityHealth(a.media, dataDir),')
 for old, new in changes:
  if new in text: continue
  if name == 'app.go' and 'catalogue sync' in old and 'if parityRunCatalogueWorkers()' in text: continue
  if name == 'app.go' and 'a.registrar.Start(ctx)' in old and 'if parityRunRegistrar()' in text: continue
  if name == 'app.go' and 'watchlist sync' in old and re.search(r'if parityRunCatalogueWorkers\(\)\s*\{\s*background\.start\(func\(\) \{ a\.cluster\.RunAsLeader\(ctx, "watchlist sync"', text): continue
  if old.startswith('a.closers = append(a.closers, a.tmdb.Close)') and 'parityConfigureTMDB(a.tmdb)' in text: continue
  if old.startswith('a.closers = append(a.closers, a.personalize.Close)') and 'parityWarmPersonalization(ctx, a)' in text: continue
  if old == 'auth.Options{\n' and re.search(r'auth\.Options\{\s*Now:\s*parityClock\(\)', text): continue
  if old == 'personalize.Options{\n' and re.search(r'personalize\.Options\{\s*Now:\s*parityClock\(\)', text): continue
  if old == 'resolver.New(a.sources, a.media,' and 'parityIndexer(a.sources, dataDir)' in text and 'parityHealth(a.media, dataDir)' in text: continue
  if old == 'Engine:     a.media,' and 'playbackEngine(a)' in text: continue
  if text.count(old) != 1: raise SystemExit(f"cannot apply parity seam: {path}: expected exactly one {old!r}")
  text = text.replace(old, new, 1)
 path.write_text(text)
personal = root / 'build_personalization.go'
data = personal.read_text()
if not re.search(r'RefreshEvery:\s*parityRefreshEvery\(\)', data):
 data, count = re.subn(r'(personalize\.Options\{\s*Now:\s*parityClock\(\),)', r'\1\n        RefreshEvery: parityRefreshEvery(),', data, count=1)
 if count != 1: raise SystemExit('cannot apply parity scheduler seam')
if not re.search(r'Ranker:\s*parityRankerOptions\(\)', data):
 data, count = re.subn(r'(RefreshEvery:\s*parityRefreshEvery\(\),)', r'\1\n        Ranker: parityRankerOptions(),', data, count=1)
 if count != 1: raise SystemExit('cannot preserve parity ranker schedule')
personal.write_text(data)
checks = {
 "build_identity.go": r'auth\.Options\{\s*Now:\s*parityClock\(\)',
 "build_personalization.go": r'personalize\.Options\{\s*Now:\s*parityClock\(\)',
}
for name, pattern in checks.items():
 if not re.search(pattern, (root / name).read_text()): raise SystemExit(f"missing clock seam in {name}")
playback = root / 'build_playback.go'
data = playback.read_text()
needle = 'transcode.New(cfg.Transcode, log, '
if data.count(needle) != 1: raise SystemExit('cannot apply parity transcoder idle seam')
playback.write_text(data.replace(needle, needle + 'transcode.WithIdleTimeout(parityTranscodeIdle()), ', 1))
live = root.parent / 'livetv/service.go'
data = live.read_text()
needle = 'func liveSessionID() string {'
if data.count(needle) != 1: raise SystemExit('cannot apply deterministic live session seam')
live.write_text(data.replace(needle, needle + '\n\treturn parityLiveSessionID()\n', 1))
dash = root.parent / 'transcode/dash_session.go'
data = dash.read_text()
needle = 'renderLiveMPD(s.created, time.Now(),'
if data.count(needle) != 1: raise SystemExit('cannot apply deterministic live DASH clock seam')
dash.write_text(data.replace(needle, 'renderLiveMPD(parityManifestTime(), parityManifestTime(),', 1))
if sys.argv[2] == '1':
 path = root.parent / 'personalize/crowd.go'
 data = path.read_text()
 needle = '\t\tce.score = 1 - math.Exp(-sum*(1+crowdConsensusBonus*float64(n-1))/crowdScale)'
 if data.count(needle) != 1: raise SystemExit('cannot instrument crowd scoring')
 data = data.replace(needle, needle + '\n\t\tif k == core.MovieKey(10027) || k == core.MovieKey(10031) { fmt.Printf("PARITY_CROWD key=%d sum=%.17g score=%.17g votes=%v hop2=%v house=%.17g\\n", k, sum, ce.score, ce.votes, ce.hop2Votes, ce.house) }', 1)
 needle = '\tfinishCrowd(g)\n\tb.crowdG = g'
 if data.count(needle) != 1: raise SystemExit('cannot instrument viewer crowd graph')
 data = data.replace(needle, '\tfinishCrowd(g)\n\tfor _, k := range []core.RatingKey{core.MovieKey(10027), core.MovieKey(10031)} { if ce := g.items[k]; ce != nil { fmt.Printf("PARITY_CROWD user=%d key=%d score=%.17g votes=%v hop2=%v house=%.17g\\n", b.user.ID, k, ce.score, ce.votes, ce.hop2Votes, ce.house) } }\n\tb.crowdG = g', 1)
 path.write_text(data)
 diag = root.parent / 'personalize/zz_parity_diag.go'
 diag.write_text('''package personalize
import "fmt"
func parityDiagItems(items []candItem) string { s := ""; for _, x := range items { s += fmt.Sprintf(" %d:%.17g:%.17g:%.17g", x.t.Key, x.base, x.fit, x.qual) }; return s }
''')
 path = root.parent / 'personalize/generators.go'
 data = path.read_text()
 needle = '\t\tverb := "finished"'
 if data.count(needle) != 1: raise SystemExit('cannot instrument because generator')
 data = data.replace(needle, '\t\tif b.user.ID == 1 && (s.Key == core.MovieKey(10001) || s.Key == core.MovieKey(10003)) { raw := ""; for i, t := range sim { raw += fmt.Sprintf(" %d:%d", i, t.Key) }; fmt.Printf("PARITY_CROWD generator user=%d seed=%d raw=%s ranked=%s\\n", b.user.ID, s.Key, raw, parityDiagItems(items)) }\n' + needle, 1)
 path.write_text(data)
 path = root.parent / 'personalize/assemble.go'
 data = path.read_text()
 needle = '\treturn a.keepPreviouslyShown(c, eligible, out)'
 if data.count(needle) != 1: raise SystemExit('cannot instrument context items')
 data = data.replace(needle, '\tres := a.keepPreviouslyShown(c, eligible, out)\n\tif a.user.ID == 1 && (c.id == "because.1000000000010001" || c.id == "because.1000000000010003") { fmt.Printf("PARITY_CROWD context user=%d row=%s limit=%d previous=%v items=%s\\n", a.user.ID, c.id, a.limit, a.previous, parityDiagItems(res)) }\n\treturn res', 1)
 needle = '\t\tout := make([]core.HomeRow, 0, len(sel))'
 if data.count(needle) != 1: raise SystemExit('cannot instrument draft')
 data = data.replace(needle, '\t\tif a.user.ID == 1 { msg := ""; for _, row := range sel { msg += fmt.Sprintf(" %s=[%s]", row.hubID, parityDiagItems(picked[row])) }; fmt.Printf("PARITY_CROWD fill user=%d limit=%d previous=%v rows=%s\\n", a.user.ID, a.limit, a.previous, msg) }\n' + needle, 1)
 path.write_text(data)
PYCODE
 # lab_media.go supplies readLabMedia/labReleaseID for the source fixture.
 # With no lab-media.json both trees keep the synthetic releases.
 cp "$root/harness/parity_runtime_default.go" "$root/harness/parity_runtime_fixture.go" "$root/harness/parity_source_default.go" "$root/harness/parity_source_fixture.go" "$root/harness/lab_media.go" "$root/$tree/internal/app/"
 # Released main has no core.ProbeFactsVersion. Both parity trees share
 # prism's current lab-media generation so the seam still compiles there.
 python3 - "$root/$tree/internal/app/lab_media.go" <<'PYCODE'
import pathlib, sys
p = pathlib.Path(sys.argv[1])
p.write_text(p.read_text().replace("core.ProbeFactsVersion", "2"))
PYCODE
 cp "$root/harness/parity_wait_fixture.go" "$root/$tree/internal/personalize/"
 cp "$root/harness/parity_session_fixture.go" "$root/$tree/internal/livetv/"
 cp "$root/harness/parity_clock_fixture.go" "$root/$tree/internal/transcode/"
 cp "$root/harness/parity_warm_settle.go" "$root/$tree/internal/playback/"
 cp "$root/harness/parity_warm_route.go" "$root/$tree/internal/plex/"
 python3 - "$root/$tree/internal/plex/routes.go" <<'PYCODE'
import pathlib, sys
p = pathlib.Path(sys.argv[1])
s = p.read_text()
needle = "r.Use(s.recoverer, s.commonHeaders, s.parse, s.requestLog, s.authenticate)\n"
if s.count(needle) != 1:
    raise SystemExit("cannot mount the warm-settle route")
p.write_text(s.replace(needle, needle + '\tr.Get("/parity/warm-settled", s.handleParityWarmSettled)\n\tr.Get("/parity/warm-probe", s.handleParityWarmProbe)\n', 1))
PYCODE
 if [ "$tree" = baseline ] && [ "$counterfactual" = 1 ]; then
  cp "$root/harness/policy_counterfactual.go.txt" "$root/$tree/internal/plex/zz_parity_policy_counterfactual.go"
  python3 - "$root/$tree/internal/plex/handlers_hubs.go" <<'PYCODE'
import pathlib,sys
p=pathlib.Path(sys.argv[1]);s=p.read_text()
search='func (s *Server) runSearch(ctx context.Context, pr *Request, query string, sectionID, limit int) ([]core.Title, error) {'
post='func (s *Server) handlePostplay(w http.ResponseWriter, r *http.Request) {'
if s.count(search)!=1 or s.count(post)!=1: raise SystemExit('counterfactual requires the pinned main handlers')
s=s.replace(search,search+'\n if s.parityPolicyRestricted(pr.User) { return s.parityPolicySearch(ctx,pr,query,sectionID,limit) }',1)
s=s.replace(post,post+'\n if s.parityPolicyRestricted(FromContext(r.Context()).User) { s.parityPolicyPostplay(w,r); return }',1)
p.write_text(s)
PYCODE
 fi
done
mkdir -p "$root/baseline/cmd/parityseed" "$root/candidate/cmd/plexparity"
cp "$root/harness/seed.go" "$root/baseline/cmd/parityseed/seed.go"
cp "$root/harness/ranker_fixture.go" "$root/baseline/cmd/parityseed/ranker_fixture.go"
cat > "$root/baseline/cmd/parityseed/main.go" <<'GO'
package main
import ("context"; "fmt"; "os")
func main() {if len(os.Args)!=2 {os.Exit(2)}; if err:=seed(context.Background(),os.Args[1]);err!=nil {fmt.Fprintln(os.Stderr,err);os.Exit(1)}}
GO
[ -x "$HOME/prism-test/.cache/ffmpeg/linux-amd64/ffmpeg" ] || { echo 'CI-pinned ffmpeg missing from remote cache' >&2; exit 1; }
network=none
pg_container=
runner_cid="$root/work/cluster-runner.cid"
if [ "$cluster_only" = 1 ]; then
 pg_image='postgres@sha256:7c688148e5e156d0e86df7ba8ae5a05a2386aaec1e2ad8e6d11bdf10504b1fb7'
 docker image inspect "$pg_image" >/dev/null 2>&1 || { echo 'pinned PostgreSQL fixture image must be cached before offline execution' >&2; exit 1; }
 network="plexparity-$(basename "$root")"
 docker network create --internal "$network" >/dev/null
 cleanup_cluster() {
  if [ -s "$runner_cid" ]; then docker stop "$(cat "$runner_cid")" >/dev/null 2>&1 || true; fi
  if [ -n "$pg_container" ]; then docker stop "$pg_container" >/dev/null 2>&1 || true; fi
  docker network rm "$network" >/dev/null 2>&1 || true
 }
 trap cleanup_cluster EXIT
 pg_container=$(docker run -d --rm --network "$network" --network-alias parity-postgres -e POSTGRES_USER=parity -e POSTGRES_PASSWORD=parity -e POSTGRES_DB=postgres "$pg_image")
 ready=0
 for _ in $(seq 1 60); do
  if docker exec "$pg_container" pg_isready -U parity -d postgres >/dev/null 2>&1; then ready=1; break; fi
  sleep 1
 done
 [ "$ready" = 1 ] || { echo 'offline PostgreSQL fixture failed to start' >&2; exit 1; }
fi
runner_args=()
if [ "$cluster_only" = 1 ]; then runner_args=(--cidfile "$runner_cid"); fi
docker run --rm "${runner_args[@]}" --network "$network" \
 -v "$root/baseline":/baseline -v "$root/candidate":/candidate -v "$root/driver":/driver -v "$root/work":/work \
 -v prism-gomod:/go/pkg/mod -v prism-gobuild:/root/.cache/go-build \
 -v "$HOME/prism-test/.cache/ffmpeg":/ffmpeg:ro \
 -e ETHER_PARITY_ATTRIBUTION="$attribution" -e ETHER_PARITY_CROWD_DIAG="$crowd_diag" -e ETHER_PARITY_NOTIFICATIONS_ONLY="$notifications_only" -e ETHER_PARITY_LIVETV_ONLY="$livetv_only" -e ETHER_PARITY_DOWNLOADS_ONLY="$downloads_only" -e ETHER_PARITY_TRICKPLAY_ONLY="$trickplay_only" -e ETHER_PARITY_SUBTITLES_ONLY="$subtitles_only" -e ETHER_PARITY_IPTV_ONLY="$iptv_only" -e ETHER_PARITY_POLICY_COUNTERFACTUAL="$counterfactual" -e ETHER_PARITY_TRAIN_RANKER="$train_ranker" -e ETHER_PARITY_RANKER_TRAIN_ONLY="$ranker_train_only" -e ETHER_PARITY_BOTH_MODELS="$both_models" -e ETHER_PARITY_EXPIRY_ONLY="$expiry_only" -e ETHER_PARITY_CLUSTER_ONLY="$cluster_only" -e ETHER_PARITY_INFUSE_ONLY="$infuse_only" -e ETHER_PARITY_EXPECT_EARLY_RESUME="$expect_early_resume" -e CGO_ENABLED=0 -e GOFLAGS=-buildvcs=false -e PATH=/ffmpeg/linux-amd64:/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
 prism-go:1.27.3 bash -c '
 set -e
 if [ "$ETHER_PARITY_RANKER_TRAIN_ONLY" = 2 ]; then
  cd /driver
  timeout 14m go test -p 1 -tags plexparity -run "^TestTrainedRankerFixtureIsActiveAndDeterministic$" -count=1 -timeout=14m -v ./cmd/plexparity
  echo "coverage incomplete: ranker repeatability-only experiment"
  exit 2
 fi
 cd /baseline
 if [ "$ETHER_PARITY_RANKER_TRAIN_ONLY" = 1 ]; then
  go build -p 1 -tags plexparity -o /work/seed ./cmd/parityseed
  timeout 14m /work/seed /work/seeddata
  printf "RANKER_PROVENANCE "; cat /work/seeddata/ranker-training.json
  echo "coverage incomplete: ranker training-only experiment"
  exit 2
 fi
 go build -p 4 -tags plexparity -o /work/baseline ./cmd/ether
 go build -p 4 -tags plexparity -o /work/seed ./cmd/parityseed
 cd /candidate
 go build -p 4 -tags plexparity -o /work/candidate ./cmd/ether
 cd /driver
 go build -p 4 -o /work/parity ./cmd/plexparity
 /work/seed /work/seeddata
 if [ "$ETHER_PARITY_TRAIN_RANKER" = 1 ]; then printf "RANKER_PROVENANCE "; cat /work/seeddata/ranker-training.json; fi
 if [ "$ETHER_PARITY_BOTH_MODELS" = 1 ]; then
  ETHER_PARITY_TRAIN_RANKER=1 /work/seed /work/rankerseed
  printf "RANKER_PROVENANCE "; cat /work/rankerseed/ranker-training.json
 fi
 ffmpeg -hide_banner -loglevel error -f lavfi -i color=c=blue:s=320x180:r=24 -f lavfi -i anullsrc=r=48000:cl=stereo -t 20 -c:v libx264 -pix_fmt yuv420p -c:a aac -movflags +faststart /work/seeddata/parity.mp4
 if [ "$ETHER_PARITY_EXPIRY_ONLY" = 1 ]; then
  ffmpeg -hide_banner -loglevel error -f lavfi -i color=c=blue:s=320x180:r=24 -f lavfi -i anullsrc=r=48000:cl=stereo -t 300 -c:v libx264 -preset ultrafast -pix_fmt yuv420p -c:a aac -movflags +faststart /work/seeddata/parity-expiry.mp4
 fi
 ffmpeg -hide_banner -loglevel error -i /work/seeddata/parity.mp4 -c copy -f mpegts /work/seeddata/iptv.ts
 printf "1\n00:00:00,000 --> 00:00:03,000\nFixture subtitle\n" > /work/seeddata/parity.srt
 ffmpeg -hide_banner -loglevel error -f lavfi -i color=c=blue:s=320x180:r=24 -f lavfi -i anullsrc=r=48000:cl=stereo -f lavfi -i anullsrc=r=48000:cl=stereo -f srt -i /work/seeddata/parity.srt -t 20 -map 0:v -map 1:a -map 2:a -map 3:s -metadata:s:a:0 language=fra -metadata:s:a:1 language=eng -metadata:s:s:0 language=eng -c:v libx264 -pix_fmt yuv420p -c:a aac -c:s mov_text -movflags +faststart /work/seeddata/parity-tracks.mp4
 if [ "$ETHER_PARITY_BOTH_MODELS" = 1 ]; then cp /work/seeddata/parity.mp4 /work/seeddata/parity-tracks.mp4 /work/seeddata/parity.srt /work/seeddata/iptv.ts /work/rankerseed/; fi
 ordinary=0
 /work/parity run /work/baseline /work/candidate /work/seeddata || ordinary=$?
 ranker=0
 if [ "$ETHER_PARITY_BOTH_MODELS" = 1 ]; then
  echo "ACTIVE RANKER PARITY"
  ETHER_PARITY_TRAIN_RANKER=1 /work/parity run /work/baseline /work/candidate /work/rankerseed || ranker=$?
 fi
 if [ "$ordinary" -ne 0 ]; then exit "$ordinary"; fi
 exit "$ranker"
 '
