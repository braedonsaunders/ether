import {
  Alert,
  AlertDescription,
  Badge,
  Button,
  EmptyState,
  Input,
  SettingsSection,
  confirmDialog,
  toast,
} from "@braedonsaunders/appkit-ui";
import { ChevronDown, Globe, Link2, Plus, Puzzle, Search } from "lucide-react";
import { useMemo, useState, type ReactNode } from "react";
import { Link } from "react-router-dom";
import { useApp } from "../../app-context";
import { localized, t, tn } from "../../i18n";
import { ActionMenu } from "../../components/ActionMenu";
import { Meter } from "../../components/charts";
import { Drawer } from "../../components/Drawer";
import { ErrorPanel, LoadingRows, MutationError } from "../../components/feedback";
import {
  Field,
  ListInput,
  SecretInput,
  Segmented,
  TestButton,
  type TestState,
} from "../../components/fields";
import { Section } from "../../components/layout";
import { SettingsRow } from "../../components/SettingsRow";
import { api, errorMessage, newId } from "../../lib/api";
import { isPlainObject } from "../../lib/format";
import { useAsync } from "../../lib/hooks";
import type {
  ClientIdentityConfig,
  EtherConfig,
  IdentityView,
  Indexer,
  IndexerProtocol,
  SiteDefinition,
  SiteTestResult,
} from "../../lib/types";
import { hasUsableDebrid } from "./Debrid";
import {
  BoolRow,
  NumRow,
  OrderButtons,
  RecordDrawer,
  RecordItem,
  TextRow,
  recordErrors,
  swap,
  toastError,
  useTests,
  type Errors,
} from "./form";
import {
  MissingDefinition,
  SiteDefinitionRow,
  SiteDefinitions,
  SitePicker,
  SiteSettingsFields,
  UploadDrawer,
  blankSiteIndexer,
  isBuiltin,
  siteKindLabel,
  siteProtocolLabel,
} from "./IndexerSites";

function blankIndexer(priority: number): Indexer {
  return {
    id: newId("idx"),
    name: "",
    url: "",
    apiKey: "",
    dailyLimit: 0,
    dailyGrabs: 0,
    priority,
    enabled: true,
    movieCategories: null,
    tvCategories: null,
    protocol: "usenet",
  };
}

/** A Stremio stream addon: torrent results streamed through debrid. */
function blankStremioIndexer(priority: number): Indexer {
  return { ...blankIndexer(priority), kind: "stremio", protocol: "torrent" };
}

export function isStremio(ix: { kind?: string }) {
  return ix.kind === "stremio";
}

/** An addon install link as saved: trimmed, with stremio:// read as https://. */
export function normalizeAddonUrl(url: string): string {
  const trimmed = url.trim();
  return /^stremio:\/\//i.test(trimmed) ? `https://${trimmed.slice("stremio://".length)}` : trimmed;
}

/** The addon's host, shown instead of a URL whose path may hold settings. */
function addonHost(url: string): string {
  try {
    return new URL(normalizeAddonUrl(url)).host;
  } catch {
    return url;
  }
}

const DEBRID_KEY_PARAM =
  /(?:^|[/|&?,;])(?:realdebrid|torbox|alldebrid|premiumize|debridlink|offcloud|putio)=/i;
const DEBRID_KEY_FIELD = /"(?:debridApiKey|apiKey|debridService)"\s*:\s*"[^"]+"/i;

/** A base64 or base64url path segment decoded, when it is a JSON object. */
function decodeJsonSegment(segment: string): string | null {
  if (segment.length < 16 || !/^[A-Za-z0-9+/_=-]+$/.test(segment)) return null;
  const b64 = segment.replace(/-/g, "+").replace(/_/g, "/");
  try {
    const text = atob(b64.padEnd(Math.ceil(b64.length / 4) * 4, "="));
    return text.trimStart().startsWith("{") ? text : null;
  } catch {
    return null;
  }
}

/** Whether an addon URL embeds a debrid API key in its configuration. */
export function addonUrlHasDebridKey(url: string): boolean {
  let path = normalizeAddonUrl(url);
  try {
    path = new URL(path).pathname;
  } catch {
    // Not a URL yet: inspect what was typed.
  }
  let decoded = path;
  try {
    decoded = decodeURIComponent(path);
  } catch {
    // Keep the raw path.
  }
  if (DEBRID_KEY_PARAM.test(decoded)) return true;
  return path.split("/").some((segment) => {
    const json = decodeJsonSegment(segment);
    return json !== null && DEBRID_KEY_FIELD.test(json);
  });
}

const PROTOCOLS: { value: IndexerProtocol; label: string }[] = localized(() => [
  { value: "usenet", label: t("Usenet (Newznab)") },
  { value: "torrent", label: t("Torrent (Torznab)") },
]);

/** The presets the server accepts, in its display order. */
const IDENTITY_PRESETS: { value: string; label: string }[] = localized(() => [
  { value: "ether", label: t("Ether") },
  { value: "prowlarr", label: t("Prowlarr") },
  { value: "sonarr", label: t("Sonarr") },
  { value: "radarr", label: t("Radarr") },
  { value: "nzbhydra2", label: t("NZBHydra2") },
  { value: "custom", label: t("Custom") },
]);

const GRAB_PRESETS: { value: string; label: string }[] = localized(() => [
  { value: "same", label: t("Same as search") },
  ...IDENTITY_PRESETS,
]);

const IDENTITY_KEYS = ["searchTv", "searchMovies", "grabTv", "grabMovies"] as const;

function identityLabel(name: string): string {
  return IDENTITY_PRESETS.find((p) => p.value === name)?.label ?? t("Ether");
}

/** A preset option labelled with the literal user agent it sends. */
function identityOptionLabel(name: string, userAgents?: Record<string, string>): string {
  const label = identityLabel(name);
  const ua = userAgents?.[name];
  return ua ? `${label} (${ua})` : label;
}

/** A select with its chevron inset from the border instead of the native one. */
function IdentitySelect({
  label,
  value,
  onChange,
  children,
}: {
  label: string;
  value: string;
  onChange: (value: string) => void;
  children: ReactNode;
}) {
  return (
    <div className="relative">
      <select
        aria-label={label}
        className={selectClass}
        value={value}
        onChange={(e) => onChange(e.currentTarget.value)}
      >
        {children}
      </select>
      <ChevronDown
        size={14}
        aria-hidden
        className="pointer-events-none absolute right-3 top-1/2 -translate-y-1/2 text-fg-subtle"
      />
    </div>
  );
}

/**
 * The precedence the server applies to one indexer's requests: its override,
 * then the global value for the purpose and media, then ether. A grab of
 * "same" (or none) follows the same-media search.
 */
function effectiveIdentity(
  policy: ClientIdentityConfig,
  searchOverride = "",
  grabOverride = "",
): { tvSearch: string; movieSearch: string; tvGrab: string; movieGrab: string } {
  const known = (name: string | undefined) =>
    IDENTITY_PRESETS.some((p) => p.value === name) ? (name as string) : "ether";
  const tvSearch = known(searchOverride || policy.searchTv);
  const movieSearch = known(searchOverride || policy.searchMovies);
  const grab = (value: string | undefined, search: string) =>
    known(grabOverride || (value && value !== "same" ? value : search));
  return {
    tvSearch,
    movieSearch,
    tvGrab: grab(policy.grabTv, tvSearch),
    movieGrab: grab(policy.grabMovies, movieSearch),
  };
}

type HeaderRow = { name: string; value: string };

function headerRecord(rows: HeaderRow[]): Record<string, string> {
  return Object.fromEntries(
    rows.filter((row) => row.name.trim()).map((row) => [row.name.trim(), row.value]),
  );
}

const selectClass =
  "w-full appearance-none rounded-md border border-border bg-surface py-2 pl-3 pr-9 text-sm";

/** Name/value rows for a custom identity's extra request headers. */
function HeaderRows({
  rows,
  onChange,
}: {
  rows: HeaderRow[];
  onChange: (rows: HeaderRow[]) => void;
}) {
  const update = (i: number, patch: Partial<HeaderRow>) =>
    onChange(rows.map((row, j) => (j === i ? { ...row, ...patch } : row)));
  return (
    <div className="space-y-2">
      {rows.map((row, i) => (
        <div key={i} className="flex items-center gap-2">
          <Input
            aria-label={t("Header name")}
            placeholder="X-Api-Key"
            value={row.name}
            onChange={(e) => update(i, { name: e.currentTarget.value })}
          />
          <Input
            aria-label={t("Header value")}
            value={row.value}
            onChange={(e) => update(i, { value: e.currentTarget.value })}
          />
          <Button
            variant="ghost"
            size="sm"
            onClick={() => onChange(rows.filter((_, j) => j !== i))}
          >
            {t("Remove")}
          </Button>
        </div>
      ))}
      <Button
        variant="outline"
        size="sm"
        onClick={() => onChange([...rows, { name: "", value: "" }])}
      >
        {t("Add header")}
      </Button>
    </div>
  );
}

/** The global client-identity policy, saved above the indexer list. */
function ClientIdentityCard({
  cfg,
  userAgents,
  onSaved,
}: {
  cfg: EtherConfig;
  userAgents?: Record<string, string>;
  onSaved: () => Promise<void> | void;
}) {
  const [draft, setDraft] = useState<ClientIdentityConfig>(() => ({
    searchTv: cfg.clientIdentity?.searchTv || "ether",
    searchMovies: cfg.clientIdentity?.searchMovies || "ether",
    grabTv: cfg.clientIdentity?.grabTv || "same",
    grabMovies: cfg.clientIdentity?.grabMovies || "same",
    userAgent: cfg.clientIdentity?.userAgent ?? "",
  }));
  const [rows, setRows] = useState<HeaderRow[]>(() =>
    Object.entries(cfg.clientIdentity?.headers ?? {}).map(([name, value]) => ({ name, value })),
  );
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<unknown>(null);
  const [errors, setErrors] = useState<Record<string, string>>({});
  const custom = IDENTITY_KEYS.some((key) => draft[key] === "custom");
  const inheriting = (cfg.indexers ?? []).filter(
    (ix) => !isBuiltin(ix) && !isStremio(ix) && !ix.searchIdentity && !ix.grabIdentity,
  ).length;

  async function save(over: Partial<ClientIdentityConfig> = {}) {
    const next = { ...draft, ...over };
    const usesCustom = IDENTITY_KEYS.some((key) => next[key] === "custom");
    const value: ClientIdentityConfig = {
      searchTv: next.searchTv || "ether",
      searchMovies: next.searchMovies || "ether",
      grabTv: next.grabTv || "same",
      grabMovies: next.grabMovies || "same",
      // Keep a stored custom pair while no selector uses it; switching back
      // to Custom must not lose the user agent or headers.
      userAgent: usesCustom ? (next.userAgent ?? "") : (cfg.clientIdentity?.userAgent ?? ""),
      headers: usesCustom ? headerRecord(rows) : (cfg.clientIdentity?.headers ?? {}),
    };
    setDraft(next);
    setSaving(true);
    setError(null);
    setErrors({});
    try {
      await api.putSection("clientIdentity", value);
      toast.success(t("Saved"));
      await onSaved();
    } catch (e) {
      setError(e);
      setErrors(recordErrors(e, "clientIdentity."));
    } finally {
      setSaving(false);
    }
  }

  const fields: { key: (typeof IDENTITY_KEYS)[number]; label: string; custom: boolean }[] = [
    { key: "searchTv", label: t("TV search"), custom: false },
    { key: "grabTv", label: t("TV grab"), custom: true },
    { key: "searchMovies", label: t("Movie search"), custom: false },
    { key: "grabMovies", label: t("Movie grab"), custom: true },
  ];

  return (
    <Section
      title={t("User agent")}
      description={t(
        "The client Ether presents to indexers. Indexer overrides take precedence over global values, which take precedence over Ether.",
      )}
      descriptionClassName="max-w-none"
      footer={
        <>
          <span className="mr-auto text-xs text-fg-muted">
            {tn(
              inheriting,
              "{count} indexer inherits this policy",
              "{count} indexers inherit this policy",
            )}
          </span>
          <Button
            variant="outline"
            disabled={saving}
            onClick={() =>
              void save({
                searchTv: "sonarr",
                searchMovies: "radarr",
                grabTv: "same",
                grabMovies: "same",
              })
            }
          >
            {t("Standard Servarr stack")}
          </Button>
          <Button disabled={saving} onClick={() => void save()}>
            {t("Save client identity")}
          </Button>
        </>
      }
    >
      {error ? <MutationError error={error} /> : null}
      <div className="grid gap-3 sm:grid-cols-2">
        {fields.map(({ key, label, custom: isGrab }) => (
          <Field key={key} label={label} error={errors[key]}>
            <IdentitySelect
              label={label}
              value={draft[key] ?? ""}
              onChange={(value) => setDraft({ ...draft, [key]: value })}
            >
              {(isGrab ? GRAB_PRESETS : IDENTITY_PRESETS).map((preset) => (
                <option key={preset.value} value={preset.value}>
                  {preset.value === "same"
                    ? preset.label
                    : identityOptionLabel(preset.value, userAgents)}
                </option>
              ))}
            </IdentitySelect>
          </Field>
        ))}
      </div>
      {custom ? (
        <>
          <Field label={t("User agent")} error={errors.userAgent}>
            <Input
              aria-label={t("User agent")}
              value={draft.userAgent ?? ""}
              placeholder="Ether/1.0"
              onChange={(e) => setDraft({ ...draft, userAgent: e.currentTarget.value })}
            />
          </Field>
          <Field label={t("Headers")} error={errors.headers}>
            <HeaderRows rows={rows} onChange={setRows} />
          </Field>
        </>
      ) : null}
    </Section>
  );
}

/** One indexer's identity overrides and the values it resolves to. */
function IndexerIdentityFields({
  draft,
  identity,
  policy,
  errors,
  onChange,
}: {
  draft: Indexer;
  identity: IdentityView | null;
  policy: ClientIdentityConfig;
  errors: Errors;
  onChange: (patch: Partial<Indexer>) => void;
}) {
  const [rows, setRows] = useState<HeaderRow[]>(() =>
    Object.entries(draft.headers ?? {}).map(([name, value]) => ({ name, value })),
  );
  const custom = draft.searchIdentity === "custom" || draft.grabIdentity === "custom";
  const stored = identity?.indexers.find((entry) => entry.id === draft.id);
  const unchanged =
    stored &&
    (stored.searchOverride ?? "") === (draft.searchIdentity ?? "") &&
    (stored.grabOverride ?? "") === (draft.grabIdentity ?? "");
  const effective =
    stored && unchanged
      ? {
          tvSearch: stored.effectiveTvSearch,
          movieSearch: stored.effectiveMovieSearch,
          tvGrab: stored.effectiveTvGrab,
          movieGrab: stored.effectiveMovieGrab,
        }
      : effectiveIdentity(policy, draft.searchIdentity, draft.grabIdentity);

  const inheritLabel = (grammar: "search" | "grab") =>
    t("Inherit (TV {tv}, movies {movies})", {
      tv: identityLabel(
        (grammar === "search" ? stored?.effectiveTvSearch : stored?.effectiveTvGrab) ??
          (grammar === "search" ? identity?.tvSearch : identity?.tvGrab) ??
          "ether",
      ),
      movies: identityLabel(
        (grammar === "search" ? stored?.effectiveMovieSearch : stored?.effectiveMovieGrab) ??
          (grammar === "search" ? identity?.movieSearch : identity?.movieGrab) ??
          "ether",
      ),
    });

  return (
    <SettingsSection
      title={t("User agent")}
      description={t(
        "Effective: TV search {tvSearch}, movies search {movieSearch}; TV grab {tvGrab}, movies grab {movieGrab}.",
        {
          tvSearch: identityLabel(effective.tvSearch),
          movieSearch: identityLabel(effective.movieSearch),
          tvGrab: identityLabel(effective.tvGrab),
          movieGrab: identityLabel(effective.movieGrab),
        },
      )}
    >
      <SettingsRow stacked title={t("Search identity")}>
        <IdentitySelect
          label={t("Search identity")}
          value={draft.searchIdentity ?? ""}
          onChange={(value) => onChange({ searchIdentity: value })}
        >
          <option value="">{inheritLabel("search")}</option>
          {IDENTITY_PRESETS.map((preset) => (
            <option key={preset.value} value={preset.value}>
              {identityOptionLabel(preset.value, identity?.userAgents)}
            </option>
          ))}
        </IdentitySelect>
        {errors.searchIdentity ? (
          <p className="text-xs text-danger" role="alert">
            {errors.searchIdentity}
          </p>
        ) : null}
      </SettingsRow>
      <SettingsRow stacked title={t("Grab identity")}>
        <IdentitySelect
          label={t("Grab identity")}
          value={draft.grabIdentity ?? ""}
          onChange={(value) => onChange({ grabIdentity: value })}
        >
          <option value="">{inheritLabel("grab")}</option>
          {IDENTITY_PRESETS.map((preset) => (
            <option key={preset.value} value={preset.value}>
              {identityOptionLabel(preset.value, identity?.userAgents)}
            </option>
          ))}
        </IdentitySelect>
        {errors.grabIdentity ? (
          <p className="text-xs text-danger" role="alert">
            {errors.grabIdentity}
          </p>
        ) : null}
      </SettingsRow>
      {custom ? (
        <>
          <SettingsRow stacked title={t("User agent")}>
            <Input
              aria-label={t("User agent")}
              value={draft.userAgent ?? ""}
              placeholder="Ether/1.0"
              onChange={(e) => onChange({ userAgent: e.currentTarget.value })}
            />
            {errors.userAgent ? (
              <p className="text-xs text-danger" role="alert">
                {errors.userAgent}
              </p>
            ) : null}
          </SettingsRow>
          <SettingsRow stacked title={t("Headers")}>
            <HeaderRows
              rows={rows}
              onChange={(next) => {
                setRows(next);
                onChange({ headers: headerRecord(next) });
              }}
            />
            {errors.headers ? (
              <p className="text-xs text-danger" role="alert">
                {errors.headers}
              </p>
            ) : null}
          </SettingsRow>
        </>
      ) : null}
    </SettingsSection>
  );
}

export function isTorrent(ix: { protocol?: string }) {
  return ix.protocol === "torrent";
}

/** The indexer API name for a protocol, as shown on badges. */
export function protocolLabel(ix: { protocol?: string }) {
  return isTorrent(ix) ? "Torznab" : "Newznab";
}

/** The host of an address, or the address as typed when it is not a URL. */
function hostOf(url: string): string {
  try {
    return new URL(url).host;
  } catch {
    return url;
  }
}

function indexerName(ix: Indexer, i: number) {
  return (
    ix.name ||
    (isStremio(ix) && ix.url ? addonHost(ix.url) : ix.url) ||
    t("Indexer {number}", { number: i + 1 })
  );
}

/** A site's test: result count and a few sample titles, or the error. */
function SiteTestResults({ result }: { result: SiteTestResult }) {
  if (!result.ok) return null;
  const titles = result.titles ?? [];
  return (
    <Alert variant="success" role="status">
      <AlertDescription>
        <p>
          {titles.length
            ? tn(
                result.results,
                "Signed in and searched: {count} result, for example:",
                "Signed in and searched: {count} results, for example:",
              )
            : tn(
                result.results,
                "Signed in and searched: {count} result.",
                "Signed in and searched: {count} results.",
              )}
        </p>
        {titles.length ? (
          <ul className="mt-1 list-disc pl-5 text-xs">
            {titles.map((title) => (
              <li key={title}>{title}</li>
            ))}
          </ul>
        ) : null}
      </AlertDescription>
    </Alert>
  );
}

/**
 * Indexers in search order: sites, Newznab/Torznab URLs and Stremio addons,
 * each edited in a drawer, followed by the site definitions and their
 * settings edited in place. `version` changes when indexers were added
 * elsewhere (Prowlarr import).
 */
export function IndexerList({ version = 0 }: { version?: number }) {
  const { dashboard, refreshDashboard } = useApp();
  const config = useAsync(() => api.getConfig(), [version]);
  const identity = useAsync(() => api.getIdentity().catch(() => null), [version]);
  const [sitesVersion, setSitesVersion] = useState(0);
  const sites = useAsync(() => api.sites.definitions(), [sitesVersion]);
  const statuses = useAsync(() => api.sites.status().catch(() => []), [version, sitesVersion]);
  const [editing, setEditing] = useState<{ index: number; draft: Indexer } | null>(null);
  const [picking, setPicking] = useState(false);
  /** Uploading a definition: "add" continues to a new indexer for it. */
  const [uploading, setUploading] = useState<"add" | "install" | null>(null);
  const [errors, setErrors] = useState<Errors>({});
  const [error, setError] = useState<unknown>(null);
  const [saving, setSaving] = useState(false);
  const { tests, runTest, reset } = useTests();
  const [siteTest, setSiteTest] = useState<{ state: TestState; result?: SiteTestResult }>({
    state: { status: "idle" },
  });

  const cfg = config.data;
  const indexers = cfg?.indexers ?? [];
  const definitions = sites.data ?? null;
  // A definition installed a moment ago, until the reloaded list includes it.
  const [installed, setInstalled] = useState<SiteDefinition | null>(null);
  const defFor = (ix: Indexer): SiteDefinition | undefined =>
    definitions?.find((d) => d.id === ix.definition) ??
    (installed && installed.id === ix.definition ? installed : undefined);
  const statusFor = (ix: Indexer) => statuses.data?.find((s) => s.indexerId === ix.id);
  const inUse = useMemo(
    () => new Set(indexers.filter(isBuiltin).map((ix) => ix.definition ?? "")),
    [indexers],
  );
  const status = dashboard?.sections?.indexers;
  const live = Array.isArray(status) ? status.filter(isPlainObject) : [];
  const statFor = (ix: Indexer) => live.find((s) => s.indexer === ix.name || s.id === ix.id);

  async function commit(list: Indexer[], message: string) {
    await api.putSection(
      "indexers",
      list.map((ix, priority) => ({ ...ix, priority })),
    );
    toast.success(message);
    await config.reload();
    void statuses.reload();
    void refreshDashboard();
  }

  const open = (index: number, draft: Indexer) => {
    setErrors({});
    setError(null);
    reset(draft.id);
    setSiteTest({ state: { status: "idle" } });
    setPicking(false);
    setEditing({ index, draft: structuredClone(draft) });
  };
  const close = () => {
    if (!saving) setEditing(null);
  };
  const addSite = (def: SiteDefinition) => {
    setInstalled(def);
    open(indexers.length, blankSiteIndexer(def, indexers.length));
  };

  async function save() {
    if (!editing) return;
    const { index } = editing;
    const draft: Indexer = isStremio(editing.draft)
      ? {
          ...editing.draft,
          url: normalizeAddonUrl(editing.draft.url),
          protocol: "torrent" as const,
        }
      : editing.draft;
    const list =
      index < indexers.length
        ? indexers.map((ix, j) => (j === index ? draft : ix))
        : [...indexers, draft];
    setSaving(true);
    setErrors({});
    setError(null);
    try {
      await commit(list, index < indexers.length ? t("Indexer saved") : t("Indexer added"));
      setEditing(null);
    } catch (e) {
      setError(e);
      setErrors(recordErrors(e, `indexers[${index}].`));
    } finally {
      setSaving(false);
    }
  }

  async function remove() {
    if (!editing) return;
    const { index, draft } = editing;
    const ok = await confirmDialog({
      title: t("Remove indexer?"),
      message: t("{indexer} will no longer be searched.", { indexer: indexerName(draft, index) }),
      confirmLabel: t("Remove"),
      tone: "danger",
    });
    if (!ok) return;
    setSaving(true);
    setError(null);
    try {
      await commit(
        indexers.filter((_, j) => j !== index),
        t("Indexer removed"),
      );
      setEditing(null);
    } catch (e) {
      setError(e);
    } finally {
      setSaving(false);
    }
  }

  async function move(i: number, d: -1 | 1) {
    try {
      await commit(swap(indexers, i, i + d), t("Indexer order saved"));
    } catch (e) {
      toastError(t("Couldn't reorder indexers"), e);
    }
  }

  async function testSite(draft: Indexer) {
    setSiteTest({ state: { status: "running" } });
    try {
      const r = await api.sites.test(draft);
      setSiteTest({
        state: r.ok
          ? { status: "ok", durationMs: r.durationMs }
          : { status: "fail", message: r.error ?? t("failed") },
        result: r,
      });
      void statuses.reload();
    } catch (e) {
      setSiteTest({ state: { status: "fail", message: errorMessage(e) } });
    }
  }

  if (config.error) return <ErrorPanel error={config.error} onRetry={() => void config.reload()} />;
  if (!cfg) return <LoadingRows rows={3} />;

  const draft = editing?.draft;
  const set = (patch: Partial<Indexer>) =>
    setEditing((e) => (e ? { ...e, draft: { ...e.draft, ...patch } } : e));
  const isNew = editing ? editing.index >= indexers.length : false;
  const stat = draft ? statFor(draft) : undefined;
  const used = Number(stat?.hitsUsed ?? 0);
  const needsDebrid = indexers.some((ix) => isTorrent(ix) && ix.enabled) && !hasUsableDebrid(cfg);
  const builtin = draft ? isBuiltin(draft) : false;
  const stremio = draft ? isStremio(draft) : false;
  const draftDef = draft && builtin ? defFor(draft) : undefined;

  return (
    <>
      {needsDebrid ? (
        <Alert variant="info" role="status">
          <AlertDescription>
            {t("Torrent indexers need a debrid account to stream from.")}{" "}
            <Link to="/sources/debrid" className="font-medium underline underline-offset-2">
              {t("Add a debrid provider")}
            </Link>
          </AlertDescription>
        </Alert>
      ) : null}
      <Section
        title={t("Indexers")}
        description={t("Searched in order. Ether handles service limits automatically.")}
        actions={
          <ActionMenu
            label={t("Add indexer")}
            variant="default"
            size="sm"
            items={[
              {
                key: "site",
                label: t("Site…"),
                icon: Globe,
                onSelect: () => {
                  setEditing(null);
                  setPicking(true);
                },
              },
              {
                key: "newznab",
                label: t("Newznab or Torznab URL…"),
                icon: Link2,
                onSelect: () => open(indexers.length, blankIndexer(indexers.length)),
              },
              {
                key: "stremio",
                label: t("Stremio addon URL…"),
                icon: Puzzle,
                onSelect: () => open(indexers.length, blankStremioIndexer(indexers.length)),
              },
            ]}
          >
            <Plus size={14} /> {t("Add indexer")}
          </ActionMenu>
        }
        bodyClassName=""
      >
        {indexers.length === 0 ? (
          <EmptyState
            icon={<Search />}
            title={t("No indexers")}
            description={t(
              "Add a site, a Newznab or Torznab indexer, or a Stremio addon so Ether can find releases.",
            )}
          />
        ) : (
          <ul className="divide-y divide-border">
            {indexers.map((ix, i) => {
              const s = statFor(ix);
              const label = indexerName(ix, i);
              const hits = Number(s?.hitsUsed ?? 0);
              const site = isBuiltin(ix);
              const def = site ? defFor(ix) : undefined;
              const tested = site ? statusFor(ix) : undefined;
              const countsSearches = site || isStremio(ix);
              return (
                <RecordItem
                  key={ix.id}
                  title={label}
                  description={[
                    i === 0 ? t("Searched first") : t("Priority {number}", { number: i + 1 }),
                    site && ix.url ? hostOf(ix.url) : "",
                    site && def && def.name !== label
                      ? t("{name} definition", { name: def.name })
                      : "",
                    ix.dailyLimit > 0
                      ? s
                        ? countsSearches
                          ? tn(
                              ix.dailyLimit,
                              "{used} of {count} search used today",
                              "{used} of {count} searches used today",
                              { used: hits },
                            )
                          : tn(
                              ix.dailyLimit,
                              "{used} of {count} API request used today",
                              "{used} of {count} API requests used today",
                              { used: hits },
                            )
                        : countsSearches
                          ? tn(ix.dailyLimit, "{count} search a day", "{count} searches a day")
                          : tn(
                              ix.dailyLimit,
                              "{count} API request a day",
                              "{count} API requests a day",
                            )
                      : site
                        ? ""
                        : t("Automatic limits"),
                    ix.dailyGrabs > 0
                      ? isTorrent(ix)
                        ? tn(
                            ix.dailyGrabs,
                            "{count} torrent download a day",
                            "{count} torrent downloads a day",
                          )
                        : tn(
                            ix.dailyGrabs,
                            "{count} NZB download a day",
                            "{count} NZB downloads a day",
                          )
                      : "",
                  ]
                    .filter(Boolean)
                    .join(" · ")}
                  meta={
                    <>
                      {site ? (
                        <>
                          <Badge variant="outline">{siteKindLabel(ix.protocol)}</Badge>
                          {definitions && !def ? (
                            <Badge variant="destructive">{t("Definition missing")}</Badge>
                          ) : null}
                          {tested ? (
                            <Badge
                              variant={tested.ok ? "success" : "destructive"}
                              title={tested.message}
                            >
                              {tested.ok ? t("Test passed") : t("Test failed")}
                            </Badge>
                          ) : null}
                        </>
                      ) : isStremio(ix) ? (
                        <Badge variant="outline">{t("Stremio addon")}</Badge>
                      ) : (
                        <Badge variant="outline">{protocolLabel(ix)}</Badge>
                      )}
                      {!ix.enabled ? <Badge variant="secondary">{t("Off")}</Badge> : null}
                      {s && Number(s.retryAt ?? 0) > Date.now() / 1000 ? (
                        <Badge variant="warning">
                          {t("Waiting for service · retries automatically")}
                        </Badge>
                      ) : s &&
                        ((ix.dailyLimit > 0 && hits >= ix.dailyLimit) ||
                          (ix.dailyGrabs > 0 && Number(s.grabsUsed ?? 0) >= ix.dailyGrabs)) ? (
                        <Badge variant="warning">{t("Personal cap reached")}</Badge>
                      ) : null}
                    </>
                  }
                  order={
                    <OrderButtons
                      label={label}
                      i={i}
                      n={indexers.length}
                      onMove={(d) => void move(i, d)}
                    />
                  }
                  editLabel={t("Edit {name}", { name: label })}
                  onEdit={() => open(i, ix)}
                />
              );
            })}
          </ul>
        )}
      </Section>

      <ClientIdentityCard
        cfg={cfg}
        userAgents={identity.data?.userAgents}
        onSaved={async () => {
          await config.reload();
          await identity.reload();
        }}
      />

      <SiteDefinitions
        cfg={cfg}
        definitions={definitions}
        definitionsError={sites.error}
        inUse={inUse}
        onAdd={addSite}
        onUpload={() => setUploading("install")}
        onConfigSaved={() => config.reload()}
        onDefinitionsChanged={() => setSitesVersion((v) => v + 1)}
      />

      <UploadDrawer
        open={uploading !== null}
        onClose={() => setUploading(null)}
        onInstalled={(def) => {
          const next = uploading;
          setUploading(null);
          setSitesVersion((v) => v + 1);
          if (next === "add") addSite(def);
        }}
      />

      <Drawer
        open={picking}
        onClose={() => setPicking(false)}
        title={t("Add a site")}
        description={t("Pick the site's definition. Its settings come next.")}
        size="lg"
      >
        {sites.error ? (
          <ErrorPanel error={sites.error} onRetry={() => void sites.reload()} />
        ) : definitions === null ? (
          <LoadingRows rows={3} />
        ) : (
          <SitePicker
            definitions={definitions}
            inUse={inUse}
            onInstalled={() => setSitesVersion((v) => v + 1)}
            onPick={addSite}
            onUpload={() => {
              setPicking(false);
              setUploading("add");
            }}
          />
        )}
      </Drawer>

      <RecordDrawer
        open={editing !== null}
        onClose={close}
        title={
          isNew
            ? builtin
              ? (draftDef?.name ?? t("New site"))
              : stremio
                ? t("New Stremio addon")
                : t("New indexer")
            : draft && editing
              ? indexerName(draft, editing.index)
              : ""
        }
        description={
          draft
            ? builtin
              ? siteKindLabel(draft.protocol)
              : stremio
                ? t("Stremio addon · Torrent")
                : isTorrent(draft)
                  ? t("Torznab indexer")
                  : t("Newznab indexer")
            : undefined
        }
        onSave={() => void save()}
        saving={saving}
        saveLabel={t("Save indexer")}
        onRemove={isNew ? undefined : () => void remove()}
        removeLabel={t("Remove indexer")}
        error={error}
        tools={
          draft ? (
            builtin ? (
              <TestButton
                state={siteTest.state}
                onTest={() => void testSite(draft)}
                disabled={!draftDef}
              />
            ) : (
              <TestButton
                state={tests[draft.id] ?? { status: "idle" }}
                onTest={() =>
                  runTest(draft.id, () =>
                    api.testIndexer(
                      stremio
                        ? { ...draft, url: normalizeAddonUrl(draft.url), protocol: "torrent" }
                        : draft,
                    ),
                  )
                }
                disabled={!draft.url}
              />
            )
          ) : null
        }
      >
        {draft ? (
          <>
            {stat && draft.dailyLimit > 0 ? (
              <Meter
                value={used / draft.dailyLimit}
                label={tn(
                  used,
                  "{count} API request used today",
                  "{count} API requests used today",
                )}
                valueLabel={t("{left} left of {limit}", {
                  left: Number(stat.hitsRemaining ?? 0),
                  limit: draft.dailyLimit,
                })}
                tone={
                  used >= draft.dailyLimit
                    ? "danger"
                    : used / draft.dailyLimit > 0.8
                      ? "warning"
                      : "primary"
                }
              />
            ) : null}
            {builtin && definitions && !draftDef ? (
              <MissingDefinition id={draft.definition} />
            ) : null}
            {builtin && siteTest.result ? <SiteTestResults result={siteTest.result} /> : null}
            {builtin ? (
              <SettingsSection title={t("Site")}>
                <TextRow label={t("Name")} value={draft.name} onChange={(v) => set({ name: v })} />
                <TextRow
                  label={t("Site address")}
                  type="url"
                  value={draft.url}
                  placeholder={draftDef?.links[0]}
                  help={
                    draftDef && draftDef.links.length > 1
                      ? t("Also: {links}", { links: draftDef.links.slice(1).join(", ") })
                      : undefined
                  }
                  error={errors.url}
                  onChange={(v) => set({ url: v })}
                />
                {draftDef ? <SiteDefinitionRow def={draftDef} /> : null}
                <SettingsRow
                  title={t("Protocol")}
                  description={
                    isTorrent(draft)
                      ? t("Torrent releases stream through your debrid account.")
                      : t("Usenet releases stream from your Usenet providers.")
                  }
                  control={<Badge variant="outline">{siteProtocolLabel(draft.protocol)}</Badge>}
                />
                <BoolRow
                  label={t("Enabled")}
                  value={draft.enabled}
                  onChange={(v) => set({ enabled: v })}
                />
              </SettingsSection>
            ) : stremio ? (
              <SettingsSection title={t("Addon")}>
                <TextRow label={t("Name")} value={draft.name} onChange={(v) => set({ name: v })} />
                <TextRow
                  label={t("Addon URL")}
                  type="url"
                  value={draft.url}
                  placeholder="https://addon.example/manifest.json"
                  help={t(
                    "Paste the addon's install link. Choose an addon that exposes torrent results; direct or proxied playback links alone are not supported. Ether streams through your configured debrid accounts.",
                  )}
                  error={errors.url}
                  onChange={(v) => set({ url: v })}
                />
                {addonUrlHasDebridKey(draft.url) ? (
                  <Alert variant="warning" role="status">
                    <AlertDescription>
                      {t(
                        "This link includes a debrid API key. Ether checks your debrid accounts itself, so the addon doesn't need it: a key only adds cached hints, and it is then also stored in this link on this server.",
                      )}
                    </AlertDescription>
                  </Alert>
                ) : null}
                <BoolRow
                  label={t("Enabled")}
                  value={draft.enabled}
                  onChange={(v) => set({ enabled: v })}
                />
              </SettingsSection>
            ) : (
              <SettingsSection title={t("Connection")}>
                <SettingsRow
                  title={t("Protocol")}
                  description={
                    isTorrent(draft)
                      ? t("Torrent releases stream through your debrid account.")
                      : t("Usenet releases stream from your Usenet providers.")
                  }
                  control={
                    <Segmented
                      ariaLabel={t("Protocol")}
                      size="sm"
                      options={PROTOCOLS}
                      value={isTorrent(draft) ? "torrent" : "usenet"}
                      onChange={(v) => set({ protocol: v })}
                    />
                  }
                />
                <TextRow label={t("Name")} value={draft.name} onChange={(v) => set({ name: v })} />
                <TextRow
                  label={t("URL")}
                  type="url"
                  value={draft.url}
                  placeholder="https://indexer.example.com"
                  help={t("/api is added automatically.")}
                  error={errors.url}
                  onChange={(v) => set({ url: v })}
                />
                <SettingsRow stacked title={t("API key")}>
                  <SecretInput
                    ariaLabel={t("API key")}
                    value={draft.apiKey}
                    onChange={(v) => set({ apiKey: v })}
                  />
                </SettingsRow>
                <BoolRow
                  label={t("Enabled")}
                  value={draft.enabled}
                  onChange={(v) => set({ enabled: v })}
                />
              </SettingsSection>
            )}
            {builtin && draftDef ? (
              <SiteSettingsFields def={draftDef} draft={draft} onChange={set} errors={errors} />
            ) : null}
            <details className="space-y-3">
              <summary className="cursor-pointer text-sm font-medium">
                {t("Advanced: personal caps")}
              </summary>
              <SettingsSection
                title={t("Optional daily caps")}
                description={
                  builtin
                    ? t(
                        "Sites report no limits to Ether. Add a cap if the site restricts how often you search or download.",
                      )
                    : stremio
                      ? t(
                          "Ether follows the addon's rate limit responses automatically. Add a cap if you want fewer searches.",
                        )
                      : t(
                          "Ether follows service limit responses automatically. Add a lower personal cap only if you want one.",
                        )
                }
              >
                <NumRow
                  label={builtin || stremio ? t("Searches per day") : t("API requests per day")}
                  value={draft.dailyLimit}
                  min={0}
                  help={
                    builtin || stremio
                      ? t("0 = no cap.")
                      : t("0 = automatic. Includes searches and capability checks.")
                  }
                  error={errors.dailyLimit}
                  onChange={(n) => set({ dailyLimit: n })}
                />
                {stremio ? null : (
                  <NumRow
                    label={
                      isTorrent(draft) ? t("Torrent downloads per day") : t("NZB downloads per day")
                    }
                    value={draft.dailyGrabs}
                    min={0}
                    help={
                      builtin ? t("0 = no cap.") : t("0 = automatic. Counts files, not movie data.")
                    }
                    error={errors.dailyGrabs}
                    onChange={(n) => set({ dailyGrabs: n })}
                  />
                )}
              </SettingsSection>
            </details>
            {builtin || stremio ? null : (
              <SettingsSection title={t("Categories")}>
                <SettingsRow
                  stacked
                  title={t("Movie categories")}
                  description={t(
                    "{protocol} category ids. Empty uses the standard movie categories.",
                    { protocol: protocolLabel(draft) },
                  )}
                >
                  <ListInput
                    numeric
                    ariaLabel={t("Movie categories")}
                    value={draft.movieCategories}
                    onChange={(v) => set({ movieCategories: v.length ? (v as number[]) : null })}
                    placeholder="2000, 2040"
                  />
                </SettingsRow>
                <SettingsRow
                  stacked
                  title={t("TV categories")}
                  description={t("Empty uses the standard TV categories.")}
                >
                  <ListInput
                    numeric
                    ariaLabel={t("TV categories")}
                    value={draft.tvCategories}
                    onChange={(v) => set({ tvCategories: v.length ? (v as number[]) : null })}
                    placeholder="5000, 5040"
                  />
                </SettingsRow>
              </SettingsSection>
            )}
            {builtin || stremio ? null : (
              <IndexerIdentityFields
                draft={draft}
                identity={identity.data ?? null}
                policy={cfg.clientIdentity ?? {}}
                errors={errors}
                onChange={set}
              />
            )}
          </>
        ) : null}
      </RecordDrawer>
    </>
  );
}
