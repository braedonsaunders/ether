import { Bell, Blocks, FolderOpen, Globe, Network, Plug, Puzzle, Tv } from "lucide-react";
import { createElement, type ReactNode } from "react";
import { getLocale, t } from "../../i18n";
import type { Viewer } from "../../lib/contract";
import { textOf, type Descriptor } from "../../lib/extend";
import type { HealthLevel } from "../../lib/health";
import { humanizeKey, isPlainObject } from "../../lib/format";
import type { FieldDef, FieldGroup } from "../settings/SchemaForm";

export interface PluginSummary {
  id: string;
  name: string;
  version: string;
  kind?: string;
  summary?: string;
  icon?: string;
  enabled: boolean;
  verified: boolean;
  shadow: boolean;
  autoUpdate: boolean;
  updateVersion?: string;
  prevVersion?: string;
  health: string;
  /** The plain-language reason behind a non-ok health. */
  healthReason?: string;
  /** "waiting" while a plugin that runs as its own program has no sidecar connected. */
  healthSidecar?: string;
  capabilities?: string[];
  sourceKind?: string;
  stremioOnly?: boolean;
  installedAt: number;
  /** A Stremio add-on's roles and which are on; never its link. */
  roles?: { role: string; on: boolean }[] | null;
  host?: string;
}

export interface PluginHealth {
  status: string;
  reason?: string;
  restarts: number;
  slow: number;
  drops: number;
  lastError?: string;
  lastOk?: string;
  nextRetry?: string;
  observed?: Record<string, number>;
  egressBytes: number;
  /** Resolves since its last complete ranking that left its ranking step out for running out of time. */
  rankingSkipped?: number;
  /** "waiting" while a plugin that runs as its own program has no sidecar connected. */
  sidecar?: string;
  rankingSkippedAt?: string;
}

export interface PluginGrants {
  viewers?: number[];
  folders?: string[];
  revoked?: string[];
  lan?: boolean;
}

export interface DeclaredPermissions {
  network?: string[];
  catalog?: string;
  viewers?: {
    identity?: string;
    watch_history?: string;
    write_watch_state?: string;
  };
  collections?: string;
  folders?: string;
  secrets?: string[];
}

export interface PluginManifest {
  id: string;
  version: string;
  name: string;
  description?: string;
  logo?: string;
  /** Stremio add-on manifests declare resources and catalogues directly. */
  resources?: (string | { name: string })[];
  catalogs?: { id: string; type: string; name?: string }[];
  ether?: {
    protocol?: number;
    kind?: string;
    capabilities?: string[];
    summary?: string;
    author?: { name?: string; url?: string };
    license?: string;
    repo?: string;
    icon?: string;
    runtime?: { kind?: string; entry?: string; module?: string; limits?: Partial<RuntimeLimits> };
    /** How to run a remote plugin's program (sidecars). */
    sidecar?: {
      docker?: string;
      download?: string;
      command?: string;
      docs?: string;
    };
    permissions?: DeclaredPermissions;
    contributes?: {
      mediaKinds?: { key: string; label: string; library: { name: string } }[];
      setup?: { steps: { key: string; label: string; required: boolean }[] };
      events?: { subscribe?: string[] };
      pages?: { key: string; label: string; route: string }[];
      slots?: { slot: string; key: string; label: string; route: string }[];
    };
  };
}

export interface RuntimeLimits {
  fetchBytes?: number;
  archiveMemberBytes?: number;
  archiveExpandedBytes?: number;
  archiveMembers?: number;
  memoryBytes: number;
  deadline: string;
  unitBudget: number;
}

export interface PluginDetail extends PluginSummary {
  runtimeLimits?: RuntimeLimits;
  manifest: PluginManifest;
  permissions: {
    lines: string[];
    declared: DeclaredPermissions;
    grants: PluginGrants;
    needsViewers: boolean;
    needsFolders: boolean;
  };
  healthDetail: PluginHealth;
  privacy: {
    declaredHosts: string[];
    observedHosts: Record<string, number>;
    undeclared?: string[];
  };
  impact: {
    calls: number;
    errors: number;
    p50Ms: number;
    p95Ms: number;
    egressBytes: number;
    kvBytes: number;
    slow: number;
    eventDrops: number;
    restarts: number;
  };
  addedPermissions?: string[];
  changelog?: string;
}

export interface InstallPreview {
  runtimeLimits?: RuntimeLimits;
  manifest: PluginManifest;
  /** Manifest summary with any "$t:" reference resolved by the server. */
  summary?: string;
  permissions: string[];
  needsViewers: boolean;
  needsFolders: boolean;
  verified: boolean;
  stremioOnly: boolean;
  alreadyInstalled?: boolean;
  installedVersion?: string;
  addedPermissions?: string[];
}

export interface JournalEntry {
  id: number;
  plugin: string;
  op: string;
  detail?: string;
  shadow: boolean;
  undone: boolean;
  created: string;
}

export interface CatalogueEntry {
  id: string;
  name: string;
  version: string;
  kind: string;
  category?: string;
  summary: string;
  description?: string;
  author?: { name?: string; url?: string };
  repo?: string;
  icon?: string;
  verified: boolean;
  capabilities?: string[];
  permissions: string[];
  needsViewers?: boolean;
  needsFolders?: boolean;
  screenshots?: string[];
  limits?: string[];
  url: string;
  manifest?: PluginManifest;
}

/** A plugin that answers as a Stremio add-on, edited in Studio or not. */
export function isAddon(p: { stremioOnly?: boolean; host?: string }): boolean {
  return Boolean(p.stremioOnly || p.host);
}

export function kindLabel(kind?: string, addon?: boolean): string {
  if (addon) return t("Stremio");
  switch (kind) {
    case "source":
      return t("Source");
    case "integration":
      return t("Integration");
    case "notification_channel":
      return t("Notification");
    case "livetv_source":
      return t("Live TV");
    case "extension":
      return t("Extension");
    default:
      return t("Plugin");
  }
}

export function categoryLabel(category?: string): string {
  switch (category) {
    case "live_tv":
      return t("Live TV");
    case "subtitles":
      return t("Subtitles");
    case "sources":
      return t("Sources");
    case "automation":
      return t("Automation");
    case "lists":
      return t("Lists and shelves");
    case "ranking":
      return t("Release ranking");
    default:
      return category ? humanizeKey(category) : t("General");
  }
}

export function kindIcon(kind?: string, size = 18): ReactNode {
  switch (kind) {
    case "source":
      return createElement(Network, { size });
    case "integration":
      return createElement(Plug, { size });
    case "notification_channel":
      return createElement(Bell, { size });
    case "livetv_source":
      return createElement(Tv, { size });
    case "extension":
      return createElement(Blocks, { size });
    default:
      return createElement(Puzzle, { size });
  }
}

export function pluginHealthLevel(health: string, enabled = true): HealthLevel {
  if (!enabled || health === "disabled") return "off";
  switch (health) {
    case "ok":
      return "ok";
    case "attention":
    case "warn":
      return "warn";
    case "error":
      return "error";
    default:
      return "unknown";
  }
}

export function isPrivateUrl(raw: string): boolean {
  try {
    const u = new URL(raw.trim());
    const h = u.hostname.toLowerCase();
    return (
      h === "localhost" ||
      h === "127.0.0.1" ||
      h.endsWith(".local") ||
      h.endsWith(".lan") ||
      /^10\./.test(h) ||
      /^192\.168\./.test(h) ||
      /^172\.(1[6-9]|2\d|3[01])\./.test(h)
    );
  } catch {
    return false;
  }
}

export interface GrantItem {
  key: string;
  label: string;
  revocable: boolean;
}

/**
 * Maps a plugin's declared permissions and capabilities to revocable grant rows
 * in plain words (SPEC §5.4).
 */
export function buildGrantItems(detail: PluginDetail): GrantItem[] {
  const d = detail.permissions.declared ?? {};
  const items: GrantItem[] = [];

  if (d.network && d.network.length > 0) {
    items.push({
      key: "network",
      label:
        d.network.length === 1
          ? t("Connects to {host}", { host: d.network[0]! })
          : t("Connects to {hosts}", { hosts: d.network.join(", ") }),
      revocable: true,
    });
  }
  if (d.catalog === "read") {
    items.push({
      key: "catalog",
      label: t("Reads your catalogue"),
      revocable: true,
    });
  }
  if (d.viewers?.identity && d.viewers.identity !== "none") {
    items.push({
      key: "viewers.identity",
      label: t("Sees who is watching (viewers you choose)"),
      revocable: true,
    });
  }
  if (d.viewers?.watch_history && d.viewers.watch_history !== "none") {
    items.push({
      key: "viewers.watch_history",
      label: t("Reads watch history"),
      revocable: true,
    });
  }
  if (d.viewers?.write_watch_state && d.viewers.write_watch_state !== "none") {
    items.push({
      key: "viewers.write_watch_state",
      label: t("Marks titles watched"),
      revocable: true,
    });
  }
  if (d.collections === "write") {
    items.push({
      key: "collections",
      label: t("Creates and updates collections"),
      revocable: true,
    });
  }
  if (d.folders === "read") {
    items.push({
      key: "folders",
      label: t("Reads folders you choose"),
      revocable: true,
    });
  }
  if (d.secrets && d.secrets.length > 0) {
    items.push({
      key: "secrets",
      label: t("Stores an access token"),
      revocable: true,
    });
  }
  const hasEvents =
    detail.capabilities?.includes("events") ||
    Boolean(detail.manifest?.ether?.contributes?.events?.subscribe?.length);
  if (hasEvents) {
    items.push({
      key: "events",
      label: t("Gets play and new-release events"),
      revocable: true,
    });
  }

  if (items.length === 0) {
    const fallbackLines =
      detail.permissions.lines.length > 0
        ? detail.permissions.lines
        : [t("Needs no access to your server or the network")];
    for (const [idx, line] of fallbackLines.entries()) {
      items.push({
        key: `line-${idx}`,
        label: line,
        revocable: false,
      });
    }
  }
  return items;
}

export interface PlacementLink {
  kind: string;
  id: string;
  label: string;
  area: string;
  href: string;
}

/**
 * Turns registry descriptors owned by a plugin into deep links showing where
 * the plugin appears across the console.
 */
export function buildPlacements(descriptors: Descriptor[], pluginId: string): PlacementLink[] {
  const owned = descriptors.filter((d) => d.owner === pluginId);
  const out: PlacementLink[] = [];
  const seen = new Set<string>();

  for (const d of owned) {
    if (d.kind === "navItem") continue;
    const rawKey = d.id.includes("/") ? (d.id.split("/").pop() ?? d.id) : d.id;
    const label = textOf(d.label) || rawKey;
    let kind: string = d.kind;
    let area = t("Console");
    let href = "/plugins";

    switch (d.kind) {
      case "page":
        area = t("Plugin page");
        href = `/p/${encodeURIComponent(pluginId)}/${encodeURIComponent(rawKey)}`;
        break;
      case "collectionSource":
        area = t("Collections");
        href = "/collections/new";
        break;
      case "rowSource":
        area = t("Home rows");
        href = "/home";
        break;
      case "liveTVSource":
        area = t("Live TV");
        href = "/livetv";
        break;
      case "rankingStep":
        area = t("Release ranking");
        href = "/releases/profiles";
        break;
      case "ruleField":
        area = t("Release rules");
        href = "/releases/profiles";
        break;
      case "channelKind":
      case "eventKind":
        area = t("Notifications");
        href = "/notifications";
        break;
      case "subtitleProvider":
      case "markerSource":
        area = t("Playback");
        href = "/playback";
        break;
      case "sourceKind":
        area = t("Sources");
        href = "/sources";
        break;
      case "integration":
        area = t("Integrations");
        href = "/services";
        break;
      case "slot": {
        const meta = (isPlainObject(d.meta) ? d.meta : {}) as { slot?: string };
        area = t("Console slot");
        if (meta.slot === "dashboard.panel") href = "/";
        else if (meta.slot === "title.panel" || meta.slot === "actionMenu.title")
          href = "/libraries";
        else if (meta.slot === "viewer.tab" || meta.slot === "actionMenu.viewer") href = "/viewers";
        else if (meta.slot === "actionMenu.collection") href = "/collections";
        else if (meta.slot === "actionMenu.release") href = "/releases";
        break;
      }
      case "command":
        area = t("Command search");
        href = "/plugins";
        break;
      case "tool":
        area = t("Ask Ether");
        href = "/plugins";
        break;
      default:
        kind = "console";
        break;
    }

    const dedupeKey = `${d.kind}:${rawKey}:${href}`;
    if (seen.has(dedupeKey)) continue;
    seen.add(dedupeKey);
    out.push({ kind, id: dedupeKey, label, area, href });
  }
  return out;
}

const LANGUAGE_CODES = ["en", "fr", "es", "de", "it", "pt", "ja", "ko", "zh"];

/** The schema language choices, named in the console language. */
function languageOptions(): { value: string; label: string }[] {
  let names: Intl.DisplayNames | null = null;
  try {
    names = new Intl.DisplayNames([getLocale()], { type: "language" });
  } catch {
    names = null;
  }
  return LANGUAGE_CODES.map((code) => ({ value: code, label: names?.of(code) ?? code }));
}

interface RawField {
  key?: string;
  name?: string;
  id?: string;
  field?: string;
  label?: string;
  title?: string;
  hint?: string;
  help?: string;
  description?: string;
  kind?: string;
  type?: string;
  placeholder?: string;
  wide?: boolean;
  min?: number;
  max?: number;
  step?: number;
  unit?: string;
  numeric?: boolean;
  options?: { value: string; label: string }[];
  enum?: string[];
  fields?: RawField[];
}

/**
 * Extracts field names that need dynamic options from GET /api/plugins/{id}/options/{field}.
 */
export function selectFieldsNeedingOptions(schema: Record<string, unknown>): string[] {
  const out: string[] = [];
  const visit = (node: unknown) => {
    if (!node) return;
    if (Array.isArray(node)) {
      node.forEach(visit);
      return;
    }
    if (isPlainObject(node)) {
      const f = node as RawField;
      const key = f.key ?? f.name ?? f.id ?? f.field;
      if (
        key &&
        (f.kind === "select" || f.kind === "multiSelect") &&
        (!Array.isArray(f.options) || f.options.length === 0)
      ) {
        out.push(key);
      }
      for (const v of Object.values(node)) visit(v);
    }
  };
  visit(schema);
  return [...new Set(out)];
}

function toFieldDef(
  raw: RawField,
  dynamicOptions: Record<string, { value: string; label: string }[]>,
  viewers: Viewer[] | null,
  currentValue: unknown,
): FieldDef | null {
  const key = raw.key ?? raw.name ?? raw.id ?? raw.field;
  if (!key) return null;
  const label = raw.label ?? raw.title ?? humanizeKey(key);
  const help = raw.hint ?? raw.help ?? raw.description;
  const kind =
    raw.kind ??
    (raw.type === "boolean"
      ? "bool"
      : raw.type === "number" || raw.type === "integer"
        ? "number"
        : raw.enum
          ? "select"
          : "text");

  switch (kind) {
    case "group":
      return {
        key,
        label,
        help,
        kind: "group",
        fields: (raw.fields ?? [])
          .map((sub) =>
            toFieldDef(
              sub,
              dynamicOptions,
              viewers,
              isPlainObject(currentValue) ? currentValue[sub.key ?? ""] : undefined,
            ),
          )
          .filter((x): x is FieldDef => x !== null),
      };
    case "bool":
      return { key, label, help, kind: "bool" };
    case "number":
      return {
        key,
        label,
        help,
        kind: "number",
        min: raw.min,
        max: raw.max,
        step: raw.step,
        unit: raw.unit,
      };
    case "bytes":
      return {
        key,
        label,
        help,
        kind: "bytes",
        unit: raw.unit === "GiB" ? "GiB" : "MiB",
      };
    case "secret":
      return {
        key,
        label,
        help,
        kind: "secret",
        placeholder: raw.placeholder,
      };
    case "duration":
      return {
        key,
        label,
        help,
        kind: "duration",
        placeholder: raw.placeholder,
      };
    case "readonly":
      return { key, label, help, kind: "readonly" };
    case "hidden":
      return { key, label, help, kind: "hidden" };
    case "list":
    case "orderedList":
    case "multiSelect":
      return {
        key,
        label,
        help,
        kind: "list",
        placeholder: raw.placeholder,
        numeric: raw.numeric,
      };
    case "language": {
      const opts = languageOptions();
      if (
        typeof currentValue === "string" &&
        currentValue &&
        !opts.some((o) => o.value === currentValue)
      ) {
        opts.push({ value: currentValue, label: currentValue });
      }
      return {
        key,
        label,
        help,
        kind: "select",
        options: opts,
      };
    }
    case "viewer": {
      const opts = (viewers ?? []).map((v) => ({
        value: String(v.id),
        label: v.title || v.username,
      }));
      return {
        key,
        label,
        help,
        kind: "select",
        options: opts,
        numeric: true,
      };
    }
    case "select": {
      const baseOpts =
        raw.options && raw.options.length > 0
          ? raw.options
          : raw.enum
            ? raw.enum.map((e) => ({ value: String(e), label: String(e) }))
            : (dynamicOptions[key] ?? []);
      const opts = [...baseOpts];
      if (
        typeof currentValue === "string" &&
        currentValue &&
        !opts.some((o) => o.value === currentValue)
      ) {
        opts.push({ value: currentValue, label: currentValue });
      }
      return {
        key,
        label,
        help,
        kind: "select",
        options: opts,
        numeric: raw.numeric,
      };
    }
    case "url":
      return {
        key,
        label,
        help,
        kind: "text",
        placeholder: raw.placeholder ?? "https://",
        wide: raw.wide ?? true,
      };
    default:
      return {
        key,
        label,
        help,
        kind: "text",
        placeholder: raw.placeholder,
        wide: raw.wide,
      };
  }
}

/**
 * Converts a plugin settings schema into FieldGroup[] for SchemaGroups.
 */
export function schemaToFieldGroups(
  schema: Record<string, unknown>,
  values: Record<string, unknown>,
  dynamicOptions: Record<string, { value: string; label: string }[]>,
  viewers: Viewer[] | null,
): FieldGroup[] {
  if (!schema || Object.keys(schema).length === 0) return [];

  if (Array.isArray(schema.groups)) {
    return (
      schema.groups as {
        id?: string;
        title?: string;
        description?: string;
        fields?: RawField[];
      }[]
    )
      .map((g, idx) => ({
        id: g.id ?? `group-${idx}`,
        title: g.title,
        description: g.description,
        fields: (g.fields ?? [])
          .map((f) => toFieldDef(f, dynamicOptions, viewers, values[f.key ?? f.name ?? ""]))
          .filter((x): x is FieldDef => x !== null),
      }))
      .filter((g) => g.fields.length > 0);
  }

  if (Array.isArray(schema.fields)) {
    const fields = (schema.fields as RawField[])
      .map((f) => toFieldDef(f, dynamicOptions, viewers, values[f.key ?? f.name ?? ""]))
      .filter((x): x is FieldDef => x !== null);
    if (fields.length === 0) return [];
    return [
      {
        id: "general",
        title: typeof schema.title === "string" ? schema.title : undefined,
        description: typeof schema.description === "string" ? schema.description : undefined,
        fields,
      },
    ];
  }

  if (isPlainObject(schema.properties)) {
    const props = schema.properties as Record<string, RawField>;
    const fields = Object.entries(props)
      .map(([k, v]) => toFieldDef({ ...v, key: k }, dynamicOptions, viewers, values[k]))
      .filter((x): x is FieldDef => x !== null);
    if (fields.length === 0) return [];
    return [{ id: "general", fields }];
  }

  return [];
}

/**
 * Maps API field errors (including JSON pointers like "/region" or "/group/key")
 * to SchemaGroups dotted error paths ("settings.region").
 */
export function mapApiFieldErrors(
  fields: { field: string; message: string }[] | undefined,
  prefix = "settings",
): Record<string, string> {
  const out: Record<string, string> = {};
  for (const f of fields ?? []) {
    const clean = f.field
      .replace(/^\/+/, "")
      .replace(/\//g, ".")
      .replace(new RegExp(`^${prefix}\\.`), "");
    const key = clean ? `${prefix}.${clean}` : prefix;
    out[key] = f.message;
  }
  return out;
}

export function journalOpLabel(op: string): string {
  switch (op) {
    case "collections.upsert":
      return t("Updated collection");
    case "viewers.setWatchState":
      return t("Changed watch state");
    default:
      return op;
  }
}

export { FolderOpen, Globe };
