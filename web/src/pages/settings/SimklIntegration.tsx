import { Button, SettingsSection } from "@braedonsaunders/appkit-ui";
import { useEffect, useRef, useState } from "react";
import { getLocale, localized, t, tn, tx } from "../../i18n";
import { api, apiFetch } from "../../lib/api";
import { formatNumber } from "../../lib/format";
import type { EtherUser } from "../../lib/types";

export type SimklSettings = { enabled: boolean; clientId: string; clientSecret: string };
type Status = {
  connected: boolean;
  needsReconnect?: boolean;
  lastSync?: string;
  warning?: string;
  queued?: number;
  backoffUntil?: number;
};
type Device = {
  userCode: string;
  verificationUri: string;
  verificationUriComplete: string;
  expiresAt: string;
  interval: number;
};

/** Human text for the sync warning codes the server reports. */
const WARNINGS = localized<Record<string, string>>(() => ({
  auth_revoked: t("Simkl authorization expired or was revoked. Reconnect to continue."),
  queue_full: t("The delivery queue is full; some completions were not queued."),
  anime_mapping: t("An anime episode was skipped because its numbering could not be verified."),
  not_found: t("Simkl could not resolve a history item."),
}));

export function SimklIntegration({
  settings: controlled,
  onSettings,
  onPersisted,
  persist,
  disabled,
}: {
  /** Parent draft. When set, this form does not refetch settings over unsaved edits. */
  settings?: SimklSettings | null;
  onSettings?: (next: SimklSettings) => void;
  /** Server document from a successful save, including redacted secrets. */
  onPersisted?: (next: SimklSettings) => void;
  persist?: (task: () => Promise<void>) => Promise<unknown>;
  disabled?: boolean;
} = {}) {
  const [local, setLocal] = useState<SimklSettings | null>(null);
  const settings = onSettings ? (controlled ?? null) : local;
  const setSettings = (next: SimklSettings) => {
    if (onSettings) onSettings(next);
    else setLocal(next);
  };
  const settingsRef = useRef(settings);
  settingsRef.current = settings;
  const [users, setUsers] = useState<EtherUser[]>([]);
  const [viewer, setViewer] = useState(0);
  const [status, setStatus] = useState<Status | null>(null);
  const [device, setDevice] = useState<Device | null>(null);
  const [message, setMessage] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  useEffect(() => {
    let live = true;
    api.users().then(
      (list) => {
        if (!live) return;
        setUsers(list);
        setViewer(list[0]?.id ?? 0);
      },
      (e) => {
        if (live) setError((e as Error).message);
      },
    );
    return () => {
      live = false;
    };
  }, []);
  useEffect(() => {
    if (onSettings) return;
    let live = true;
    apiFetch<{ value: SimklSettings }>("/config/simkl").then(
      (cfg) => {
        if (live) setLocal(cfg.value);
      },
      (e) => {
        if (live) setError((e as Error).message);
      },
    );
    return () => {
      live = false;
    };
  }, [onSettings]);
  useEffect(() => {
    let live = true;
    setDevice(null);
    setStatus(null);
    setMessage("");
    if (viewer)
      apiFetch<Status>(`/simkl/${viewer}/status`).then(
        (s) => {
          if (live) setStatus(s);
        },
        (e) => {
          if (live) setError((e as Error).message);
        },
      );
    return () => {
      live = false;
    };
  }, [viewer]);
  const refresh = async () => setStatus(await apiFetch<Status>(`/simkl/${viewer}/status`));
  const act = async (fn: () => Promise<void>) => {
    setBusy(true);
    setError("");
    setMessage("");
    try {
      await fn();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  };
  const post = <T,>(path: string) => apiFetch<T>(`/simkl/${viewer}/${path}`, { method: "POST" });
  const verification = (() => {
    if (!device) return "";
    try {
      const u = new URL(device.verificationUriComplete || device.verificationUri);
      return u.protocol === "https:" && u.hostname === "simkl.com" ? u.href : "";
    } catch {
      return "";
    }
  })();
  return (
    <SettingsSection
      title="Simkl"
      description={t(
        "Connect a viewer's Simkl account to import watched history and send completed playback.",
      )}
    >
      {error && (
        <p role="alert" className="text-sm text-danger">
          {error}
        </p>
      )}
      {message && (
        <p role="status" className="text-sm">
          {message}
        </p>
      )}
      {settings && (
        <div className="space-y-3">
          {settings.enabled && !settings.clientId.trim() ? (
            <p role="status" className="text-sm text-fg-muted">
              {t("Simkl needs a client ID before it can connect.")}
            </p>
          ) : null}
          <label className="block">
            <input
              type="checkbox"
              checked={settings.enabled}
              onChange={(e) => setSettings({ ...settings, enabled: e.target.checked })}
            />{" "}
            {t("Enable Simkl")}
          </label>
          <label className="block">
            {t("Client ID")}{" "}
            <input
              className="w-full rounded border border-border bg-surface px-2 py-1"
              value={settings.clientId}
              onChange={(e) => setSettings({ ...settings, clientId: e.target.value })}
            />
          </label>
          <label className="block">
            {t("Client secret")}{" "}
            <input
              className="w-full rounded border border-border bg-surface px-2 py-1"
              type="password"
              value={settings.clientSecret}
              onChange={(e) => setSettings({ ...settings, clientSecret: e.target.value })}
            />
          </label>
          <Button
            disabled={busy || disabled}
            onClick={() => {
              const run = () =>
                act(async () => {
                  const current = settingsRef.current;
                  if (!current) return;
                  const saved = await api.putSection("simkl", current);
                  const value = (saved.value ?? current) as SimklSettings;
                  if (onPersisted) onPersisted(value);
                  else setLocal(value);
                  setDevice(null);
                  setMessage(t("Simkl settings saved."));
                });
              if (persist) void persist(run);
              else void run();
            }}
          >
            {t("Save Simkl settings")}
          </Button>
        </div>
      )}
      <div className="mt-4 space-y-3">
        <label className="block">
          {t("Viewer")}{" "}
          <select
            disabled={busy}
            value={viewer}
            onChange={(e) => setViewer(Number(e.target.value))}
            className="rounded border border-border bg-surface px-2 py-1"
          >
            {users.map((u) => (
              <option key={u.id} value={u.id}>
                {u.title || u.username}
              </option>
            ))}
          </select>
        </label>
        {status && (
          <p className="text-sm">
            {status.connected
              ? status.needsReconnect
                ? t("Reconnect required")
                : t("Connected")
              : t("Not connected")}
          </p>
        )}
        {status?.queued ? (
          <p className="text-sm">
            {t("Queued history entries: {count}", { count: formatNumber(status.queued) })}
          </p>
        ) : null}
        {status?.lastSync && (
          <p className="text-sm text-fg-muted">
            {t("Last imported activity: {when}", { when: status.lastSync })}
          </p>
        )}
        {status?.backoffUntil && status.backoffUntil > Date.now() ? (
          <p className="text-sm text-fg-muted">
            {t("Sync paused until {when}.", {
              when: new Date(status.backoffUntil).toLocaleString(getLocale()),
            })}
          </p>
        ) : null}
        {status?.warning && (
          <p className="text-sm text-fg-muted">
            {WARNINGS[status.warning] ?? t("A history item could not be sent.")}
          </p>
        )}
        {device && (
          <div className="space-y-2">
            <p>
              {tx("Open the Simkl approval page and enter code {code}.", {
                code: <strong>{device.userCode}</strong>,
              })}
            </p>
            {verification && (
              <a
                href={verification}
                target="_blank"
                rel="noopener noreferrer"
                className="underline"
              >
                {t("Open Simkl approval")}
              </a>
            )}
            <p className="text-sm text-fg-muted">
              {tn(
                device.interval,
                "The code expires at {time}. Wait at least {count} second between checks.",
                "The code expires at {time}. Wait at least {count} seconds between checks.",
                { time: new Date(device.expiresAt).toLocaleTimeString(getLocale()) },
              )}
            </p>
            <Button
              disabled={busy}
              onClick={() =>
                void act(async () => {
                  const r = await post<{ status: string }>("poll");
                  if (r.status === "authorized") {
                    setDevice(null);
                    await refresh();
                    setMessage(t("Simkl account connected."));
                  } else setMessage(t("Waiting for approval."));
                })
              }
            >
              {t("Check connection")}
            </Button>
          </div>
        )}
        <div className="flex flex-wrap gap-2">
          <Button
            disabled={busy || !viewer || !settings?.enabled}
            onClick={() =>
              void act(async () => {
                setDevice(await post<Device>("start"));
              })
            }
          >
            {t("Connect")}
          </Button>
          <Button
            disabled={
              busy || !viewer || !status?.connected || status.needsReconnect || !settings?.enabled
            }
            onClick={() =>
              void act(async () => {
                const r = await post<{ imported: number; skipped: number }>("sync");
                await refresh();
                setMessage(
                  tn(
                    r.imported,
                    "Processed {count} history entry; skipped {skipped}.",
                    "Processed {count} history entries; skipped {skipped}.",
                    { skipped: formatNumber(r.skipped) },
                  ),
                );
              })
            }
          >
            {t("Sync history")}
          </Button>
          <Button
            disabled={busy || !viewer || (!status?.connected && !device)}
            onClick={() =>
              void act(async () => {
                await post("disconnect");
                setDevice(null);
                await refresh();
                setMessage(t("Simkl disconnected."));
              })
            }
          >
            {t("Disconnect")}
          </Button>
        </div>
        <p className="text-sm text-fg-muted">
          {t(
            "Sync keeps existing history and does not remove watched items. Anime episodes without a verified mapping are skipped.",
          )}
        </p>
      </div>
    </SettingsSection>
  );
}
