#!/usr/bin/env bash
# Fetches unvendored API descriptions; redistribution terms are unclear.
set -euo pipefail
out="${1:-${XDG_CACHE_HOME:-$HOME/.cache}/ether/conform/specs}"
mkdir -p "$out"
curl -fL 'https://repo.jellyfin.org/files/openapi/stable/jellyfin-openapi-12.4.json' -o "$out/jellyfin-openapi-12.4.json"
curl -fL 'https://raw.githubusercontent.com/MediaBrowser/Emby.SDK/45e73012eddf985dcfe878ca031b500085e16496/Resources/OpenApi/openapi_v2.json' -o "$out/emby-openapi-v2.json"
curl -fL 'https://raw.githubusercontent.com/Silo-Server/silo-server/44a345dbfb8c0a5892dc366ee0dd33fc091b7cf2/contracts/api/v2/openapi.json' -o "$out/silo-openapi.json"
curl -fL 'https://raw.githubusercontent.com/Silo-Server/silo-server/44a345dbfb8c0a5892dc366ee0dd33fc091b7cf2/contracts/api/v2/route-inventory.json' -o "$out/silo-route-inventory.json"
for spec in '02a6064916b7e08f1f1d8d7ffc1fb1c7623df0e378729fcd7fd353766868036e jellyfin-openapi-12.4.json' '4dffb6e928f2b16892c87ca74b31c4dd7b6937c1aa72d7f265e80c1b3c7e9f70 emby-openapi-v2.json' '7bd930f36640541315d631b721593942019cb8333f9b0bc5c2148d45dd2acc07 silo-openapi.json' '93ec729cc191156ac5d5928e46a03f350419eb9b847f94dac8bacf949290efc3 silo-route-inventory.json'; do
  set -- $spec
  got="$(shasum -a 256 "$out/$2" | cut -d ' ' -f 1)"
  [[ "$got" == "$1" ]] || { echo "SHA256 mismatch for $2: $got" >&2; exit 1; }
done
echo "Cached external API descriptions at $out."
