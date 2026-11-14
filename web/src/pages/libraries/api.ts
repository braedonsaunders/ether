import { apiFetch } from "../../lib/api";
import type { VisibilityRule } from "./browser/LibraryVisibility";

/** Owner API shapes for /libraries and /plugins-setup (spec §11). */

export interface Presentations {
  adapterOnly?: boolean;
  plex?: { base: string };
  mediaBrowser?: { collectionType: string; itemTypes?: Record<string, string> };
  stremio?: { type: string };
  xtream?: { category: string };
  silo?: { type: string };
}

export interface KindLevel {
  key: string;
  label: string;
  playable?: boolean;
  container?: boolean;
}
export interface KindField {
  key: string;
  type: string;
  label: string;
  indexed?: boolean;
  enum?: string[];
}
export interface MediaKind {
  key: string;
  label: string;
  library: { name: string; icon?: string };
  levels: KindLevel[];
  fields?: KindField[];
  art?: { role: string; label: string }[];
  present: Presentations;
  browse: { sorts?: string[]; facets?: string[]; search: boolean; searchFields?: string[] };
  sync: { mode: string; refresh?: string };
  cache?: { maxBytes?: number; ttl?: string };
  match?: { thresholdDefault?: number };
}

export interface LibrarySync {
  pluginId: string;
  kindKey: string;
  generation: number;
  lastStarted?: string;
  lastOk?: string;
  lastError?: string;
  items: number;
}

export interface CatalogueSource {
  logo?: string;
  lastError?: string;
  id: string;
  name: string;
  owner: string;
  group: "builtin" | "plugin" | "stremio";
  mediaTypes: string[];
  health: string;
  itemCount?: number;
  pluginId?: string;
  kindKey?: string;
  libraries: { id: number; name: string }[];
}

export interface Library {
  id: number;
  key: string;
  name: string;
  sources?: string[];
  source: "builtin" | "plugin" | "catalogue" | "collection";
  mediaType?: string;
  pluginId?: string;
  kindKey?: string;
  /** Collection-source libraries: the collection whose titles it shows. */
  collectionId?: number;
  rules?: VisibilityRule[];
  visibilityDefault?: string;
  present?: Presentations;
  /** Quality profile titles in this library rank with; "" = Default. */
  qualityProfile?: string;
  sort: number;
  hidden: boolean;
  createdAt: string;
  updatedAt: string;
  itemCount?: number;
  sync?: LibrarySync;
  kind?: MediaKind;
  pluginName?: string;
}

export interface KindItem {
  unavailable?: { code: string; detail?: string };
  ref: string;
  key: number;
  id: string;
  level: string;
  parentId?: string;
  index?: number;
  title: string;
  sortTitle?: string;
  summary?: string;
  year?: number;
  released?: number;
  rating?: number;
  playable?: boolean;
  childCount?: number;
  leafCount?: number;
  art?: { role: string; url: string; width?: number; height?: number }[];
  fields?: Record<string, unknown>;
  etag?: string;
}

export interface MatchRecord {
  itemRef: string;
  candidateRef: string;
  score: number;
  reasons?: string[];
  source: string;
  chosen: boolean;
  updatedAt: number;
}

export interface LibraryCache {
  libraryId: number;
  used: number;
  blobs: number;
  maxBytes: number;
  ttlSeconds: number;
  sharedUsed?: number;
  sharedBlobs?: number;
  sharedCap?: number;
}

export interface SetupStep {
  key: string;
  label: string;
  hint?: string;
  fields: string[];
  test: string;
  required: boolean;
  status: "pending" | "ok" | "failed" | "skipped";
  detail?: string;
}

export interface PluginSetup {
  pluginId: string;
  pluginName: string;
  steps: SetupStep[];
  blockers: string[];
  settings: Record<string, unknown>;
  schema: unknown;
  kinds: MediaKind[];
  libraries: Library[];
}

/** One collection the "From a collection" picker can bind a library to. */
export interface CollectionSource {
  id: number;
  name: string;
  mediaType: "movie" | "show" | "mixed";
  itemCount: number;
  builtIn?: boolean;
}

export interface PluginLibrarySource {
  pluginId: string;
  pluginName: string;
  summary?: string;
  kinds: MediaKind[];
  libraries: Library[];
}

export const librariesApi = {
  syncCatalogueSource: (id: string) =>
    apiFetch<{ ok: boolean }>(`/libraries/catalogue-sources/${encodeURIComponent(id)}/sync`, {
      method: "POST",
    }),
  catalogueSources: (signal?: AbortSignal) =>
    apiFetch<{ sources: CatalogueSource[] }>("/libraries/catalogue-sources", { signal }).then(
      (r) => r.sources,
    ),
  list: (signal?: AbortSignal) =>
    apiFetch<{ libraries: Library[] }>("/libraries", { signal }).then((r) => r.libraries),
  get: (id: number, signal?: AbortSignal) => apiFetch<Library>(`/libraries/${id}`, { signal }),
  create: (
    name: string,
    mediaType: string,
    rules: VisibilityRule[],
    visibilityDefault: string,
    present?: Presentations,
    sources?: string[],
  ) =>
    apiFetch<Library>("/libraries", {
      method: "POST",
      body: JSON.stringify({
        name,
        mediaType,
        rules,
        visibilityDefault,
        sources,
        ...(present ? { present } : {}),
      }),
    }),
  createCollection: (
    name: string,
    collectionId: number,
    mediaType: string,
    present?: Presentations,
    sources?: string[],
  ) =>
    apiFetch<Library>("/libraries", {
      method: "POST",
      body: JSON.stringify({
        name,
        collectionId,
        mediaType,
        sources,
        ...(present ? { present } : {}),
      }),
    }),
  listCollectionSources: (signal?: AbortSignal) =>
    apiFetch<{ collections: CollectionSource[] }>("/libraries/collection-sources", { signal }).then(
      (r) => r.collections ?? [],
    ),
  patch: (id: number, body: Record<string, unknown>) =>
    apiFetch<Library>(`/libraries/${id}`, { method: "PATCH", body: JSON.stringify(body) }),
  remove: (id: number) => apiFetch<void>(`/libraries/${id}`, { method: "DELETE" }),
  items: (id: number, params: Record<string, string>, signal?: AbortSignal) =>
    apiFetch<{ items: KindItem[]; total: number; nextCursor?: string }>(
      `/libraries/${id}/items?${new URLSearchParams(params)}`,
      { signal },
    ),
  facets: (id: number, field: string, level = "", signal?: AbortSignal) =>
    apiFetch<{ field: string; values: { value?: string; title?: string; count: number }[] }>(
      `/libraries/${id}/facets/${encodeURIComponent(field)}${level ? `?level=${encodeURIComponent(level)}` : ""}`,
      { signal },
    ),
  item: (id: number, ref: string, signal?: AbortSignal) =>
    apiFetch<{
      item: KindItem;
      children: KindItem[];
      matches: MatchRecord[];
      kind?: MediaKind;
      resolution?: Record<string, unknown>;
    }>(`/libraries/${id}/item/${encodeURIComponent(ref)}`, { signal }),
  sync: (id: number, fullWalk = false) =>
    apiFetch<{ upserted: number; deleted: number; skipped: number }>(`/libraries/${id}/sync`, {
      method: "POST",
      body: JSON.stringify({ fullWalk }),
    }),
  pinMatch: (id: number, ref: string, candidateRef: string) =>
    apiFetch<{ matches: MatchRecord[] }>(`/libraries/${id}/item/${encodeURIComponent(ref)}/match`, {
      method: "PUT",
      body: JSON.stringify({ candidateRef }),
    }),
  clearMatch: (id: number, ref: string) =>
    apiFetch<{ matches: MatchRecord[] }>(`/libraries/${id}/item/${encodeURIComponent(ref)}/match`, {
      method: "DELETE",
    }),
  rematch: (id: number, ref: string) =>
    apiFetch<{ matches: MatchRecord[] }>(`/libraries/${id}/item/${encodeURIComponent(ref)}/match`, {
      method: "POST",
    }),
  cache: (id: number, signal?: AbortSignal) =>
    apiFetch<LibraryCache>(`/libraries/${id}/cache`, { signal }),
  setCache: (id: number, maxBytes: number, ttlSeconds: number) =>
    apiFetch<LibraryCache>(`/libraries/${id}/cache`, {
      method: "PUT",
      body: JSON.stringify({ maxBytes, ttlSeconds }),
    }),
  sweepCache: (id: number) =>
    apiFetch<LibraryCache>(`/libraries/${id}/cache/sweep`, { method: "POST" }),
  purgeCache: (id: number) =>
    apiFetch<LibraryCache>(`/libraries/${id}/cache/purge`, { method: "POST" }),
  listPluginSources: (signal?: AbortSignal) =>
    apiFetch<{ plugins: PluginLibrarySource[] }>("/plugins-setup", { signal }).then(
      (r) => r.plugins ?? [],
    ),
  ensurePluginSetup: (plugin: string) =>
    apiFetch<PluginSetup>(`/plugins-setup/${encodeURIComponent(plugin)}/ensure`, {
      method: "POST",
    }),
  setup: (plugin: string, signal?: AbortSignal) =>
    apiFetch<PluginSetup>(`/plugins-setup/${encodeURIComponent(plugin)}`, { signal }),
  librarySetup: (id: number, signal?: AbortSignal) =>
    apiFetch<PluginSetup>(`/libraries/${id}/setup`, { signal }),
  saveSetupSettings: (plugin: string, values: Record<string, unknown>) =>
    apiFetch<{ fieldErrors: Record<string, string> | null }>(
      `/plugins-setup/${encodeURIComponent(plugin)}/settings`,
      { method: "PUT", body: JSON.stringify(values) },
    ),
  testSetupStep: (plugin: string, step: string) =>
    apiFetch<{ ok: boolean; detail?: string; fields?: Record<string, string> }>(
      `/plugins-setup/${encodeURIComponent(plugin)}/${encodeURIComponent(step)}/test`,
      { method: "POST" },
    ),
  skipSetupStep: (plugin: string, step: string) =>
    apiFetch<{ ok: boolean }>(
      `/plugins-setup/${encodeURIComponent(plugin)}/${encodeURIComponent(step)}/skip`,
      { method: "POST" },
    ),
};

/** Protocols a library is published to, for list-card badges. */
export function publishedTo(lib: Library): string[] {
  if (lib.hidden) return [];
  if (lib.source === "builtin") return ["plex", "mediaBrowser", "stremio", "xtream", "silo"];
  const p = lib.present ?? {};
  const out: string[] = [];
  if (p.adapterOnly) out.push("adapter");
  if (p.plex) out.push("plex");
  if (p.mediaBrowser) out.push("mediaBrowser");
  if (p.stremio) out.push("stremio");
  if (p.xtream) out.push("xtream");
  if (p.silo) out.push("silo");
  return out;
}

export function artUrl(item: KindItem, role = "poster"): string {
  const hit = item.art?.find((a) => a.role === role) ?? item.art?.[0];
  return hit && /^https?:\/\//.test(hit.url) ? hit.url : "";
}
