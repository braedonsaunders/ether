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
import { useState } from "react";
import { useApp } from "../../app-context";
import { ErrorPanel, LoadingRows } from "../../components/feedback";
import { SecretInput, TestButton } from "../../components/fields";
import { Section } from "../../components/layout";
import { SettingsRow } from "../../components/SettingsRow";
import { api, newId } from "../../lib/api";
import { formatBytes, isPlainObject } from "../../lib/format";
import { useAsync } from "../../lib/hooks";
import type { EtherConfig, UsenetProvider } from "../../lib/types";
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
 * node's share.
 */
export function connectionText(stat: Record<string, unknown> | undefined, configured: number) {
  if (!stat) return [tn(configured, "{count} connection", "{count} connections")];
  const total = Number(stat.configuredConnections ?? configured);
  const share = Number(stat.nodeShare ?? 0);
  const active = Number(stat.activeConnections ?? 0);
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
    ];
  return [
    tn(total, "{active} of {count} connection busy", "{active} of {count} connections busy", {
      active,
    }),
  ];
}

/** Usenet providers in priority order; each provider is edited in a drawer. */
export function UsenetProviders() {
  const { dashboard, refreshDashboard } = useApp();
  const config = useAsync(() => api.getConfig(), []);
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
        title={t("Providers")}
        description={t(
          "Tried in order. Backfill providers are only used for articles missing everywhere else.",
        )}
        actions={
          <Button size="sm" onClick={() => open(providers.length, blankProvider(providers.length))}>
            <Plus size={14} /> {t("Add provider")}
          </Button>
        }
        bodyClassName=""
      >
        {providers.length === 0 ? (
          <EmptyState
            icon={<Network />}
            title={t("No providers")}
            description={t("Add your Usenet provider. Block accounts can be added as backfill.")}
          />
        ) : (
          <ul className="divide-y divide-border">
            {providers.map((p, i) => {
              const stat = live.find((s) => s.id === p.id);
              const label = providerName(p, i);
              return (
                <RecordItem
                  key={p.id}
                  title={label}
                  description={[
                    role(p, i),
                    p.host ? `${p.host}:${p.port}` : t("No host"),
                    ...connectionText(stat, p.connections),
                    stat?.bytes ? t("{size} fetched", { size: formatBytes(stat.bytes) }) : "",
                  ]
                    .filter(Boolean)
                    .join(" · ")}
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
                  editLabel={t("Edit {name}", { name: label })}
                  onEdit={() => open(i, p)}
                />
              );
            })}
          </ul>
        )}
      </Section>

      <Section
        title={t("Repair Ahead")}
        description={t(
          "Restore missing source data during readahead when it can finish within your limits.",
        )}
      >
        <SettingsRow
          title={cfg?.usenet.repair?.enabled ? t("Repair enabled") : t("Repair disabled")}
          description={t(
            "Repair uses additional download bandwidth. Playback can still switch sources when repair cannot finish in time.",
          )}
          control={
            <Button
              variant="outline"
              onClick={() => {
                setError(null);
                setRepairDraft(
                  cfg?.usenet.repair ?? {
                    enabled: false,
                    summaryRows: 4,
                    adaptiveSummaries: true,
                    maxDownloadBytes: 512 * 1024 * 1024,
                    liveWait: "2s",
                    aheadWait: "1m30s",
                  },
                );
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
        title={t("Repair Ahead")}
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
                "Additional transfer budget per repair attempt; requests already in flight can finish.",
              )}
              onChange={(n) =>
                setRepairDraft({
                  ...repairDraft,
                  maxDownloadBytes: Math.min(4096, Math.max(32, n)) * 1024 * 1024,
                })
              }
            />
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
            <NumRow
              label={t("Connections")}
              value={draft.connections}
              min={0}
              max={500}
              error={errors.connections}
              help={t("Your plan's connection limit.")}
              onChange={(n) => set({ connections: n })}
            />
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
