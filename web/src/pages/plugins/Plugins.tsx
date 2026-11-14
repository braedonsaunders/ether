import {
  Badge,
  Button,
  EmptyState,
  Popover,
  Switch,
  buttonVariants,
} from "@braedonsaunders/appkit-ui";
import {
  ArrowUpCircle,
  Code2,
  Eye,
  FolderSearch,
  Link2,
  Plus,
  Puzzle,
  Sparkles,
  Upload,
} from "lucide-react";
import { useCallback, useEffect, useMemo, useState, type ReactNode } from "react";
import { Link, useNavigate, useSearchParams } from "react-router-dom";
import { Artwork } from "../../components/Artwork";
import { ErrorPanel } from "../../components/feedback";
import { DomainPage, useRouteTab, type PageTab } from "../../components/layout";
import { LevelBadge } from "../../components/status";
import { t } from "../../i18n";
import { useLocale } from "../../i18n/react";
import { apiFetch } from "../../lib/api";
import { invalidateRegistry } from "../../lib/extend";
import { useStreamEvent } from "../../lib/stream";
import { CatalogueTab } from "./CatalogueTab";
import { DirectoryBrowser } from "./DirectoryBrowser";
import { openInStudio } from "./openInStudio";
import { InstallDrawer } from "./InstallDrawer";
import { InstalledPluginDrawer } from "./InstalledPluginDrawer";
import { healthHeadline } from "./drawer/health";
import { kindIcon, isAddon, kindLabel, type PluginSummary } from "./types";

type TabKey = "installed" | "marketplace";

/** Installed plugins, the marketplace and imports, on the real plugin API. */
export default function Plugins() {
  const navigate = useNavigate();
  const { locale } = useLocale();
  const [plugins, setPlugins] = useState<PluginSummary[]>([]);
  const [loading, setLoading] = useState(true);
  const [loadError, setLoadError] = useState<unknown>(null);
  const [toggling, setToggling] = useState<string | null>(null);
  // ?plugin=<id> opens that plugin's drawer (Studio links here after an
  // install, and from its Actions menu).
  const [params, setParams] = useSearchParams();
  const [open, setOpenState] = useState<string | null>(() => params.get("plugin"));
  const setOpen = (id: string | null) => {
    setOpenState(id);
    if (!id && params.has("plugin")) {
      const next = new URLSearchParams(params);
      next.delete("plugin");
      setParams(next, { replace: true });
    }
  };
  const [installUrl, setInstallUrl] = useState<string | null>(null);
  const [installMode, setInstallMode] = useState<"link" | "file">("link");
  const [browsing, setBrowsing] = useState(false);
  const [logRevision, setLogRevision] = useState(0);
  const [liveLogs, setLiveLogs] = useState<Record<string, string[]>>({});

  const load = useCallback(async (quiet = false) => {
    if (!quiet) setLoading(true);
    try {
      const res = await apiFetch<{ plugins?: PluginSummary[] }>("/plugins");
      setPlugins(Array.isArray(res?.plugins) ? res.plugins : []);
      setLoadError(null);
    } catch (e) {
      setLoadError(e);
    } finally {
      if (!quiet) setLoading(false);
    }
  }, []);
  useEffect(() => {
    void load();
  }, [load]);
  useStreamEvent("plugins.changed", () => {
    void load(true);
    invalidateRegistry();
    setLogRevision((n) => n + 1);
  });
  useStreamEvent<{ plugin?: string; id?: string; line?: string }>("plugins.log", (ev) => {
    const id = ev.data?.plugin ?? ev.data?.id;
    const line = ev.data?.line;
    if (!id || !line) return;
    setLiveLogs((prev) => ({ ...prev, [id]: [...(prev[id] ?? []), line].slice(-200) }));
    setLogRevision((n) => n + 1);
  });

  const tabs: readonly (PageTab & { key: TabKey })[] = useMemo(
    () => [
      {
        key: "installed",
        label: t("Installed"),
        count: plugins.length > 0 ? plugins.length : undefined,
      },
      { key: "marketplace", label: t("Marketplace") },
    ],
    [plugins.length, locale],
  );
  const active = useRouteTab(tabs, "installed");
  const installedIds = useMemo(() => new Set(plugins.map((p) => p.id)), [plugins]);

  const toggle = async (p: PluginSummary, on: boolean) => {
    setToggling(p.id);
    setPlugins((list) => list.map((x) => (x.id === p.id ? { ...x, enabled: on } : x)));
    try {
      await apiFetch(`/plugins/${encodeURIComponent(p.id)}/${on ? "enable" : "disable"}`, {
        method: "POST",
        body: JSON.stringify({}),
      });
      invalidateRegistry();
    } catch (e) {
      setLoadError(e);
    } finally {
      await load(true);
      setToggling(null);
    }
  };
  const installed = async (id: string) => {
    setInstallUrl(null);
    await load(true);
    invalidateRegistry();
    if (active !== "installed") navigate("/plugins/installed");
    setOpen(id);
  };

  return (
    <DomainPage
      title={t("Plugins")}
      base="/plugins"
      tabs={tabs}
      active={active}
      actions={
        <>
          <Link
            to="/plugins/reference/getting-started"
            className={buttonVariants({ variant: "outline", size: "sm" })}
          >
            {t("Reference")}
          </Link>
          <AddPluginMenu
            onBrowse={() => setBrowsing(true)}
            onLink={() => {
              setInstallMode("link");
              setInstallUrl("");
            }}
            onUpload={() => {
              setInstallMode("file");
              setInstallUrl("");
            }}
            onCode={() => navigate("/plugins/studio?view=code")}
            onAI={() => navigate("/plugins/studio")}
          />
        </>
      }
    >
      {active === "installed" ? (
        <>
          {loadError ? <ErrorPanel error={loadError} onRetry={() => void load()} /> : null}
          {loading ? (
            <p className="text-xs text-fg-muted">{t("Loading plugins…")}</p>
          ) : plugins.length === 0 && !loadError ? (
            <EmptyState
              icon={<Puzzle size={18} />}
              title={t("No plugins installed")}
              description={t(
                "Plugins add sources, channels, subtitle providers and custom pages to Ether.",
              )}
              action={
                <div className="flex flex-wrap justify-center gap-2">
                  <Button
                    size="sm"
                    variant="outline"
                    onClick={() => navigate("/plugins/marketplace")}
                  >
                    {t("Open the marketplace")}
                  </Button>
                  <Link to="/plugins/studio" className={buttonVariants({ size: "sm" })}>
                    <Sparkles size={14} /> {t("Build a plugin")}
                  </Link>
                </div>
              }
            />
          ) : (
            <ul
              className="grid gap-4 md:grid-cols-2 xl:grid-cols-3"
              aria-label={t("Installed plugins")}
            >
              {plugins.map((p) => (
                <li key={p.id}>
                  <PluginCard
                    plugin={p}
                    busy={toggling === p.id}
                    onOpen={() => setOpen(p.id)}
                    onToggle={(next) => void toggle(p, next)}
                  />
                </li>
              ))}
            </ul>
          )}
        </>
      ) : (
        <CatalogueTab
          installedIds={installedIds}
          onInstalled={(id) => void installed(id)}
          onSelectInstalled={(id) => {
            navigate("/plugins/installed");
            setOpen(id);
          }}
          onOpenInstall={() => setInstallUrl("")}
          onBrowseDirectories={() => setBrowsing(true)}
        />
      )}

      <InstalledPluginDrawer
        pluginId={open}
        onClose={() => setOpen(null)}
        onChanged={() => void load(true)}
        onRemoved={() => {
          setOpen(null);
          void load(true);
        }}
        logRevision={logRevision}
        liveLogLines={open ? (liveLogs[open] ?? []) : []}
      />
      <InstallDrawer
        open={installUrl !== null}
        initialUrl={installUrl ?? ""}
        mode={installMode}
        onClose={() => setInstallUrl(null)}
        onInstalled={(id) => void installed(id)}
      />
      {browsing ? (
        <DirectoryBrowser
          onClose={() => setBrowsing(false)}
          onImport={(address) => {
            setBrowsing(false);
            setInstallMode("link");
            setInstallUrl(address);
          }}
        />
      ) : null}
    </DomainPage>
  );
}

/** One way in for every kind of plugin, like Live TV's Add menu. */
function AddPluginMenu({
  onBrowse,
  onLink,
  onUpload,
  onCode,
  onAI,
}: {
  onBrowse: () => void;
  onLink: () => void;
  onUpload: () => void;
  onCode: () => void;
  onAI: () => void;
}) {
  const [open, setOpen] = useState(false);
  const item = (icon: ReactNode, title: string, hint: string, fn: () => void) => (
    <button
      type="button"
      role="menuitem"
      onClick={() => {
        setOpen(false);
        fn();
      }}
      className="flex w-full items-start gap-3 rounded-md px-2.5 py-2 text-left hover:bg-surface-hover"
    >
      <span className="mt-0.5 text-primary">{icon}</span>
      <span>
        <span className="block text-sm font-medium text-fg">{title}</span>
        <span className="block text-xs text-fg-muted">{hint}</span>
      </span>
    </button>
  );
  return (
    <Popover
      open={open}
      onOpenChange={setOpen}
      align="end"
      className="w-80 p-1"
      trigger={
        <Button
          size="sm"
          onClick={() => setOpen((o) => !o)}
          aria-haspopup="menu"
          aria-expanded={open}
        >
          <Plus size={14} /> {t("Add")}
        </Button>
      }
    >
      <div role="menu" aria-label={t("Add a plugin")}>
        {item(
          <Sparkles size={16} />,
          t("Describe it"),
          t("Say what you want; Ether builds and tests it"),
          onAI,
        )}
        {item(
          <Code2 size={16} />,
          t("Write the code"),
          t("Studio's editor with a live preview"),
          onCode,
        )}
        {item(
          <Upload size={16} />,
          t("Upload a file"),
          t("A .etherpkg or .zip plugin package from your computer"),
          onUpload,
        )}
        {item(
          <Link2 size={16} />,
          t("From a link"),
          t("A plugin, GitHub repository or Stremio plugin address"),
          onLink,
        )}
        {item(
          <FolderSearch size={16} />,
          t("Browse plugin directories"),
          t("Lists of plugins published by plugins you installed"),
          onBrowse,
        )}
      </div>
    </Popover>
  );
}

function PluginCard({
  plugin,
  busy,
  onOpen,
  onToggle,
}: {
  plugin: PluginSummary;
  busy: boolean;
  onOpen: () => void;
  onToggle: (on: boolean) => void;
}) {
  const health = healthHeadline(plugin);
  const navigate = useNavigate();
  return (
    // The whole card opens the drawer; its own controls (the title button
    // for keyboards, Open in Studio, the switch) keep their own actions.
    <article
      aria-label={plugin.name}
      onClick={(e) => {
        if ((e.target as HTMLElement).closest("button, a, input, label, [role=switch]")) return;
        onOpen();
      }}
      className="flex h-full cursor-pointer flex-col rounded-xl border border-border bg-surface p-5 transition hover:border-primary/40 hover:shadow-md"
    >
      <button
        type="button"
        onClick={onOpen}
        className="flex items-start gap-3 rounded-lg text-left focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/50"
      >
        {plugin.icon ? (
          <Artwork
            src={plugin.icon}
            alt={plugin.name}
            icon={kindIcon(plugin.kind, 20)}
            className="size-11 shrink-0 rounded-lg bg-bg-subtle"
            imgClassName="object-contain p-1"
          />
        ) : (
          <span
            aria-hidden
            className="flex size-11 shrink-0 items-center justify-center rounded-lg bg-primary/10 text-primary"
          >
            {kindIcon(plugin.kind, 22)}
          </span>
        )}
        <div className="min-w-0 flex-1 space-y-1">
          <h3 className="truncate font-semibold text-fg">{plugin.name}</h3>
          <div className="flex flex-wrap items-center gap-1.5">
            {!plugin.enabled ? (
              <Badge variant="secondary">{t("Off")}</Badge>
            ) : plugin.shadow ? (
              <Badge variant="info">
                <Eye size={12} aria-hidden /> {t("Trying out")}
              </Badge>
            ) : (
              <LevelBadge level={health.level} label={health.word} />
            )}
            <Badge variant="secondary">{kindLabel(plugin.kind, isAddon(plugin))}</Badge>
            {plugin.updateVersion ? (
              <Badge variant="info">
                <ArrowUpCircle size={11} aria-hidden />{" "}
                {t("Update v{version}", { version: plugin.updateVersion })}
              </Badge>
            ) : null}
            {!plugin.verified ? <Badge variant="warning">{t("Unverified")}</Badge> : null}
          </div>
        </div>
      </button>
      <p className="mt-3 line-clamp-2 min-h-10 text-sm text-fg-muted">{plugin.summary}</p>
      <div className="mt-auto flex items-center justify-between gap-3 pt-4">
        <span className="font-mono text-xs text-fg-subtle">v{plugin.version}</span>
        <Button
          variant="ghost"
          size="sm"
          className="ml-auto"
          onClick={() =>
            void openInStudio(plugin.id)
              .then((to) => navigate(to))
              .catch(() => onOpen())
          }
        >
          <Code2 size={14} /> {t("Open in Studio")}
        </Button>
        <Switch
          aria-label={t("Enable {name}", { name: plugin.name })}
          checked={plugin.enabled}
          disabled={busy}
          onChange={(e) => onToggle(e.currentTarget.checked)}
        />
      </div>
    </article>
  );
}
