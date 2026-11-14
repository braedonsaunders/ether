/**
 * Host service clients for Ether plugins (SPEC §5.1.5, §5.2, §5.3).
 *
 * In hosted mode (QuickJS on wazero), calls go through the synchronous `ether`
 * and `__ether_host` globals provided by internal/plugins/wasm.
 * In sidecar mode (remote HTTP or tunnel), calls go over HTTP to
 * `/ether/plugin-host/*` authenticated with `X-Ether-Plugin-Credential`.
 */

import type { MaybePromise } from "./types.ts";
import type { ArchiveMember } from "./kit.ts";

export interface HostFetchRequest {
  method?: string;
  url: string;
  headers?: Record<string, string>;
  body?: string;
  bodyBase64?: string;
}

export interface HostFetchResponse {
  status: number;
  headers: Record<string, string>;
  body: string;
  bodyBase64?: string;
}

export interface DataRange { offset: number; length: number }
export interface FetchCacheRequest { url: string; headers?: Record<string, string>; libraryKey: string; name: string }
export interface FetchCacheResult { blob: string; size: number; crc32: string; md5: string; sha1: string; status: number }

function checkDataRange(r: DataRange): void {
  if (!Number.isSafeInteger(r.offset) || r.offset < 0 || r.offset > 256 * 1024 * 1024 || !Number.isSafeInteger(r.length) || r.length <= 0 || r.length > 1024 * 1024) throw new Error("Data range needs nonnegative offset and length 1..1048576");
}
function encodeData(data: Uint8Array): string {
  const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";
  const parts: string[] = [];
  for (let i = 0; i < data.length; i += 3) {
    const n = (data[i]! << 16) | ((data[i + 1] ?? 0) << 8) | (data[i + 2] ?? 0);
    parts.push(alphabet[(n >>> 18) & 63]! + alphabet[(n >>> 12) & 63]! + (i + 1 < data.length ? alphabet[(n >>> 6) & 63] : "=") + (i + 2 < data.length ? alphabet[n & 63] : "="));
  }
  return parts.join("");
}
function encodeText(text: string): Uint8Array {
  const encoded = encodeURIComponent(text); const bytes: number[] = [];
  for (let i = 0; i < encoded.length; i++) {
    if (encoded[i] === "%") { bytes.push(parseInt(encoded.slice(i + 1, i + 3), 16)); i += 2; }
    else bytes.push(encoded.charCodeAt(i));
  }
  return new Uint8Array(bytes);
}
function decodeData(r: { bodyBase64: string }): Uint8Array {
  const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";
  const text = r.bodyBase64;
  const out = new Uint8Array(Math.floor(text.length * 3 / 4) - (text.endsWith("==") ? 2 : text.endsWith("=") ? 1 : 0));
  let acc = 0, bits = 0, pos = 0;
  for (const c of text) {
    if (c === "=") break;
    const n = alphabet.indexOf(c); if (n < 0) throw new Error("Invalid base64 host result");
    acc = (acc << 6) | n; bits += 6;
    if (bits >= 8) { bits -= 8; out[pos++] = (acc >> bits) & 255; }
  }
  return out;
}

export interface FolderHashJob { id: string; state: "running" | "done" | "canceled" | "error"; bytes: number; result?: FolderHash; error?: string }
export interface FolderHash { size: number; modTime: number; crc32: string; md5: string; sha1: string }

export interface FolderEntry {
  name: string;
  path: string;
  dir: boolean;
  size?: number;
  mtime?: number;
}

export interface CatalogTitleSummary {
  id: number | string;
  title: string;
  type: string;
  year?: number;
  imdbId?: string;
  tmdbId?: number;
  genres?: string[];
  posterPath?: string;
  summary?: string;
}

export interface ViewerWatchEntry {
  titleId: string;
  title?: string;
  watchedAt?: string;
  type?: string;
  progress?: number;
  completed?: boolean;
}

export interface EtherHost {
  settings?: { get(): MaybePromise<Record<string, unknown>> };
  kv: {
    get(key: string): MaybePromise<string | null>;
    put(key: string, value: string): MaybePromise<void>;
    delete(key: string): MaybePromise<void>;
    list(prefix?: string): MaybePromise<string[]>;
  };
  secret: {
    get(name: string): MaybePromise<string>;
  };
  http: {
    fetch(req: HostFetchRequest): MaybePromise<HostFetchResponse>;
    fetchToCache?(req: FetchCacheRequest): MaybePromise<FetchCacheResult>;
  };
  catalog: {
    search(
      query: string,
      limit?: number,
      offset?: number,
    ): MaybePromise<{ titles: CatalogTitleSummary[] }>;
    get(id: string): MaybePromise<CatalogTitleSummary | null>;
  };
  viewers: {
    granted(): MaybePromise<Array<{ id: number; name: string }>>;
    history(
      viewerId: number,
      limit?: number,
      offset?: number,
    ): MaybePromise<ViewerWatchEntry[]>;
    setWatchState(
      viewerId: number,
      titleId: string,
      watched: boolean,
    ): MaybePromise<void>;
  };
  collections: {
    upsert(collection: Record<string, unknown>): MaybePromise<void>;
  };
  folders: {
    list(root: string, path?: string): MaybePromise<FolderEntry[]>;
    stat(root: string, path: string): MaybePromise<FolderEntry>;
    hash(root: string, path: string): MaybePromise<FolderHash>;
    hashStart(root: string, path: string): MaybePromise<FolderHashJob>;
    hashPoll(id: string): MaybePromise<FolderHashJob>;
    hashCancel(id: string): MaybePromise<FolderHashJob>;
  };
  notify: {
    owner(text: string): MaybePromise<void>;
  };
  /** Content-kind host services (remote + mock clients provide them). */
  kinds?: {
    upsert(kind: string, items: import("./types.ts").KindItem[]): MaybePromise<number>;
    delete(kind: string, externalIds: string[]): MaybePromise<void>;
    items(kind: string, cursor?: string, limit?: number): MaybePromise<import("./types.ts").KindPage>;
    resolveSources(q: import("./types.ts").SourceQuery): MaybePromise<import("./types.ts").SourceCandidate[]>;
  };
  cache?: {
    put(libraryKey: string, name: string, data: Uint8Array): MaybePromise<string>;
    open(blobRef: string): MaybePromise<Uint8Array>;
    read?(blobRef: string, range: DataRange): MaybePromise<Uint8Array>;
  };
  archive?: {
    list(blobRef: string): MaybePromise<ArchiveMember[]>;
    open(blobRef: string, member: number, range: DataRange): MaybePromise<Uint8Array>;
  };
  /**
   * Frontend-adapter host services (spec §6.5). streamUrl mints a scoped,
   * short-lived Ether stream URL for one of this plugin's own kind items;
   * bytes are then served by Ether, never by the plugin.
   */
  frontend?: {
    streamUrl(viewerId: number, itemRef: string): MaybePromise<string>;
  };
  log(level: string, message: string, attrs?: Record<string, unknown>): MaybePromise<void>;
  abort(message?: string): never;
}

interface GlobalEtherBindings {
  __ether_host?: (name: string, argsJson: string) => string;
  ether?: {
    log: (...args: unknown[]) => void;
    abort: (msg?: string) => never;
    kv: {
      get: (k: string) => string | null;
      put: (k: string, v: string) => void;
      delete: (k: string) => void;
      list: (prefix?: string) => string[];
    };
    secret: {
      get: (name: string) => string;
    };
    http: {
      fetch: (req: HostFetchRequest) => HostFetchResponse;
    };
    catalog: {
      search: (q: string, limit?: number, offset?: number) => { titles: CatalogTitleSummary[] };
      get: (id: string) => CatalogTitleSummary | null;
    };
    viewers: {
      granted: () => Array<{ id: number; name: string }>;
      history: (viewerId: number, limit?: number, offset?: number) => ViewerWatchEntry[];
      setWatchState: (
        viewerIdOrObj: number | { viewerId: number; titleId: string; watched: boolean },
        titleId?: string,
        watched?: boolean,
      ) => void;
    };
    collections: {
      upsert: (c: Record<string, unknown>) => void;
    };
    folders: {
      list: (root: string, path?: string) => FolderEntry[];
      stat: (root: string, path: string) => FolderEntry;
      hash: (root: string, path: string) => FolderHash;
      hashStart: (root: string, path: string) => FolderHashJob;
      hashPoll: (id: string) => FolderHashJob;
      hashCancel: (id: string) => FolderHashJob;
    };
    notify: {
      owner: (text: string) => void;
    };
    frontend: {
      streamURL: (itemRef: string) => { url: string };
    };
  };
}

/**
 * Returns true when running inside the Ether QuickJS hosted runtime.
 */
export function isHostedRuntime(): boolean {
  const g = globalThis as unknown as GlobalEtherBindings;
  return typeof g.__ether_host === "function" || typeof g.ether === "object";
}

/**
 * Creates a host client bound to the QuickJS hosted runtime globals (`globalThis.ether` / `__ether_host`).
 */
export function createHostedClient(): EtherHost {
  const g = globalThis as unknown as GlobalEtherBindings;

  const callHost = <T>(name: string, args: unknown[]): T => {
    if (typeof g.__ether_host === "function") {
      const raw = g.__ether_host(name, JSON.stringify(args));
      return (raw ? JSON.parse(raw) : null) as T;
    }
    throw new Error(`Host service ${name} is not available outside the Ether runtime`);
  };

  return {
    archive: {
      list: (blob) => callHost<ArchiveMember[]>("archive.list", [blob]),
      open: (blob, member, range) => { checkDataRange(range); return decodeData(callHost("archive.open", [blob, member, range])); },
    },
    cache: {
      put: (lib, name, data) => callHost<{blob: string}>("cache.put", [lib, name, {bodyBase64: encodeData(data)}]).blob,
      open: (blob) => { const r = callHost<{body?: string; bodyBase64?: string}>("cache.open", [blob]); return r.bodyBase64 ? decodeData({bodyBase64: r.bodyBase64}) : encodeText(r.body ?? ""); },
      read: (blob, range) => { checkDataRange(range); return decodeData(callHost("cache.read", [blob, range])); },
    },
    settings: { get: () => callHost<Record<string, unknown>>("settings.get", []) },
    kv: {
      get(key: string): string | null {
        if (g.ether?.kv) return g.ether.kv.get(key) ?? null;
        return callHost<string | null>("kv.get", [key]);
      },
      put(key: string, value: string): void {
        if (g.ether?.kv) {
          g.ether.kv.put(key, String(value));
          return;
        }
        callHost("kv.put", [key, String(value)]);
      },
      delete(key: string): void {
        if (g.ether?.kv) {
          g.ether.kv.delete(key);
          return;
        }
        callHost("kv.delete", [key]);
      },
      list(prefix = ""): string[] {
        if (g.ether?.kv) return g.ether.kv.list(prefix) ?? [];
        return callHost<string[]>("kv.list", [prefix]) ?? [];
      },
    },
    secret: {
      get(name: string): string {
        if (g.ether?.secret) return g.ether.secret.get(name);
        return callHost<string>("secret.get", [name]);
      },
    },
    http: {
      fetchToCache: (req) => callHost<FetchCacheResult>("http.fetchToCache", [req]),
      fetch(req: HostFetchRequest): HostFetchResponse {
        const normalized = {
          method: req.method ?? "GET",
          url: req.url,
          headers: req.headers ?? {},
          ...(req.bodyBase64 !== undefined ? { bodyBase64: req.bodyBase64 } : { body: req.body ?? "" }),
        };
        if (g.ether?.http) return g.ether.http.fetch(normalized);
        return callHost<HostFetchResponse>("http.fetch", [normalized]);
      },
    },
    catalog: {
      search(query: string, limit = 50, offset = 0): { titles: CatalogTitleSummary[] } {
        if (typeof g.__ether_host === "function") {
          return callHost<{ titles: CatalogTitleSummary[] }>("catalog.search", [
            query,
            limit,
            offset,
          ]);
        }
        if (g.ether?.catalog) return g.ether.catalog.search(query, limit, offset);
        return { titles: [] };
      },
      get(id: string): CatalogTitleSummary | null {
        if (g.ether?.catalog) return g.ether.catalog.get(id);
        return callHost<CatalogTitleSummary | null>("catalog.get", [id]);
      },
    },
    viewers: {
      granted() { return callHost<Array<{ id: number; name: string }>>("viewers.granted", []) ?? []; },
      history(viewerId: number, limit = 50, offset = 0): ViewerWatchEntry[] {
        if (typeof g.__ether_host === "function") {
          return callHost<ViewerWatchEntry[]>("viewers.history", [viewerId, limit, offset]) ?? [];
        }
        if (g.ether?.viewers) return g.ether.viewers.history(viewerId, limit, offset) ?? [];
        return [];
      },
      setWatchState(viewerId: number, titleId: string, watched: boolean): void {
        if (typeof g.__ether_host === "function") {
          callHost("viewers.setWatchState", [viewerId, titleId, watched]);
          return;
        }
        if (g.ether?.viewers) {
          g.ether.viewers.setWatchState(viewerId, titleId, watched);
          return;
        }
        callHost("viewers.setWatchState", [viewerId, titleId, watched]);
      },
    },
    collections: {
      upsert(collection: Record<string, unknown>): void {
        if (g.ether?.collections) {
          g.ether.collections.upsert(collection);
          return;
        }
        callHost("collections.upsert", [collection]);
      },
    },
    folders: {
      list(root: string, path = ""): FolderEntry[] {
        if (g.ether?.folders) return g.ether.folders.list(root, path) ?? [];
        return callHost<FolderEntry[]>("folders.list", [root, path]) ?? [];
      },
      hashStart(root: string, path: string): FolderHashJob { if (g.ether?.folders) return g.ether.folders.hashStart(root, path); return callHost<FolderHashJob>("folders.hashStart", [root, path]); },
      hashPoll(id: string): FolderHashJob { if (g.ether?.folders) return g.ether.folders.hashPoll(id); return callHost<FolderHashJob>("folders.hashPoll", [id]); },
      hashCancel(id: string): FolderHashJob { if (g.ether?.folders) return g.ether.folders.hashCancel(id); return callHost<FolderHashJob>("folders.hashCancel", [id]); },
      hash(root: string, path: string): FolderHash {
        if (g.ether?.folders) return g.ether.folders.hash(root, path);
        return callHost<FolderHash>("folders.hash", [root, path]);
      },
      stat(root: string, path: string): FolderEntry {
        if (g.ether?.folders) return g.ether.folders.stat(root, path);
        return callHost<FolderEntry>("folders.stat", [root, path]);
      },
    },
    notify: {
      owner(text: string): void {
        if (g.ether?.notify) {
          g.ether.notify.owner(text);
          return;
        }
        callHost("notify.owner", [text]);
      },
    },
    frontend: {
      streamUrl(viewerId: number, itemRef: string): string {
        // The hosted binding is viewer-scoped by Ether already; the viewerId
        // argument is accepted for parity with the remote client.
        void viewerId;
        const r = callHost<{ url: string }>("frontend.streamURL", [itemRef]);
        return r?.url ?? "";
      },
    },
    log(level: string, message: string, attrs?: Record<string, unknown>): void {
      if (typeof g.__ether_host === "function") {
        try {
          g.__ether_host(
            "log",
            JSON.stringify(attrs ? [level, message, attrs] : [level, message]),
          );
        } catch {
          // Logging never fails the caller.
        }
        return;
      }
      if (g.ether?.log) {
        g.ether.log(level, message, attrs ? JSON.stringify(attrs) : "");
      }
    },
    abort(message = "aborted"): never {
      if (g.ether?.abort) {
        return g.ether.abort(message);
      }
      throw new Error(
        "__ETHER_FAULT__" + JSON.stringify({ code: "aborted", message: String(message) }),
      );
    },
  };
}

export const createHostedHostClient = createHostedClient;
// Choose the host transport for the runtime that loaded this plugin.
export const ether: EtherHost = isHostedRuntime()
  ? createHostedClient()
  : createRemoteHostClient();

export interface RemoteHostOptions {
  /** Base URL of the Ether server (for example "http://127.0.0.1:32450"). */
  baseUrl?: string;
  /** Plugin credential issued during pairing (`X-Ether-Plugin-Credential`). */
  credential?: string;
  /** Optional custom fetch implementation for testing. */
  fetchImpl?: typeof fetch;
}

function readEnv(name: string): string | undefined {
  const g = globalThis as { process?: { env?: Record<string, string | undefined> } };
  return g.process?.env?.[name];
}

/**
 * Creates a host client for remote/sidecar plugins that calls `/ether/plugin-host/*`
 * on Ether with `X-Ether-Plugin-Credential` (SPEC §5.1.5, WS8b).
 */
export function createRemoteHostClient(opts: RemoteHostOptions = {}): EtherHost {
  const rawBase = opts.baseUrl ?? readEnv("ETHER_HOST_URL") ?? "http://127.0.0.1:32450";
  const base = rawBase.replace(/\/+$/, "");
  const credential = opts.credential ?? readEnv("ETHER_PLUGIN_CREDENTIAL") ?? "";
  const doFetch = opts.fetchImpl ?? fetch;

  const callJson = async <T>(
    method: string,
    path: string,
    body?: unknown,
    allow404 = false,
  ): Promise<T | null> => {
    const headers: Record<string, string> = {
      "X-Ether-Plugin-Credential": credential,
      Accept: "application/json",
    };
    let payload: string | undefined;
    if (body !== undefined) {
      headers["Content-Type"] = "application/json";
      payload = JSON.stringify(body);
    }
    const resp = await doFetch(`${base}${path}`, {
      method,
      headers,
      body: payload,
    });
    if (allow404 && resp.status === 404) {
      return null;
    }
    const text = await resp.text();
    const parsed = text ? (JSON.parse(text) as Record<string, unknown>) : null;
    if (!resp.ok) {
      const errObj =
        parsed && typeof parsed.error === "object" ? (parsed.error as Record<string, string>) : null;
      throw new Error(
        errObj?.message ?? `Host call ${method} ${path} failed with HTTP ${resp.status}`,
      );
    }
    return parsed as T;
  };

  return {
    archive: {
      list: async (blob) => (await callJson<ArchiveMember[]>("POST", "/ether/plugin-host/data/archive.list", [blob])) ?? [],
      open: async (blob, member, range) => { checkDataRange(range); return decodeData((await callJson<{bodyBase64: string}>("POST", "/ether/plugin-host/data/archive.open", [blob, member, range]))!); },
    },
    ...(credential ? { settings: { get: async () => (await callJson<{ settings: Record<string, unknown> }>("GET", "/ether/plugin-host/settings"))?.settings ?? {} } } : {}),
    kv: {
      async get(key: string): Promise<string | null> {
        const res = await callJson<{ key: string; value: string }>(
          "GET",
          `/ether/plugin-host/kv/${encodeURIComponent(key)}`,
          undefined,
          true,
        );
        return res ? res.value : null;
      },
      async put(key: string, value: string): Promise<void> {
        await callJson("PUT", `/ether/plugin-host/kv/${encodeURIComponent(key)}`, {
          value: String(value),
        });
      },
      async delete(key: string): Promise<void> {
        await callJson("DELETE", `/ether/plugin-host/kv/${encodeURIComponent(key)}`);
      },
      async list(prefix = ""): Promise<string[]> {
        const q = prefix ? `?prefix=${encodeURIComponent(prefix)}` : "";
        const res = await callJson<{ keys: string[] }>("GET", `/ether/plugin-host/kv${q}`);
        return res?.keys ?? [];
      },
    },
    secret: {
      get(_name: string): Promise<string> {
        return Promise.reject(
          new Error(
            "secret.get is only available in hosted plugins; remote plugins receive secrets via /ether/configure",
          ),
        );
      },
    },
    http: {
      fetchToCache: async (req) => (await callJson<FetchCacheResult>("POST", "/ether/plugin-host/data/http.fetchToCache", [req]))!,
      async fetch(req: HostFetchRequest): Promise<HostFetchResponse> {
        const resp = await doFetch(req.url, {
          method: req.method ?? "GET",
          headers: req.headers,
          body: req.body,
        });
        const outHeaders: Record<string, string> = {};
        resp.headers.forEach((v, k) => {
          outHeaders[k] = v;
        });
        return {
          status: resp.status,
          headers: outHeaders,
          body: await resp.text(),
        };
      },
    },
    catalog: {
      async search(
        query: string,
        limit = 50,
        offset = 0,
      ): Promise<{ titles: CatalogTitleSummary[] }> {
        const params = new URLSearchParams({
          q: query,
          limit: String(limit),
          offset: String(offset),
        });
        const res = await callJson<{ titles: CatalogTitleSummary[] }>(
          "GET",
          `/ether/plugin-host/catalog/search?${params.toString()}`,
        );
        return res ?? { titles: [] };
      },
      async get(id: string): Promise<CatalogTitleSummary | null> {
        return callJson<CatalogTitleSummary>(
          "GET",
          `/ether/plugin-host/catalog/${encodeURIComponent(id)}`,
          undefined,
          true,
        );
      },
    },
    viewers: {
      async granted() { return (await callJson<Array<{ id: number; name: string }>>("GET", "/ether/plugin-host/viewers/granted")) ?? []; },
      async history(viewerId: number, limit = 50, offset = 0): Promise<ViewerWatchEntry[]> {
        const params = new URLSearchParams({
          limit: String(limit),
          offset: String(offset),
        });
        const res = await callJson<ViewerWatchEntry[]>(
          "GET",
          `/ether/plugin-host/viewers/${viewerId}/history?${params.toString()}`,
        );
        return res ?? [];
      },
      async setWatchState(viewerId: number, titleId: string, watched: boolean): Promise<void> {
        await callJson("POST", "/ether/plugin-host/viewers/watch-state", {
          viewerId,
          titleId,
          watched,
        });
      },
    },
    collections: {
      async upsert(collection: Record<string, unknown>): Promise<void> {
        await callJson("POST", "/ether/plugin-host/collections/upsert", collection);
      },
    },
    folders: {
      list(): Promise<FolderEntry[]> {
        return Promise.reject(
          new Error(
            "folders.list is for hosted plugins; sidecar plugins read their local fileRoot directly",
          ),
        );
      },
      hashStart(): Promise<FolderHashJob> { return Promise.reject(new Error("folders.hash requires the hosted runtime")); },
      hashPoll(): Promise<FolderHashJob> { return Promise.reject(new Error("folders.hash requires the hosted runtime")); },
      hashCancel(): Promise<FolderHashJob> { return Promise.reject(new Error("folders.hash requires the hosted runtime")); },
      hash(): Promise<FolderHash> { return Promise.reject(new Error("folders.hash requires the hosted runtime")); },
      stat(): Promise<FolderEntry> {
        return Promise.reject(
          new Error(
            "folders.stat is for hosted plugins; sidecar plugins read their local fileRoot directly",
          ),
        );
      },
    },
    notify: {
      async owner(text: string): Promise<void> {
        await callJson("POST", "/ether/plugin-host/notify", { text });
      },
    },
    kinds: {
      async upsert(kind, items) {
        const r = await callJson<{ accepted: number }>("POST", `/ether/plugin-host/kinds/${encodeURIComponent(kind)}/upsert`, { items });
        return r?.accepted ?? 0;
      },
      async delete(kind, externalIds) {
        await callJson("POST", `/ether/plugin-host/kinds/${encodeURIComponent(kind)}/delete`, { externalIds });
      },
      async items(kind, cursor = "", limit = 200) {
        return (await callJson("GET", `/ether/plugin-host/kinds/${encodeURIComponent(kind)}/items?cursor=${encodeURIComponent(cursor)}&limit=${limit}`)) ?? { items: [] };
      },
      async resolveSources(q) {
        const r = await callJson<{ candidates: unknown[] }>("POST", "/ether/plugin-host/sources/resolve", q);
        return (r?.candidates ?? []) as never[];
      },
    },
    cache: {
      read: async (blob, range) => { checkDataRange(range); return decodeData((await callJson<{bodyBase64: string}>("POST", "/ether/plugin-host/data/cache.read", [blob, range]))!); },
      async put(libraryKey, name, data) {
        const resp = await doFetch(`${base}/ether/plugin-host/cache/${encodeURIComponent(libraryKey)}?name=${encodeURIComponent(name)}`, {
          method: "POST",
          headers: credential ? { "X-Ether-Plugin-Credential": credential } : {},
          body: data as BodyInit,
        });
        if (!resp.ok) throw new Error(`plugin-host cache put: status ${resp.status}`);
        return ((await resp.json()) as { blob?: string }).blob ?? "";
      },
      async open(blobRef) {
        const resp = await doFetch(`${base}/ether/plugin-host/cache/${blobRef.replace(/^\//, "")}`, {
          headers: credential ? { "X-Ether-Plugin-Credential": credential } : {},
        });
        if (!resp.ok) throw new Error(`plugin-host cache open: status ${resp.status}`);
        return new Uint8Array(await resp.arrayBuffer());
      },
    },
    frontend: {
      async streamUrl(viewerId: number, itemRef: string): Promise<string> {
        const r = await callJson<{ url: string }>("POST", "/ether/plugin-host/frontend/stream-url", {
          viewerId,
          itemRef,
        });
        return r?.url ?? "";
      },
    },
    async log(level: string, message: string, attrs?: Record<string, unknown>): Promise<void> {
      await callJson("POST", "/ether/plugin-host/log", { level, message, attrs });
    },
    abort(message = "aborted"): never {
      throw new Error(message);
    },
  };
}
