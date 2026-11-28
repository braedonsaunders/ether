"""Retain gate work attribution without request credentials or database files."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys


def validate_trace(record):
    """Accept the real PerfTraceRecord shape, not merely valid JSON."""
    if not isinstance(record, dict):
        raise ValueError("trace must be an object")
    for key in ("surface", "method", "start"):
        if not isinstance(record.get(key), str) or not record[key]:
            raise ValueError("missing trace identity")
    if not isinstance(record.get("path"), str):
        raise ValueError("missing request path")
    if "tag" in record and not isinstance(record["tag"], str):
        raise ValueError("invalid tag")
    for key in ("status", "bytes", "duration", "queries", "writes", "rows", "dbTime", "lockWait"):
        if type(record.get(key)) is not int or record[key] < 0:
            raise ValueError("invalid trace counter")
    if not 100 <= record["status"] <= 599:
        raise ValueError("invalid HTTP status")
    for key in ("allocBytes", "allocs"):
        if key in record and (type(record[key]) is not int or record[key] < 0):
            raise ValueError("invalid allocation counter")
    if not isinstance(record.get("statements", []), list):
        raise ValueError("statements must be an array")
    for statement in record.get("statements", []):
        if not isinstance(statement, dict) or not isinstance(statement.get("sql"), str):
            raise ValueError("invalid SQL statement")
        if "site" in statement and not isinstance(statement["site"], str):
            raise ValueError("invalid call site")
        for key in ("count", "rows", "time"):
            if type(statement.get(key)) is not int or statement[key] < 0:
                raise ValueError("invalid statement counter")


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("root", type=Path)
    parser.add_argument("--matrix", action="store_true")
    args = parser.parse_args()
    root = args.root
    output = root / "perf-gate-artifacts"
    output.mkdir(exist_ok=True)
    # Explicit provenance allowlist; never dump the environment or config.
    provenance = {
        key: os.environ.get(key, "")
        for key in ("GITHUB_SHA", "GITHUB_RUN_ID", "GITHUB_RUN_ATTEMPT", "RUNNER_OS", "RUNNER_ARCH")
    }
    provenance["checkout_sha"] = subprocess.check_output(
        ["git", "rev-parse", "HEAD"], text=True
    ).strip()
    report = root / "perf-gate.md"
    provenance["report_present"] = report.is_file()
    if report.is_file():
        shutil.copyfile(report, output / "report.md")
    # The gate tags contain phase/client/step/sequence, never credentials.
    # Paths are deliberately absent: Stremio puts a grant in the URL path.
    fields = ("tag", "surface", "method", "status", "bytes", "start", "duration",
              "queries", "writes", "rows", "dbTime", "lockWait", "allocBytes", "allocs")
    # QueryStats stores SQL templates, not bound parameters. Also redact SQL
    # literals and comments; keep table names, call sites and all work counters.
    literals = re.compile(r"'(?:''|[^'])*'|\"(?:\"\"|[^\"])*\"|--[^\n]*|/\*.*?\*/", re.S)
    private = root / "perf-gate-private"
    traces = ("small", "large")
    if args.matrix:
        manifest = private / "matrix-cases.txt"
        cases = manifest.read_text().splitlines() if manifest.is_file() else []
        valid = len(set(cases)) == len(cases) and all(re.fullmatch(r"[a-z0-9-]+", name) for name in cases)
        traces = tuple(f"matrix-{name}" for name in cases) if valid else ()
    complete = report.is_file() and bool(traces)
    for trace in traces:
        source = private / (trace + "-trace.jsonl")
        status = {"records": 0, "tagged_records": 0, "malformed_lines": 0,
                  "invalid_records": 0, "present": source.is_file()}
        if source.is_file():
            target = output / source.name
            with source.open() as src, target.open("w") as dst:
                for line in src:
                    try:
                        record = json.loads(line)
                    except ValueError:
                        status["malformed_lines"] += 1
                        continue
                    try:
                        validate_trace(record)
                        safe = {key: record[key] for key in fields if key in record}
                        safe["statements"] = [
                            {"sql": literals.sub("?", st["sql"]),
                             **{key: st[key] for key in ("site", "count", "rows", "time") if key in st}}
                            for st in record.get("statements", [])
                        ]
                    except (ValueError, TypeError, KeyError, AttributeError):
                        # A timeout may leave a partial last record. Preserve
                        # valid records, record the loss and fail preparation.
                        status["invalid_records"] += 1
                        continue
                    dst.write(json.dumps(safe) + "\n")
                    status["records"] += 1
                    status["tagged_records"] += bool(record.get("tag"))
            status["sanitized_sha256"] = hashlib.sha256(target.read_bytes()).hexdigest()
        complete = (complete and status["tagged_records"] > 0
                    and status["malformed_lines"] == 0 and status["invalid_records"] == 0)
        provenance[trace] = status
    provenance["evidence_complete"] = complete
    (output / "provenance.json").write_text(json.dumps(provenance, indent=2) + "\n")
    return 0 if complete else 1


if __name__ == "__main__":
    sys.exit(main())
