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
import { t } from "./i18n";
import { I18nProvider, LanguagePicker } from "./i18n/react";
import { Brand, BrandSplash, EtherMark } from "./components/brand";
import { CommandSearch } from "./components/CommandSearch";
import { Drawer } from "./components/Drawer";
import { ErrorPanel } from "./components/feedback";
import { SaveBarHost } from "./components/SaveBar";
import { UpdateReady } from "./components/UpdateReady";
import { LEVEL_LABEL, LevelBadge, StatusDot } from "./components/status";
import { api, getToken, setToken } from "./lib/api";
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
const Releases = lazy(() => import("./pages/releases/Releases"));
const Cache = lazy(() => import("./pages/cache/Cache"));
const Playback = lazy(() => import("./pages/playback/Playback"));
const Profile = lazy(() => import("./pages/Profile"));

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

/**
 * Grouped by the owner's questions: what's happening and who watches; what
 * viewers see in their apps; how titles reach the screen; how the server runs.
 */
function navigation(): SidebarNavGroup[] {
  const icon = 18;
  return [
    {
      label: "",
      items: [
        { href: "/", label: t("Overview"), icon: <Gauge size={icon} />, exact: true, mobile: true },
        {
          href: "/activity",
          label: t("Activity"),
          icon: <ActivityIcon size={icon} />,
          mobile: true,
        },
        { href: "/viewers", label: t("Viewers"), icon: <Users size={icon} />, mobile: true },
      ],
    },
    {
      label: t("Library"),
      items: [
        { href: "/catalog", label: t("Catalog"), icon: <Library size={icon} /> },
        {
          href: "/collections",
          label: t("Collections"),
          icon: <Layers size={icon} />,
          mobile: true,
        },
        { href: "/home", label: t("Home"), icon: <LayoutTemplate size={icon} /> },
        { href: "/livetv", label: t("Live TV"), icon: <Tv size={icon} /> },
        { href: "/calendar", label: t("Calendar"), icon: <CalendarDays size={icon} /> },
        { href: "/metadata", label: t("Metadata"), icon: <Tags size={icon} /> },
      ],
    },
    {
      label: t("Playback"),
      items: [
        { href: "/sources", label: t("Sources"), icon: <Network size={icon} /> },
        { href: "/releases", label: t("Releases"), icon: <ListFilter size={icon} /> },
        { href: "/cache", label: t("Cache"), icon: <HardDrive size={icon} /> },
        { href: "/playback", label: t("Playback"), icon: <MonitorPlay size={icon} /> },
      ],
    },
    {
      label: t("Server"),
      items: [
        { href: "/server", label: t("General"), icon: <Server size={icon} /> },
        { href: "/apps", label: t("Apps"), icon: <Globe size={icon} /> },
        { href: "/services", label: t("Integrations"), icon: <Plug size={icon} /> },
        { href: "/notifications", label: t("Notifications"), icon: <Bell size={icon} /> },
        { href: "/cluster", label: t("Cluster"), icon: <Boxes size={icon} /> },
        { href: "/maintenance", label: t("Maintenance"), icon: <LifeBuoy size={icon} /> },
      ],
    },
  ];
}

function HealthIndicator() {
  const { health, overall, dashboard, dashboardError, boot } = useApp();
  const [open, setOpen] = useState(false);
  // Before setup finishes, unconfigured sources and an unclaimed server are
  // expected, not problems: show progress instead of a red verdict.
  const settingUp = !boot.setupComplete;
  // "Operational" means no participating failure. It does not mean TLS was verified.
  const label = settingUp
    ? t("Setting up")
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
          <StatusDot level={settingUp ? "unknown" : overall === "off" ? "ok" : overall} pulse />
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
                  <span className="block truncate text-xs text-fg-muted">{h.summary}</span>
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

function Shell() {
  const { boot } = useApp();
  const location = useLocation();
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
        groups={location.pathname === "/setup" ? [] : navigation()}
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
            <ThemeToggle
              collapsed
              labels={{
                label: t("Theme"),
                light: t("Light"),
                dark: t("Dark"),
                system: t("System"),
                current: (mode) =>
                  t("Theme: {mode}", {
                    mode: mode === "light" ? t("Light") : mode === "dark" ? t("Dark") : t("System"),
                  }),
              }}
            />
            <Link
              to="/profile"
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
          <Suspense fallback={<PageFallback />}>
            <Routes>
              <Route
                path="/"
                element={
                  boot.setupComplete || boot.ownerConfigured ? (
                    <Home />
                  ) : (
                    <Navigate to="/setup" replace />
                  )
                }
              />
              <Route path="/setup" element={<Setup />} />
              <Route path="/activity/:tab?" element={<Activity />} />
              <Route path="/viewers" element={<ViewersList />} />
              <Route path="/viewers/:id/:tab?" element={<ViewerById />} />
              <Route path="/home" element={<Designer />} />
              <Route path="/collections" element={<CollectionsLayout />}>
                <Route path="new" element={<CollectionDrawer fresh />} />
                <Route path=":id" element={<CollectionDrawer />} />
              </Route>
              <Route path="/livetv/:tab?" element={<LiveTV />} />
              <Route path="/calendar/:view?" element={<Calendar />} />
              <Route path="/catalog/:tab?" element={<Catalog />} />
              <Route path="/metadata/:tab?" element={<Metadata />} />
              <Route path="/sources/:tab?/:sub?" element={<Sources />} />
              <Route path="/releases/:tab?" element={<Releases />} />
              <Route path="/cache/:tab?" element={<Cache />} />
              <Route path="/playback/:tab?/:section?" element={<Playback />} />
              <Route path="/notifications/:tab?" element={<Notifications />} />
              <Route path="/profile" element={<Profile />} />
              <Route path="/server/:tab?" element={<GeneralPage />} />
              <Route path="/apps/:tab?/:section?" element={<Apps />} />
              <Route path="/services/:tab?" element={<ServicesPage />} />
              <Route path="/cluster" element={<ClusterPage />} />
              <Route path="/maintenance/:tab?/:backupTab?" element={<MaintenancePage />} />
              <Route path="*" element={<Navigate to="/" replace />} />
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

function Gate({ onReady }: { onReady: () => void }) {
  const [boot, setBoot] = useState<Bootstrap | null>(null);
  const [error, setError] = useState<unknown>(null);
  const [unlocked, setUnlocked] = useState(false);

  const loadBoot = useCallback(async () => {
    try {
      setBoot(await api.bootstrap());
      setError(null);
    } catch (e) {
      setError(e);
    }
  }, []);
  useEffect(() => {
    void loadBoot();
  }, [loadBoot]);
  useEffect(() => {
    if (boot || error) onReady();
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
  const onReady = useCallback(() => setReady(true), []);
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
            {location.pathname !== "/me" ? <BrandSplash ready={ready} /> : null}
            <UpdateReady />
            <Toaster richColors closeButton position="bottom-right" />
          </UiLinkProvider>
        </ThemeProvider>
      )}
    </I18nProvider>
  );
}
