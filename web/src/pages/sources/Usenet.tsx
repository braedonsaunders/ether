import { t, tn } from "../../i18n";
import {
  Badge,
  Button,
  EmptyState,
  SettingsSection,
  confirmDialog,
  toast,
} from "@braedonsaunders/appkit-ui";
import { Network, Plus } from "lucide-react";
import { useEffect, useRef, useState } from "react";
import { useApp } from "../../app-context";
import { MoreOptions } from "../../components/ConfigSection";
import { ErrorPanel, LoadingRows } from "../../components/feedback";
import { SecretInput, TestButton } from "../../components/fields";
import { Section } from "../../components/layout";
import { SortableList } from "../../components/Sortable";
import { SettingsRow } from "../../components/SettingsRow";
import { api, newId } from "../../lib/api";
import { formatBytes, isPlainObject } from "../../lib/format";
import { useAsync } from "../../lib/hooks";
import type { EtherConfig, UsenetProvider } from "../../lib/types";
import { SourceCard, usenetFix } from "./cards";
import {
  BoolRow,
  NumRow,
  OrderButtons,
  RecordDrawer,
  TextRow,
  recordErrors,
  swap,
  toastError,
  useTests,
  type Errors,
} from "./form";

function blankProvider(priority: number): UsenetProvider {
  return {
    id: newId("prov"),
    name: "",
    host: "",
    port: 563,
    tls: true,
    username: "",
    password: "",
    connections: 20,
    priority,
    backfill: false,
    enabled: true,
  };
}

function providerName(p: UsenetProvider, i: number) {
  return p.name || p.host || t("Provider {number}", { number: i + 1 });
}

function role(p: UsenetProvider, i: number) {
  return i === 0
    ? t("Tried first")
    : p.backfill
      ? t("Backfill")
      : t("Priority {number}", { number: i + 1 });
}

/**
 * Connection use for a provider row. In a cluster the account's connections
 * are divided among the nodes (nodeShare), so busy counts are out of this
 * node's share. When the server refused connections, Ether learned a lower
 * limit and runs at maxConnections until it widens back.
 */
export function connectionText(stat: Record<string, unknown> | undefined, configured: number) {
  if (!stat) return [tn(configured, "{count} connection", "{count} connections")];
  const total = Number(stat.configuredConnections ?? configured);
  const share = Number(stat.nodeShare ?? 0);
  const active = Number(stat.activeConnections ?? 0);
  const learned = stat.limitLearned
    ? [
        tn(
          share > 0 ? share : total,
          "using {using} of {count} connection (server limit learned)",
          "using {using} of {count} connections (server limit learned)",
          { using: Number(stat.maxConnections ?? 0) },
        ),
      ]
    : [];
  if (share > 0)
    return [
      tn(share, "{active} of {count} connection busy", "{active} of {count} connections busy", {
        active,
      }),
      tn(
        total,
        "this node: {share} of {count} connection",
        "this node: {share} of {count} connections",
        {
          share,
        },
      ),
      ...learned,
    ];
  return [
    tn(total, "{active} of {count} connection busy", "{active} of {count} connections busy", {
      active,
    }),
    ...learned,
  ];
}

const DEFAULT_REPAIR: NonNullable<EtherConfig["usenet"]["repair"]> = {
  enabled: false,
  summaryRows: 4,
  adaptiveSummaries: true,
  maxDownloadBytes: 512 * 1024 * 1024,
  liveWait: "2s",
  aheadWait: "1m30s",
};

/** Usenet providers in priority order; each provider is edited in a drawer. */
export function UsenetProviders({
  addNonce,
}: {
  /** Changes when the add gallery asks for a new provider. */
  addNonce?: number | null;
} = {}) {
  const { dashboard, refreshDashboard } = useApp();
  const config = useAsync(() => api.getConfig(), []);
  const seenAdd = useRef(0);
  const [repairDraft, setRepairDraft] = useState<NonNullable<
    EtherConfig["usenet"]["repair"]
  > | null>(null);
  const [editing, setEditing] = useState<{ index: number; draft: UsenetProvider } | null>(null);
  const [errors, setErrors] = useState<Errors>({});
  const [error, setError] = useState<unknown>(null);
  const [saving, setSaving] = useState(false);
  const { tests, runTest, reset } = useTests();

  const cfg = config.data;
  const providers = cfg?.usenet.providers ?? [];
  const status = dashboard?.sections?.providers;
  const live = Array.isArray(status) ? status.filter(isPlainObject) : [];

  async function commit(list: UsenetProvider[], message: string) {
    if (!cfg) return;
    const usenet: EtherConfig["usenet"] = {
      ...cfg.usenet,
      providers: list.map((p, priority) => ({ ...p, priority })),
    };
    await api.putSection("usenet", usenet);
    toast.success(message);
    await config.reload();
    void refreshDashboard();
  }

  async function saveRepair() {
    if (!cfg || !repairDraft) return;
    setSaving(true);
    setError(null);
    try {
      await api.putSection("usenet", { ...cfg.usenet, repair: repairDraft });
      toast.success(t("Repair settings saved"));
      setRepairDraft(null);
      await config.reload();
    } catch (e) {
      setError(e);
    } finally {
      setSaving(false);
    }
  }
  const open = (index: number, draft: UsenetProvider) => {
    setErrors({});
    setError(null);
    reset(draft.id);
    setEditing({ index, draft: structuredClone(draft) });
  };
  useEffect(() => {
    if (!addNonce || !cfg || seenAdd.current === addNonce) return;
    seenAdd.current = addNonce;
    open(providers.length, blankProvider(providers.length));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [addNonce, cfg, providers.length]);
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
      setErrors(recordErrors(e, `usenet.providers[${index}].`));
    } finally {
      setSaving(false);
    }
  }

  async function remove() {
    if (!editing) return;
    const { index, draft } = editing;
    const ok = await confirmDialog({
      title: t("Remove provider?"),
      message: t("{provider} will no longer be used for streaming.", {
        provider: providerName(draft, index),
      }),
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
  if (!cfg) return <LoadingRows rows={3} />;

  const draft = editing?.draft;
  const set = (patch: Partial<UsenetProvider>) =>
    setEditing((e) => (e ? { ...e, draft: { ...e.draft, ...patch } } : e));
  const isNew = editing ? editing.index >= providers.length : false;
  const name = draft && editing ? providerName(draft, editing.index) : "";

  return (
    <>
      <Section
        id="sources-usenet"
        title={t("Usenet providers")}
        description={t(
          "Ether downloads articles from these accounts in order. A backfill account is used only when the article is missing everywhere else.",
        )}
        actions={
          <Button size="sm" onClick={() => open(providers.length, blankProvider(providers.length))}>
            <Plus size={14} /> {t("Add a Usenet provider")}
          </Button>
        }
        bodyClassName="space-y-3 p-4"
      >
        {providers.length === 0 ? (
          <EmptyState
            icon={<Network />}
            title={t("No Usenet providers yet")}
            description={t(
              "Add the account from your Usenet plan. A block account can be backfill.",
            )}
          />
        ) : (
          <SortableList
            items={providers}
            getId={(p) => p.id}
            className="space-y-3"
            onReorder={(next) => void commit(next, t("Provider order saved"))}
            renderItem={(p, handle, i) => {
              const stat = live.find((s) => s.id === p.id);
              const label = providerName(p, i);
              return (
                <SourceCard
                  name={label}
                  position={i + 1}
                  description={[
                    role(p, i),
                    p.host ? `${p.host}:${p.port}` : t("No host"),
                    ...connectionText(stat, p.connections),
                    stat?.bytes ? t("{size} fetched", { size: formatBytes(stat.bytes) }) : "",
                  ]
                    .filter(Boolean)
                    .join(" · ")}
                  problem={usenetFix(p.enabled, stat?.state)}
                  meta={
                    <>
                      {!p.enabled ? <Badge variant="secondary">{t("Off")}</Badge> : null}
                      {stat ? (
                        <Badge variant={stat.state === "ok" ? "success" : "warning"}>
                          {stat.state === "ok"
                            ? t("Connected")
                            : stat.state != null
                              ? String(stat.state)
                              : t("Unknown")}
                        </Badge>
                      ) : null}
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
                />
              );
            }}
          />
        )}
      </Section>

      <Section
        title={t("Repair missing pieces")}
        description={t(
          "While a video is playing, Ether can fetch a little extra to fill gaps. Playback still switches sources if that cannot finish in time. This uses extra bandwidth.",
        )}
      >
        <SettingsRow
          title={cfg?.usenet.repair?.enabled ? t("Repair enabled") : t("Repair disabled")}
          control={
            <Button
              variant="outline"
              onClick={() => {
                setError(null);
                setRepairDraft(cfg?.usenet.repair ?? DEFAULT_REPAIR);
              }}
            >
              {t("Configure repair")}
            </Button>
          }
        />
      </Section>
      <RecordDrawer
        open={repairDraft !== null}
        onClose={() => {
          if (!saving) setRepairDraft(null);
        }}
        title={t("Repair missing pieces")}
        onSave={() => void saveRepair()}
        saving={saving}
        saveLabel={t("Save repair settings")}
        error={error}
      >
        {repairDraft ? (
          <SettingsSection title={t("Recovery")}>
            <BoolRow
              label={t("Enable repair")}
              value={repairDraft.enabled}
              onChange={(enabled) => setRepairDraft({ ...repairDraft, enabled })}
            />
            <NumRow
              label={t("Extra download budget")}
              value={Math.round((repairDraft.maxDownloadBytes || 512 * 1024 * 1024) / 1024 / 1024)}
              min={32}
              max={4096}
              unit="MiB"
              help={t(
                "How much extra Ether may download for one repair. 512 MiB is plenty for most gaps.",
              )}
              onChange={(n) =>
                setRepairDraft({
                  ...repairDraft,
                  maxDownloadBytes: Math.min(4096, Math.max(32, n)) * 1024 * 1024,
                })
              }
            />
            <MoreOptions label={t("More options")}>
              <BoolRow
                label={t("Remember downloaded data")}
                value={repairDraft.summaryRows > 0}
                help={t(
                  "After damage is detected, retain repair information when its expected benefit justifies the memory and CPU cost.",
                )}
                onChange={(enabled) =>
                  setRepairDraft({
                    ...repairDraft,
                    summaryRows: enabled ? 4 : 0,
                    adaptiveSummaries: true,
                  })
                }
              />
            </MoreOptions>
          </SettingsSection>
        ) : null}
      </RecordDrawer>

      <RecordDrawer
        open={editing !== null}
        onClose={close}
        title={isNew ? t("New provider") : name}
        description={editing && !isNew ? role(editing.draft, editing.index) : undefined}
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
              onTest={() => runTest(draft.id, () => api.testUsenet(draft))}
              disabled={!draft.host}
            />
          ) : null
        }
      >
        {draft ? (
          <SettingsSection title={t("Connection")}>
            <TextRow label={t("Name")} value={draft.name} onChange={(v) => set({ name: v })} />
            <TextRow
              label={t("Host")}
              value={draft.host}
              placeholder="news.example.com"
              error={errors.host}
              onChange={(v) => set({ host: v })}
            />
            <NumRow
              label={t("Port")}
              value={draft.port}
              min={1}
              max={65535}
              error={errors.port}
              onChange={(n) => set({ port: n })}
            />
            <BoolRow
              label={t("SSL / TLS")}
              value={draft.tls}
              onChange={(v) =>
                set({
                  tls: v,
                  port: v && draft.port === 119 ? 563 : !v && draft.port === 563 ? 119 : draft.port,
                })
              }
            />
            <TextRow
              label={t("Username")}
              value={draft.username}
              error={errors.username}
              onChange={(v) => set({ username: v })}
            />
            <SettingsRow stacked title={t("Password")}>
              <SecretInput
                ariaLabel={t("Password")}
                value={draft.password}
                onChange={(v) => set({ password: v })}
              />
            </SettingsRow>
            <MoreOptions label={t("More options")}>
              <NumRow
                label={t("Connections")}
                value={draft.connections}
                min={0}
                max={500}
                error={errors.connections}
                help={t(
                  "How many downloads Ether opens at once. 20 suits a typical plan. Lower it only if the provider cuts you off.",
                )}
                onChange={(n) => set({ connections: n })}
              />
            </MoreOptions>
            <BoolRow
              label={t("Backfill only")}
              value={draft.backfill}
              help={t(
                "Use for block accounts: only consulted when an article is missing elsewhere.",
              )}
              onChange={(v) => set({ backfill: v })}
            />
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
