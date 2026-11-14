import {
  Alert,
  AlertDescription,
  AlertTitle,
  Badge,
  Button,
  EmptyState,
  Input,
  Switch,
} from "@braedonsaunders/appkit-ui";
import {
  CheckCircle2,
  Download,
  ExternalLink,
  Plus,
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
import { invalidateRegistry } from "../../lib/extend";
import { PermissionSheet, RuntimeLimitsCard } from "./PermissionSheet";
import {
  categoryLabel,
  kindIcon,
  kindLabel,
  type CatalogueEntry,
  type InstallPreview,
  type PluginSummary,
} from "./types";

const KIND_FILTERS: { value: string; label: () => string }[] = [
  { value: "all", label: () => t("All kinds") },
  { value: "source", label: () => t("Source") },
  { value: "integration", label: () => t("Integration") },
  { value: "webhook", label: () => t("Webhook") },
  { value: "folder", label: () => t("Folder") },
  { value: "composite", label: () => t("Composite") },
];

export function CatalogueTab({
  installedIds,
  onInstalled,
  onSelectInstalled,
  onOpenInstall,
  onBrowseDirectories,
}: {
  installedIds: Set<string>;
  onInstalled: (pluginId: string) => void;
  onSelectInstalled: (pluginId: string) => void;
  onOpenInstall: () => void;
  onBrowseDirectories?: () => void;
}) {
  const { locale } = useLocale();
  const [entries, setEntries] = useState<CatalogueEntry[]>([]);
  const [loading, setLoading] = useState(true);
  const [unavailable, setUnavailable] = useState(false);

  const [query, setQuery] = useState("");
  const [kindFilter, setKindFilter] = useState("all");
  const [categoryFilter, setCategoryFilter] = useState("all");

  const [selected, setSelected] = useState<CatalogueEntry | null>(null);
  const [preview, setPreview] = useState<InstallPreview | null>(null);
  const [viewers, setViewers] = useState<number[]>([]);
  const [folders, setFolders] = useState<string[]>([]);
  const [shadow, setShadow] = useState(false);
  const [installing, setInstalling] = useState(false);
  const [installErr, setInstallErr] = useState<string | null>(null);

  const loadCatalogue = useCallback(async () => {
    setLoading(true);
    try {
      const res = await apiFetch<{ entries?: CatalogueEntry[] }>("/plugins/catalogue");
      const list = Array.isArray(res?.entries) ? res.entries : [];
      setEntries(list);
      setUnavailable(list.length === 0);
    } catch {
      setEntries([]);
      setUnavailable(true);
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void loadCatalogue();
  }, [loadCatalogue]);

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
        (entry.category && categoryLabel(entry.category).toLowerCase().includes(q))
      );
    });
  }, [entries, query, kindFilter, categoryFilter, locale]);

  useEffect(() => {
    let active = true;
    setPreview(null);
    if (selected && !installedIds.has(selected.id)) {
      apiFetch<InstallPreview>("/plugins/install/preview", {
        method: "POST",
        body: JSON.stringify({ url: selected.url }),
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
  }, [selected, installedIds]);

  const openEntry = (entry: CatalogueEntry) => {
    setSelected(entry);
    setViewers([]);
    setFolders([]);
    setShadow(false);
    setInstallErr(null);
  };

  const handleInstall = async () => {
    if (!selected || !preview) return;
    setInstalling(true);
    setInstallErr(null);
    try {
      const installed = await apiFetch<PluginSummary>("/plugins/install", {
        method: "POST",
        body: JSON.stringify({
          url: selected.url,
          grants: {
            viewers: preview.needsViewers ? viewers : undefined,
            folders: preview.needsFolders ? folders : undefined,
          },
          shadow,
          acceptAddedPermissions: !!preview.addedPermissions?.length,
        }),
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

  if (loading) {
    return (
      <Section title={t("Catalogue")}>
        <p className="text-xs text-fg-muted">{t("Loading catalogue…")}</p>
      </Section>
    );
  }

  if (unavailable) {
    return (
      <Section title={t("Catalogue")}>
        <EmptyState
          icon={<Store size={18} />}
          title={t("The catalogue is not available yet")}
          description={t(
            "You can still install plugins directly from a manifest link, a GitHub repository, or a package file.",
          )}
          action={
            <div className="flex flex-wrap justify-center gap-2">
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

  const isSelectedInstalled = selected ? installedIds.has(selected.id) : false;

  return (
    <div className="space-y-6">
      <Section
        title={t("Catalogue")}
        description={t(
          "Reviewed plugins for sources, subtitles, notifications and household tools.",
        )}
      >
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
              {KIND_FILTERS.map((f) => (
                <button
                  key={f.value}
                  type="button"
                  onClick={() => setKindFilter(f.value)}
                  aria-pressed={kindFilter === f.value}
                  className={`rounded-md border px-2.5 py-1 text-xs font-medium transition-colors ${
                    kindFilter === f.value
                      ? "border-accent bg-accent-subtle text-accent"
                      : "border-border bg-surface text-fg-muted hover:bg-surface-hover hover:text-fg"
                  }`}
                >
                  {f.label()}
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
            {filtered.map((entry) => {
              const installed = installedIds.has(entry.id);
              return (
                <button
                  key={entry.id}
                  type="button"
                  onClick={() => openEntry(entry)}
                  className="flex flex-col justify-between gap-3 rounded-xl border border-border bg-surface p-4 text-left transition-colors hover:border-border-strong hover:bg-surface-hover"
                >
                  <div className="flex items-start gap-3.5">
                    <Artwork
                      src={entry.icon}
                      alt={entry.name}
                      className="h-11 w-11 shrink-0 rounded-lg"
                      icon={kindIcon(entry.kind, 18)}
                    />
                    <div className="min-w-0 flex-1">
                      <div className="flex flex-wrap items-center gap-1.5">
                        <span className="truncate text-sm font-semibold text-fg">{entry.name}</span>
                        {entry.verified ? (
                          <Badge variant="success">
                            <ShieldCheck size={11} aria-hidden /> {t("Verified")}
                          </Badge>
                        ) : null}
                        {installed ? (
                          <Badge variant="info">
                            <CheckCircle2 size={11} aria-hidden /> {t("Installed")}
                          </Badge>
                        ) : null}
                      </div>
                      <p className="mt-1 line-clamp-2 text-xs text-fg-muted">{entry.summary}</p>
                    </div>
                  </div>

                  <div className="flex flex-wrap items-center justify-between gap-2 border-t border-border/60 pt-2.5 text-[11px] text-fg-subtle">
                    <div className="flex flex-wrap items-center gap-1.5">
                      <Badge>{kindLabel(entry.kind)}</Badge>
                      {entry.category ? <Badge>{categoryLabel(entry.category)}</Badge> : null}
                    </div>
                    <span className="font-mono">v{entry.version}</span>
                  </div>
                </button>
              );
            })}
          </div>
        )}
      </Section>

      <Drawer
        open={selected !== null}
        onClose={() => setSelected(null)}
        title={selected?.name ?? t("Plugin")}
        description={selected ? `${selected.id} (v${selected.version})` : undefined}
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
              <Artwork
                src={selected.icon}
                alt={selected.name}
                className="h-12 w-12 shrink-0 rounded-lg"
                icon={kindIcon(selected.kind, 20)}
              />
              <div className="min-w-0 flex-1">
                <div className="flex flex-wrap items-center gap-2">
                  <h3 className="text-base font-semibold text-fg">{selected.name}</h3>
                  <Badge>{kindLabel(selected.kind)}</Badge>
                  {selected.category ? <Badge>{categoryLabel(selected.category)}</Badge> : null}
                  <span className="font-mono text-xs text-fg-subtle">v{selected.version}</span>
                </div>
                <p className="mt-1 text-xs text-fg-muted">{selected.summary}</p>
                {selected.description ? (
                  <p className="mt-2 text-xs text-fg">{selected.description}</p>
                ) : null}
                {selected.author?.name || selected.repo ? (
                  <div className="mt-2.5 flex flex-wrap items-center gap-3 text-xs text-fg-muted">
                    {selected.author?.name ? (
                      <span>{t("By {author}", { author: selected.author.name })}</span>
                    ) : null}
                    {selected.repo ? (
                      <a
                        href={selected.repo}
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

            {selected.limits && selected.limits.length > 0 ? (
              <div className="rounded-xl border border-border bg-surface p-4">
                <h4 className="mb-2 text-xs font-semibold uppercase tracking-wider text-fg-muted">
                  {t("Known limits")}
                </h4>
                <ul className="list-disc space-y-1 pl-4 text-xs text-fg-muted">
                  {selected.limits.map((lim) => (
                    <li key={lim}>{t(lim)}</li>
                  ))}
                </ul>
              </div>
            ) : null}

            {preview ? (
              <>
              <RuntimeLimitsCard limits={preview.runtimeLimits} requested={preview.manifest.ether?.runtime?.limits} />
              <PermissionSheet
                permissions={preview.permissions}
                verified={preview.verified}
                needsViewers={preview.needsViewers}
                needsFolders={preview.needsFolders}
                viewers={viewers}
                onChangeViewers={setViewers}
                folders={folders}
                onChangeFolders={setFolders}
              />
              </>
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
