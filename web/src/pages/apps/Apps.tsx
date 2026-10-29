import { Button, Input, Switch, toast } from "@braedonsaunders/appkit-ui";
import { useEffect, useState } from "react";
import { Link, useNavigate } from "react-router-dom";
import { useApp } from "../../app-context";
import { ConfigSection } from "../../components/ConfigSection";
import { RemoteDiagnostics } from "../../components/RemoteDiagnostics";
import { api } from "../../lib/api";
import * as schema from "../settings/schema";
import { PlexStatus } from "../settings/sections";
import { ErrorPanel, LoadingRows } from "../../components/feedback";
import { DomainPage, Section, useRouteTab } from "../../components/layout";
import { SaveBar, useDraft } from "../../components/SaveBar";
import { localized, t } from "../../i18n";
import { API_BASE, apiBlob, apiFetch } from "../../lib/api";
import { useAsync, useInterval } from "../../lib/hooks";

type FrontendInfo = {
  descriptor: {
    id: string;
    name: string;
    version?: string;
    certifiedApps?: { name: string; steps: string[] }[];
    defaultPort?: number;
  };
  enabled: boolean;
  status: {
    addresses: string[] | null;
    connectUrls?: string[];
    lan: string;
    wan: string;
    clients24h: number;
    clients24hKnown?: boolean;
    liveStreams: number;
    liveStreamsKnown?: boolean;
    targetVersion: string;
    conformance: string;
    error?: string;
    warnings?: string[];
  };
};
type Settings = {
  port?: number;
  serverName?: string;
  discovery?: boolean;
  quickConnect?: boolean;
  publicViewerPicker?: boolean;
};
const tabs = localized(
  () =>
    [
      { key: "overview", label: t("Overview") },
      { key: "plex", label: t("Plex") },
      { key: "jellyfin", label: t("Jellyfin") },
      { key: "emby", label: t("Emby") },
      { key: "silo", label: t("Silo") },
      { key: "stremio", label: t("Stremio") },
      { key: "files", label: t("Files") },
      { key: "dlna", label: t("DLNA") },
      { key: "remote", label: t("Remote access") },
      { key: "secure", label: t("Secure connections") },
    ] as const,
);
const protocols = localized(
  () =>
    [
      { id: "plex", name: t("Plex") },
      { id: "jellyfin", name: t("Jellyfin") },
      { id: "emby", name: t("Emby") },
      { id: "silo", name: t("Silo") },
      { id: "stremio", name: t("Stremio") },
      { id: "webdav", name: t("Files") },
      { id: "dlna", name: t("DLNA") },
    ] as const,
);
const pathFor = (id: string) => (id === "webdav" ? "files" : id);
const protocolFor = (tab: string) => (tab === "files" ? "webdav" : tab);

function reachability(value: string) {
  switch (value) {
    case "reachable":
      return t("Reachable");
    case "unreachable":
      return t("Unreachable");
    default:
      return t("Unknown");
  }
}
function conformance(value: string) {
  switch (value) {
    case "passing":
      return t("Passing");
    case "failing":
      return t("Failing");
    case "partial":
      return t("Partial");
    default:
      return t("Unknown");
  }
}

function connectionStep(code: string) {
  switch (code) {
    case "openApp":
      return t("Open the app on your device.");
    case "addServer":
      return t("Add the server address shown above.");
    case "signIn":
      return t("Sign in with your viewer account.");
    case "installAddon":
      return t("Install the addon using the address shown above.");
    default:
      return null;
  }
}

function ConnectDevice({ info }: { info: FrontendInfo }) {
  const [open, setOpen] = useState(false);
  const [qr, setQR] = useState<string>();
  const [selectedURL, setSelectedURL] = useState("");
  const urls = info.status.connectUrls ?? [];
  const url = urls.includes(selectedURL) ? selectedURL : urls[0];
  useEffect(() => {
    if (!open || !url) return;
    const controller = new AbortController();
    let objectURL: string | undefined;
    void apiBlob(`${API_BASE}/frontends/qr?url=${encodeURIComponent(url)}`, controller.signal)
      .then((blob) => {
        if (!controller.signal.aborted) {
          objectURL = URL.createObjectURL(blob);
          setQR(objectURL);
        }
      })
      .catch(() => setQR(undefined));
    return () => {
      controller.abort();
      if (objectURL) URL.revokeObjectURL(objectURL);
      setQR(undefined);
    };
  }, [open, url]);
  if (!info.enabled || !url) return null;
  return (
    <div>
      <Button onClick={() => setOpen(!open)} aria-expanded={open}>
        {t("Connect a device")}
      </Button>
      {open && (
        <div className="space-y-2 pt-3">
          <p>{t("Enter this server address in a supported app:")}</p>
          {urls.length > 1 ? (
            <label>
              {t("Server address")}
              <select value={url} onChange={(event) => setSelectedURL(event.currentTarget.value)}>
                {urls.map((address) => (
                  <option key={address} value={address}>
                    {address}
                  </option>
                ))}
              </select>
            </label>
          ) : (
            <code>{url}</code>
          )}
          {qr && (
            <img src={qr} alt={t("QR code for the server address")} width={192} height={192} />
          )}
          {info.descriptor.certifiedApps?.length ? (
            <>
              <p>{t("Certified native apps:")}</p>
              {info.descriptor.certifiedApps.map((app) => (
                <div key={app.name}>
                  <p>{app.name}</p>
                  <ol>
                    {app.steps.map((step, index) => {
                      const label = connectionStep(step);
                      return label ? <li key={index}>{label}</li> : null;
                    })}
                  </ol>
                </div>
              ))}
            </>
          ) : (
            <p>{t("No native apps have been certified yet.")}</p>
          )}
        </div>
      )}
    </div>
  );
}

function PlexSettings({
  tab,
  hidden,
  enabled,
}: {
  tab: string;
  hidden: boolean;
  enabled?: boolean;
}) {
  const { dashboard, reloadBoot } = useApp();
  const navigate = useNavigate();
  async function reopenSetup() {
    try {
      await api.setupReopen();
      await reloadBoot();
      navigate("/setup");
    } catch (error) {
      toast.error(t("Couldn't reopen setup"), { description: (error as Error).message });
    }
  }
  return (
    <>
      {!hidden && tab === "plex" && enabled === false && (
        <p>{t("Plex apps: off (account sign-in still active)")}</p>
      )}
      <ConfigSection
        configKey="server"
        endpoint="server"
        label={t("Server")}
        groups={schema.SERVER.map((group) => ({
          ...group,
          fields: group.fields.filter((field) => ["port", "bindAddress"].includes(field.key)),
        })).filter((group) => group.fields.length > 0)}
        showAdvanced={false}
        hidden={hidden || tab !== "plex"}
      />
      <ConfigSection
        configKey="plex"
        endpoint="plex"
        groups={schema.PLEX}
        label={t("Plex settings")}
        hidden={hidden}
        show={(group) =>
          tab === "plex"
            ? group.id === "account" || group.id === "compatibility"
            : tab === "remote"
              ? group.id === "remote"
              : group.id === "secure"
        }
        showAdvanced={tab === "plex"}
        before={(_, config) =>
          tab === "plex" ? (
            <PlexStatus
              cfg={config}
              remote={dashboard?.sections?.remote}
              onReopen={() => void reopenSetup()}
            />
          ) : null
        }
        after={() => (tab === "plex" ? <RemoteDiagnostics /> : null)}
      />
    </>
  );
}

function ProtocolSettings({ id, hidden }: { id: string; hidden: boolean }) {
  const state = useAsync(() => apiFetch<Settings>(`/frontends/${id}/settings`), [id]);
  const { draft, setDraft, dirty, reset } = useDraft(state.data);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<unknown>();
  async function save() {
    if (!draft) return;
    setSaving(true);
    setError(undefined);
    try {
      await apiFetch(`/frontends/${id}/settings`, { method: "PUT", body: JSON.stringify(draft) });
      await state.reload();
    } catch (err) {
      setError(err);
    } finally {
      setSaving(false);
    }
  }
  if (state.error)
    return hidden ? null : <ErrorPanel error={state.error} onRetry={() => void state.reload()} />;
  if (!draft) return hidden ? null : <LoadingRows />;
  return (
    <>
      <div hidden={hidden}>
        <Section title={t("Settings")} description={t("Changes apply to this app.")}>
          {["jellyfin", "emby", "silo", "dlna"].includes(id) && (
            <label className="flex flex-col gap-1 text-sm">
              {t("Server name")}
              <Input
                value={draft.serverName ?? ""}
                onChange={(e) => setDraft({ ...draft, serverName: e.currentTarget.value })}
              />
            </label>
          )}
          {draft.port !== undefined && (
            <label className="flex flex-col gap-1 text-sm">
              {t("Port")}
              <Input
                type="number"
                min={1}
                max={65535}
                value={draft.port}
                onChange={(e) => setDraft({ ...draft, port: Number(e.currentTarget.value) })}
              />
            </label>
          )}
          {["jellyfin", "emby", "silo", "dlna"].includes(id) && (
            <label className="flex items-center gap-2 text-sm">
              <input
                type="checkbox"
                checked={draft.discovery ?? false}
                onChange={(e) => setDraft({ ...draft, discovery: e.currentTarget.checked })}
              />
              {t("Discovery")}
            </label>
          )}
          {(id === "stremio" || id === "webdav") && draft.port === undefined && (
            <p>{t("This app uses the main server address.")}</p>
          )}
          {id === "jellyfin" && (
            <label className="flex items-center gap-2 text-sm">
              <input
                type="checkbox"
                checked={draft.quickConnect ?? false}
                onChange={(e) => setDraft({ ...draft, quickConnect: e.currentTarget.checked })}
              />
              {t("Quick Connect")}
            </label>
          )}
          {(id === "jellyfin" || id === "emby") && (
            <label className="flex items-center gap-2 text-sm">
              <input
                type="checkbox"
                checked={draft.publicViewerPicker ?? false}
                onChange={(e) =>
                  setDraft({ ...draft, publicViewerPicker: e.currentTarget.checked })
                }
              />
              {t("Show local viewer names on the sign-in screen")}
            </label>
          )}
        </Section>
      </div>
      <SaveBar dirty={dirty} saving={saving} onSave={save} onDiscard={reset} error={error} />
    </>
  );
}

export default function Apps() {
  const tab = useRouteTab(tabs);
  const [visited, setVisited] = useState(() => new Set([tab]));
  useEffect(() => {
    setVisited((old) => (old.has(tab) ? old : new Set([...old, tab])));
  }, [tab]);
  const state = useAsync((signal) => apiFetch<FrontendInfo[]>("/frontends", { signal }), []);
  useInterval(() => void state.reload(), 10_000);
  const [error, setError] = useState<unknown>();
  const [busy, setBusy] = useState<string>();
  async function toggle(info: FrontendInfo) {
    setBusy(info.descriptor.id);
    setError(undefined);
    try {
      await apiFetch(`/frontends/${info.descriptor.id}/enabled`, {
        method: "PUT",
        body: JSON.stringify({ enabled: !info.enabled }),
      });
      await state.reload();
    } catch (err) {
      setError(err);
    } finally {
      setBusy(undefined);
    }
  }
  return (
    <DomainPage
      title={t("Apps")}
      description={t("Manage the apps and protocols your household uses.")}
      base="/apps"
      tabs={tabs}
      active={tab}
    >
      {tab === "overview" &&
        (state.error ? (
          <ErrorPanel error={state.error} onRetry={() => void state.reload()} />
        ) : !state.data ? (
          <LoadingRows />
        ) : (
          <div className="grid gap-4 md:grid-cols-2">
            {protocols.map((protocol) => {
              const info = state.data?.find((item) => item.descriptor.id === protocol.id);
              if (!info)
                return (
                  <Section key={protocol.id} title={protocol.name}>
                    <p>{t("Not available yet.")}</p>
                  </Section>
                );
              return (
                <Section
                  key={info.descriptor.id}
                  title={info.descriptor.name}
                  actions={
                    <Switch
                      checked={info.enabled}
                      disabled={busy === info.descriptor.id}
                      aria-label={t("{app} enabled", { app: info.descriptor.name })}
                      onChange={() => void toggle(info)}
                    />
                  }
                >
                  <p>{info.enabled ? t("On") : t("Off")}</p>
                  <p>{info.status.addresses?.join(", ") || t("No listener")}</p>
                  <p>
                    {t("LAN: {lan} · WAN: {wan}", {
                      lan: reachability(info.status.lan),
                      wan: reachability(info.status.wan),
                    })}
                  </p>
                  <p>
                    {t("Version: {version} · Conformance: {conformance}", {
                      version: info.status.targetVersion || info.descriptor.version || t("Unknown"),
                      conformance: conformance(info.status.conformance),
                    })}
                  </p>
                  <p>
                    {t("Clients seen in the last 24 hours: {clients} · Live streams: {streams}", {
                      clients: info.status.clients24hKnown ? info.status.clients24h : t("Unknown"),
                      streams: info.status.liveStreamsKnown
                        ? info.status.liveStreams
                        : t("Unknown"),
                    })}
                  </p>
                  {info.status.error && <p className="text-danger">{info.status.error}</p>}
                  {info.status.warnings?.map((warning) => (
                    <p key={warning}>{warning}</p>
                  ))}
                  <ConnectDevice info={info} />
                  <Link
                    className="text-primary underline"
                    to={`/apps/${pathFor(info.descriptor.id)}`}
                  >
                    {t("Settings")}
                  </Link>
                </Section>
              );
            })}
          </div>
        ))}
      {(visited.has("plex") || visited.has("remote") || visited.has("secure")) && (
        <PlexSettings
          tab={tab}
          hidden={tab !== "plex" && tab !== "remote" && tab !== "secure"}
          enabled={state.data?.find((item) => item.descriptor.id === "plex")?.enabled}
        />
      )}
      {tabs
        .filter(
          (item) =>
            !["overview", "plex", "remote", "secure"].includes(item.key) && visited.has(item.key),
        )
        .map((item) => (
          <ProtocolSettings key={item.key} id={protocolFor(item.key)} hidden={tab !== item.key} />
        ))}
      {error != null && <ErrorPanel error={error} />}
    </DomainPage>
  );
}
