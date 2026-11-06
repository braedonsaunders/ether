import { McpAuthorize } from "./components/agent/McpAuthorize";
import {
  AppShell,
  Button,
  ConfirmRoot,
  DrawerTextProvider,
  Popover,
  PromptRoot,
  Spinner,
  ThemeProvider,
  ThemeToggle,
  Toaster,
  UiLinkProvider,
  type LinkRender,
  type SidebarNavGroup,
} from "@braedonsaunders/appkit-ui";
import {
  Activity as ActivityIcon,
  HardDrive,
  Library,
  Boxes,
  Globe,
  LifeBuoy,
  Plug,
  Puzzle,
  Server,
  Bell,
  CalendarDays,
  Gauge,
  LayoutTemplate,
  Layers,
  ListFilter,
  LogOut,
  UserRound,
  MonitorPlay,
  Network,
  Tags,
  Tv,
  Users,
} from "lucide-react";
import {
  forwardRef,
  lazy,
  Suspense,
  useCallback,
  useEffect,
  useRef,
  useState,
  type AnchorHTMLAttributes,
  type ReactNode,
} from "react";
import {
  Link,
  Navigate,
  Route,
  Routes,
  useLocation,
  useNavigate,
  useOutlet,
  useParams,
  useOutletContext,
  useSearchParams,
} from "react-router-dom";
import { AppProvider, useApp } from "./app-context";
import { hasExemptDiagnostic } from "./lib/health";
import { t, tn } from "./i18n";
import { I18nProvider, LanguagePicker } from "./i18n/react";
import { Brand, BrandSplash, EtherMark } from "./components/brand";
import { CommandSearch } from "./components/CommandSearch";
import { AskEther } from "./components/agent/AskEther";
import { Drawer } from "./components/Drawer";
import { ErrorPanel } from "./components/feedback";
import { SaveBarHost } from "./components/SaveBar";
import { SettingFocus } from "./components/SettingFocus";
import { UpdateReady } from "./components/UpdateReady";
import { LEVEL_LABEL, LevelBadge, StatusDot } from "./components/status";
import { useOwnerViewerPath } from "./components/pickers";
import { api, getToken, setToken } from "./lib/api";
import { BUILTIN, textOf, useRegistry, type Descriptor } from "./lib/extend";
import {
  pluginNavGroup,
  pluginPageRoute,
  type NavItemMeta,
  type PageMeta,
} from "./lib/console-descriptors";
import type { Bootstrap } from "./lib/types";
import { theme } from "./theme";
import Home from "./pages/Home";
import Login from "./pages/Login";
import Startup, { isStarting } from "./pages/Startup";

const CollectionsGallery = lazy(() => import("./pages/collections/Gallery"));
const CollectionEditor = lazy(() => import("./pages/collections/Editor"));
const Designer = lazy(() => import("./pages/designer/Designer"));
const ViewersList = lazy(() => import("./pages/viewers/ViewersList"));
const ViewerProfile = lazy(() => import("./pages/viewers/ViewerProfile"));
const settingsPage = <K extends keyof typeof import("./pages/settings/pages")>(name: K) =>
  lazy(() => import("./pages/settings/pages").then((m) => ({ default: m[name] })));
const GeneralPage = settingsPage("GeneralPage");
const Apps = lazy(() => import("./pages/apps/Apps"));
const ServicesPage = settingsPage("ServicesPage");
const MaintenancePage = settingsPage("MaintenancePage");
const ClusterPage = settingsPage("ClusterPage");
const Setup = lazy(() => import("./pages/Setup"));
const Activity = lazy(() => import("./pages/activity/Activity"));
const Notifications = lazy(() => import("./pages/notifications/Notifications"));
const LiveTV = lazy(() => import("./pages/livetv/LiveTV"));
const Calendar = lazy(() => import("./pages/calendar/Calendar"));
const Catalog = lazy(() => import("./pages/catalog/Catalog"));
const Metadata = lazy(() => import("./pages/metadata/Metadata"));
const Sources = lazy(() => import("./pages/sources/Sources"));
const Quality = lazy(() => import("./pages/releases/Releases"));
const Cache = lazy(() => import("./pages/cache/Cache"));
const Playback = lazy(() => import("./pages/playback/Playback"));
const Plugins = lazy(() => import("./pages/plugins/Plugins"));
const Studio = lazy(() => import("./pages/plugins/Studio"));
const PluginPage = lazy(() => import("./viewspec/PluginPage"));

/** react-router links for every AppKit navigation primitive. */
const RouterAnchor = forwardRef<
  HTMLAnchorElement,
  AnchorHTMLAttributes<HTMLAnchorElement> & { href: string }
>(function RouterAnchor({ href, ...rest }, ref) {
  if (/^(https?:)?\/\//.test(href)) return <a ref={ref} href={href} {...rest} />;
  return <Link ref={ref} to={href} {...rest} />;
});

const linkRender: LinkRender = ({ href, children, className, title, ariaCurrent, role }) => (
  <Link to={href} className={className} title={title} aria-current={ariaCurrent} role={role}>
    {children}
  </Link>
);

/** Lucide icons by the name page descriptors carry. */
const NAV_ICONS = {
  Gauge,
  Activity: ActivityIcon,
  Users,
  Library,
  Layers,
  LayoutTemplate,
  Tv,
  CalendarDays,
  Tags,
  Network,
  ListFilter,
  HardDrive,
  MonitorPlay,
  Server,
  Globe,
  Plug,
  Puzzle,
  Bell,
  Boxes,
  LifeBuoy,
} as const;

function navIcon(icon: string | undefined): ReactNode {
  const size = 18;
  if (icon && /^(https?:)?\/\//.test(icon)) {
    return <img src={icon} alt="" width={size} height={size} className="rounded-sm" />;
  }
  const Icon = (icon && NAV_ICONS[icon as keyof typeof NAV_ICONS]) || Puzzle;
  return <Icon size={size} />;
}

/** Sidebar groups in display order; the top group has no heading. */
const NAV_GROUPS: { key: string; label: () => string }[] = [
  { key: "", label: () => "" },
  { key: "library", label: () => t("Library") },
  { key: "streaming", label: () => t("Streaming") },
  { key: "server", label: () => t("Server") },
];

/**
 * The sidebar, from registry descriptors (WS1d). Built-in navItems come from
 * Go; a plugin page joins its declared nav group (or Server) with the
 * plugin's icon, after the built-ins of the same order.
 * Grouped by the owner's questions: what's happening and who watches; what
 * viewers see in their apps; how titles reach the screen; how the server runs.
 */
export function buildNavigation(descriptors: Descriptor[]): SidebarNavGroup[] {
  type Item = {
    group: string;
    order: number;
    builtin: boolean;
    item: SidebarNavGroup["items"][number];
  };
  const items: Item[] = [];
  const navPages = new Set<string>();
  const pageById = new Map<string, Descriptor<PageMeta>>();
  for (const d of descriptors) {
    if (d.kind === "page") {
      pageById.set(d.id, d as Descriptor<PageMeta>);
    }
  }
  for (const d of descriptors) {
    // Plugins do not add sidebar entries (yet); their pages open from the
    // plugin's drawer and from the surfaces they extend.
    if (d.kind !== "navItem" || d.owner !== BUILTIN) continue;
    const meta = d.meta as NavItemMeta | undefined;
    if (!meta?.route) continue;
    const pageId = meta.page ?? (meta.pageKey ? `plugin:${d.owner}/${meta.pageKey}` : undefined);
    if (pageId) navPages.add(pageId);
    navPages.add(d.id);
    const page = pageId ? pageById.get(pageId) : undefined;
    const pageMeta = page?.meta;
    const group =
      d.owner === BUILTIN
        ? (d.group ?? "")
        : ((page ? pluginNavGroup(page) : undefined) ?? d.group ?? "server");
    const href =
      d.owner !== BUILTIN && meta.pageKey ? pluginPageRoute(d.owner, meta.pageKey) : meta.route;
    items.push({
      group,
      order: d.order ?? page?.order ?? 0,
      builtin: d.owner === BUILTIN,
      item: {
        href,
        label: textOf(d.label),
        icon: navIcon(d.icon ?? page?.icon ?? pageMeta?.icon),
        exact: meta.exact || undefined,
        mobile: meta.mobile || undefined,
      },
    });
  }
  items.sort((a, b) => a.order - b.order || Number(b.builtin) - Number(a.builtin));
  const groups: SidebarNavGroup[] = NAV_GROUPS.map((g) => ({
    label: g.label(),
    items: items.filter((i) => i.group === g.key).map((i) => i.item),
  }));
  // A plugin may declare a group the console does not know; keep it visible
  // under Server rather than dropping the page.
  const known = new Set(NAV_GROUPS.map((g) => g.key));
  const stray = items.filter((i) => !known.has(i.group)).map((i) => i.item);
  if (stray.length) groups[groups.length - 1]!.items.push(...stray);
  return groups.filter((g) => g.items.length > 0);
}

function HealthIndicator() {
  const { health, overall, dashboard, dashboardError, boot } = useApp();
  const [open, setOpen] = useState(false);
  // Before setup finishes, unconfigured sources and an unclaimed server are
  // expected, not problems: show progress instead of a red verdict.
  const settingUp = !boot.setupComplete;
  const setupGaps = health.filter((item) => item.setupGap);
  const gettingStarted =
    !settingUp && setupGaps.length > 0 && (overall === "ok" || overall === "off");
  // "Operational" means no participating failure. It does not mean TLS was verified.
  const label = settingUp
    ? t("Setting up")
    : gettingStarted
      ? t("Getting started")
      : overall === "ok" || overall === "off"
        ? hasExemptDiagnostic(health)
          ? t("Operational")
          : t("All systems normal")
        : overall === "unknown"
          ? dashboard
            ? t("Status incomplete")
            : t("Checking…")
          : LEVEL_LABEL[overall];
  return (
    <Popover
      open={open}
      onOpenChange={setOpen}
      trigger={
        <button
          type="button"
          onClick={() => setOpen((o) => !o)}
          aria-label={t("Server health: {status}", { status: label })}
          className="inline-flex h-9 items-center gap-2 rounded-md px-2.5 text-sm text-fg-muted transition-colors hover:bg-surface-hover hover:text-fg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/50"
        >
          <StatusDot
            level={settingUp || gettingStarted ? "unknown" : overall === "off" ? "ok" : overall}
            pulse
          />
          <span className="hidden md:inline">{label}</span>
        </button>
      }
    >
      <div className="w-80 p-1">
        <p className="px-2 pb-2 pt-1 text-xs font-semibold uppercase tracking-wider text-fg-subtle">
          {t("Server health")}
        </p>
        {settingUp ? (
          <p className="px-2 pb-2 text-sm text-fg-muted">
            {t("Finish setup to bring everything online. Items below fill in as you go.")}
          </p>
        ) : gettingStarted ? (
          <p className="px-2 pb-2 text-sm text-fg-muted">
            {tn(setupGaps.length, "{count} thing to set up", "{count} things to set up")}
          </p>
        ) : null}
        {dashboardError && !dashboard ? (
          <p className="px-2 pb-2 text-sm text-danger">{t("Status is unavailable right now.")}</p>
        ) : null}
        <ul className="space-y-0.5">
          {health.map((h) => (
            <li key={h.id}>
              <Link
                to={h.href}
                onClick={() => setOpen(false)}
                className="flex items-start gap-2.5 rounded-md px-2 py-1.5 hover:bg-surface-hover"
              >
                <StatusDot level={h.level} className="mt-1.5" />
                <span className="min-w-0 flex-1">
                  <span className="flex items-center justify-between gap-2 text-sm font-medium text-fg">
                    {h.label}
                    <LevelBadge level={h.level} label={h.statusLabel} />
                  </span>
                  <span className="block text-pretty text-xs text-fg-muted">{h.summary}</span>
                  {h.note ? (
                    <span className="mt-1 block text-xs text-fg-subtle">{h.note}</span>
                  ) : null}
                </span>
              </Link>
            </li>
          ))}
        </ul>
      </div>
    </Popover>
  );
}

/** "v1.2.3" for releases, the raw string ("dev") otherwise. */
function versionLabel(v: string): string {
  return /^\d/.test(v) ? `v${v}` : v;
}

/** Keep the library mounted behind every collection drawer, including lazy editor loads. */
function CollectionsLayout() {
  const location = useLocation();
  const navigate = useNavigate();
  const editing = location.pathname.replace(/\/$/, "") !== "/collections";
  const browseSearch = useRef(editing ? "" : location.search);
  if (!editing) browseSearch.current = location.search;
  const outlet = useOutlet({ close: () => navigate(`/collections${browseSearch.current}`) });
  return (
    <>
      <div
        aria-hidden={editing ? true : undefined}
        inert={editing}
        className="flex min-h-0 flex-1 flex-col"
      >
        <CollectionsGallery
          browseSearch={editing ? browseSearch.current : undefined}
          active={!editing}
        />
      </div>
      {outlet}
    </>
  );
}

/** Collection Studio opens over the persistent library as a record drawer. */
function CollectionDrawer({ fresh = false }: { fresh?: boolean }) {
  const { close } = useOutletContext<{ close: () => void }>();
  const [search] = useSearchParams();
  return (
    <Drawer
      open
      onClose={close}
      title={fresh ? t("New collection") : t("Edit collection")}
      size="2xl"
      bodyClassName="flex min-h-0 flex-1 flex-col overflow-hidden p-0"
    >
      {fresh ? <CollectionEditor key={search.get("draft") ?? "new"} /> : <CollectionEditor />}
    </Drawer>
  );
}

function PageFallback() {
  return (
    <div className="grid flex-1 place-items-center p-10">
      <Spinner size={22} label={t("Loading")} />
    </div>
  );
}

// Compiled components and local parameters; base paths come from page descriptors.
const BUILTIN_PAGES: Record<
  string,
  {
    element: ReactNode;
    suffix?: string;
    children?: ReactNode;
    extra?: { suffix: string; element: ReactNode }[];
  }
> = {
  overview: { element: <Home /> },
  activity: { element: <Activity />, suffix: "/:tab?" },
  viewers: { element: <ViewersList />, extra: [{ suffix: "/:id/:tab?", element: <ViewerById /> }] },
  home: { element: <Designer /> },
  collections: {
    element: <CollectionsLayout />,
    children: (
      <>
        <Route path="new" element={<CollectionDrawer fresh />} />
        <Route path=":id" element={<CollectionDrawer />} />
      </>
    ),
  },
  livetv: { element: <LiveTV />, suffix: "/:tab?" },
  calendar: { element: <Calendar />, suffix: "/:view?" },
  catalog: { element: <Catalog />, suffix: "/:tab?" },
  metadata: { element: <Metadata />, suffix: "/:tab?" },
  sources: { element: <Sources />, suffix: "/:tab?" },
  releases: { element: <Quality />, suffix: "/:tab?" },
  cache: { element: <Cache />, suffix: "/:tab?" },
  playback: { element: <Playback />, suffix: "/:tab?/:section?" },
  notifications: { element: <Notifications />, suffix: "/:tab?" },
  general: { element: <GeneralPage /> },
  apps: { element: <Apps />, suffix: "/:tab?/:section?" },
  integrations: { element: <ServicesPage />, suffix: "/:tab?" },
  cluster: { element: <ClusterPage /> },
  maintenance: { element: <MaintenancePage />, suffix: "/:tab?/:backupTab?" },
};

function Shell() {
  const { boot } = useApp();
  const ownerPath = useOwnerViewerPath();
  const location = useLocation();
  const {
    descriptors: navDescriptors,
    loading: registryLoading,
    error: registryError,
    reload: reloadRegistry,
  } = useRegistry<PageMeta>("page", "navItem");
  const remoteSession = boot.authRequired && Boolean(getToken());
  const version = versionLabel(boot.version);
  const serverName = boot.serverName?.trim() || "Ether";
  return (
    <div
      className={
        location.pathname === "/setup" ? "ether-shell ether-setup contents" : "ether-shell contents"
      }
    >
      <AppShell
        groups={location.pathname === "/setup" ? [] : buildNavigation(navDescriptors)}
        pathname={location.pathname}
        navigationMode="sidebar"
        linkRender={linkRender}
        brand={
          <Link
            to="/"
            aria-label={t("Ether home")}
            className="block rounded-md focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/50"
          >
            <Brand />
          </Link>
        }
        headerMiddle={<CommandSearch />}
        header={
          <>
            <span
              className="inline-flex h-9 min-w-0 max-w-[9rem] items-center gap-1.5 rounded-md px-2.5 text-sm text-fg-muted sm:max-w-[14rem]"
              title={serverName}
            >
              <Server size={15} className="shrink-0" aria-hidden />
              <span className="hidden truncate sm:inline">{serverName}</span>
            </span>
            <HealthIndicator />
            <AskEther />
            <ThemeToggle
              collapsed
              labels={{
                label: t("Theme"),
                light: t("Light"),
                dark: t("Dark"),
                system: t("System"),
                current: (mode) =>
                  t("Theme: {mode}", {
                    // AppKit passes the already-translated option label.
                    mode:
                      (
                        { light: t("Light"), dark: t("Dark"), system: t("System") } as Record<
                          string,
                          string
                        >
                      )[mode] ?? mode,
                  }),
              }}
            />
            <Link
              to={ownerPath}
              aria-label={t("Your account")}
              className="inline-flex h-9 items-center gap-2 rounded-md px-2.5 text-sm text-fg-muted transition-colors hover:bg-surface-hover hover:text-fg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/50"
            >
              <UserRound size={15} />
              <span className="hidden sm:inline">{t("Your account")}</span>
            </Link>
            {remoteSession ? (
              <Button
                variant="ghost"
                size="sm"
                onClick={() => {
                  // End the session on the server too, so a copy cannot outlive
                  // sign-out; leave the browser either way.
                  void api
                    .signOut()
                    .catch(() => undefined)
                    .finally(() => {
                      setToken("");
                      window.location.reload();
                    });
                }}
              >
                <LogOut size={15} /> <span className="hidden sm:inline">{t("Sign out")}</span>
              </Button>
            ) : null}
          </>
        }
        sidebarFooter={
          <div className="flex min-w-0 items-center justify-between gap-2">
            <p className="min-w-0 truncate px-1 text-xs text-fg-subtle" title={version}>
              {version}
            </p>
            <LanguagePicker className="h-7 shrink-0 px-1.5 text-xs" />
          </div>
        }
        sidebarCollapsedFooter={
          <div className="flex flex-col items-center gap-2">
            <LanguagePicker iconOnly className="h-7 px-1.5" />
            <EtherMark size={20} />
            <p
              className="max-w-full truncate text-center text-[10px] leading-tight text-fg-subtle"
              title={version}
            >
              {version}
            </p>
          </div>
        }
        moreLabel={t("More")}
        menuLabel={t("Menu")}
        openNavigationLabel={t("Open navigation")}
        primaryNavigationLabel={t("Primary navigation")}
        mobileTabCount={4}
      >
        <SaveBarHost>
          <SettingFocus />
          <Suspense fallback={<PageFallback />}>
            <Routes>
              <Route path="/setup" element={<Setup />} />
              <Route path="/mcp/authorize" element={<McpAuthorize />} />
              <Route path="/plugins/studio" element={<Studio />} />
              <Route path="/plugins/:tab?/*" element={<Plugins />} />
              <Route path="/p/:plugin/:page/*" element={<PluginPage />} />
              {navDescriptors
                .filter((d) => d.kind === "page" && d.owner === BUILTIN && d.meta?.route)
                .flatMap((d) => {
                  const page = BUILTIN_PAGES[d.id];
                  if (!page) return [];
                  const base = d.meta!.route!;
                  const element =
                    d.id === "overview" && !(boot.setupComplete || boot.ownerConfigured) ? (
                      <Navigate to="/setup" replace />
                    ) : (
                      page.element
                    );
                  return [
                    <Route
                      key={d.id}
                      path={`${base === "/" ? "" : base}${page.suffix ?? ""}` || "/"}
                      element={element}
                    >
                      {page.children}
                    </Route>,
                    ...(page.extra ?? []).map(({ suffix, element }) => (
                      <Route
                        key={`${d.id}:${suffix}`}
                        path={`${base}${suffix}`}
                        element={element}
                      />
                    )),
                  ];
                })}
              <Route
                path="*"
                element={
                  registryLoading ? (
                    <PageFallback />
                  ) : registryError ? (
                    <ErrorPanel error={registryError} onRetry={reloadRegistry} />
                  ) : (
                    <Navigate to="/" replace />
                  )
                }
              />
            </Routes>
          </Suspense>
        </SaveBarHost>
      </AppShell>
    </div>
  );
}

/** Viewer profiles are numeric ids. Anything else is the people list. */
function ViewerById() {
  const { id } = useParams();
  if (!id || !/^[1-9]\d*$/.test(id)) return <Navigate to="/viewers" replace />;
  return <ViewerProfile />;
}

function Gate({ onReady }: { onReady: (animation?: boolean) => void }) {
  const [boot, setBoot] = useState<Bootstrap | null>(null);
  const [error, setError] = useState<unknown>(null);
  const [unlocked, setUnlocked] = useState(false);

  const loadBoot = useCallback(async () => {
    try {
      const next = await api.bootstrap();
      setBoot(next);
      onReady(next.startupAnimation !== false);
      setError(null);
    } catch (e) {
      setError(e);
    }
  }, [onReady]);
  useEffect(() => {
    void loadBoot();
  }, [loadBoot]);
  useEffect(() => {
    if (error) onReady();
  }, [boot, error, onReady]);

  if (!boot && isStarting(error)) return <Startup onReady={() => void loadBoot()} />;
  if (!boot) {
    return (
      <main className="grid min-h-screen place-items-center bg-bg-subtle p-4">
        {error ? (
          <div className="w-full max-w-md">
            <ErrorPanel error={error} onRetry={() => void loadBoot()} />
          </div>
        ) : (
          <div className="flex flex-col items-center gap-3 text-sm text-fg-muted">
            <EtherMark size={40} motion="loop" />
            <span role="status">{t("Connecting to Ether…")}</span>
          </div>
        )}
      </main>
    );
  }
  if (boot.authRequired && !boot.authenticated && !unlocked) {
    return (
      <Login
        boot={boot}
        onReady={() => {
          setUnlocked(true);
          void loadBoot();
        }}
      />
    );
  }
  return (
    <AppProvider boot={boot} reloadBoot={loadBoot}>
      <Shell />
    </AppProvider>
  );
}

/** AppKit's shared dialogs and drawers, labelled in the active language. */
function Chrome({ children }: { children: ReactNode }) {
  return (
    <DrawerTextProvider
      value={{
        close: t("Close"),
        fullscreen: t("Fullscreen"),
        exitFullscreen: t("Exit fullscreen"),
      }}
    >
      {children}
      <ConfirmRoot
        labels={{
          defaultTitle: t("Are you sure?"),
          dangerTitle: t("Are you sure?"),
          confirm: t("Confirm"),
          cancel: t("Cancel"),
        }}
      />
      <PromptRoot defaultConfirmLabel={t("Save")} defaultCancelLabel={t("Cancel")} />
    </DrawerTextProvider>
  );
}

const Me = lazy(() => import("./pages/me/Me"));

export default function App() {
  const location = useLocation();
  const [ready, setReady] = useState(false);
  // Decided at page open from the last known server value, so the splash's
  // minimum time runs from load (not from bootstrap) and never flashes on a
  // server that turned it off. The server's answer can only hide it.
  const [animation, setAnimation] = useState<boolean>(() => {
    try {
      return localStorage.getItem("ether.startupAnimation") !== "false";
    } catch {
      return true;
    }
  });
  const onReady = useCallback((enabled?: boolean) => {
    if (enabled !== undefined) {
      try {
        localStorage.setItem("ether.startupAnimation", String(enabled));
      } catch {
        /* Storage is optional. */
      }
      if (!enabled) setAnimation(false);
    }
    setReady(true);
  }, []);
  return (
    <I18nProvider>
      {() => (
        <ThemeProvider {...theme}>
          <UiLinkProvider link={RouterAnchor}>
            <Chrome>
              {location.pathname === "/me" ? (
                <Suspense fallback={<PageFallback />}>
                  <Me />
                </Suspense>
              ) : (
                <Gate onReady={onReady} />
              )}
            </Chrome>
            {location.pathname !== "/me" && animation ? <BrandSplash ready={ready} /> : null}
            <UpdateReady />
            <Toaster richColors closeButton position="bottom-right" />
          </UiLinkProvider>
        </ThemeProvider>
      )}
    </I18nProvider>
  );
}
