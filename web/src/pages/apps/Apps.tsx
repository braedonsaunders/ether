import { Badge, Button, Input, confirmDialog, toast } from "@braedonsaunders/appkit-ui";
import { useEffect, useState, type ReactNode } from "react";
import { Link, useNavigate, useParams, useSearchParams } from "react-router-dom";
import { useApp } from "../../app-context";
import { ConfigSection } from "../../components/ConfigSection";
import { ErrorPanel, LoadingRows } from "../../components/feedback";
import { IntegrationAppCard } from "../../components/IntegrationAppCard";
import { EmbyMark, JellyfinMark, PlexMark } from "../../components/integration-brands";
import { DomainPage, Section, useRouteTab } from "../../components/layout";
import { RemoteDiagnostics } from "../../components/RemoteDiagnostics";
import { SaveBar, useDraft } from "../../components/SaveBar";
import { t } from "../../i18n";
import { API_BASE, api, apiBlob, apiFetch } from "../../lib/api";
import { useAsync, useInterval } from "../../lib/hooks";
import * as schema from "../settings/schema";
import { PlexStatus } from "../settings/sections";
import { viewerStatus } from "../viewers/ViewerStatus";
import { appLabel, appPath, appPurpose } from "./catalog";

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
  viewerId?: number;
};
const pathFor = appPath;
const protocolFor = (tab: string) => (tab === "files" ? "webdav" : tab);
const nameFor = (info: FrontendInfo) => appLabel(info.descriptor.id, info.descriptor.name);

/** Vendored marks only. Silo, Stremio, Files, DLNA, and M3U have none. */
function appLogo(id: string): ReactNode | undefined {
  switch (id) {
    case "plex":
      return <PlexMark />;
    case "jellyfin":
      return <JellyfinMark />;
    case "emby":
      return <EmbyMark />;
    default:
      return undefined;
  }
}

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
  if (!info.enabled) return null;
  if (["stremio", "webdav", "m3u"].includes(info.descriptor.id)) {
    return (
      <Link className="text-primary underline" to="/me">
        {t("Connect a device")}
      </Link>
    );
  }
  if (!url) return null;
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

function PlexSettings({ hidden }: { hidden: boolean }) {
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
      <ConfigSection
        configKey="server"
        endpoint="server"
        label={t("Server")}
        groups={schema.SERVER.map((group) => ({
          ...group,
          fields: group.fields.filter((field) => ["port", "bindAddress"].includes(field.key)),
        })).filter((group) => group.fields.length > 0)}
        showAdvanced={false}
        hidden={hidden}
      />
      <ConfigSection
        configKey="plex"
        endpoint="plex"
        groups={schema.PLEX}
        label={t("Plex settings")}
        hidden={hidden}
        showAdvanced
        before={(_, config) => (
          <PlexStatus
            cfg={config}
            remote={dashboard?.sections?.remote}
            onReopen={() => void reopenSetup()}
          />
        )}
        after={() => <RemoteDiagnostics />}
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
          {id === "dlna" && (
            <DLNAViewer
              value={draft.viewerId}
              onChange={(viewerId) => setDraft((old) => (old ? { ...old, viewerId } : old))}
            />
          )}
        </Section>
      </div>
      <SaveBar dirty={dirty} saving={saving} onSave={save} onDiscard={reset} error={error} />
    </>
  );
}

function DLNAViewer({ value, onChange }: { value?: number; onChange: (id?: number) => void }) {
  const viewers = useAsync(() => api.viewers(), []);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>();
  async function createGuest() {
    if (
      !(await confirmDialog({
        title: t("Create a living-room guest"),
        message: t("Starts with the Kids preset (PG). You can change it on the guest's profile."),
        confirmLabel: t("Create a living-room guest"),
      }))
    )
      return;
    setBusy(true);
    setError(undefined);
    try {
      const viewer = await apiFetch<{ id: number }>("/frontends/dlna/guest", {
        method: "POST",
        body: "{}",
      });
      await viewers.reload();
      onChange(viewer.id);
    } catch (err) {
      setError(err);
    } finally {
      setBusy(false);
    }
  }
  return (
    <div className="space-y-2">
      <label className="flex flex-col gap-1 text-sm">
        {t("Browse as")}
        <select
          value={value ?? ""}
          onChange={(e) =>
            onChange(e.currentTarget.value ? Number(e.currentTarget.value) : undefined)
          }
        >
          <option value="">{t("No viewer selected")}</option>
          {(viewers.data ?? [])
            .filter((v) => ["owner", "active"].includes(viewerStatus(v)))
            .map((v) => (
              <option key={v.id} value={v.id}>
                {v.title || v.username}
              </option>
            ))}
        </select>
      </label>
      <Button disabled={busy} onClick={() => void createGuest()}>
        {t("Create a living-room guest")}
      </Button>
      {error != null && <ErrorPanel error={error} />}
    </div>
  );
}

function AppStatus({ info }: { info: FrontendInfo }) {
  return (
    <>
      <Badge variant={info.enabled ? "success" : "secondary"}>
        {info.enabled ? t("On") : t("Off")}
      </Badge>
      {info.status.error ? <span className="text-danger">{info.status.error}</span> : null}
    </>
  );
}

function AppDetail({ info }: { info: FrontendInfo }) {
  return (
    <Section title={t("Status")}>
      {info.status.addresses?.length ? <p>{info.status.addresses.join(", ")}</p> : null}
      <p>
        {t("LAN: {lan} · WAN: {wan}", {
          lan: reachability(info.status.lan),
          wan: reachability(info.status.wan),
        })}
      </p>
      {info.status.error ? <p className="text-danger">{info.status.error}</p> : null}
      {info.status.warnings?.map((warning) => (
        <p key={warning}>{warning}</p>
      ))}
      <ConnectDevice info={info} />
    </Section>
  );
}

const PLEX_SECTIONS = new Set(["remote", "secure", "account"]);

export default function Apps() {
  const state = useAsync((signal) => apiFetch<FrontendInfo[]>("/frontends", { signal }), []);
  const routeTab = useParams().tab;
  const [searchParams] = useSearchParams();
  const navigate = useNavigate();
  const section = searchParams.get("section");
  const apps = state.data ?? [];
  const plexInfo = apps.find((info) => info.descriptor.id === "plex");
  const plexOn = plexInfo?.enabled === true;
  const showPlex =
    plexOn && (routeTab === "plex" || routeTab === "remote" || routeTab === "secure");
  const tabs = [
    { key: "overview", label: t("Overview") },
    ...apps
      .filter((info) => info.enabled)
      .map((info) => ({ key: pathFor(info.descriptor.id), label: nameFor(info) })),
  ];
  const routed = useRouteTab(tabs, "overview");
  const tab = showPlex ? "plex" : routed;
  const [visited, setVisited] = useState(() => new Set([tab]));
  useEffect(() => {
    setVisited((old) => (old.has(tab) ? old : new Set([...old, tab])));
  }, [tab]);
  useEffect(() => {
    if (!state.data || !routeTab || routeTab === "overview") return;
    const plexEnabled = state.data.some((info) => info.descriptor.id === "plex" && info.enabled);
    if (routeTab === "remote" || routeTab === "secure") {
      navigate(plexEnabled ? `/apps/plex?section=${routeTab}` : "/apps/overview", {
        replace: true,
      });
      return;
    }
    if (routeTab === "plex") {
      if (!plexEnabled) navigate("/apps/overview", { replace: true });
      return;
    }
    const match = state.data.find((info) => pathFor(info.descriptor.id) === routeTab);
    if (!match?.enabled) navigate("/apps/overview", { replace: true });
  }, [state.data, routeTab, navigate]);
  useEffect(() => {
    if (tab !== "plex" || !section || !PLEX_SECTIONS.has(section)) return;
    const title =
      section === "remote"
        ? t("Remote access")
        : section === "secure"
          ? t("Secure connections")
          : t("Account");
    const heading = [...document.querySelectorAll("h2, h3")].find(
      (node) => node.textContent === title,
    );
    if (heading && typeof heading.scrollIntoView === "function") {
      heading.scrollIntoView({ block: "nearest" });
    }
  }, [tab, section, state.data]);
  useInterval(() => void state.reload(), 10_000);
  const [error, setError] = useState<unknown>();
  const [busy, setBusy] = useState<string>();
  async function toggle(info: FrontendInfo, enabled: boolean) {
    setBusy(info.descriptor.id);
    setError(undefined);
    try {
      await apiFetch(`/frontends/${info.descriptor.id}/enabled`, {
        method: "PUT",
        body: JSON.stringify({ enabled }),
      });
      await state.reload();
      if (!enabled && routeTab === pathFor(info.descriptor.id)) {
        navigate("/apps/overview", { replace: true });
      }
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
          <div className="grid gap-4 sm:grid-cols-2 xl:grid-cols-3">
            {state.data.map((info) => (
              <IntegrationAppCard
                key={info.descriptor.id}
                identity={{ id: info.descriptor.id, name: nameFor(info) }}
                logo={appLogo(info.descriptor.id)}
                description={appPurpose(info.descriptor.id)}
                status={<AppStatus info={info} />}
                enabled={info.enabled}
                busy={busy === info.descriptor.id}
                disabled={busy != null && busy !== info.descriptor.id}
                toggleLabel={t("{app} enabled", { app: nameFor(info) })}
                onToggle={(enabled) => void toggle(info, enabled)}
                configureHref={info.enabled ? `/apps/${pathFor(info.descriptor.id)}` : undefined}
                configureLabel={t("Settings")}
              />
            ))}
          </div>
        ))}
      {tab === "plex" && plexInfo ? <AppDetail info={plexInfo} /> : null}
      {(visited.has("plex") || showPlex) && <PlexSettings hidden={tab !== "plex"} />}
      {tabs
        .filter((item) => !["overview", "plex"].includes(item.key) && visited.has(item.key))
        .map((item) => {
          const info = apps.find((app) => pathFor(app.descriptor.id) === item.key);
          return (
            <div key={item.key}>
              {tab === item.key && info ? <AppDetail info={info} /> : null}
              <ProtocolSettings id={protocolFor(item.key)} hidden={tab !== item.key} />
            </div>
          );
        })}
      {error != null && <ErrorPanel error={error} />}
    </DomainPage>
  );
}
