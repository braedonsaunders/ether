# `@ether/plugin-sdk` (TypeScript / JavaScript)

Authoring kit for Ether plugins targeting Ether Plugin Protocol v1 (EPP/1).

The same `plugin({ manifest, routes })` definition runs:
- **Hosted** inside Ether's embedded QuickJS + wazero sandbox (`ether.runtime.kind: "hosted"`), bundled into a single artifact with `ether plugin pack`.
- **As a sidecar** on Node or Bun (`ether.runtime.kind: "remote"`), either serving HTTP directly (`serve(p, { mode: "http" })`) or dialing outbound over the `ether-tunnel-v1` WebSocket tunnel (`serve(p, { mode: "tunnel" })`).

Hosted plugins and Studio previews have no direct host filesystem, environment,
process or network access. QuickJS `std` and `os` modules are unavailable.
Use the permission-checked `ether` host APIs for services such as HTTP and
folders. Remote sidecars run under their own operating-system permissions and
are not covered by the hosted sandbox.

## Usage

```ts
import { plugin, serve } from "@ether/plugin-sdk";

const hello = plugin({
  manifest: {
    id: "org.ether.examples.hello",
    name: "Hello plugin",
    version: "0.1.0",
    description: "Minimal greeting plugin for Ether.",
    types: ["movie"],
    idPrefixes: ["tt"],
    catalogs: [{ type: "movie", id: "hello", name: "Hello movies" }],
    resources: ["catalog", "meta"],
    ether: {
      protocol: 1,
      kind: "extension",
      capabilities: ["list_source", "ui"],
      summary: "Minimal greeting plugin for Ether.",
      runtime: { kind: "hosted", entry: "plugin.ts" },
    },
  },
  routes: {
    catalog({ type, id }) {
      return {
        metas: [
          {
            id: "tt0133093",
            type,
            name: `Hello from ${id}`,
            releaseInfo: "1999",
          },
        ],
      };
    },
    meta({ type, id }) {
      return {
        meta: {
          id,
          type,
          name: `Title ${id}`,
          releaseInfo: "1999",
          description: "Sample metadata entry.",
        },
      };
    },
  },
});

export const handle = hello.handle;
export const rank_key = hello.rank_key;
export const cond_match = hello.cond_match;
export default hello;

// In a separate Node/Bun entry point: await serve(hello);
```

## Capabilities and routes

- `catalog` (`GET /catalog/{type}/{id}[/{extra}].json`)
- `meta` (`GET /meta/{type}/{id}.json`)
- `stream` (`GET /stream/{type}/{id}[/{extra}].json`) and `openStream` (`POST /ether/stream/open`)
- `subtitles` (`GET /subtitles/{type}/{id}[/{extra}].json`, requires `X-Ether-Pick` header)
- `markers` (`GET /ether/markers/{type}/{id}.json`)
- `channels` (`GET /ether/channels.json`) and `guide` (`GET /ether/guide/{channelId}.json`)
- `notify` (`POST /ether/notify`)
- `event` (`POST /ether/event`)
- `view` (`GET /ether/view/{page}`) and `action` (`POST /ether/action/{action}`)
- `tool` (`POST /ether/tool/{name}`) and `job` (`POST /ether/job/{name}`)
- `file` (`GET /ether/file/{path...}`)
- `rankKey` and `condMatch` hot-path exports (`rank_key`, `cond_match`)

Tunnel mode currently makes one connection. Use a process supervisor to
restart the sidecar after a disconnection.

## Sidecars

A sidecar is a remote plugin whose program runs on a machine you choose and
dials out to Ether over the `ether-tunnel-v1` WebSocket, so it needs no inbound
port. Pair once and the SDK saves the credential:

```sh
ETHER_URL=https://ether.example.com \
ETHER_PLUGIN_STATE_DIR=/data \
ETHER_PAIRING_CODE=PAIR-1234 \
node server.js
```

Create the code in Ether under Plugins › your plugin › Connect a sidecar; it is
valid for 5 minutes and single use. `serve(plugin, { mode: "tunnel" })` resolves
the credential in this order: an explicit credential, then the saved file, then
the pairing code (exchanged once and saved). With none it throws
`CredentialError` with one clear line.

- `ETHER_URL` — the server's `http(s)` base; the pair and `ws(s)` tunnel URLs
  are derived from it.
- `ETHER_PLUGIN_CREDENTIAL` — an explicit credential; always wins.
- `ETHER_PLUGIN_STATE_DIR` — where the credential is saved, else `/data` when it
  exists and is writable (a Docker volume), else `~/.config/ether-plugins/<id>/`.
- `ETHER_PAIRING_CODE` — a one-time code.

The credential is saved at `<state dir>/credential` (mode `0600`, directory
`0700`). If the tunnel rejects a saved credential, the file is deleted and
`serve` throws `CredentialError` instead of reconnecting forever.

A Dockerfile that keeps the credential across restarts:

```dockerfile
FROM node:22-slim
WORKDIR /app
COPY . .
VOLUME /data
ENV ETHER_PLUGIN_STATE_DIR=/data
CMD ["node", "server.js"]
```

Declare how the console runs it with an `ether.sidecar` manifest block:

```json
"ether": {
  "runtime": { "kind": "remote" },
  "sidecar": {
    "docker": "ghcr.io/you/my-sidecar:1.0.0",
    "docs": "https://example.com/my-sidecar"
  }
}
```

`docker` is an image reference; `download` (an https URL) with `command` (a
single line) is the alternative; `docs` adds a setup link.

Searchable settings: a `select` field with `search: true` shows an explicit search
control in the installed plugin's Settings drawer. Ether forwards `q` and
`locale` to `GET /ether/options/{key}` in `request.query` (array values when hosted). Return an array of
`{value, label}` choices. The chosen string is stored verbatim; a JSON string can
carry coordinates and a display name together. Search never saves settings.
Background requests receive the server process locale (`LC_ALL`, `LC_MESSAGES`,
then `LANG`) in the `X-Ether-Locale` header when the caller has not supplied one.

### Owner-controlled resources and continuing jobs

`ether.runtime.limits` requests `memoryBytes`, `deadline` (a positive Go duration
such as `12h`), `unitBudget`, `fetchBytes`, `archiveMemberBytes`,
`archiveExpandedBytes` and `archiveMembers`. Ether grants each request up to the
owner's global or per-plugin ceiling. Owners can raise or lower ceilings under
Plugins → Settings and in each plugin's drawer. A per-plugin value overrides the
global value, including raising it. Zero inherits; global zero uses the default.
Saving cancels affected jobs and closes runtimes so lowered ceilings apply.

The default memory ceiling is **2 GiB**, adjustable up to wasm32's **4 GiB**;
omitted guest heap requests still use **32 MiB**, allocated only as needed.
The default call/job time ceiling is **12 hours** and background compute is
effectively unlimited (2^60 units per call); owners and manifests may lower it.
The default download and expanded-member ceilings are **8 GiB** each, total
archive expansion **32 GiB**, and archive members **65,535**. Downloads also obey
the library cache quota (2 GiB unless the manifest declares a different quota).
The host reports requested fields in the manifest and exact grants as
`runtimeLimits` in install preview and detail, displayed side by side.

Viewer request-path calls remain bounded: `/stream/`, `/catalog/`, `/meta/`,
`/subtitles/`, `/ether/open`, `/ether/metadata/`, `/ether/subtitles/`,
`/ether/markers`, and `/ether/v1/kinds/` (browse/search/resolve/match) get at most
10,000 units and 30 seconds, further shortened by each route/caller's deadline.
Ranking stays at its existing 2 ms per-candidate and 50 ms per-batch defaults.
Administrative setup, actions, tools, events and jobs use the owner-granted
resources; short health/configuration/view/options protocol deadlines still
protect the host's control plane. Remote plugin processes manage their own CPU
and memory; owner download/archive ceilings also apply to their host services.

An action returning `{job: "build-catalogue"}` starts an owner-triggered job.
Return `{continueInMs: 0, continuation: {next: 41}, progress: "Platform 41 of 92"}`
from its handler to continue. The next `JobRequest.continuation` is that opaque
JSON token. Return `{done: true, progress: "Ready"}` to finish, or
`{failed: "reason"}` to stop. A response without `continueInMs` also finishes.
Non-2xx responses, invalid results and call failures stop the job.

Ether allows 0–300,000 ms between calls with a minimum 100 ms pause. Total wall
time uses the owner time ceiling (12 hours by default), with a 1,000,000-call
sanity bound. Each call gets fresh per-call grants. At most two jobs run per
plugin and the same route cannot overlap. Job threads use OS background CPU/IO
priority on macOS, Linux and Windows. They pause before calls and host-service
operations during playback, Live TV viewing or high CPU/GPU load; pure compute
runs at reduced scheduling priority. Cancellation interrupts in-flight hosted
calls. Work starts only following an owner action; no eager background work.

Tokens are limited to 64 KiB and progress to 1,024 UTF-8 bytes. The drawer shows
progress and a Cancel button. The owner API provides `GET /api/plugins/{id}/jobs`
and `POST /api/plugins/{id}/jobs/cancel` with `{"path":"/ether/job/build-catalogue"}`.
Status includes total-time and attempt caps. Disabling, removing or reloading a
plugin, or stopping Ether, cancels work. The last sixteen route snapshots per
plugin are retained in memory. Store durable checkpoints in KV to recover on a
later owner-started run after restart; Ether does not restart work automatically.

Owner policy is persisted in `plugins.runtimeLimits` and
`plugins.runtimeOverrides[pluginId]`, with the same resource field names as the
manifest. `GET /api/plugins/runtime-limits` returns policy;
`PUT /api/plugins/runtime-limits` takes `{plugin?: string, limits: {...}}`.
Memory cannot exceed 4 GiB, job time has a one-year sanity bound, and member count
has a 1,000,000-entry sanity bound. The ZIP directory has a 64 MiB allocation
bound; traversal, encryption, special files and 1000:1 expansion checks remain.
