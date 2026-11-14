import { sourceLabel } from "./CatalogueSources";
import { Button, EmptyState, Switch, toast } from "@braedonsaunders/appkit-ui";
import {
  ArrowDown,
  ArrowUp,
  Clapperboard,
  EyeOff,
  Film,
  ListFilter,
  Pencil,
  Plug,
  Plus,
  Trash2,
  Tv,
} from "lucide-react";
import { useState } from "react";
import { Link, Navigate, Route, Routes, useNavigate } from "react-router-dom";
import { useApp } from "../../app-context";
import { Drawer } from "../../components/Drawer";
import { ErrorPanel, LoadingRows } from "../../components/feedback";
import { DomainPage } from "../../components/layout";
import { LevelBadge } from "../../components/status";
import { t, tn } from "../../i18n";
import { errorMessage } from "../../lib/api";
import { formatNumber } from "../../lib/format";
import { useAsync } from "../../lib/hooks";
import { librariesApi, publishedTo, type Library } from "./api";
import { NewLibraryDrawer } from "./LibraryEditor";
import { LibraryPage } from "./LibraryPage";
import "./libraries.css";

export default function Libraries() {
  return (
    <Routes>
      <Route index element={<LibrariesList />} />
      <Route path=":id/:tab?" element={<LibraryPage />} />
      <Route path="*" element={<Navigate to="/libraries" replace />} />
    </Routes>
  );
}

function sourceBadge(lib: Library): string {
  if (lib.source === "builtin") return lib.mediaType === "show" ? t("TV shows") : t("Movies");
  if (lib.source === "catalogue") return t("Catalogue sources");
  if (lib.source === "collection") return t("Collection");
  return lib.pluginName || lib.pluginId || t("Plugin");
}

function SourceIcon({ lib }: { lib: Library }) {
  if (lib.source === "catalogue") return <ListFilter size={18} aria-hidden />;
  if (lib.source === "plugin") return <Plug size={18} aria-hidden />;
  return lib.mediaType === "show" ? <Tv size={18} aria-hidden /> : <Film size={18} aria-hidden />;
}

function protocolLabel(key: string): string {
  switch (key) {
    case "plex":
      return "Plex";
    case "mediaBrowser":
      return t("Jellyfin & Emby");
    case "stremio":
      return "Stremio";
    case "xtream":
      return "Xtream";
    case "silo":
      return "Silo";
    default:
      return t("Its own app");
  }
}

function LibrariesList() {
  const navigate = useNavigate();
  const { health } = useApp();
  const list = useAsync((signal) => librariesApi.list(signal), []);
  const registry = useAsync((signal) => librariesApi.catalogueSources(signal), []);
  const [creating, setCreating] = useState(false);
  const [renaming, setRenaming] = useState<Library | null>(null);
  const [nameDraft, setNameDraft] = useState("");
  const [busy, setBusy] = useState(false);
  const catalogHealth = health.find((h) => h.id === "catalog");
  const libs = list.data ?? [];

  const act = async (run: () => Promise<unknown>, done?: string) => {
    setBusy(true);
    try {
      await run();
      await list.reload();
      if (done) toast.success(done);
    } catch (e) {
      toast.error(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  const move = (lib: Library, dir: -1 | 1) => {
    const other = libs[libs.indexOf(lib) + dir];
    if (!other) return;
    void act(async () => {
      await librariesApi.patch(lib.id, { sort: other.sort });
      await librariesApi.patch(other.id, { sort: lib.sort });
    });
  };

  return (
    <DomainPage
      title={t("Libraries")}
      description={t("Everything your viewers can open, in one place.")}
      base="/libraries"
      actions={
        <Button size="sm" onClick={() => setCreating(true)}>
          <Plus size={14} /> {t("New library")}
        </Button>
      }
    >
      {list.error ? <ErrorPanel error={list.error} onRetry={() => void list.reload()} /> : null}
      {list.loading && !list.data ? <LoadingRows /> : null}
      {!list.loading && libs.length === 0 && !list.error ? (
        <EmptyState
          icon={<Clapperboard size={22} aria-hidden />}
          title={t("No libraries yet")}
          description={t("Your movies and TV shows appear here once the catalog is ready.")}
        />
      ) : (
        <div className="libraries-grid">
          {libs.map((lib, i) => {
            const shown = publishedTo(lib);
            const sources =
              lib.source === "collection"
                ? t("Collection")
                : registry.data
                    ?.filter(
                      (source) =>
                        lib.sources?.includes(source.id) ||
                        source.libraries.some((item) => item.id === lib.id),
                    )
                    .map(sourceLabel)
                    .join(", ");
            const syncBad = lib.sync?.lastError;
            return (
              <section key={lib.id} className="library-card" aria-label={lib.name}>
                <div className="library-card-head">
                  <span className="library-card-icon">
                    <SourceIcon lib={lib} />
                  </span>
                  <div className="min-w-0 flex-1">
                    <Link to={`/libraries/${lib.id}`} className="library-card-name">
                      {lib.name}
                    </Link>
                    <p className="library-card-meta">
                      {sources || sourceBadge(lib)}
                      {sources && lib.source === "catalogue" && Boolean(lib.rules?.length)
                        ? ` · ${t("Rules")}`
                        : ""}
                      {typeof lib.itemCount === "number"
                        ? ` · ${tn(lib.itemCount, "{count} item", "{count} items", {
                            count: formatNumber(lib.itemCount, 0),
                          })}`
                        : ""}
                    </p>
                  </div>
                  {lib.source === "builtin" && catalogHealth ? (
                    <LevelBadge
                      level={catalogHealth.level}
                      label={catalogHealth.level === "ok" ? t("Healthy") : t("Needs attention")}
                    />
                  ) : syncBad ? (
                    <LevelBadge level="error" label={t("Sync failed")} />
                  ) : lib.source === "plugin" && lib.sync?.lastOk ? (
                    <LevelBadge level="ok" label={t("Healthy")} />
                  ) : null}
                </div>
                <p className="text-sm text-fg-muted">
                  {t("Titles from: {sources}", {
                    sources:
                      registry.data
                        ?.filter(
                          (source) =>
                            lib.sources?.includes(source.id) ||
                            source.libraries.some((item) => item.id === lib.id),
                        )
                        .map(sourceLabel)
                        .join(", ") || sourceBadge(lib),
                  })}
                </p>
                <p className="library-card-apps">
                  {lib.hidden ? (
                    <>
                      <EyeOff size={13} aria-hidden /> {t("Hidden from apps")}
                    </>
                  ) : shown.length ? (
                    t("In apps: {apps}", { apps: shown.map(protocolLabel).join(", ") })
                  ) : (
                    t("Not shown in any app yet")
                  )}
                </p>
                {syncBad ||
                (lib.source === "builtin" && catalogHealth && catalogHealth.level !== "ok") ? (
                  <p className="library-card-error" role="alert">
                    {syncBad || catalogHealth?.summary}
                  </p>
                ) : null}
                <div className="library-card-actions">
                  <Button size="sm" onClick={() => navigate(`/libraries/${lib.id}`)}>
                    {t("Open")}
                  </Button>
                  <Button
                    size="sm"
                    variant="ghost"
                    aria-label={t("Rename {name}", { name: lib.name })}
                    onClick={() => {
                      setRenaming(lib);
                      setNameDraft(lib.name);
                    }}
                  >
                    <Pencil size={14} /> {t("Rename")}
                  </Button>
                  <label className="library-card-hide">
                    <Switch
                      aria-label={t("Hide {name} from apps", { name: lib.name })}
                      checked={lib.hidden}
                      disabled={busy}
                      onChange={(e) =>
                        void act(() => librariesApi.patch(lib.id, { hidden: e.target.checked }))
                      }
                    />
                    <span>{t("Hidden")}</span>
                  </label>
                  <span className="flex-1" />
                  <span className="inline-flex shrink-0 items-center">
                    <Button
                      size="icon"
                      variant="ghost"
                      aria-label={t("Move {name} up", { name: lib.name })}
                      disabled={busy || i === 0}
                      onClick={() => move(lib, -1)}
                    >
                      <ArrowUp size={14} />
                    </Button>
                    <Button
                      size="icon"
                      variant="ghost"
                      aria-label={t("Move {name} down", { name: lib.name })}
                      disabled={busy || i === libs.length - 1}
                      onClick={() => move(lib, 1)}
                    >
                      <ArrowDown size={14} />
                    </Button>
                  </span>
                  {lib.source === "catalogue" ? (
                    <Button
                      size="icon"
                      variant="ghost"
                      aria-label={t("Delete {name}", { name: lib.name })}
                      disabled={busy}
                      onClick={() => {
                        if (
                          window.confirm(
                            t(
                              "Delete the library “{name}”? Its titles stay in Movies and TV Shows.",
                              {
                                name: lib.name,
                              },
                            ),
                          )
                        )
                          void act(() => librariesApi.remove(lib.id), t("Library deleted"));
                      }}
                    >
                      <Trash2 size={14} />
                    </Button>
                  ) : null}
                </div>
              </section>
            );
          })}
        </div>
      )}
      {creating ? (
        <NewLibraryDrawer
          onClose={() => setCreating(false)}
          onCreated={(lib) => {
            setCreating(false);
            void list.reload();
            navigate(`/libraries/${lib.id}`);
          }}
          onPluginOpened={(libId) => {
            setCreating(false);
            void list.reload();
            if (libId) navigate(`/libraries/${libId}`);
          }}
        />
      ) : null}
      <Drawer
        open={renaming !== null}
        onClose={() => setRenaming(null)}
        title={t("Rename library")}
        footer={
          <div className="flex justify-end gap-2">
            <Button variant="ghost" onClick={() => setRenaming(null)}>
              {t("Cancel")}
            </Button>
            <Button
              disabled={busy || !nameDraft.trim()}
              onClick={() => {
                if (!renaming) return;
                void act(async () => {
                  await librariesApi.patch(renaming.id, { name: nameDraft.trim() });
                  setRenaming(null);
                }, t("Library renamed"));
              }}
            >
              {t("Save")}
            </Button>
          </div>
        }
      >
        <label className="library-field">
          <span>{t("Library name")}</span>
          <input
            value={nameDraft}
            onChange={(e) => setNameDraft(e.target.value)}
            aria-label={t("Library name")}
          />
        </label>
        <p className="text-sm text-fg-muted">
          {t("This is the name viewers see in their apps. You can change it any time.")}
        </p>
      </Drawer>
    </DomainPage>
  );
}
