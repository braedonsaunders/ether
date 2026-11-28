import {
  Alert,
  AlertDescription,
  AlertTitle,
  Badge,
  Button,
  EmptyState,
  Input,
  Switch,
  buttonVariants,
} from "@braedonsaunders/appkit-ui";
import {
  ArrowUpCircle,
  CheckCircle2,
  Download,
  ExternalLink,
  Plus,
  RefreshCw,
  Search,
  ShieldCheck,
  Store,
} from "lucide-react";
import { useCallback, useEffect, useMemo, useState } from "react";
import { Artwork } from "../../components/Artwork";
import { Drawer } from "../../components/Drawer";
import { Section } from "../../components/layout";
import { t, tn } from "../../i18n";
import { useLocale } from "../../i18n/react";
import { apiFetch, errorMessage } from "../../lib/api";
import { confirmLibraryRebuild } from "./rebuildLibrary";
import { invalidateRegistry } from "../../lib/extend";
import { useStreamEvent } from "../../lib/stream";
import { PermissionSheet, RuntimeLimitsCard } from "./PermissionSheet";
import { safeHref } from "../../viewspec/cells";
import {
  categoryLabel,
  kindIcon,
  kindLabel,
  type CatalogueEntry,
  type InstallPreview,
  type PluginSummary,
} from "./types";

const SUBMIT_PLUGIN_URL = "https://github.com/braedonsaunders/ether-plugins#submit-a-plugin";

const KIND_FILTERS = [
  "all",
  "source",
  "integration",
  "notification_channel",
  "livetv_source",
  "extension",
  "content",
] as const;

interface CatalogueResponse {
  entries?: CatalogueEntry[];
  configured?: boolean;
  fetchedAt?: number;
  lastError?: string;
}

export function CatalogueTab({
  onInstalled,
  onSelectInstalled,
  onOpenInstall,
  onBrowseDirectories,
}: {
  onInstalled: (pluginId: string) => void;
  onSelectInstalled: (pluginId: string) => void;
  onOpenInstall: () => void;
  onBrowseDirectories?: () => void;
}) {
  const { locale } = useLocale();
  const [entries, setEntries] = useState<CatalogueEntry[]>([]);
  const [loading, setLoading] = useState(true);
  const [configured, setConfigured] = useState(true);
  const [fetchFailed, setFetchFailed] = useState(false);
  const [refreshing, setRefreshing] = useState(false);
  const [refreshErr, setRefreshErr] = useState<string | null>(null);

  const [query, setQuery] = useState("");
  const [kindFilter, setKindFilter] = useState("all");
  const [categoryFilter, setCategoryFilter] = useState("all");

  const [selected, setSelected] = useState<CatalogueEntry | null>(null);
  const [preview, setPreview] = useState<InstallPreview | null>(null);
  const [viewers, setViewers] = useState<number[]>([]);
  const [folders, setFolders] = useState<string[]>([]);
  const [operatorHosts, setOperatorHosts] = useState<string[]>([]);
  const [shadow, setShadow] = useState(false);
  const [installing, setInstalling] = useState(false);
  const [installErr, setInstallErr] = useState<string | null>(null);

  const loadCatalogue = useCallback(async (quiet = false) => {
    if (!quiet) setLoading(true);
    try {
      const res = await apiFetch<CatalogueResponse>("/plugins/catalogue");
      const list = Array.isArray(res?.entries) ? res.entries : [];
      const isConfigured = res?.configured !== false;
      const failedInitialFetch =
        isConfigured && list.length === 0 && !res?.fetchedAt && Boolean(res?.lastError);
      setEntries(list);
      setConfigured(isConfigured);
      setFetchFailed(failedInitialFetch);
      setSelected((prev) => (prev ? (list.find((e) => e.id === prev.id) ?? prev) : null));
    } catch {
      setEntries([]);
      setConfigured(true);
      setFetchFailed(true);
    } finally {
      if (!quiet) setLoading(false);
    }
  }, []);

  useEffect(() => {
    void loadCatalogue();
  }, [loadCatalogue]);

  useStreamEvent("plugins.changed", () => {
    void loadCatalogue(true);
  });

  const handleRefresh = async () => {
    setRefreshing(true);
    setRefreshErr(null);
    try {
      await apiFetch<CatalogueResponse>("/plugins/catalogue/refresh", {
        method: "POST",
        body: JSON.stringify({}),
      });
      await loadCatalogue(true);
    } catch (e) {
      setRefreshErr(errorMessage(e));
    } finally {
      setRefreshing(false);
    }
  };

  const categories = useMemo(() => {
    const seen = new Set<string>();
    for (const entry of entries) {
      if (entry.category) seen.add(entry.category);
    }
    return Array.from(seen);
  }, [entries]);

  const filtered = useMemo(() => {
    const q = query.trim().toLowerCase();
    return entries.filter((entry) => {
      if (kindFilter !== "all" && entry.kind !== kindFilter) return false;
      if (categoryFilter !== "all" && entry.category !== categoryFilter) return false;
      if (!q) return true;
      return (
        entry.name.toLowerCase().includes(q) ||
        entry.summary.toLowerCase().includes(q) ||
        entry.id.toLowerCase().includes(q) ||
        (entry.description && entry.description.toLowerCase().includes(q)) ||
        (entry.author && entry.author.toLowerCase().includes(q)) ||
        (entry.category && categoryLabel(entry.category).toLowerCase().includes(q))
      );
    });
  }, [entries, query, kindFilter, categoryFilter, locale]);

  useEffect(() => {
    let active = true;
    setPreview(null);
    if (selected && !selected.installed) {
      apiFetch<InstallPreview>("/plugins/install/preview", {
        method: "POST",
        body: JSON.stringify({ kind: "catalogue", id: selected.id }),
      })
        .then((result) => {
          if (active) setPreview(result);
        })
        .catch((e) => {
          if (active) setInstallErr(errorMessage(e));
        });
    }
    return () => {
      active = false;
    };
  }, [selected]);

  const openEntry = (entry: CatalogueEntry) => {
    setSelected(entry);
    setViewers([]);
    setFolders([]);
    setOperatorHosts([]);
    setShadow(false);
    setInstallErr(null);
  };

  const handleInstall = async () => {
    if (!selected || !preview) return;
    setInstalling(true);
    setInstallErr(null);
    const install = (rebuildConfirmation?: string) =>
      apiFetch<PluginSummary>("/plugins/install", {
        method: "POST",
        body: JSON.stringify({
          kind: "catalogue",
          id: selected.id,
          replaceLibraries: rebuildConfirmation ? true : undefined,
          rebuildConfirmation,
          grants: {
            viewers: preview.needsViewers ? viewers : undefined,
            folders: preview.needsFolders ? folders : undefined,
            operatorHosts: preview.needsOperatorHosts ? operatorHosts : undefined,
          },
          shadow,
          acceptAddedPermissions: !!preview.addedPermissions?.length,
        }),
      });
    try {
      const installed = await install().catch(async (error: unknown) => {
        const confirmation = await confirmLibraryRebuild(error);
        if (!confirmation) throw error;
        return install(confirmation);
      });
      invalidateRegistry();
      setSelected(null);
      onInstalled(installed.id);
    } catch (e) {
      setInstallErr(errorMessage(e));
    } finally {
      setInstalling(false);
    }
  };

  const headerActions = (
    <>
      <a
        href={SUBMIT_PLUGIN_URL}
        target="_blank"
        rel="noreferrer"
        className="inline-flex items-center gap-1 px-2 py-1 text-xs text-fg-muted transition-colors hover:text-fg"
      >
        {t("Submit a plugin")} <ExternalLink size={12} aria-hidden />
      </a>
      {configured ? (
        <Button
          size="sm"
          variant="outline"
          onClick={() => void handleRefresh()}
          disabled={refreshing || loading}
        >
          <RefreshCw size={13} className={refreshing ? "animate-spin" : undefined} aria-hidden />{" "}
          {t("Refresh")}
        </Button>
      ) : null}
    </>
  );

  if (loading) {
    return (
      <Section title={t("Marketplace")} actions={headerActions}>
        <p className="text-xs text-fg-muted">{t("Loading marketplace…")}</p>
      </Section>
    );
  }

  if (!configured || fetchFailed) {
    return (
      <Section title={t("Marketplace")} actions={headerActions}>
        {refreshErr ? (
          <Alert variant="destructive">
            <AlertTitle>{t("Could not refresh marketplace")}</AlertTitle>
            <AlertDescription>{refreshErr}</AlertDescription>
          </Alert>
        ) : null}
        <EmptyState
          icon={<Store size={18} />}
          title={
            !configured ? t("No marketplace configured") : t("Could not reach the marketplace")
          }
          description={
            !configured
              ? t(
                  "You can still install plugins directly from a manifest link, a GitHub repository, or a package file.",
                )
              : t(
                  "Check this server's internet connection and refresh, or install a plugin directly from a link or package file.",
                )
          }
          action={
            <div className="flex flex-wrap justify-center gap-2">
              {configured ? (
                <Button
                  size="sm"
                  variant="outline"
                  onClick={() => void handleRefresh()}
                  disabled={refreshing}
                >
                  <RefreshCw
                    size={14}
                    className={refreshing ? "animate-spin" : undefined}
                    aria-hidden
                  />{" "}
                  {t("Refresh")}
                </Button>
              ) : null}
              {onBrowseDirectories ? (
                <Button size="sm" variant="outline" onClick={onBrowseDirectories}>
                  {t("Browse plugin directories")}
                </Button>
              ) : null}
              <Button size="sm" onClick={onOpenInstall}>
                <Plus size={14} /> {t("Install from a link")}
              </Button>
            </div>
          }
        />
      </Section>
    );
  }

  if (entries.length === 0) {
    return (
      <Section
        title={t("Marketplace")}
        description={t(
          "Reviewed plugins for sources, subtitles, notifications and household tools.",
        )}
        actions={headerActions}
      >
        {refreshErr ? (
          <Alert variant="destructive">
            <AlertTitle>{t("Could not refresh marketplace")}</AlertTitle>
            <AlertDescription>{refreshErr}</AlertDescription>
          </Alert>
        ) : null}
        <EmptyState
          icon={<Store size={18} />}
          title={t("No plugins in the marketplace yet")}
          description={t("Reviewed community plugins appear here once published.")}
          action={
            <a
              href={SUBMIT_PLUGIN_URL}
              target="_blank"
              rel="noreferrer"
              className={buttonVariants({ variant: "outline", size: "sm" })}
            >
              {t("Submit a plugin")} <ExternalLink size={13} aria-hidden />
            </a>
          }
        />
      </Section>
    );
  }

  const isSelectedInstalled = Boolean(selected?.installed);
  const selectedIcon = selected?.icon || preview?.icon;

  return (
    <div className="space-y-6">
      <Section
        title={t("Marketplace")}
        description={t(
          "Reviewed plugins for sources, subtitles, notifications and household tools.",
        )}
        actions={headerActions}
      >
        {refreshErr ? (
          <Alert variant="destructive">
            <AlertTitle>{t("Could not refresh marketplace")}</AlertTitle>
            <AlertDescription>{refreshErr}</AlertDescription>
          </Alert>
        ) : null}

        <div className="space-y-3">
          <div className="flex flex-wrap items-center justify-between gap-3">
            <div className="relative min-w-[240px] flex-1">
              <Search
                size={15}
                className="pointer-events-none absolute left-3 top-1/2 -translate-y-1/2 text-fg-subtle"
                aria-hidden
              />
              <Input
                value={query}
                onChange={(e) => setQuery(e.currentTarget.value)}
                placeholder={t("Search catalogue…")}
                aria-label={t("Search catalogue")}
                className="pl-9"
              />
            </div>
            <div
              className="flex flex-wrap items-center gap-1"
              role="group"
              aria-label={t("Filter by kind")}
            >
              {KIND_FILTERS.map((kind) => (
                <button
                  key={kind}
                  type="button"
                  onClick={() => setKindFilter(kind)}
                  aria-pressed={kindFilter === kind}
                  className={`rounded-md border px-2.5 py-1 text-xs font-medium transition-colors ${
                    kindFilter === kind
                      ? "border-accent bg-accent-subtle text-accent"
                      : "border-border bg-surface text-fg-muted hover:bg-surface-hover hover:text-fg"
                  }`}
                >
                  {kind === "all" ? t("All kinds") : kindLabel(kind)}
                </button>
              ))}
            </div>
          </div>

          {categories.length > 0 ? (
            <div
              className="flex flex-wrap items-center gap-1.5"
              role="group"
              aria-label={t("Filter by category")}
            >
              <button
                type="button"
                onClick={() => setCategoryFilter("all")}
                aria-pressed={categoryFilter === "all"}
                className={`rounded-full border px-2.5 py-1 text-xs font-medium transition-colors ${
                  categoryFilter === "all"
                    ? "border-accent bg-accent-subtle text-accent"
                    : "border-border bg-surface text-fg-muted hover:bg-surface-hover hover:text-fg"
                }`}
              >
                {t("All categories")}
              </button>
              {categories.map((cat) => (
                <button
                  key={cat}
                  type="button"
                  onClick={() => setCategoryFilter(cat)}
                  aria-pressed={categoryFilter === cat}
                  className={`rounded-full border px-2.5 py-1 text-xs font-medium transition-colors ${
                    categoryFilter === cat
                      ? "border-accent bg-accent-subtle text-accent"
                      : "border-border bg-surface text-fg-muted hover:bg-surface-hover hover:text-fg"
                  }`}
                >
                  {categoryLabel(cat)}
                </button>
              ))}
            </div>
          ) : null}
        </div>

        {filtered.length === 0 ? (
          <div className="mt-4">
            <EmptyState
              icon={<Search size={18} />}
              title={t("No matching plugins")}
              description={t("Try clearing your search or choosing a different kind or category.")}
              action={
                <Button
                  size="sm"
                  variant="outline"
                  onClick={() => {
                    setQuery("");
                    setKindFilter("all");
                    setCategoryFilter("all");
                  }}
                >
                  {t("Clear filters")}
                </Button>
              }
            />
          </div>
        ) : (
          <div className="mt-4 grid gap-3 sm:grid-cols-2">
            {filtered.map((entry) => (
              <button
                key={entry.id}
                type="button"
                onClick={() => openEntry(entry)}
                className="flex flex-col justify-between gap-3 rounded-xl border border-border bg-surface p-4 text-left transition-colors hover:border-border-strong hover:bg-surface-hover"
              >
                <div className="flex items-start gap-3.5">
                  {entry.icon ? (
                    <Artwork
                      src={entry.icon}
                      alt={entry.name}
                      title=""
                      className="size-11 shrink-0 rounded-lg bg-bg-subtle"
                      imgClassName="object-contain"
                      icon={kindIcon(entry.kind, 20)}
                    />
                  ) : (
                    <span
                      aria-hidden
                      className="flex size-11 shrink-0 items-center justify-center rounded-lg bg-primary/10 text-primary"
                    >
                      {kindIcon(entry.kind, 22)}
                    </span>
                  )}
                  <div className="min-w-0 flex-1">
                    <div className="flex flex-wrap items-center gap-1.5">
                      <span className="truncate text-sm font-semibold text-fg">{entry.name}</span>
                      <Badge variant="success">
                        <ShieldCheck size={11} aria-hidden /> {t("Reviewed")}
                      </Badge>
                      {entry.installed ? (
                        <Badge variant="info">
                          <CheckCircle2 size={11} aria-hidden /> {t("Installed")}
                        </Badge>
                      ) : null}
                      {entry.updateAvailable ? (
                        <Badge variant="warning">
                          <ArrowUpCircle size={11} aria-hidden /> {t("Update available")}
                        </Badge>
                      ) : null}
                    </div>
                    <p className="mt-1 line-clamp-2 text-xs text-fg-muted">{entry.summary}</p>
                  </div>
                </div>

                <div className="flex flex-wrap items-center justify-between gap-2 border-t border-border/60 pt-2.5 text-[11px] text-fg-subtle">
                  <div className="flex flex-wrap items-center gap-1.5">
                    <Badge>{kindLabel(entry.kind)}</Badge>
                    {entry.category && categoryLabel(entry.category) !== kindLabel(entry.kind) ? (
                      <Badge>{categoryLabel(entry.category)}</Badge>
                    ) : null}
                  </div>
                  {entry.latestVersion ? (
                    <span className="font-mono">v{entry.latestVersion}</span>
                  ) : null}
                </div>
              </button>
            ))}
          </div>
        )}
      </Section>

      <Drawer
        open={selected !== null}
        onClose={() => setSelected(null)}
        title={selected?.name ?? t("Plugin")}
        description={
          selected
            ? selected.latestVersion
              ? `${selected.id} (v${selected.latestVersion})`
              : selected.id
            : undefined
        }
        size="lg"
        footer={
          selected ? (
            <div className="flex w-full items-center justify-between gap-2">
              <Button variant="ghost" onClick={() => setSelected(null)}>
                {t("Close")}
              </Button>
              {isSelectedInstalled ? (
                <Button
                  onClick={() => {
                    const id = selected.id;
                    setSelected(null);
                    onSelectInstalled(id);
                  }}
                >
                  {t("Open installed plugin")}
                </Button>
              ) : (
                <Button onClick={handleInstall} disabled={installing || !preview}>
                  <Download size={14} /> {t("Install")}
                </Button>
              )}
            </div>
          ) : undefined
        }
      >
        {selected ? (
          <div className="space-y-5">
            {installErr ? (
              <Alert variant="destructive">
                <AlertTitle>{t("Could not install plugin")}</AlertTitle>
                <AlertDescription>{installErr}</AlertDescription>
              </Alert>
            ) : null}

            <div className="flex items-start gap-3.5 rounded-xl border border-border bg-bg-subtle p-4">
              {selectedIcon ? (
                <Artwork
                  src={selectedIcon}
                  alt={selected.name}
                  title=""
                  className="size-12 shrink-0 rounded-lg bg-bg-subtle"
                  imgClassName="object-contain"
                  icon={kindIcon(selected.kind, 20)}
                />
              ) : (
                <span
                  aria-hidden
                  className="flex size-12 shrink-0 items-center justify-center rounded-lg bg-primary/10 text-primary"
                >
                  {kindIcon(selected.kind, 22)}
                </span>
              )}
              <div className="min-w-0 flex-1">
                <div className="flex flex-wrap items-center gap-2">
                  <h3 className="text-base font-semibold text-fg">{selected.name}</h3>
                  <Badge variant="success">
                    <ShieldCheck size={11} aria-hidden /> {t("Reviewed")}
                  </Badge>
                  {selected.installed ? (
                    <Badge variant="info">
                      <CheckCircle2 size={11} aria-hidden /> {t("Installed")}
                    </Badge>
                  ) : null}
                  {selected.updateAvailable ? (
                    <Badge variant="warning">
                      <ArrowUpCircle size={11} aria-hidden /> {t("Update available")}
                    </Badge>
                  ) : null}
                  <Badge>{kindLabel(selected.kind)}</Badge>
                  {selected.category &&
                  categoryLabel(selected.category) !== kindLabel(selected.kind) ? (
                    <Badge>{categoryLabel(selected.category)}</Badge>
                  ) : null}
                  {selected.latestVersion ? (
                    <span className="font-mono text-xs text-fg-subtle">
                      v{selected.latestVersion}
                    </span>
                  ) : null}
                </div>
                <p className="mt-1 text-xs text-fg-muted">{selected.summary}</p>
                {selected.description ? (
                  <p className="mt-2 text-xs text-fg">{selected.description}</p>
                ) : null}
                {selected.author || selected.license || selected.homepage || selected.repo ? (
                  <div className="mt-2.5 flex flex-wrap items-center gap-3 text-xs text-fg-muted">
                    {selected.author ? (
                      <span>{t("By {author}", { author: selected.author })}</span>
                    ) : null}
                    {selected.license ? <span>{selected.license}</span> : null}
                    {selected.homepage && safeHref(selected.homepage)?.startsWith("https://") ? (
                      <a
                        href={safeHref(selected.homepage)!}
                        target="_blank"
                        rel="noreferrer"
                        className="inline-flex items-center gap-1 text-accent hover:underline"
                      >
                        {t("Homepage")} <ExternalLink size={12} aria-hidden />
                      </a>
                    ) : null}
                    {selected.repo && safeHref(selected.repo)?.startsWith("https://") ? (
                      <a
                        href={safeHref(selected.repo)!}
                        target="_blank"
                        rel="noreferrer"
                        className="inline-flex items-center gap-1 text-accent hover:underline"
                      >
                        {t("Source repository")} <ExternalLink size={12} aria-hidden />
                      </a>
                    ) : null}
                  </div>
                ) : null}
              </div>
            </div>

            {selected.screenshots && selected.screenshots.length > 0 ? (
              <div className="space-y-2">
                <h4 className="text-xs font-semibold uppercase tracking-wider text-fg-muted">
                  {tn(selected.screenshots.length, "{count} screenshot", "{count} screenshots")}
                </h4>
                <div className="grid grid-cols-2 gap-2.5">
                  {selected.screenshots.map((shot, idx) => (
                    <Artwork
                      key={shot}
                      src={shot}
                      alt={t("{name} preview {index}", { name: selected.name, index: idx + 1 })}
                      className="rounded-lg border border-border"
                    />
                  ))}
                </div>
              </div>
            ) : null}

            {preview ? (
              <>
                <RuntimeLimitsCard
                  limits={preview.runtimeLimits}
                  requested={preview.manifest.ether?.runtime?.limits}
                />
                <PermissionSheet
                  permissions={preview.permissions}
                  durableQuota={preview.durableQuota}
                  verified={preview.verified}
                  needsViewers={preview.needsViewers}
                  needsFolders={preview.needsFolders}
                  needsOperatorHosts={preview.needsOperatorHosts}
                  viewers={viewers}
                  onChangeViewers={setViewers}
                  folders={folders}
                  onChangeFolders={setFolders}
                  operatorHosts={operatorHosts}
                  pendingOperatorHosts={preview.pendingOperatorHosts}
                  onChangeOperatorHosts={setOperatorHosts}
                  addedPermissions={preview.addedPermissions}
                />
              </>
            ) : isSelectedInstalled && selected.permissionLines ? (
              <PermissionSheet
                permissions={selected.permissionLines}
                verified
                viewers={viewers}
                onChangeViewers={setViewers}
                folders={folders}
                onChangeFolders={setFolders}
              />
            ) : null}

            {!isSelectedInstalled ? (
              <div className="flex items-center justify-between gap-4 rounded-xl border border-border bg-surface px-4 py-3">
                <div className="min-w-0">
                  <div className="text-xs font-medium text-fg">{t("Start in shadow mode")}</div>
                  <p className="text-xs text-fg-muted">
                    {t("Record what this plugin would change without touching your library.")}
                  </p>
                </div>
                <Switch
                  aria-label={t("Start in shadow mode")}
                  checked={shadow}
                  onChange={(e) => setShadow(e.currentTarget.checked)}
                />
              </div>
            ) : null}
          </div>
        ) : null}
      </Drawer>
    </div>
  );
}
