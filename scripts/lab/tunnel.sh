#!/usr/bin/env bash
# From the Mac: create localhost forwards into the WSL-only reference lab.
# Set LAB_PORT_BASE (or pass a base port) to avoid colliding with another worker.
# Example: LAB_PORT_BASE=18396 scripts/lab/tunnel.sh
set -euo pipefail

action=start
if [[ "${1:-}" == "down" ]]; then
  action=down
  shift
fi

if [[ -n "${1:-}" ]]; then
  LAB_PORT_BASE="$1"
  shift
fi
if (($#)); then
  echo "usage: LAB_PORT_BASE=18096 $0 [down] [port-base]" >&2
  exit 2
fi

LAB_PORT_BASE="${LAB_PORT_BASE:-18096}"
if ! [[ "$LAB_PORT_BASE" =~ ^[0-9]+$ ]] || ((LAB_PORT_BASE < 1024 || LAB_PORT_BASE > 65533)); then
  echo "LAB_PORT_BASE must be an integer from 1024 through 65533" >&2
  exit 2
fi

pidfile="${TMPDIR:-/tmp}/prism-lab-tunnel-${LAB_PORT_BASE}.pid"
port_jellyfin=$LAB_PORT_BASE
port_emby=$((LAB_PORT_BASE + 1))
port_silo=$((LAB_PORT_BASE + 2))

if [[ "$action" == down ]]; then
  if [[ ! -f "$pidfile" ]]; then
    echo "no tunnel pidfile for port base $LAB_PORT_BASE" >&2
    exit 1
  fi
  read -r ssh_pid < "$pidfile"
  if ! [[ "$ssh_pid" =~ ^[0-9]+$ ]] || ! kill -0 "$ssh_pid" 2>/dev/null; then
    unlink "$pidfile" 2>/dev/null || true
    echo "removed stale tunnel pidfile for port base $LAB_PORT_BASE"
    exit 0
  fi
  command_line="$(ps -p "$ssh_pid" -o command= 2>/dev/null || true)"
  if [[ "$command_line" != *"-L ${port_jellyfin}:127.0.0.1:8096"* || \
        "$command_line" != *"-L ${port_emby}:127.0.0.1:8097"* || \
        "$command_line" != *"-L ${port_silo}:127.0.0.1:8098"* || \
        "$command_line" != *"gpu-workstation"* ]]; then
    echo "pid $ssh_pid does not match this lab tunnel; leaving it untouched" >&2
    exit 1
  fi
  kill -TERM "$ssh_pid"
  for _ in {1..20}; do
    process_state="$(ps -p "$ssh_pid" -o state= 2>/dev/null | tr -d ' ')"
    [[ -z "$process_state" || "$process_state" == Z* ]] && break
    sleep 0.1
  done
  process_state="$(ps -p "$ssh_pid" -o state= 2>/dev/null | tr -d ' ')"
  if [[ -n "$process_state" && "$process_state" != Z* ]]; then
    echo "tunnel pid $ssh_pid did not exit after SIGTERM; leaving its pidfile in place" >&2
    exit 1
  fi
  unlink "$pidfile" 2>/dev/null || true
  echo "stopped lab tunnel pid $ssh_pid (port base $LAB_PORT_BASE)"
  exit 0
fi

if [[ -f "$pidfile" ]]; then
  read -r old_pid < "$pidfile"
  if [[ "$old_pid" =~ ^[0-9]+$ ]] && kill -0 "$old_pid" 2>/dev/null; then
    echo "lab tunnel for port base $LAB_PORT_BASE is already running as pid $old_pid" >&2
    exit 1
  fi
  unlink "$pidfile" 2>/dev/null || true
fi

ssh -N -o ExitOnForwardFailure=yes \
  -o ServerAliveInterval=15 -o ServerAliveCountMax=4 \
  -L "${port_jellyfin}:127.0.0.1:8096" \
  -L "${port_emby}:127.0.0.1:8097" \
  -L "${port_silo}:127.0.0.1:8098" gpu-workstation &
ssh_pid=$!
printf '%s\n' "$ssh_pid" > "$pidfile"

cleanup() {
  if [[ -f "$pidfile" ]] && [[ "$(cat "$pidfile")" == "$ssh_pid" ]]; then
    unlink "$pidfile" 2>/dev/null || true
  fi
}
trap cleanup EXIT
trap 'kill -TERM "$ssh_pid" 2>/dev/null || true; wait "$ssh_pid" || true; exit 130' INT TERM
wait "$ssh_pid"
