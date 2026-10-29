#!/usr/bin/env bash
# From the Mac: create localhost forwards into the WSL-only reference lab.
set -euo pipefail
if [[ "${1:-}" == "down" ]]; then
  pkill -f 'ssh .*18096:127.0.0.1:8096.*18097:127.0.0.1:8097.*18098:127.0.0.1:8098' || true
  exit 0
fi
exec ssh -N -o ExitOnForwardFailure=yes \
  -L 18096:127.0.0.1:8096 \
  -L 18097:127.0.0.1:8097 \
  -L 18098:127.0.0.1:8098 gpu-workstation
