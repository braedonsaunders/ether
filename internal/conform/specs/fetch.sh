#!/usr/bin/env bash
# API source snapshots with unclear redistribution terms are fetched on demand.
# They are intentionally not vendored. Run from any directory; outputs stay local.
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
out="${1:-$here/.downloaded}"
mkdir -p "$out"
curl -fL 'https://repo.jellyfin.org/files/openapi/stable/jellyfin-openapi-12.4.json' -o "$out/jellyfin-openapi-12.4.json"
curl -fL 'https://raw.githubusercontent.com/MediaBrowser/Emby.SDK/45e73012eddf985dcfe878ca031b500085e16496/Resources/OpenApi/openapi_v2.json' -o "$out/emby-openapi-v2.json"
echo "Fetched API snapshots to $out; upstream redistribution licenses were not explicit, so these files are not committed."
