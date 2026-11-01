import { t, tn, tx } from "../../i18n";
import {
  Alert,
  AlertDescription,
  Badge,
  Button,
  EmptyState,
  SearchSelect,
  SettingsSection,
  confirmDialog,
  toast,
} from "@braedonsaunders/appkit-ui";
import { Cloud, Plus } from "lucide-react";
import { useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { Link } from "react-router-dom";
import { useApp } from "../../app-context";
import { MoreOptions } from "../../components/ConfigSection";
import { ErrorPanel, LoadingRows } from "../../components/feedback";
import { SecretInput, TestButton } from "../../components/fields";
import { Section } from "../../components/layout";
import { SaveBar, useDraft } from "../../components/SaveBar";
import { SettingsRow } from "../../components/SettingsRow";
import { api, ApiError, newId } from "../../lib/api";
import { fixed, formatBytes, formatNumber, relativeTime } from "../../lib/format";
import { useAsync, useInterval } from "../../lib/hooks";
import type {
  DebridConfig,
  DebridEngineStatus,
  DebridLatency,
  DebridKind,
  DebridProvider,
  DebridProviderStatus,
  EtherConfig,
} from "../../lib/types";
import { SchemaField } from "../settings/SchemaForm";
import { DEBRID } from "../settings/schema";
import { DEBRID_KINDS, DEBRID_TOKEN_URL, SourceCard, debridFix } from "./cards";
import { debridMark } from "./marks";

export { DEBRID_KINDS };
import {
  BoolRow,
  OrderButtons,
  RecordDrawer,
  TextRow,
  recordErrors,
  swap,
  toastError,
  useTests,
  type Errors,
} from "./form";
import { SortableList } from "../../components/Sortable";

export function debridKindLabel(kind: string): string {
  return DEBRID_KINDS.find((k) => k.value === kind)?.label ?? kind;
}

/** Defaults for servers that report no debrid section yet. */
const EMPTY: DebridConfig = { providers: [], cachedOnly: true, removeUncached: true };

/** Debrid settings for `cfg`, with defaults filled in. */
export function debridOf(cfg: EtherConfig): DebridConfig {
  return { ...EMPTY, ...cfg.debrid };
}

/** Whether any debrid account can be used (enabled and has a key). */
export function hasUsableDebrid(cfg: EtherConfig): boolean {
  return (cfg.debrid?.providers ?? []).some((p) => p.enabled && p.apiKey !== "");
}

function blankProvider(priority: number): DebridProvider {
  return {
    id: newId("debrid"),
    name: "",
    kind: "realdebrid",
    apiKey: "",
    priority,
    enabled: true,
  };
}

function providerName(p: DebridProvider, i: number) {
  return p.name || debridKindLabel(p.kind) || t("Provider {number}", { number: i + 1 });
}

function role(i: number) {
  return i === 0 ? t("Tried first") : t("Priority {number}", { number: i + 1 });
}

/** Counters and last activity for one provider's row. */
function activity(s: DebridProviderStatus): string[] {
  const out: string[] = [];
  const requests = Number(s.requests ?? 0);
  if (requests > 0) out.push(tn(requests, "{count} request", "{count} requests"));
  if (s.cached || s.notCached)
    out.push(
      t("{cached} cached · {notCached} not cached", {
        cached: formatNumber(Number(s.cached ?? 0)),
        notCached: formatNumber(Number(s.notCached ?? 0)),
      }),
    );
  if (s.blocked)
    out.push(t("{count} blocked by provider", { count: formatNumber(Number(s.blocked)) }));
  if (s.failures) out.push(tn(Number(s.failures), "{count} failure", "{count} failures"));
  if (s.reused) out.push(t("{count} reused", { count: formatNumber(Number(s.reused)) }));
  if (s.linksRefreshed)
    out.push(tn(Number(s.linksRefreshed), "{count} link refreshed", "{count} links refreshed"));
  // In a cluster the account's concurrent API calls are divided among nodes.
  const slots = Number(s.slots ?? 0);
  const account = Number(s.accountSlots ?? 0);
  if (account > 0 && slots !== account)
    out.push(
      tn(
        account,
        "this node: {slots} of {count} API connection",
        "this node: {slots} of {count} API connections",
        { slots },
      ),
    );
  if (s.lastSuccess) out.push(t("last worked {time}", { time: relativeTime(s.lastSuccess) }));
  return out;
}

function ms(n: number): string {
  // Half a second and up reads better in seconds ("0.8 s").
  return n >= 500 ? `${fixed(n / 1000, 1)} s` : `${fixed(n, 0)} ms`;
}

/** Megabytes per second, decimal (as speed tests report it). */
function mbps(bytesPerSecond: number): string {
  const v = bytesPerSecond / 1e6;
  return `${fixed(v, v >= 10 ? 0 : 1)} MB/s`;
}

/** Averaged timings for one provider, or null before it has been used. */
export function latencySummary(l: DebridLatency | undefined): string | null {
  if (!l || !Number(l.samples ?? 0)) return null;
  const out: string[] = [];
  if (l.resolveMs) out.push(t("API {time}", { time: ms(l.resolveMs) }));
  if (l.linkMs) out.push(t("link {time}", { time: ms(l.linkMs) }));
  if (l.ttfbMs) out.push(t("first byte {time}", { time: ms(l.ttfbMs) }));
  if (l.throughputBps) out.push(mbps(l.throughputBps));
  return out.length ? out.join(" · ") : null;
}

/** One line about the shared streaming engine, or null when the server reports none. */
export function engineSummary(e: DebridEngineStatus | undefined): string | null {
  if (!e) return null;
  const n = (v: number | undefined) => Number(v ?? 0);
  const hits = n(e.memoryHits) + n(e.diskHits);
  const out = [
    tn(n(e.openFiles), "{count} open file", "{count} open files"),
    tn(
      hits,
      "{count} cache hit ({memory} memory, {disk} disk) vs {fetched} fetched",
      "{count} cache hits ({memory} memory, {disk} disk) vs {fetched} fetched",
      {
        memory: formatNumber(n(e.memoryHits)),
        disk: formatNumber(n(e.diskHits)),
        fetched: formatNumber(n(e.blocksFetched)),
      },
    ) + (n(e.bytesFetched) ? ` (${formatBytes(n(e.bytesFetched))})` : ""),
  ];
  if (n(e.hedges))
    out.push(
      tn(n(e.hedges), "{count} hedged request, {won} won", "{count} hedged requests, {won} won", {
        won: formatNumber(n(e.hedgeWins)),
      }),
    );
  if (n(e.linkRefreshes))
    out.push(tn(n(e.linkRefreshes), "{count} link refreshed", "{count} links refreshed"));
  if (n(e.errors)) out.push(tn(n(e.errors), "{count} error", "{count} errors"));
  return out.join(" · ");
}

function StateBadge({ p, s }: { p: DebridProvider; s?: DebridProviderStatus }) {
  if (!p.enabled) return <Badge variant="secondary">{t("Off")}</Badge>;
  if (!s) return null;
  switch (s.state) {
    case "ok":
      return <Badge variant="success">{t("Ready")}</Badge>;
    case "backoff":
      return (
        <Badge variant="warning">
          {s.backoffUntil
            ? t("Paused · retries {time}", { time: relativeTime(s.backoffUntil) })
            : t("Paused")}
        </Badge>
      );
    case "auth_failed":
      return <Badge variant="destructive">{t("Key rejected")}</Badge>;
    case "disabled":
      return <Badge variant="secondary">{t("Inactive")}</Badge>;
    default:
      return <Badge variant="secondary">{s.state ? String(s.state) : t("Unknown")}</Badge>;
  }
}

/**
 * Debrid accounts in priority order, each edited in a drawer, followed by the
 * section's torrent-handling settings edited in place.
 */
export function DebridSources({
  addKind,
}: {
  /** Set when the add gallery picks a debrid service. */
  addKind?: { nonce: number; kind: DebridKind } | null;
} = {}) {
  const { refreshDashboard } = useApp();
  const config = useAsync(() => api.getConfig(), []);
  // Live status refreshes on its own; a failed poll leaves the list usable.
  const status = useAsync(() => api.debridStatus().catch(() => null), []);
  useInterval(() => void status.reload(), 15_000);
  const [editing, setEditing] = useState<{ index: number; draft: DebridProvider } | null>(null);
  const [errors, setErrors] = useState<Errors>({});
  const [error, setError] = useState<unknown>(null);
  const [saving, setSaving] = useState(false);
  const { tests, runTest, reset } = useTests();
  const seenAdd = useRef(0);

  const cfg = config.data;
  const debrid = cfg ? debridOf(cfg) : null;
  const providers = useMemo(
    () => [...(debrid?.providers ?? [])].sort((a, b) => (a.priority ?? 0) - (b.priority ?? 0)),
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [JSON.stringify(debrid?.providers ?? [])],
  );
  const live = status.data?.providers ?? [];
  const engine = engineSummary(status.data?.engine);
  const torrentIndexers = (cfg?.indexers ?? []).filter(
    (ix) => ix.protocol === "torrent" && ix.enabled,
  ).length;

  const reloadAll = useCallback(async () => {
    await config.reload();
    void status.reload();
    void refreshDashboard();
  }, [config, status, refreshDashboard]);

  async function commit(list: DebridProvider[], message: string) {
    if (!debrid) return;
    await api.putSection("debrid", {
      ...debrid,
      providers: list.map((p, priority) => ({ ...p, priority })),
    });
    toast.success(message);
    await reloadAll();
  }

  const open = (index: number, draft: DebridProvider) => {
    setErrors({});
    setError(null);
    reset(draft.id);
    setEditing({ index, draft: structuredClone(draft) });
  };
  useEffect(() => {
    if (!addKind || !debrid || seenAdd.current === addKind.nonce) return;
    seenAdd.current = addKind.nonce;
    open(providers.length, { ...blankProvider(providers.length), kind: addKind.kind });
    // Open once per gallery choice. `open` closes over the list at that moment.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [addKind, debrid, providers.length]);
  const close = () => {
    if (!saving) setEditing(null);
  };

  async function save() {
    if (!editing) return;
    const { index, draft } = editing;
    const list =
      index < providers.length
        ? providers.map((p, j) => (j === index ? draft : p))
        : [...providers, draft];
    setSaving(true);
    setErrors({});
    setError(null);
    try {
      await commit(list, index < providers.length ? t("Provider saved") : t("Provider added"));
      setEditing(null);
    } catch (e) {
      setError(e);
      setErrors(recordErrors(e, `debrid.providers[${index}].`));
    } finally {
      setSaving(false);
    }
  }

  async function remove() {
    if (!editing) return;
    const { index, draft } = editing;
    const ok = await confirmDialog({
      title: t("Remove provider?"),
      message: t(
        "{provider} will no longer be used for streaming. Its API key is deleted from Ether.",
        { provider: providerName(draft, index) },
      ),
      confirmLabel: t("Remove"),
      tone: "danger",
    });
    if (!ok) return;
    setSaving(true);
    setError(null);
    try {
      await commit(
        providers.filter((_, j) => j !== index),
        t("Provider removed"),
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
      await commit(swap(providers, i, i + d), t("Provider order saved"));
    } catch (e) {
      toastError(t("Couldn't reorder providers"), e);
    }
  }

  if (config.error) return <ErrorPanel error={config.error} onRetry={() => void config.reload()} />;
  if (!cfg || !debrid) return <LoadingRows rows={3} />;

  const draft = editing?.draft;
  const set = (patch: Partial<DebridProvider>) =>
    setEditing((e) => (e ? { ...e, draft: { ...e.draft, ...patch } } : e));
  const isNew = editing ? editing.index >= providers.length : false;
  const name = draft && editing ? providerName(draft, editing.index) : "";
  const needsTorrentSource = hasUsableDebrid(cfg) && torrentIndexers === 0;

  return (
    <>
      {needsTorrentSource ? (
        <div role="status">
          <Alert variant="warning">
            <AlertDescription>
              {tx("Debrid is connected but no torrent source is set up yet. {action}", {
                action: (
                  <Link to="/sources/indexers" className="font-medium underline underline-offset-2">
                    {t("Add one")}
                  </Link>
                ),
              })}
            </AlertDescription>
          </Alert>
        </div>
      ) : null}

      <Section
        id="sources-debrid"
        title={t("Debrid providers")}
        description={t(
          "Ether asks these accounts in order when a torrent needs a debrid service. The first one that can stream it is used.",
        )}
        actions={
          <Button size="sm" onClick={() => open(providers.length, blankProvider(providers.length))}>
            <Plus size={14} /> {t("Add a debrid service")}
          </Button>
        }
        bodyClassName="space-y-3 p-4"
      >
        {providers.length === 0 ? (
          <EmptyState
            icon={<Cloud />}
            title={t("No debrid services yet")}
            description={t(
              "Add Real-Debrid, TorBox, AllDebrid, Premiumize, Debrid-Link or Offcloud to stream torrent releases.",
            )}
          />
        ) : (
          <SortableList
            items={providers}
            getId={(p) => p.id}
            className="space-y-3"
            onReorder={(next) => void commit(next, t("Provider order saved"))}
            renderItem={(p, handle, i) => {
              const s = live.find((x) => x.id === p.id);
              const label = providerName(p, i);
              const latency = latencySummary(s?.latency);
              const details: ReactNode = (
                <>
                  {[role(i), ...(s ? activity(s) : [])].join(" · ")}
                  {latency ? <span className="block tabular-nums">{latency}</span> : null}
                  {s?.lastError ? (
                    <span className="block truncate text-danger" title={s.lastError}>
                      {s.lastError}
                    </span>
                  ) : null}
                </>
              );
              return (
                <SourceCard
                  name={label}
                  position={i + 1}
                  description={details}
                  problem={debridFix(p.enabled, s?.state)}
                  meta={
                    <>
                      <Badge variant="outline">{debridKindLabel(p.kind)}</Badge>
                      <StateBadge p={p} s={s} />
                    </>
                  }
                  order={
                    <OrderButtons
                      label={label}
                      i={i}
                      n={providers.length}
                      onMove={(d) => void move(i, d)}
                    />
                  }
                  handle={handle}
                  editLabel={t("Edit {name}", { name: label })}
                  onEdit={() => open(i, p)}
                  logo={debridMark(p.kind) ?? undefined}
                />
              );
            }}
          />
        )}
        {engine ? (
          <p
            aria-label={t("Streaming engine")}
            className="border-t border-border px-5 py-3 text-xs text-fg-muted tabular-nums"
          >
            <span className="font-medium text-fg">{t("Streaming engine")}</span> · {engine}
          </p>
        ) : null}
      </Section>

      <DebridSettings debrid={debrid} onSaved={reloadAll} />

      <RecordDrawer
        open={editing !== null}
        onClose={close}
        title={isNew ? t("New debrid provider") : name}
        description={editing && !isNew ? role(editing.index) : t("Debrid account")}
        onSave={() => void save()}
        saving={saving}
        saveLabel={t("Save provider")}
        onRemove={isNew ? undefined : () => void remove()}
        removeLabel={t("Remove provider")}
        error={error}
        tools={
          draft ? (
            <TestButton
              state={tests[draft.id] ?? { status: "idle" }}
              onTest={() => runTest(draft.id, () => api.testDebrid(draft))}
              disabled={!draft.apiKey}
            />
          ) : null
        }
      >
        {draft ? (
          <SettingsSection title={t("Account")}>
            <TextRow
              label={t("Name")}
              value={draft.name}
              placeholder={debridKindLabel(draft.kind)}
              error={errors.name}
              onChange={(v) => set({ name: v })}
            />
            <SettingsRow
              title={t("Service")}
              description={
                errors.kind ? (
                  <span className="block text-xs text-danger" role="alert">
                    {errors.kind}
                  </span>
                ) : undefined
              }
              control={
                <div className="w-48">
                  <SearchSelect
                    value={draft.kind}
                    onChange={(v) => set({ kind: v as DebridKind })}
                    options={DEBRID_KINDS}
                    ariaLabel={t("Service")}
                    searchable={false}
                    placeholder={t("Select…")}
                    selectLabel={t("Select")}
                    closeLabel={t("Close")}
                  />
                </div>
              }
            />
            <SettingsRow
              stacked
              title={t("API key")}
              description={
                <>
                  {t("From your debrid account's settings.")}{" "}
                  {DEBRID_TOKEN_URL[draft.kind] ? (
                    <a
                      className="text-primary underline"
                      href={DEBRID_TOKEN_URL[draft.kind]}
                      target="_blank"
                      rel="noreferrer"
                    >
                      {t("Find your {service} API token", {
                        service: debridKindLabel(draft.kind),
                      })}
                    </a>
                  ) : null}
                  {errors.apiKey ? (
                    <span className="block text-xs text-danger" role="alert">
                      {errors.apiKey}
                    </span>
                  ) : null}
                </>
              }
            >
              <SecretInput
                ariaLabel={t("API key")}
                value={draft.apiKey}
                invalid={Boolean(errors.apiKey)}
                onChange={(v) => set({ apiKey: v })}
              />
            </SettingsRow>
            <BoolRow
              label={t("Enabled")}
              value={draft.enabled}
              onChange={(v) => set({ enabled: v })}
            />
          </SettingsSection>
        ) : null}
      </RecordDrawer>
    </>
  );
}

type Obj = Record<string, unknown>;

/** cachedOnly, removeUncached and the advanced streaming knobs, edited in place. */
function DebridSettings({
  debrid,
  onSaved,
}: {
  debrid: DebridConfig;
  onSaved: () => Promise<void>;
}) {
  const saved = useMemo<Obj>(() => {
    const { providers: _providers, ...rest } = debrid;
    return {
      chunkBytes: 0,
      readAheadChunks: 0,
      maxConnections: 0,
      ...rest,
    };
  }, [debrid]);
  const { draft, setDraft, dirty, reset } = useDraft<Obj>(saved);
  const [errors, setErrors] = useState<Record<string, string>>({});
  const [error, setError] = useState<unknown>(null);
  const [saving, setSaving] = useState(false);

  async function save() {
    if (!draft) return;
    setSaving(true);
    setError(null);
    setErrors({});
    try {
      // Providers come from the last saved config so unsaved settings never
      // resurrect a removed provider (their keys stay redacted and are kept).
      await api.putSection("debrid", { ...debrid, ...draft });
      toast.success(t("Settings saved"), { description: t("Debrid") });
      await onSaved();
    } catch (e) {
      setError(e);
      if (e instanceof ApiError && e.fields)
        setErrors(Object.fromEntries(e.fields.map((f) => [f.field, f.message])));
    } finally {
      setSaving(false);
    }
  }

  if (!draft) return null;
  // Connection knobs stay under More options whether the schema lists them as
  // ordinary fields or on FieldGroup.more. omit is not used here: the draft
  // still saves every key.
  const power = new Set(["chunkBytes", "readAheadChunks", "maxConnections"]);
  const collapsed = new Map<string, (typeof DEBRID)[number]["fields"][number]>();
  for (const group of DEBRID) {
    for (const item of [...group.fields, ...(group.more ?? [])]) {
      if (power.has(item.key) || group.more?.some((extra) => extra.key === item.key)) {
        collapsed.set(item.key, item);
      }
    }
  }
  return (
    <>
      {DEBRID.map((g) => {
        const fields = g.fields.filter((f) => !collapsed.has(f.key));
        if (!fields.length) return null;
        return (
          <SettingsSection key={g.title} title={g.title} description={g.description}>
            {fields.map((f) => (
              <SchemaField
                key={f.key}
                def={f}
                value={draft[f.key]}
                path={`debrid.${f.key}`}
                error={errors[`debrid.${f.key}`]}
                onChange={(v) => setDraft({ ...draft, [f.key]: v })}
              />
            ))}
          </SettingsSection>
        );
      })}
      {collapsed.size ? (
        <MoreOptions label={t("More options")}>
          <p className="px-4 pt-1 text-sm text-fg-muted">
            {t(
              "Leave these at 0. Ether already chooses a fast first byte and how far to read ahead. Change them only when a player stutters.",
            )}
          </p>
          <div>
            {[...collapsed.values()].map((def) => (
              <SchemaField
                key={def.key}
                def={def}
                value={draft[def.key]}
                path={`debrid.${def.key}`}
                error={errors[`debrid.${def.key}`]}
                onChange={(v) => setDraft({ ...draft, [def.key]: v })}
              />
            ))}
          </div>
        </MoreOptions>
      ) : null}
      <SaveBar
        dirty={dirty}
        saving={saving}
        error={error}
        onSave={save}
        onDiscard={() => {
          reset();
          setErrors({});
          setError(null);
        }}
      />
    </>
  );
}
