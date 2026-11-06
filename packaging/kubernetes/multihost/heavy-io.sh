#!/usr/bin/env bash
# Shared worker-server I/O gate. Run one explicitly approved heavy argv.
set -euo pipefail
(( $# > 0 )) || { echo 'usage: heavy-io.sh COMMAND [ARGS...]' >&2; exit 64; }
queue_start=$(date +%s)
if [[ ${ETHER_HEAVY_IO_LOCKED:-0} != 1 ]]; then
 while [[ -e /data/bb/thread-storage/batch-gate.active ]]; do sleep 60; done
 exec flock /data/bb/thread-storage/heavy-io.lock ionice -c3 nice -n 19 \
  env ETHER_HEAVY_IO_LOCKED=1 ETHER_HEAVY_QUEUE_START="$queue_start" "$0" "$@"
fi
# A batch gate that won the lock race retains priority. Caller may resubmit.
[[ ! -e /data/bb/thread-storage/batch-gate.active ]] || exit 75
run_start=$(date +%s)
finish() {
 local end
 end=$(date +%s)
 printf 'heavy_io queue_seconds=%s run_seconds=%s total_seconds=%s\n' \
  "$((run_start-ETHER_HEAVY_QUEUE_START))" "$((end-run_start))" "$((end-ETHER_HEAVY_QUEUE_START))" >&2
}
trap finish EXIT
"$@"
