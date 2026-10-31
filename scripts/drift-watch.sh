#!/usr/bin/env bash
# Plan reference image bumps, re-record the lab corpus, and open a review PR.
# Credentials stay in scripts/lab/lab.env and scripts/lab/.conform, which are gitignored.
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"
out="drift-watch-out"
export GOMAXPROCS="${GOMAXPROCS:-2}"
export GIT_TERMINAL_PROMPT=0

usage() {
  echo "usage: scripts/drift-watch.sh plan|record|report|pr|run" >&2
  exit 2
}

build() {
  mkdir -p "$out/bin" "$out/blocking" "$out/recordings"
  go build -p 1 -o "$out/bin/api-drift" ./scripts/api-drift
  go build -p 1 -o "$out/bin/conformdiff" ./cmd/conformdiff
  go build -p 1 -o "$out/bin/drift-watch" ./scripts/drift-watch
}

blocking_json_count() {
  local count=0
  local path
  shopt -s nullglob
  for path in "$out/blocking"/*.json; do
    count=$((count + 1))
  done
  shopt -u nullglob
  echo "$count"
}

write_report() {
  local -a args
  args=(-api "$out/api.md" -images "$out/images.json" -out docs/acceptance/drift-watch-report.json)
  if [[ "${DRIFT_RECORDED:-}" == 1 ]]; then
    args+=(-recorded)
  fi
  if [[ -f "$out/record-error.txt" ]]; then
    args+=(-record-error "$out/record-error.txt")
  fi
  if [[ "$(blocking_json_count)" != 0 ]]; then
    args+=(-blocking "$out/blocking")
  fi
  "$out/bin/drift-watch" report "${args[@]}"
}

plan() {
  build
  "$out/bin/api-drift" -conformdiff "$out/bin/conformdiff" -out "$out/api.md"
  "$out/bin/drift-watch" images -compose scripts/lab/compose.yaml -write -out "$out/images.json"
  write_report
}

refuse_production_host() {
  if [[ "$(uname -s)" == Darwin ]]; then
    echo "refusing to change reference containers on macOS; run on bb-worker-server or gpu-workstation" >&2
    exit 2
  fi
  local addrs=""
  addrs="$(hostname -I 2>/dev/null || true)"
  case "$addrs" in
    *10.0.0.43*)
      echo "refusing to run on HYPERV03" >&2
      exit 2
      ;;
  esac
}

apply_pins() {
  [[ "${DRIFT_WATCH_APPLY:-}" == 1 ]] || {
    echo "set DRIFT_WATCH_APPLY=1 on bb-worker-server or gpu-workstation" >&2
    exit 2
  }
  refuse_production_host
  [[ -f scripts/lab/lab.env ]] || {
    echo "scripts/lab/lab.env is missing" >&2
    exit 2
  }
  local mode
  mode="$(stat -c %a scripts/lab/lab.env)"
  case "$mode" in
    600|400) ;;
    *)
      echo "scripts/lab/lab.env must be mode 600" >&2
      exit 2
      ;;
  esac
  command -v docker >/dev/null 2>&1 || {
    echo "docker is required" >&2
    exit 2
  }
  docker compose -f scripts/lab/compose.yaml --env-file scripts/lab/lab.env pull jellyfin emby silo
  docker compose -f scripts/lab/compose.yaml --env-file scripts/lab/lab.env up -d jellyfin emby silo
}

record_protocol() {
  local protocol="$1"
  local token_file="scripts/lab/.conform/${protocol}-reference.token"
  [[ -f "$token_file" ]] || {
    echo "missing ${token_file}; run scripts/lab/discover.sh in a lab window" >&2
    return 1
  }
  local token=""
  token="$(tr -d '[:space:]' < "$token_file")"
  [[ -n "$token" ]] || {
    echo "empty token file for ${protocol}" >&2
    return 1
  }
  local host="${LAB_HOST:-192.168.68.66}"
  local url=""
  case "$protocol" in
    jellyfin) url="${CONFORM_REFERENCE_URL_JELLYFIN:-http://${host}:8096}" ;;
    emby) url="${CONFORM_REFERENCE_URL_EMBY:-http://${host}:8097/emby}" ;;
    silo) url="${CONFORM_REFERENCE_URL_SILO:-http://${host}:8098}" ;;
    *)
      echo "unsupported protocol" >&2
      return 2
      ;;
  esac
  local stamp dest
  stamp="$(date -u +%Y%m%dT%H%M%SZ)"
  dest="$out/recordings/${protocol}-${stamp}"
  [[ ! -e "$dest" ]] || {
    echo "recording path already exists" >&2
    return 1
  }
  local varfile="scripts/lab/.conform/${protocol}-reference-vars.json"
  local rc=0
  set +e
  if [[ -f "$varfile" ]]; then
    CONFORM_REFERENCE_VARS="$varfile" \
      CONFORM_ALLOW_RECORD=1 \
      CONFORM_REFERENCE_URL="$url" \
      CONFORM_REFERENCE_TOKEN="$token" \
      CONFORMDIFF="$out/bin/conformdiff" \
      scripts/reference-drift.sh record "$protocol" "$dest" \
      >"$out/blocking/${protocol}-record.log" 2>&1
  else
    CONFORM_ALLOW_RECORD=1 \
      CONFORM_REFERENCE_URL="$url" \
      CONFORM_REFERENCE_TOKEN="$token" \
      CONFORMDIFF="$out/bin/conformdiff" \
      scripts/reference-drift.sh record "$protocol" "$dest" \
      >"$out/blocking/${protocol}-record.log" 2>&1
  fi
  rc=$?
  set -e
  if [[ -s "$dest/drift-report.json" ]]; then
    cp "$dest/drift-report.json" "$out/blocking/${protocol}-recording.json"
  fi
  if [[ -n "${CONFORM_TARGET_URL:-}" && -d "$dest" ]]; then
    set +e
    if [[ -n "${CONFORM_TARGET_TOKEN:-}" ]]; then
      CONFORM_TARGET_TOKEN="$CONFORM_TARGET_TOKEN" \
        "$out/bin/conformdiff" compare -protocol "$protocol" -format json -summary \
        -fixtures "$dest" -target "$CONFORM_TARGET_URL" \
        >"$out/blocking/${protocol}-compare.json" 2>>"$out/blocking/${protocol}-compare.log"
    else
      "$out/bin/conformdiff" compare -protocol "$protocol" -format json -summary \
        -fixtures "$dest" -target "$CONFORM_TARGET_URL" \
        >"$out/blocking/${protocol}-compare.json" 2>>"$out/blocking/${protocol}-compare.log"
    fi
    set -e
    if [[ ! -s "$out/blocking/${protocol}-compare.json" ]]; then
      rm -f "$out/blocking/${protocol}-compare.json"
    fi
  fi
  if [[ -s "$dest/drift-report.json" ]]; then
    return 0
  fi
  echo "record failed for ${protocol}" >&2
  return "$rc"
}

record_corpus() {
  [[ -x "$out/bin/conformdiff" ]] || build
  DRIFT_RECORDED=1
  export DRIFT_RECORDED
  rm -f "$out/record-error.txt"
  local failed=""
  local protocol
  for protocol in jellyfin emby silo; do
    if ! record_protocol "$protocol"; then
      failed="${failed} ${protocol}"
    fi
  done
  if [[ -n "${failed}" ]]; then
    echo "record failed:${failed}" >"$out/record-error.txt"
    return 1
  fi
}

worktree_is_drift_only() {
  local line path
  while IFS= read -r line; do
    [[ -z "$line" ]] && continue
    case "$line" in
      *" -> "*)
        echo "unrelated change: ${line}" >&2
        return 1
        ;;
    esac
    path="${line:3}"
    case "$path" in
      scripts/lab/compose.yaml | docs/acceptance/drift-watch-report.json) ;;
      drift-watch-out | drift-watch-out/ | drift-watch-out/*) ;;
      *)
        echo "unrelated change: ${path}" >&2
        return 1
        ;;
    esac
  done < <(git status --porcelain)
}

commit_drift() {
  if [[ -n "${GITHUB_ACTIONS:-}" ]]; then
    git -c user.name="github-actions[bot]" -c user.email="41898282+github-actions[bot]@users.noreply.github.com" commit -m "$(cat <<'EOF'
Record upstream API drift and reference image pins.

Endpoint changes and blocking conformance diffs need review before merge.
EOF
)"
  else
    git commit -m "$(cat <<'EOF'
Record upstream API drift and reference image pins.

Endpoint changes and blocking conformance diffs need review before merge.
EOF
)"
  fi
}

copy_recordings() {
  local scratch="$1"
  local protocol dest path
  for protocol in jellyfin emby silo; do
    dest=""
    while IFS= read -r path; do
      dest="$path"
    done < <(find "$scratch/recordings" -mindepth 1 -maxdepth 1 -type d -name "${protocol}-*" | sort)
    if [[ -z "$dest" ]]; then
      echo "no recording to commit for ${protocol}" >&2
      exit 2
    fi
    while IFS= read -r path; do
      case "$path" in
        *token* | */lab.env)
          echo "recording contains a secret path" >&2
          exit 2
          ;;
      esac
    done < <(find "$dest" -type f)
    rm -rf "testdata/conform/${protocol}"
    mkdir -p "testdata/conform/${protocol}"
    cp -R "$dest/." "testdata/conform/${protocol}/"
  done
}

publish() {
  [[ "${DRIFT_WATCH_OPEN_PR:-}" == 1 ]] || {
    echo "set DRIFT_WATCH_OPEN_PR=1 to publish" >&2
    exit 2
  }
  local gh_token="${GH_TOKEN:-${GITHUB_TOKEN:-}}"
  [[ -n "$gh_token" ]] || {
    echo "GH_TOKEN is required" >&2
    exit 2
  }
  export GH_TOKEN="$gh_token"
  [[ -f docs/acceptance/drift-watch-report.json ]] || {
    echo "missing drift report; run plan first" >&2
    exit 2
  }
  [[ -x "$out/bin/drift-watch" ]] || build
  local decision
  decision="$("$out/bin/drift-watch" should-open -report docs/acceptance/drift-watch-report.json)"
  if [[ "$decision" != yes ]]; then
    echo "No endpoint drift, image bump, or blocking diff to publish."
    return 0
  fi
  worktree_is_drift_only
  local base="${DRIFT_PR_BASE:-prism}"
  git fetch origin
  if ! git show-ref --verify --quiet "refs/remotes/origin/${base}"; then
    base="$(git symbolic-ref --short refs/remotes/origin/HEAD)"
    base="${base#origin/}"
  fi
  local scratch
  scratch="$(mktemp -d)"
  # shellcheck disable=SC2064
  trap "rm -rf '$scratch'" RETURN
  cp scripts/lab/compose.yaml "$scratch/compose.yaml"
  cp docs/acceptance/drift-watch-report.json "$scratch/report.json"
  if [[ "${DRIFT_WATCH_COMMIT_FIXTURES:-}" == 1 ]]; then
    mkdir -p "$scratch/recordings"
    cp -R "$out/recordings/." "$scratch/recordings/"
  fi
  git checkout -- scripts/lab/compose.yaml
  if git ls-files --error-unmatch docs/acceptance/drift-watch-report.json >/dev/null 2>&1; then
    git checkout -- docs/acceptance/drift-watch-report.json
  else
    rm -f docs/acceptance/drift-watch-report.json
  fi
  local existing_branch existing_number
  existing_branch="$(gh pr list --base "$base" --state open --limit 50 --json number,headRefName,title --jq 'map(select(.title | startswith("Drift watch:"))) | .[0].headRefName // ""')"
  existing_number="$(gh pr list --base "$base" --state open --limit 50 --json number,headRefName,title --jq 'map(select(.title | startswith("Drift watch:"))) | .[0].number // ""')"
  local branch="drift-watch/${GITHUB_RUN_ID:-local-$(date -u +%Y%m%dT%H%M%SZ)}"
  if [[ -n "$existing_branch" ]]; then
    branch="$existing_branch"
    git fetch origin "$branch"
    git checkout -B "$branch" "origin/${branch}"
  else
    git checkout -B "$branch" "origin/${base}"
  fi
  cp "$scratch/compose.yaml" scripts/lab/compose.yaml
  mkdir -p docs/acceptance
  cp "$scratch/report.json" docs/acceptance/drift-watch-report.json
  if [[ "${DRIFT_WATCH_COMMIT_FIXTURES:-}" == 1 ]]; then
    copy_recordings "$scratch"
  fi
  git add scripts/lab/compose.yaml docs/acceptance/drift-watch-report.json
  if [[ "${DRIFT_WATCH_COMMIT_FIXTURES:-}" == 1 ]]; then
    git add testdata/conform/jellyfin testdata/conform/emby testdata/conform/silo
  fi
  local committed=0
  if ! git diff --cached --quiet; then
    commit_drift
    git push origin "HEAD:refs/heads/${branch}"
    committed=1
  fi
  if [[ -z "$existing_number" && "$committed" -eq 0 ]]; then
    echo "drift is already on ${base}"
    return 0
  fi
  local body
  body="$(mktemp)"
  "$out/bin/drift-watch" pr-body -report docs/acceptance/drift-watch-report.json -out "$body"
  local title="Drift watch: reference endpoints and image pins"
  if [[ -n "$existing_number" ]]; then
    gh pr edit "$existing_number" --title "$title" --body-file "$body"
  else
    gh pr create --base "$base" --head "$branch" --title "$title" --body-file "$body"
  fi
  rm -f "$body"
}

run_all() {
  plan
  local rc=0
  if [[ "${DRIFT_WATCH_APPLY:-}" == 1 ]]; then
    apply_pins
  fi
  if [[ "${DRIFT_WATCH_SKIP_RECORD:-}" != 1 ]]; then
    record_corpus || rc=$?
    write_report
  fi
  if [[ "${DRIFT_WATCH_OPEN_PR:-}" == 1 ]]; then
    publish
  fi
  return "$rc"
}

case "${1:-}" in
  plan) plan ;;
  record) record_corpus; write_report ;;
  report) write_report ;;
  pr) publish ;;
  run) run_all ;;
  *) usage ;;
esac
