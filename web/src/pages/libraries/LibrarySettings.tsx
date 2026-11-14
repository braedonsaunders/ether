import { sourceLabel } from "./CatalogueSources";
import {
  Button,
  Input,
  SearchSelect,
  SettingsSection,
  Switch,
  buttonVariants,
  toast,
} from "@braedonsaunders/appkit-ui";
import { Film, Plug, Tv } from "lucide-react";
import { useMemo, useState } from "react";
import { Link, useNavigate } from "react-router-dom";
import { ToolbarPicker } from "../../components/ToolbarPicker";
import { useApp } from "../../app-context";
import { SettingsRow } from "../../components/SettingsRow";
import { SaveBar, useDraft } from "../../components/SaveBar";
import { LevelBadge } from "../../components/status";
import { ErrorPanel } from "../../components/feedback";
import { LoadingRows } from "../../components/feedback";
import { t, tn } from "../../i18n";
import { errorMessage } from "../../lib/api";
import { useAsync } from "../../lib/hooks";
import { useQuality } from "../releases/useQuality";
import { librariesApi, publishedTo, type Library, type Presentations } from "./api";
import { LibraryCacheCard } from "./LibraryCache";
import { TitlesComeFrom } from "./TitlesComeFrom";
import { NarrowingEditor } from "./NarrowingEditor";
function presentationLabel(value: string): string {
  const labels: Record<string, string> = {
    movie: t("Movies"),
    show: t("TV shows"),
    series: t("TV shows"),
    movies: t("Movies"),
    tvshows: t("TV shows"),
    artist: t("Music"),
    music: t("Music"),
    photo: t("Photos"),
    photos: t("Photos"),
    audiobooks: t("Audiobooks"),
    books: t("Books"),
    homevideos: t("Home videos"),
    channel: t("Channels"),
    live: t("Live TV"),
    tv: t("Live TV"),
  };
  return labels[value] ?? value;
}
const PLEX_BASES = ["movie", "show", "artist", "photo"];
const MB_TYPES = ["movies", "tvshows", "music", "audiobooks", "books", "photos", "homevideos"];
const STREMIO_TYPES = ["movie", "series", "channel", "tv"];
const XTREAM_TYPES = ["movie", "series", "live"];

function defaultPlexBase(lib: Library): string {
  if (lib.source === "plugin") return (lib.kind?.levels.length ?? 1) >= 3 ? "show" : "movie";
  return lib.mediaType === "show" ? "show" : "movie";
}
function defaultMBType(lib: Library): string {
  const declared =
    lib.present?.mediaBrowser?.collectionType ?? lib.kind?.present?.mediaBrowser?.collectionType;
  if (declared) return declared;
  return defaultPlexBase(lib) === "show" ? "tvshows" : "movies";
}

/** "Show in apps", per protocol (spec §6.2, §15.3). Builtins are synthesised. */
function PresentationsEditor({
  library,
  present,
  onChange,
}: {
  library: Library;
  present: Presentations;
  onChange: (value: Presentations) => void;
}) {
  const toggle = (key: keyof Presentations, on: boolean, make: () => unknown) =>
    onChange(
      (() => {
        const p = present;
        const next: Presentations = { ...p };
        if (on) (next as Record<string, unknown>)[key] = make();
        else delete (next as Record<string, unknown>)[key];
        return next;
      })(),
    );

  const row = (
    key: "plex" | "mediaBrowser" | "stremio" | "xtream" | "silo",
    title: string,
    value: string,
    options: string[],
    make: (v: string) => unknown,
    current: string,
  ) => (
    <SettingsRow
      title={title}
      control={
        <span className="library-present-row">
          <Switch
            aria-label={t("Show in {app}", { app: title })}
            checked={Boolean(present[key])}
            onChange={(e) => toggle(key, e.target.checked, () => make(current))}
          />
          {present[key] ? (
            <ToolbarPicker
              ariaLabel={t("How {app} shows it", { app: title })}
              value={value}
              options={options.map((o) => ({ value: o, label: presentationLabel(o) }))}
              onChange={(v) => toggle(key, true, () => make(v))}
            />
          ) : null}
        </span>
      }
    />
  );

  return (
    <SettingsSection title={t("Show in apps")}>
      <p className="px-5 pt-4 pb-2 text-sm text-fg-muted">
        {t("Choose where this library appears. Anything off stays just in this console.")}
      </p>
      {row(
        "plex",
        "Plex",
        present.plex?.base ?? defaultPlexBase(library),
        PLEX_BASES,
        (v) => ({ base: v }),
        defaultPlexBase(library),
      )}
      {row(
        "mediaBrowser",
        t("Jellyfin & Emby"),
        present.mediaBrowser?.collectionType ?? defaultMBType(library),
        MB_TYPES,
        (v) => ({ collectionType: v }),
        defaultMBType(library),
      )}
      {row(
        "stremio",
        "Stremio",
        present.stremio?.type ?? (defaultPlexBase(library) === "show" ? "series" : "movie"),
        STREMIO_TYPES,
        (v) => ({ type: v }),
        defaultPlexBase(library) === "show" ? "series" : "movie",
      )}
      {row(
        "xtream",
        "Xtream",
        present.xtream?.category ?? (defaultPlexBase(library) === "show" ? "series" : "movie"),
        XTREAM_TYPES,
        (v) => ({ category: v }),
        defaultPlexBase(library) === "show" ? "series" : "movie",
      )}
      {row(
        "silo",
        "Silo",
        present.silo?.type ?? defaultMBType(library),
        MB_TYPES,
        (v) => ({ type: v }),
        defaultMBType(library),
      )}
      {library.source === "plugin" ? (
        <SettingsRow
          title={t("Only in its own app")}
          description={t(
            "The plugin serves this library itself instead of through the apps above.",
          )}
          control={
            <Switch
              aria-label={t("Only in its own app")}
              checked={Boolean(present.adapterOnly)}
              onChange={(e) => toggle("adapterOnly", e.target.checked, () => true)}
            />
          }
        />
      ) : null}
    </SettingsSection>
  );
}

/**
 * "Quality profile" (spec #419): Default, or a profile from Releases ›
 * Profiles filtered to this library's media type. Titles in this library
 * rank with it wherever they appear.
 */
function QualityProfileRow({
  library,
  value,
  onChange,
}: {
  library: Library;
  value: string;
  onChange: (value: string) => void;
}) {
  const { quality } = useQuality();
  const media = library.mediaType === "show" ? "show" : "movie";
  const profileMedia = media === "show" ? "tv" : "movie";
  const names = (quality?.profiles ?? [])
    .filter((p) => !p.mediaType || p.mediaType === profileMedia)
    .map((p) => p.name);
  const current = value && names.includes(value) ? value : "";

  return (
    <SettingsRow
      title={t("Quality profile")}
      description={t(
        "Titles in this library rank with this profile. Rules and the Away profile still take precedence.",
      )}
      control={
        <div className="w-56">
          <SearchSelect
            value={current}
            onChange={onChange}
            options={[
              { value: "", label: t("Default") },
              ...names.map((name) => ({ value: name, label: name })),
            ]}
            ariaLabel={t("Quality profile")}
            searchable={false}
          />
        </div>
      }
    />
  );
}

export function LibrarySettings({ library, onSaved }: { library: Library; onSaved: () => void }) {
  const navigate = useNavigate();
  const { health } = useApp();
  const collectionSources = useAsync(
    (signal) =>
      library.source === "collection"
        ? librariesApi.listCollectionSources(signal)
        : Promise.resolve([]),
    [library.source],
  );
  const registry = useAsync((signal) => librariesApi.catalogueSources(signal), []);
  const cache = useAsync(
    (signal) =>
      library.source === "plugin" ? librariesApi.cache(library.id, signal) : Promise.resolve(null),
    [library.id, library.source],
  );
  const stored = useMemo(
    () => ({
      name: library.name,
      hidden: library.hidden,
      present: library.present ?? {},
      qualityProfile: library.qualityProfile ?? "",
      sources: library.sources ?? [],
      collectionId: library.collectionId,
      cacheMaxBytes: cache.data?.maxBytes ?? 0,
      cacheTTL: cache.data?.ttlSeconds ?? 0,
    }),
    [library, cache.data],
  );
  const { draft, setDraft, dirty, reset } = useDraft(stored);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<unknown>(null);
  const [editing, setEditing] = useState(false);
  if (library.source === "plugin" && cache.loading && !cache.data) return <LoadingRows />;
  if (!draft) return <LoadingRows />;
  const sourceNames =
    registry.data?.filter((source) => draft.sources.includes(source.id)).map(sourceLabel) ?? [];
  const save = async () => {
    setSaving(true);
    setError(null);
    try {
      const body: Record<string, unknown> = {
        name: draft.name.trim(),
        hidden: draft.hidden,
      };
      if (library.source !== "plugin" && draft.qualityProfile !== stored.qualityProfile)
        body.qualityProfile = draft.qualityProfile;
      if (library.source !== "builtin") body.present = draft.present;
      if (JSON.stringify(draft.sources) !== JSON.stringify(stored.sources))
        body.sources = draft.sources;
      if (draft.collectionId !== stored.collectionId) body.collectionId = draft.collectionId;
      await librariesApi.patch(library.id, body);
      if (
        cache.data &&
        (draft.cacheMaxBytes !== stored.cacheMaxBytes || draft.cacheTTL !== stored.cacheTTL)
      )
        await librariesApi.setCache(library.id, draft.cacheMaxBytes, draft.cacheTTL);
      onSaved();
      void cache.reload();
      toast.success(t("Settings saved"));
    } catch (e) {
      setError(e);
    } finally {
      setSaving(false);
    }
  };
  const summaryLibrary = { ...library, hidden: draft.hidden, present: draft.present };
  return (
    <>
      <div className="grid items-start gap-5 xl:grid-cols-[minmax(0,1fr)_300px]">
        <div className="min-w-0 space-y-5">
          <TitlesComeFrom
            library={library}
            sources={draft.sources}
            onChange={(sources) => setDraft({ ...draft, sources })}
          >
            {library.source === "collection" ? (
              <SettingsRow
                title={t("Collection")}
                description={
                  collectionSources.data?.find((c) => c.id === draft.collectionId)?.name ??
                  t("Collection")
                }
                control={
                  <Button size="sm" variant="outline" onClick={() => setEditing(true)}>
                    {t("Change")}
                  </Button>
                }
              />
            ) : null}
          </TitlesComeFrom>
          <SettingsSection title={t("Basics")}>
            <SettingsRow
              title={t("Name")}
              description={t("The name viewers see in their apps.")}
              control={
                <Input
                  aria-label={t("Library name")}
                  value={draft.name}
                  onChange={(e) => setDraft({ ...draft, name: e.target.value })}
                />
              }
            />
            <SettingsRow
              title={t("Hide from apps")}
              description={t("The library stays in this console but disappears from every app.")}
              control={
                <Switch
                  aria-label={t("Hide from apps")}
                  checked={draft.hidden}
                  onChange={(e) => setDraft({ ...draft, hidden: e.target.checked })}
                />
              }
            />
            <SettingsRow
              title={t("Sharing")}
              description={t("Choose who can open each library on the Viewers page.")}
              control={
                <Link className={buttonVariants({ variant: "outline", size: "sm" })} to="/viewers">
                  {t("Open Viewers")}
                </Link>
              }
            />
          </SettingsSection>
          {library.source === "builtin" ? (
            <SettingsSection title={t("Show in apps")}>
              <SettingsRow
                title={t("Show in apps")}
                description={t(
                  "Movies and TV Shows appear in every app. Use “Hide from apps” above to hide one.",
                )}
                control={
                  <Link className={buttonVariants({ variant: "outline", size: "sm" })} to="/apps">
                    {t("Apps")}
                  </Link>
                }
              />
            </SettingsSection>
          ) : (
            <PresentationsEditor
              library={library}
              present={draft.present}
              onChange={(present) => setDraft({ ...draft, present })}
            />
          )}
          {library.source !== "plugin" && (
            <SettingsSection title={t("Quality")}>
              <QualityProfileRow
                library={library}
                value={draft.qualityProfile}
                onChange={(qualityProfile) => setDraft({ ...draft, qualityProfile })}
              />
            </SettingsSection>
          )}
          {cache.data ? (
            <LibraryCacheCard
              library={library}
              data={cache.data}
              maxBytes={draft.cacheMaxBytes}
              ttlSeconds={draft.cacheTTL}
              onChange={(cacheMaxBytes, cacheTTL) =>
                setDraft({ ...draft, cacheMaxBytes, cacheTTL })
              }
              onReload={() => void cache.reload()}
            />
          ) : null}
          {cache.error ? (
            <ErrorPanel error={cache.error} onRetry={() => void cache.reload()} />
          ) : null}
          {library.source === "catalogue" || library.source === "collection" ? (
            <SettingsSection title={t("Remove")}>
              <SettingsRow
                title={t("Delete this library")}
                description={t("Its titles stay in Movies and TV Shows.")}
                control={
                  <Button
                    size="sm"
                    variant="ghost"
                    disabled={saving}
                    onClick={() => {
                      if (
                        window.confirm(
                          t(
                            "Delete the library “{name}”? Its titles stay in Movies and TV Shows.",
                            { name: library.name },
                          ),
                        )
                      )
                        void librariesApi
                          .remove(library.id)
                          .then(() => navigate("/libraries"))
                          .catch((e) => toast.error(errorMessage(e)));
                    }}
                  >
                    {t("Delete")}
                  </Button>
                }
              />
            </SettingsSection>
          ) : null}
        </div>
        <aside className="rounded-xl border border-border bg-surface p-5 xl:sticky xl:top-5">
          <div className="flex items-center gap-3">
            {library.source === "plugin" ? (
              <Plug size={24} />
            ) : defaultPlexBase(library) === "show" ? (
              <Tv size={24} />
            ) : (
              <Film size={24} />
            )}
            <h2 className="font-semibold">{draft.name}</h2>
          </div>
          {library.itemCount !== undefined ? (
            <p className="mt-4 text-2xl font-semibold">
              {tn(library.itemCount, "{count} title", "{count} titles")}
            </p>
          ) : null}
          <div className="mt-3">
            <LevelBadge
              label={
                sourceNames.length > 0 &&
                registry.data
                  ?.filter((s) => draft.sources.includes(s.id))
                  .every((s) => s.health === "unknown")
                  ? t("Not loaded yet")
                  : undefined
              }
              level={
                library.sync?.lastError
                  ? "error"
                  : sourceNames.length > 0 &&
                      registry.data
                        ?.filter((s) => draft.sources.includes(s.id))
                        .every((s) => s.health === "unknown")
                    ? "unknown"
                    : registry.data?.some(
                          (source) =>
                            draft.sources.includes(source.id) &&
                            ["error", "attention"].includes(source.health),
                        )
                      ? "warn"
                      : (health.find((item) => item.id === "catalog")?.level ?? "unknown")
              }
            />
          </div>
          <p className="mt-4 text-sm text-fg-muted">
            {t("Titles from: {sources}", { sources: sourceNames.join(", ") })}
          </p>
          <p className="mt-3 text-sm text-fg-muted">
            {draft.hidden
              ? t("Hidden from apps")
              : publishedTo(summaryLibrary).length
                ? t("In apps: {apps}", {
                    apps: publishedTo(summaryLibrary)
                      .map(
                        (app) =>
                          ({
                            mediaBrowser: t("Jellyfin & Emby"),
                            plex: "Plex",
                            stremio: "Stremio",
                            xtream: "Xtream",
                            silo: "Silo",
                            adapter: t("Its own app"),
                          })[app] ?? app,
                      )
                      .join(", "),
                  })
                : t("Not shown in any app yet")}
          </p>
        </aside>
      </div>
      <SaveBar
        dirty={dirty}
        saving={saving}
        disabled={!draft.name.trim()}
        error={error}
        onSave={() => void save()}
        onDiscard={() => {
          reset();
          setError(null);
        }}
      />
      {editing ? (
        <NarrowingEditor
          library={{ ...library, collectionId: draft.collectionId }}
          onClose={() => setEditing(false)}
          onChange={(collectionId) => {
            setDraft({
              ...draft,
              ...(collectionId ? { collectionId } : {}),
            });
            setEditing(false);
          }}
        />
      ) : null}
    </>
  );
}
