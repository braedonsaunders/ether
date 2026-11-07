#!/bin/bash
# macOS/Linux AMD64 or ARM64, with a working C/C++ toolchain for cgo runs.
# Run serially: concurrent test/build workloads would distort the measurements.
set -euo pipefail
task_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$task_root"
result_dir="${1:-$task_root/docs/yenc-backend-benchmarks}"
mkdir -p "$result_dir"
python3 - "$result_dir" <<'PY'
import datetime,json,os,pathlib,platform,subprocess,sys

def read_command(args):
    try:
        return subprocess.check_output(args,text=True,stderr=subprocess.DEVNULL).strip()
    except (OSError,subprocess.CalledProcessError):
        return None

metadata={
    "started_utc":datetime.datetime.now(datetime.timezone.utc).isoformat(),
    "implementation_commit":read_command(["git","rev-parse","HEAD"]),
    "go_version":read_command(["go","version"]),
    "platform":platform.platform(),
    "cpu_model":read_command(["sysctl","-n","machdep.cpu.brand_string"]) if sys.platform=="darwin" else platform.processor(),
    "logical_cpus":os.cpu_count(),
    "duration":"300ms","samples":3,"serial_gomaxprocs":1,"concurrent_gomaxprocs":8,
    "workers":[1,4,8,16],"sizes_kib":[384,768],"line_lengths":[128,256],
    "fixture_seed":95,"crc":True,"units":"decoded decimal MB/s",
    "confirmation":{"duration":"1s","samples":5} if os.environ.get("BENCH_CONFIRM","1")=="1" else None,
    "scenarios":{
        "go-simd":{"CGO_ENABLED":"0","GOEXPERIMENT":"simd"},
        "cgo-no-simd":{"CGO_ENABLED":"1","GOEXPERIMENT":""},
        "swar":{"CGO_ENABLED":"0","GOEXPERIMENT":""},
        "cgo-with-simd":{"CGO_ENABLED":"1","GOEXPERIMENT":"simd"}
    }
}
pathlib.Path(sys.argv[1],"metadata.json").write_text(json.dumps(metadata,indent=2)+"\n")
PY
run_scenario() {
 local name="$1" cgo="$2" simd="$3"
 CGO_ENABLED="$cgo" GOEXPERIMENT="$simd" go env -json CGO_ENABLED GOEXPERIMENT GOOS GOARCH > "$result_dir/$name-environment.json"
 CGO_ENABLED="$cgo" GOEXPERIMENT="$simd" go list -f '{{join .GoFiles "\n"}}' ./internal/usenet > "$result_dir/$name-files.txt"
 python3 - "$result_dir" "$name" "$cgo" "$simd" <<'PY'
import json,pathlib,sys
root,name,cgo,simd=pathlib.Path(sys.argv[1]),*sys.argv[2:]
env=json.loads((root/f"{name}-environment.json").read_text())
if (env["CGO_ENABLED"],env["GOEXPERIMENT"]) != (cgo,simd):
    raise SystemExit(f"Unexpected build environment for {name}: {env}")
files=(root/f"{name}-files.txt").read_text().splitlines()
expected="rapid_fallback.go" if name=="swar" else "rapid.go"
if expected not in files:
    raise SystemExit(f"Expected {expected} in {name}: {files}")
PY
 CGO_ENABLED="$cgo" GOEXPERIMENT="$simd" go test ./internal/usenet -run '^TestRapidWireBoundary$' -count=1 -v > "$result_dir/$name-selection.txt" 2>&1
 CGO_ENABLED="$cgo" GOEXPERIMENT="$simd" go test ./internal/usenet -run '^$' -bench '^BenchmarkBodyDecode$' -benchmem -benchtime=300ms -count=3 -cpu=1 > "$result_dir/$name-serial.txt" 2>&1
 printf '%s serial complete\n' "$name"
 CGO_ENABLED="$cgo" GOEXPERIMENT="$simd" go test ./internal/usenet -run '^$' -bench '^BenchmarkBodyDecodeConcurrent$' -benchmem -benchtime=300ms -count=3 -cpu=8 > "$result_dir/$name-concurrent.txt" 2>&1
 printf '%s concurrency complete\n' "$name"
 if [ "$cgo" = 1 ] || [ "$simd" = simd ]; then
  CGO_ENABLED="$cgo" GOEXPERIMENT="$simd" go test ./internal/usenet -run '^$' -bench '^BenchmarkRapidDirect$' -benchmem -benchtime=300ms -count=3 -cpu=1 > "$result_dir/$name-direct.txt" 2>&1
  printf '%s direct-package comparison complete\n' "$name"
 fi
}
run_scenario go-simd 0 simd
run_scenario cgo-no-simd 1 ''
run_scenario swar 0 ''
run_scenario cgo-with-simd 1 simd
if [ "${BENCH_CONFIRM:-1}" = 1 ]; then
 confirm() {
  local name="$1" cgo="$2" simd="$3"
  CGO_ENABLED="$cgo" GOEXPERIMENT="$simd" go test ./internal/usenet -run '^$' -bench '^BenchmarkBodyDecode$/^line128$/^768KiB$/^bufferfresh$/^SWAR(false|true)$' -benchmem -benchtime=1s -count=5 -cpu=1 > "$result_dir/$name-serial-confirmation.txt" 2>&1
  if [ "$cgo" = 1 ] || [ "$simd" = simd ]; then
   CGO_ENABLED="$cgo" GOEXPERIMENT="$simd" go test ./internal/usenet -run '^$' -bench '^BenchmarkRapidDirect$/^line128$/^768KiB$' -benchmem -benchtime=1s -count=5 -cpu=1 > "$result_dir/$name-direct-confirmation.txt" 2>&1
  fi
  printf '%s longer confirmation complete\n' "$name"
 }
 confirm go-simd 0 simd
 confirm cgo-no-simd 1 ''
 confirm swar 0 ''
 confirm cgo-with-simd 1 simd
 CGO_ENABLED=0 GOEXPERIMENT=simd go test ./internal/usenet -run '^$' -bench '^BenchmarkBodyDecode$/^line128$/^768KiB$/^buffer(exact|headroom)$/^SWARfalse$' -benchmem -benchtime=1s -count=5 -cpu=1 > "$result_dir/go-simd-buffer-confirmation.txt" 2>&1
fi
python3 - "$result_dir" <<'PY'
import datetime,json,pathlib,re,sys
p=pathlib.Path(sys.argv[1],"metadata.json")
m=json.loads(p.read_text())
m["finished_utc"]=datetime.datetime.now(datetime.timezone.utc).isoformat()
match=re.search(r"^cpu: (.+)$",p.with_name("go-simd-serial.txt").read_text(),re.M)
if match:m["cpu_model"]=match[1]
p.write_text(json.dumps(m,indent=2)+"\n")
PY
python3 scripts/report_yenc_backends.py "$result_dir" --output "${result_dir}.md"
