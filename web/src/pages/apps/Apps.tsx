import { WatchButton } from "../../components/WatchButton";
import {
  Button,
  Input,
  SettingsSection,
  SearchSelect,
  Switch,
  confirmDialog,
} from "@braedonsaunders/appkit-ui";
import { useEffect, useState, type ReactNode } from "react";
import { Link, useNavigate, useParams, useSearchParams } from "react-router-dom";
import { useApp } from "../../app-context";
import { ErrorPanel, LoadingRows } from "../../components/feedback";
import { IntegrationAppCard } from "../../components/IntegrationAppCard";
import {
  EmbyMark,
  householdIcon,
  JellyfinMark,
  PlexMark,
  SiloMark,
} from "../../components/integration-brands";
import { StremioMark } from "../sources/marks";
import { MoreOptions } from "../../components/ConfigSection";
import { DomainPage, useRouteTab } from "../../components/layout";
import { SaveBar, useDraft } from "../../components/SaveBar";
import { SettingsRow } from "../../components/SettingsRow";
import { t } from "../../i18n";
import { useOwnerViewerPath } from "../../components/pickers";
import { API_BASE, api, apiBlob, apiFetch } from "../../lib/api";
import { isPlainObject } from "../../lib/format";
import { useAsync, useInterval } from "../../lib/hooks";
import { viewerStatus } from "../viewers/ViewerStatus";
import { XtreamLogins } from "./XtreamLogins";
import { AwayFromHome } from "./AwayFromHome";
import { PlexApp } from "./PlexApp";
import { appLabel, appPath, appPurpose, appRank } from "./catalog";
import { LevelBadge } from "../../components/status";
import {
  StatusRow,
  AddressChip,
  ConnectionStatusRow,
  StatusHelp,
} from "../../components/AppStatusList";
import { HomeStatusRow } from "./HomeStatus";
import { awaySummary, usableAddresses } from "./reachability";
import { validateCustomUrls } from "./plex-account";

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
  publicUrl?: string;
  serverName?: string;
  discovery?: boolean;
  quickConnect?: boolean;
  publicViewerPicker?: boolean;
  viewerId?: number;
  homeRows?: boolean;
  homeRowCount?: number;
  homeRowKinds?: string[];
};

const HOME_ROW_KINDS = [
  ["because-you-watched", () => t("Because you watched")],
  ["trending", () => t("Trending")],
  ["top-10", () => t("Top 10 for you")],
  ["fans-also-loved", () => t("Fans also loved")],
  ["new-releases", () => t("New releases")],
  ["hidden-gems", () => t("Hidden gems")],
  ["more-from-person", () => t("People you watch")],
  ["popular-on-server", () => t("Popular on this server")],
] as const;
const pathFor = appPath;
const protocolFor = (tab: string) => (tab === "files" ? "webdav" : tab);
const nameFor = (info: FrontendInfo) => appLabel(info.descriptor.id, info.descriptor.name);

/** Vendored marks only. Files, DLNA, and M3U have none. */
function appLogo(id: string): ReactNode | undefined {
  switch (id) {
    case "plex":
      return <PlexMark />;
    case "jellyfin":
      return <JellyfinMark />;
    case "emby":
      return <EmbyMark />;
    case "silo":
      return <SiloMark />;
    case "stremio":
      return <StremioMark />;
    default:
      return householdIcon(id);
  }
}

function frontendWarning(warning: string): string {
  const port =
    /^Another server on this computer is using port (\d+); Ether still works at (.+)$/.exec(
      warning,
    );
  if (port) {
    return t(
      "Another server on this computer is using port {port}; Ether still works at {address}",
      { port: port[1], address: port[2] },
    );
  }
  const discovery =
    /^Another server on this computer is using discovery; Ether still works at (.+)$/.exec(warning);
  if (discovery) {
    return t("Another server on this computer is using discovery; Ether still works at {address}", {
      address: discovery[1],
    });
  }
  return warning;
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

function viewerPortalAddress(chosen?: string): string {
  if (chosen) {
    try {
      const u = new URL(chosen);
      const prefix = u.pathname.replace(/\/(jellyfin|emby|silo)\/?$/, "");
      if (prefix !== u.pathname) {
        return `${u.origin}${prefix}/ether/me`;
      }
    } catch {
      // ignore invalid URL
    }
  }
  if (
    typeof window !== "undefined" &&
    window.location?.origin &&
    window.location.origin !== "null"
  ) {
    return `${window.location.origin}/ether/me`;
  }
  return "/ether/me";
}

function ConnectDevice({
  info,
  startOpen = false,
  selectedURL: selectedURLProp,
  onSelectedURL,
}: {
  info: FrontendInfo;
  startOpen?: boolean;
  selectedURL?: string;
  onSelectedURL?: (url: string) => void;
}) {
  const [open, setOpen] = useState(startOpen);
  const [showOther, setShowOther] = useState(false);
  const [qr, setQR] = useState<string>();
  const [selectedURLState, setSelectedURLState] = useState("");
  const selectedURL = selectedURLProp ?? selectedURLState;
  const setSelectedURL = onSelectedURL ?? setSelectedURLState;
  const urls = info.status.connectUrls ?? [];
  const likely = urls.slice(0, Math.min(2, urls.length));
  const other = urls.slice(likely.length);
  const shown = showOther ? urls : likely;
  const url = urls.includes(selectedURL) ? selectedURL : likely[0];
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
  if (["stremio", "webdav", "m3u", "xtream"].includes(info.descriptor.id)) {
    return (
      <Button asChild variant="outline">
        <Link to="/me">{t("Connect a device")}</Link>
      </Button>
    );
  }
  if (!url) return null;
  return (
    <div>
      {open ? null : (
        <Button variant="outline" onClick={() => setOpen(true)}>
          {t("Connect a device")}
        </Button>
      )}
      {open ? (
        <div className="space-y-2 pt-3">
          <p>{t("Enter this server address in a supported app:")}</p>
          {shown.length > 1 ? (
            <label>
              {t("Server address")}
              <select value={url} onChange={(event) => setSelectedURL(event.currentTarget.value)}>
                {shown.map((address) => (
                  <option key={address} value={address}>
                    {address}
                  </option>
                ))}
              </select>
            </label>
          ) : (
            <code>{url}</code>
          )}
          {other.length > 0 ? (
            <div>
              <Button variant="ghost" size="sm" onClick={() => setShowOther((value) => !value)}>
                {showOther ? t("Likely addresses") : t("Other addresses")}
              </Button>
            </div>
          ) : null}
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
            <p>
              {t(
                "Open the app, add the server address above, and sign in. Infuse, Swiftfin, Findroid, and Streamyfin are the usual places to start.",
              )}
            </p>
          )}
        </div>
      ) : null}
    </div>
  );
}

function ProtocolSettings({
  id,
  info,
  hidden,
}: {
  id: string;
  info?: FrontendInfo;
  hidden: boolean;
}) {
  const state = useAsync(() => apiFetch<Settings>(`/frontends/${id}/settings`), [id]);
  const { draft, setDraft, dirty, reset } = useDraft(state.data);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<unknown>();
  async function save() {
    if (!draft) return;
    const publicUrlError = validateCustomUrls(draft.publicUrl ? [draft.publicUrl] : []);
    if (publicUrlError) {
      setError(new Error(publicUrlError));
      return;
    }
    setSaving(true);
    setError(undefined);
    try {
      const { serverName: _serverName, port: _port, ...body } = draft;
      await apiFetch(`/frontends/${id}/settings`, { method: "PUT", body: JSON.stringify(body) });
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
  const named = ["jellyfin", "emby", "silo", "dlna"].includes(id);
  const more: {
    key: "discovery" | "publicViewerPicker";
    label: string;
    help: string;
  }[] = [];
  if (named) {
    more.push({
      key: "discovery",
      label: t("Let apps find this server"),
      help: t("Apps on your network can find this server without typing an address."),
    });
  }
  if (id === "jellyfin" || id === "emby") {
    more.push({
      key: "publicViewerPicker",
      label: t("Show local viewer names on the sign-in screen"),
      help: t("Leave this off on a network you do not trust."),
    });
  }
  const shared =
    id === "stremio"
      ? t("Stremio uses an addon address from your account page. It does not have its own port.")
      : id === "webdav"
        ? t("Other apps open this library as files. The address is on your account page.")
        : id === "m3u"
          ? t("Live TV apps use a playlist from your account page.")
          : "";
  return (
    <>
      <div hidden={hidden} className="space-y-6">
        {id === "vela" ? <WatchButton app="vela" /> : null}
        {["jellyfin", "emby", "silo"].includes(id) ? (
          <SignInSection
            info={info}
            quickConnect={
              id === "jellyfin"
                ? {
                    checked: Boolean(draft.quickConnect),
                    onChange: (next) => setDraft({ ...draft, quickConnect: next }),
                  }
                : undefined
            }
          />
        ) : null}
        {id === "xtream" && info?.enabled ? <XtreamLogins /> : null}
        {shared ? (
          <SettingsSection title={t("Connect")} description={shared}>
            <div className="px-5 py-3">
              <Button asChild variant="outline">
                <Link to="/me">{t("Open your account")}</Link>
              </Button>
            </div>
          </SettingsSection>
        ) : null}
        {id !== "plex" && id !== "dlna" ? (
          <SettingsSection title={t("Away from home")}>
            <SettingsRow
              title={<label htmlFor={`${id}-public-url`}>{t("Public address")}</label>}
              description={t(
                "The address viewers use to reach this app from outside your network, e.g. https://jelly.example.com",
              )}
              control={
                <Input
                  id={`${id}-public-url`}
                  aria-label={t("Public address")}
                  type="url"
                  inputMode="url"
                  autoComplete="off"
                  spellCheck={false}
                  className="w-72"
                  placeholder={`https://${id === "jellyfin" ? "jelly" : id}.example.com`}
                  value={draft.publicUrl ?? ""}
                  onChange={(e) => setDraft({ ...draft, publicUrl: e.currentTarget.value })}
                />
              }
            />
          </SettingsSection>
        ) : null}
        {id === "jellyfin" || id === "emby" ? (
          <SettingsSection
            title={t("Home")}
            description={t("{app} apps show this viewer's For You rows.", {
              app: protocolName(id),
            })}
          >
            <SettingsRow
              title={t("Personalized rows")}
              description={t("Each viewer gets their own rows, filled from what they watch.")}
              control={
                <Switch
                  checked={draft.homeRows !== false}
                  aria-label={t("Personalized rows")}
                  onChange={(e) => setDraft({ ...draft, homeRows: e.currentTarget.checked })}
                />
              }
            />
            <MoreOptions label={t("More options: {title}", { title: t("Home") })}>
              <SettingsRow
                title={<label htmlFor={`${id}-home-rows`}>{t("How many rows")}</label>}
                description={t(
                  "About four is enough for a home screen. The most relevant rows come first.",
                )}
                control={
                  <Input
                    id={`${id}-home-rows`}
                    aria-label={t("How many rows")}
                    type="number"
                    min={0}
                    max={12}
                    value={draft.homeRowCount ?? 4}
                    onChange={(e) =>
                      setDraft({ ...draft, homeRowCount: Number(e.currentTarget.value) })
                    }
                  />
                }
              />
              <SettingsRow
                title={t("Which rows")}
                description={t(
                  "Leave them all on to use every personalized row. Turn one off to hide that kind.",
                )}
              />
              {HOME_ROW_KINDS.map(([kind, label]) => {
                const selected = draft.homeRowKinds ?? HOME_ROW_KINDS.map(([id]) => id);
                return (
                  <SettingsRow
                    key={kind}
                    title={label()}
                    control={
                      <Switch
                        checked={selected.includes(kind)}
                        aria-label={label()}
                        onChange={(e) => {
                          const current = new Set(
                            draft.homeRowKinds ?? HOME_ROW_KINDS.map(([id]) => id),
                          );
                          if (e.currentTarget.checked) current.add(kind);
                          else current.delete(kind);
                          const next = HOME_ROW_KINDS.map(([id]) => id).filter((id) =>
                            current.has(id),
                          );
                          setDraft({
                            ...draft,
                            homeRowKinds: next.length === HOME_ROW_KINDS.length ? undefined : next,
                          });
                        }}
                      />
                    }
                  />
                );
              })}
            </MoreOptions>
          </SettingsSection>
        ) : null}
        {named ? (
          <SettingsSection
            title={id === "dlna" ? t("What the TV shows") : t("Find this server")}
            description={
              id === "dlna"
                ? t("DLNA players browse the library as one viewer.")
                : t("How {app} apps find this server.", { app: protocolName(id) })
            }
          >
            {id === "dlna" ? (
              <DLNAViewer
                value={draft.viewerId}
                onChange={(viewerId) => setDraft((old) => (old ? { ...old, viewerId } : old))}
              />
            ) : null}
            {more.length ? (
              <MoreOptions
                label={t("More options: {title}", {
                  title: id === "dlna" ? t("What the TV shows") : t("Find this server"),
                })}
              >
                {more.map((field) => (
                  <SettingsRow
                    key={field.key}
                    title={field.label}
                    description={field.help}
                    control={
                      <Switch
                        checked={Boolean(draft[field.key])}
                        aria-label={field.label}
                        onChange={(e) =>
                          setDraft({ ...draft, [field.key]: e.currentTarget.checked })
                        }
                      />
                    }
                  />
                ))}
              </MoreOptions>
            ) : null}
          </SettingsSection>
        ) : null}
      </div>
      <SaveBar dirty={dirty} saving={saving} onSave={save} onDiscard={reset} error={error} />
    </>
  );
}

function protocolName(id: string): string {
  switch (id) {
    case "jellyfin":
      return "Jellyfin";
    case "emby":
      return "Emby";
    case "vela":
      return "Vela";
    case "silo":
      return "Silo";
    default:
      return id;
  }
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
    <div className="space-y-3 px-5 py-4">
      <SearchSelect
        ariaLabel={t("Browse as")}
        placeholder={t("No viewer selected")}
        value={value == null ? "" : String(value)}
        onChange={(next) => onChange(next ? Number(next) : undefined)}
        clearable
        searchable
        options={(viewers.data ?? [])
          .filter((v) => ["owner", "active"].includes(viewerStatus(v)))
          .map((v) => ({ value: String(v.id), label: v.title || v.username }))}
      />
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
      <LevelBadge
        level={info.status.error ? "error" : info.enabled ? "ok" : "off"}
        label={info.enabled ? t("On") : t("Off")}
      />
      {info.status.error ? <span className="text-danger">{info.status.error}</span> : null}
    </>
  );
}

/** How viewers sign in to Jellyfin, Emby and Silo, with Jellyfin's Quick Connect switch. */
function SignInSection({
  info,
  quickConnect,
}: {
  info?: FrontendInfo;
  quickConnect?: { checked: boolean; onChange: (next: boolean) => void };
}) {
  const ownerPath = useOwnerViewerPath();
  const portalURL = viewerPortalAddress(info ? usableAddresses(info.status)[0] : undefined);
  return (
    <SettingsSection title={t("How people sign in")}>
      <SettingsRow
        title={t("Viewer accounts")}
        description={t("People sign in with their Ether viewer account.")}
        control={
          <Button asChild variant="outline" size="sm">
            <Link to="/viewers">{t("Viewers")}</Link>
          </Button>
        }
      />
      <SettingsRow
        title={t("Quick Connect")}
        description={t("Approve the code in the viewer portal.")}
        control={
          <div className="flex flex-wrap items-center gap-3">
            <AddressChip address={portalURL} />
            {quickConnect ? (
              <Switch
                checked={quickConnect.checked}
                aria-label={t("Quick Connect")}
                onChange={(e) => quickConnect.onChange(e.currentTarget.checked)}
              />
            ) : null}
          </div>
        }
      />
      <SettingsRow
        title={
          <span className="flex items-center gap-2">
            {t("App password")}
            <StatusHelp
              text={
                info?.descriptor.id === "emby"
                  ? t("The Emby for TV app isn't supported.")
                  : t("Infuse needs an app password.")
              }
            />
          </span>
        }
        description={t("Create an app password under Connect apps in your account.")}
        control={
          <Button asChild variant="outline" size="sm">
            <Link to={ownerPath}>{t("Open your account")}</Link>
          </Button>
        }
      />
    </SettingsSection>
  );
}

function AppDetail({ info, startOpen = false }: { info: FrontendInfo; startOpen?: boolean }) {
  const addresses = usableAddresses(info.status);
  const [selectedURL, setSelectedURL] = useState("");
  const chosen = addresses.includes(selectedURL) ? selectedURL : addresses[0];
  const away = awaySummary(info.status, { homeNetwork: true });
  return (
    <div className="space-y-6 pb-6">
      <SettingsSection title={t("Status")}>
        <HomeStatusRow status={info.status} address={chosen} />
        <ConnectionStatusRow state={info.status.wan} title={away.title} detail={away.detail} />
        {info.status.error ? (
          <StatusRow
            state="unreachable"
            title={t("Connection problem")}
            description={info.status.error}
          />
        ) : null}
        {info.status.warnings?.map((warning) => (
          <StatusRow
            key={warning}
            state="unknown"
            title={t("Needs attention")}
            description={frontendWarning(warning)}
          />
        ))}
        <div className="px-5 py-3">
          <ConnectDevice
            info={info}
            startOpen={startOpen}
            selectedURL={chosen}
            onSelectedURL={setSelectedURL}
          />
        </div>
      </SettingsSection>
    </div>
  );
}

export default function Apps() {
  const { dashboard } = useApp();
  const state = useAsync((signal) => apiFetch<FrontendInfo[]>("/frontends", { signal }), []);
  const { tab: routeTab, section: routeSection } = useParams();
  const [searchParams] = useSearchParams();
  const connectOpen = searchParams.get("connect") === "1";
  const navigate = useNavigate();
  const apps = [...(state.data ?? [])].sort(
    (a, b) => appRank(a.descriptor.id) - appRank(b.descriptor.id),
  );
  const plexInfo = apps.find((info) => info.descriptor.id === "plex");
  const plexOn = plexInfo?.enabled === true;
  const showPlex = plexOn && routeTab === "plex";
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
    if (routeTab === "plex") {
      if (!plexEnabled) navigate("/apps/overview", { replace: true });
      return;
    }
    const match = state.data.find((info) => pathFor(info.descriptor.id) === routeTab);
    if (!match?.enabled) navigate("/apps/overview", { replace: true });
  }, [state.data, routeTab, navigate]);
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
    <DomainPage title={t("Apps")} base="/apps" tabs={tabs} active={tab}>
      {tab === "overview" &&
        (state.error ? (
          <ErrorPanel error={state.error} onRetry={() => void state.reload()} />
        ) : !state.data ? (
          <LoadingRows />
        ) : (
          <div className="space-y-4">
            {plexOn &&
            isPlainObject(dashboard?.sections.remote) &&
            dashboard.sections.remote.claimed === false ? (
              <p className="text-sm text-fg">
                <Link className="text-primary underline" to="/apps/plex/account">
                  {t("Finish connecting Plex")}
                </Link>
              </p>
            ) : null}
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
            <AwayFromHome />
          </div>
        ))}
      {(visited.has("plex") || showPlex) && (
        <PlexApp hidden={tab !== "plex"} section={routeSection} status={plexInfo?.status} />
      )}
      {tabs
        .filter((item) => !["overview", "plex"].includes(item.key) && visited.has(item.key))
        .map((item) => {
          const info = apps.find((app) => pathFor(app.descriptor.id) === item.key);
          return (
            <div key={item.key}>
              {tab === item.key && info ? <AppDetail info={info} startOpen={connectOpen} /> : null}
              <ProtocolSettings id={protocolFor(item.key)} info={info} hidden={tab !== item.key} />
            </div>
          );
        })}
      {error != null && <ErrorPanel error={error} />}
    </DomainPage>
  );
}
