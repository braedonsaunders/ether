#!/usr/bin/env bash
# Explicit reference recording; default check mode has no network operations.
set -euo pipefail
mode=${1:-check}
protocol=${2:?usage: reference-drift.sh check|record jellyfin|emby|silo CANDIDATE_DIR}
candidate=${3:?candidate directory required}
case "$protocol" in jellyfin|emby|silo) ;; *) echo 'unsupported protocol' >&2; exit 2;; esac
bin=${CONFORMDIFF:-conformdiff}
case "$mode" in
  check)
    exec "$bin" drift -protocol "$protocol" -fixtures "testdata/conform/$protocol" -candidate "$candidate"
    ;;
  record)
    # Lab Keeper supplies the address during an allocated reference window.
    [[ ${CONFORM_ALLOW_RECORD:-0} == 1 ]] || { echo 'record requires CONFORM_ALLOW_RECORD=1 and a Lab Keeper window' >&2; exit 2; }
    : "${CONFORM_REFERENCE_URL:?reference URL required}"
    [[ ! -e "$candidate" ]] || { echo 'candidate directory must not exist; preserve previous recordings' >&2; exit 2; }
    mkdir -p "$candidate"
    # Credentials are read by conformdiff from the environment, not argv.
    "$bin" record -protocol "$protocol" -reference "$CONFORM_REFERENCE_URL" -out "$candidate" -format json > "$candidate/record-report.json"
    "$bin" drift -protocol "$protocol" -fixtures "testdata/conform/$protocol" -candidate "$candidate" > "$candidate/drift-report.json"
    ;;
  *) echo 'mode must be check or record' >&2; exit 2;;
esac
