# `ether_plugin` (Python SDK)

Single-file, standard-library-only Python 3.9+ SDK for Ether Plugin Protocol v1 (EPP/1).

## Features

- Zero external pip dependencies (`http.server`, `urllib.request`, `json`, `socket`, `base64`).
- Same declarative `plugin(manifest=..., routes=...)` router shape as `@ether/plugin-sdk` and `sdk/go`.
- Runs as an HTTP sidecar (`serve(p, mode="http")`) or an outbound WebSocket tunnel sidecar (`serve(p, mode="tunnel")`).
- Includes `RemoteHostClient` for `/ether/plugin-host/*` authenticated with `X-Ether-Plugin-Credential`, plus `create_test_harness`, `create_mock_host_client`, `fixtures`, and starter capability templates.

## Usage

```python
from ether_plugin import plugin, serve

hello = plugin(
    manifest={
        "id": "org.ether.examples.hello",
        "name": "Hello plugin",
        "version": "0.1.0",
        "description": "Minimal greeting plugin for Ether.",
        "types": ["movie"],
        "idPrefixes": ["tt"],
        "catalogs": [{"type": "movie", "id": "hello", "name": "Hello movies"}],
        "resources": ["catalog", "meta"],
        "ether": {
            "protocol": 1,
            "kind": "extension",
            "capabilities": ["list_source", "ui"],
            "summary": "Minimal greeting plugin for Ether.",
            "runtime": {"kind": "remote"},
        },
    },
    routes={
        "catalog": lambda req, ctx: {
            "metas": [
                {
                    "id": "tt0133093",
                    "type": req["type"],
                    "name": f"Hello from {req['id']}",
                    "releaseInfo": "1999",
                }
            ]
        },
    },
)

if __name__ == "__main__":
    serve(hello, background=False)
```

Tunnel mode makes one connection and returns when it drops. Use a process
supervisor to restart the sidecar after a disconnection.

## Sidecars

A sidecar is a remote plugin whose program runs on a machine you choose and
dials out to Ether over the `ether-tunnel-v1` WebSocket, so it needs no inbound
port. Pair once and the SDK saves the credential:

```sh
ETHER_URL=https://ether.example.com \
ETHER_PLUGIN_STATE_DIR=/data \
ETHER_PAIRING_CODE=PAIR-1234 \
python3 plugin.py
```

Create the code in Ether under Plugins › your plugin › Connect a sidecar; it is
valid for 5 minutes and single use. `serve(plugin, mode="tunnel")` resolves the
credential in this order: an explicit credential, then the saved file, then the
pairing code (exchanged once and saved). With none it raises `CredentialError`
with one clear line.

- `ETHER_URL` — the server's `http(s)` base; the pair and `ws(s)` tunnel URLs
  are derived from it.
- `ETHER_PLUGIN_CREDENTIAL` — an explicit credential; always wins.
- `ETHER_PLUGIN_STATE_DIR` — where the credential is saved, else `/data` when it
  exists and is writable (a Docker volume), else `~/.config/ether-plugins/<id>/`.
- `ETHER_PAIRING_CODE` — a one-time code.

The credential is saved at `<state dir>/credential` (mode `0600`, directory
`0700`). If the tunnel rejects a saved credential, the file is deleted and
`serve` raises `CredentialError` instead of reconnecting forever.

A Dockerfile that keeps the credential across restarts:

```dockerfile
FROM python:3.12-slim
WORKDIR /app
COPY plugin.py ether_plugin.py manifest.json settings.schema.json ./
VOLUME /data
ENV ETHER_PLUGIN_STATE_DIR=/data
CMD ["python3", "plugin.py"]
```

Declare how the console runs it with an `ether.sidecar` manifest block:

```json
"ether": {
  "runtime": { "kind": "remote" },
  "sidecar": {
    "command": "python3 plugin.py",
    "docs": "https://example.com/my-sidecar"
  }
}
```

`docker` is an image reference; `download` (an https URL) with `command` (a
single line) is the alternative; `docs` adds a setup link.


Content-kind item dictionaries may include `unavailable: {"code": "no_verified_source",
"detail": "No verified source"}`. Ether keeps these items visible with the reason
and refuses resolution until a later upsert omits the field. `pattern: "file"`
with a `path` is reserved for hosted resolvers with `folders: read`; Python
sidecars cannot access host files or call `folders.hash`. See `docs/plugin-kinds.md`.

### Continuing background jobs

A `job` handler receives `{"name": ..., "continuation": ...}`. Return
`{"continueInMs": 0, "continuation": {"next": 41}, "progress": "Platform 41 of 92"}`
to continue. Return `{"done": True}` or `{"failed": "reason"}` to finish; an
absent `continueInMs` also finishes. Store durable checkpoints in KV to recover
on a later owner-started run after restart. Tokens are at most 64 KiB and progress
at most 1,024 UTF-8 bytes. The drawer supports progress and cancellation.

Ether uses owner-controlled global/per-plugin ceilings under Plugins: default
2 GiB memory (up to wasm32 4 GiB), 12 hours, effectively unlimited background
compute, 8 GiB downloads/member expansion, 32 GiB total expansion and 65,535 ZIP
members. Downloads also obey library cache quotas. Hosted manifests request
`ether.runtime.limits` with `memoryBytes`, `deadline`, `unitBudget`, `fetchBytes`,
`archiveMemberBytes`, `archiveExpandedBytes` and `archiveMembers`. Install preview
and detail show requested and granted values. Jobs pause during playback/high
host load and use low CPU/IO priority. A 1,000,000-call sanity bound applies;
viewer browse/search/resolve/playback calls retain tight time and compute budgets.
Remote Python processes manage their own CPU/memory. See the TypeScript SDK
README for route classification, policy APIs and complete job behavior.
