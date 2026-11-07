"""Validate raw yEnc benchmark output and summarize medians, ranges and CPU estimates."""
import argparse
import json
import re
import statistics
from pathlib import Path

SCENARIOS = {
    "go-simd": "Go SIMD",
    "cgo-no-simd": "C, experiment off",
    "swar": "SWAR",
    "cgo-with-simd": "C, experiment on",
}


def read_samples(path, expected_cases, repetitions):
    raw = path.read_text()
    if "\nPASS\n" not in raw or "FAIL" in raw:
        raise ValueError(f"Incomplete/failed benchmark: {path}")
    cases = {}
    for line in raw.splitlines():
        if not line.startswith("Benchmark") or " ns/op" not in line:
            continue
        fields = line.split()
        name = re.sub(r"-\d+$", "", fields[0])
        sample = {unit: float(value) for value, unit in zip(fields[2::2], fields[3::2])}
        required = {"ns/op", "MB/s", "cpu-ns/op", "cpu-ms/GiB", "cpu-%", "B/op", "allocs/op"}
        if required - sample.keys():
            raise ValueError(f"Missing CPU/benchmark metrics in {path}: {name}")
        cases.setdefault(name, []).append(sample)
    if len(cases) != expected_cases or any(len(v) != repetitions for v in cases.values()):
        raise ValueError(f"Expected {expected_cases} cases, {repetitions} samples each: {path}")
    return {
        name: {
            unit: {"median": statistics.median(s[unit] for s in samples),
                   "min": min(s[unit] for s in samples),
                   "max": max(s[unit] for s in samples)}
            for unit in samples[0]
        }
        for name, samples in cases.items()
    }


def value(case, unit):
    return case[unit]["median"]


def serial(run, size=768, line=128, buffer="fresh", swar=False):
    return run["serial"][f"BenchmarkBodyDecode/line{line}/{size}KiB/buffer{buffer}/SWAR{str(swar).lower()}"]


def parallel(run, workers, buffer="fresh"):
    return run["concurrent"][f"BenchmarkBodyDecodeConcurrent/buffer{buffer}/workers{workers}/SWARfalse"]


def reference(run):
    key = "BenchmarkBodyDecode/line128/768KiB/bufferfresh/SWARfalse"
    return run.get("serial_confirmation", run["serial"])[key]


def direct(run, reuse=False):
    return run.get("direct_confirmation", run["direct"])[f"BenchmarkRapidDirect/line128/768KiB/reuse{str(reuse).lower()}"]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("results", type=Path)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    root = args.results
    metadata = json.loads((root / "metadata.json").read_text())
    count = metadata["samples"]
    runs = {}
    for scenario in SCENARIOS:
        runs[scenario] = {
            "serial": read_samples(root / f"{scenario}-serial.txt", 24, count),
            "concurrent": read_samples(root / f"{scenario}-concurrent.txt", 16, count),
        }
        if scenario != "swar":
            runs[scenario]["direct"] = read_samples(root / f"{scenario}-direct.txt", 8, count)
        if metadata.get("confirmation"):
            repetitions = metadata["confirmation"]["samples"]
            runs[scenario]["serial_confirmation"] = read_samples(root / f"{scenario}-serial-confirmation.txt", 2, repetitions)
            if scenario != "swar":
                runs[scenario]["direct_confirmation"] = read_samples(root / f"{scenario}-direct-confirmation.txt", 2, repetitions)
    if metadata.get("confirmation"):
        runs["go-simd"]["buffer_confirmation"] = read_samples(root / "go-simd-buffer-confirmation.txt", 2, metadata["confirmation"]["samples"])
    noisy = sum(
        case["MB/s"]["max"] / case["MB/s"]["min"] > 1.2
        for run in runs.values() for kind in ("serial", "concurrent", "direct") if kind in run for case in run[kind].values()
    )
    out = [
        "# Ether yEnc backend scenarios", "",
        f"Measured {metadata['finished_utc'][:10]}, {metadata['cpu_model']}, {metadata['logical_cpus']} logical CPUs, {metadata['go_version']}.",
        f"Decoder implementation: `{metadata['implementation_commit']}`. Platform: `{metadata['platform']}`.",
        f"Broad matrix: three {metadata['duration']} samples per case; medians reported. Serial GOMAXPROCS=1; concurrency GOMAXPROCS=8.",
        "Representative serial, buffer and bare-package results use five 1-second confirmation samples when present; the broad matrix uses three 300 ms samples.",
        "Random 384/768 KiB payloads, 128/256-byte lines, CRC enabled. All backend runs were sequential.",
        "The Ether cases include BodyReader, framing, limits and CRC; the source is an in-memory NNTP response, not a live socket.", "",
        "**Units:** MB/s means decoded decimal megabytes per second. CPU time is process user + system time across all threads, including GC.",
        "**100% busy CPU = one occupied core.** Divide by the machine's logical CPU count for whole-machine percentage.",
        "CPU ms/GiB measures work per decoded byte; lower is better. Busy CPU alone does not measure efficiency.", "",
        f"**Variability:** {noisy} cases had a greater than 20% max/min throughput spread. This is a shared machine; interpret small differences cautiously.",
        "Ranges below show the actual minimum–maximum samples, not confidence intervals. CPU cost is reported separately from wall throughput.", "",
        "## Build combinations", "",
        "| Scenario | CGO_ENABLED | GOEXPERIMENT | Selected decoder with fresh output |",
        "|---|---:|---|---|",
    ]
    actual_decoders = {}
    for scenario, label in SCENARIOS.items():
        flags = metadata["scenarios"][scenario]
        decoder = "Ether SWAR"
        if scenario != "swar":
            selection = (root / f"{scenario}-selection.txt").read_text()
            match = re.search(r"RapidYenc kernel: (\S+)", selection)
            if not match:
                raise ValueError(f"Missing RapidYenc kernel selection: {scenario}")
            decoder = "Ether SWAR (generic kernel)" if match[1] == "generic" else f"RapidYenc {'Go' if scenario == 'go-simd' else 'C'} ({match[1]})"
        actual_decoders[scenario] = decoder
        out.append(f"| {label} | {flags['CGO_ENABLED']} | `{flags['GOEXPERIMENT'] or '(off)'}` | {decoder} |")
    out += ["", "Caller-supplied exact/headroom buffers always select SWAR in the current integration, in every build combination.",
        "Both cgo configurations selected the same C NEON decoder here. Their timing differences do not establish a different backend or a causal benefit from enabling the Go SIMD experiment.", "",
            "## Representative CPU and throughput", "",
            "768 KiB articles, 128-byte lines, fresh output; selected decoder.", "",
            "| Scenario | Median MB/s | MB/s range | CPU µs/article | CPU ms/GiB | Allocations/article |",
            "|---|---:|---:|---:|---:|---:|"]
    for scenario, label in SCENARIOS.items():
        c = reference(runs[scenario])
        out.append(f"| {label} | {value(c,'MB/s'):,.0f} | {c['MB/s']['min']:,.0f}–{c['MB/s']['max']:,.0f} | {value(c,'cpu-ns/op')/1000:.1f} | {value(c,'cpu-ms/GiB'):,.1f} | {value(c,'allocs/op'):.0f} |")
    out += ["", "## Fresh-output sizes and line lengths", "",
            "Broad matrix (three 300 ms samples). Each cell is median MB/s and throughput ratio against forced SWAR **in the same build**. Repeated SWAR controls expose run variation.", "",
            "| Article / line length | " + " | ".join(SCENARIOS.values()) + " |",
            "|---|---:|---:|---:|---:|"]
    for size, line in [(384,128),(768,128),(384,256),(768,256)]:
        cells = []
        for scenario in SCENARIOS:
            c = serial(runs[scenario],size,line)
            baseline = serial(runs[scenario],size,line,swar=True)
            cells.append(f"{value(c,'MB/s'):,.0f} ({value(c,'MB/s')/value(baseline,'MB/s'):.2f}×)")
        out.append(f"| {size} KiB / {line} | " + " | ".join(cells) + " |")
    out += ["", "## Output-buffer scenarios", "",
            "768 KiB / 128-byte lines, Go SIMD build. These distinguish allocations and actual decoder selection; all four builds were measured in the raw files.", "",
            "| Buffer | Actual decoder | MB/s | CPU ms/GiB | B/article | Allocations/article |",
            "|---|---|---:|---:|---:|---:|"]
    for buffer in ("fresh", "exact", "headroom"):
        c = reference(runs["go-simd"]) if buffer == "fresh" else runs["go-simd"].get("buffer_confirmation", {}).get(
            f"BenchmarkBodyDecode/line128/768KiB/buffer{buffer}/SWARfalse", serial(runs["go-simd"], buffer=buffer))
        out.append(f"| {buffer} | {actual_decoders['go-simd'] if buffer=='fresh' else 'Ether SWAR'} | {value(c,'MB/s'):,.0f} | {value(c,'cpu-ms/GiB'):,.1f} | {value(c,'B/op'):,.0f} | {value(c,'allocs/op'):.0f} |")
    out += ["", "`exact` has capacity equal to payload size; `headroom` adds 64 bytes. Both reuse the same caller-owned buffer on each read.", ""]
    for buffer in ("fresh", "exact"):
        out += [f"## Measured saturated concurrency: {buffer} output", "",
                "768 KiB / 128-byte lines; fixed independent readers continuously decode. Each cell is aggregate MB/s / process CPU%.",
                "Goroutine launch/join and runtime/GC are included; fixture/reader setup is excluded. These are not rate-limited playback measurements.", "",
                "| Readers | " + " | ".join(SCENARIOS.values()) + " |",
                "|---:|---:|---:|---:|---:|"]
        for workers in metadata["workers"]:
            cells = []
            for scenario in SCENARIOS:
                c = parallel(runs[scenario],workers,buffer)
                cells.append(f"{value(c,'MB/s'):,.0f} / {value(c,'cpu-%'):.0f}%")
            out.append(f"| {workers} | " + " | ".join(cells) + " |")
        out += [""]
    out += ["## Bare-package diagnostic", "",
            "768 KiB / 128-byte lines. Public RapidYenc decoder reused between complete responses; fresh output, or reusable output with 64 bytes of headroom.",
            "This removes Ether's BodyReader and extra guard/metadata checks. It is a diagnostic comparison, **not a production-safe replacement**.", "",
            "| Backend | Fresh MB/s | Fresh CPU ms/GiB | Reused MB/s | Reused CPU ms/GiB |",
            "|---|---:|---:|---:|---:|"]
    for scenario, label in SCENARIOS.items():
        if scenario == "swar":
            continue
        fresh,reused = direct(runs[scenario]),direct(runs[scenario],True)
        out.append(f"| {label} | {value(fresh,'MB/s'):,.0f} | {value(fresh,'cpu-ms/GiB'):,.1f} | {value(reused,'MB/s'):,.0f} | {value(reused,'cpu-ms/GiB'):,.1f} |")
    out += ["", "## Projected decoder CPU for multiple reads", "",
            "**Estimates, not rate-limited measurements.** Use serial 768 KiB / 128-byte / fresh-output CPU cost above.",
            "Each independent, uncached reader decodes its payload once. Rates are decoded payload **Mb/s**, not MB/s or NNTP wire rates.", "",
            "`CPU% = 100 × readers × (Mb/s per reader × 1e6 / 8) × CPU ns/article / article bytes / 1e9`", "",
            "| Readers | Mb/s per reader | " + " | ".join(SCENARIOS.values()) + " |",
            "|---:|---:|---:|---:|---:|---:|"]
    for rate in (25,100):
        for workers in metadata["workers"]:
            percentages = [100*workers*rate*1e6/8*value(reference(runs[s]),'cpu-ns/op')/(768<<10)/1e9 for s in SCENARIOS]
            out.append(f"| {workers} | {rate} | " + " | ".join(f"{p:.2f}%" for p in percentages) + " |")
    out += ["", "At a fixed aggregate bitrate, additional readers do not automatically multiply CPU: use the total decoded rate.",
            "These estimates exclude TLS, sockets, disk, application work and transcoding. Cache hits can avoid decoding; retries, prefetch and read amplification can add it.",
            "Linear serial-cost projections do not capture the higher per-byte cost sometimes seen under saturated concurrency. They are useful below saturation, not an achievable-throughput guarantee.", "",
            "## Reproduce and inspect", "",
            "CPU metrics are implemented for macOS/Linux. A compatible C/C++ toolchain is required for cgo configurations.", "",
            "```sh", "bash scripts/bench_yenc_backends.sh /tmp/ether-yenc-results", "```", "",
            "The runner saves raw samples/metadata and calls the standard-library-only report script. Benchmarks do not change production decoder code.", ""]
    relative = root.name if args.output.parent.resolve() == root.parent.resolve() else str(root.resolve())
    for scenario,label in SCENARIOS.items():
        links = [f"[{kind}]({relative}/{scenario}-{kind}.txt)" for kind in ("serial","concurrent","selection")]
        links += [f"[build environment]({relative}/{scenario}-environment.json)",
                  f"[selected source files]({relative}/{scenario}-files.txt)"]
        if scenario != "swar":
            links.append(f"[bare package]({relative}/{scenario}-direct.txt)")
        if metadata.get("confirmation"):
            links.append(f"[serial confirmation]({relative}/{scenario}-serial-confirmation.txt)")
            if scenario != "swar":
                links.append(f"[bare-package confirmation]({relative}/{scenario}-direct-confirmation.txt)")
        out.append(f"- {label}: " + ", ".join(links) + ".")
    if metadata.get("confirmation"):
        out += ["", f"[Buffer-reuse confirmation]({relative}/go-simd-buffer-confirmation.txt)."]
    args.output.write_text("\n".join(out)+"\n")


if __name__ == "__main__":
    main()
