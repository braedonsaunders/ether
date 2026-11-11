import {
  Button,
  SearchSelect,
  SettingsSection,
  Spinner,
  Switch,
  toast,
} from "@braedonsaunders/appkit-ui";
import { AlertTriangle, CheckCircle2, RefreshCw, Wand2 } from "lucide-react";
import { useCallback, useEffect, useMemo, useState } from "react";
import { Link, useNavigate } from "react-router-dom";
import { useApp } from "../../app-context";
import { MoreOptions } from "../../components/ConfigSection";
import { ErrorPanel } from "../../components/feedback";
import { Segmented } from "../../components/fields";
import { SaveBar, useDraft } from "../../components/SaveBar";
import { SettingsRow } from "../../components/SettingsRow";
import { LevelBadge } from "../../components/status";
import { ToolbarPicker } from "../../components/ToolbarPicker";
import { t, tn } from "../../i18n";
import { api, ApiError, apiFetch, errorMessage } from "../../lib/api";
import { formatDateTime, relativeTime } from "../../lib/format";
import { useAsync } from "../../lib/hooks";
import type { ReleaseMediaType } from "../../lib/release-mode";
import { isRuleComplete } from "../../lib/rules";
import { CATALOG } from "../settings/schema";
import { asObj, SchemaField, type FieldGroup } from "../settings/SchemaForm";
import { useQuality } from "../releases/useQuality";
import { librariesApi, type Library, type Presentations } from "./api";
import { CatalogPickers, isCatalogPicker } from "./browser/CatalogPickers";
import { CatalogActivity, UpcomingEpisodeSettings } from "./browser/HomeReleaseSettings";
import { NewReleasesSettings, useAvailabilityDraft } from "./browser/NewReleasesSettings";
import { TierMigrationStatus } from "./browser/TierMigrationStatus";
import { TrendingStatus } from "./browser/TrendingStatus";
import { LibraryCacheCard } from "./LibraryCache";
import { SetupWizard } from "./SetupWizard";
import "./browser/catalog.css";

type Obj = Record<string, unknown>;

const PLEX_BASES = ["movie", "show", "artist", "photo"];
const MB_TYPES = ["movies", "tvshows", "music", "audiobooks", "books", "photos", "homevideos"];
const STREMIO_TYPES = ["movie", "series", "channel", "tv"];
const XTREAM_TYPES = ["movie", "series", "live"];

function defaultPlexBase(lib: Library): string {
  if (lib.source === "plugin") return (lib.kind?.levels.length ?? 1) >= 3 ? "show" : "movie";
  return lib.mediaType === "show" || lib.filter?.type === "show" ? "show" : "movie";
}
function defaultMBType(lib: Library): string {
  const declared =
    lib.present?.mediaBrowser?.collectionType ?? lib.kind?.present?.mediaBrowser?.collectionType;
  if (declared) return declared;
  return defaultPlexBase(lib) === "show" ? "tvshows" : "movies";
}

function briefGroup(g: FieldGroup): FieldGroup {
  return {
    ...g,
    description: undefined,
    fields: g.fields.map((f) =>
      "help" in f && typeof f.help === "string" && f.help.length > 90
        ? { ...f, help: undefined }
        : f,
    ),
  };
}

/** Off / Some / Most for catalog.minPopularity + catalog.minVotes. Some is the default. */
const OBSCURE_PRESETS = {
  off: { minPopularity: 0, minVotes: 0 },
  some: { minPopularity: 5, minVotes: 100 },
  most: { minPopularity: 15, minVotes: 500 },
} as const;
type ObscurePreset = keyof typeof OBSCURE_PRESETS;

function obscurePreset(draft: Obj): ObscurePreset | "custom" {
  const pop = Number(draft.minPopularity ?? 0);
  const votes = Number(draft.minVotes ?? 0);
  for (const [key, v] of Object.entries(OBSCURE_PRESETS))
    if (v.minPopularity === pop && v.minVotes === votes) return key as ObscurePreset;
  return "custom";
}

function ObscureTitles({ draft, onChange }: { draft: Obj; onChange: (next: Obj) => void }) {
  const preset = obscurePreset(draft);
  return (
    <SettingsRow
      setting="hide-obscure-titles"
      title={t("Hide obscure titles")}
      description={
        preset === "custom"
          ? t("Custom limits are set under More options.")
          : preset === "off"
            ? t(
                "Browse lists every title with a real audience. Everything else is still found by search.",
              )
            : t("Hidden titles are still found by search.")
      }
      control={
        <Segmented
          ariaLabel={t("Hide obscure titles")}
          value={preset === "custom" ? "" : preset}
          onChange={(v) => onChange({ ...draft, ...OBSCURE_PRESETS[v as ObscurePreset] })}
          options={[
            { value: "off", label: t("Off") },
            { value: "some", label: t("Some") },
            { value: "most", label: t("Most") },
          ]}
        />
      }
    />
  );
}

/** "Show in apps", per protocol (spec §6.2, §15.3). Builtins are synthesised. */
function PresentationsEditor({ library, onSaved }: { library: Library; onSaved: () => void }) {
  const [present, setPresent] = useState<Presentations>(library.present ?? {});
  const [busy, setBusy] = useState(false);
  useEffect(() => setPresent(library.present ?? {}), [library.present, library.id]);
  const dirty = JSON.stringify(present) !== JSON.stringify(library.present ?? {});

  const toggle = (key: keyof Presentations, on: boolean, make: () => unknown) =>
    setPresent((p) => {
      const next: Presentations = { ...p };
      if (on) (next as Record<string, unknown>)[key] = make();
      else delete (next as Record<string, unknown>)[key];
      return next;
    });

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
              options={options.map((o) => ({ value: o, label: o }))}
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
      <div className="pt-2">
        <Button
          size="sm"
          disabled={busy || !dirty}
          onClick={() => {
            setBusy(true);
            librariesApi
              .patch(library.id, { present })
              .then(() => {
                toast.success(t("Saved"));
                onSaved();
              })
              .catch((e) => toast.error(errorMessage(e)))
              .finally(() => setBusy(false));
          }}
        >
          {t("Save")}
        </Button>
      </div>
    </SettingsSection>
  );
}

/**
 * "Quality profile" (spec #419): Default, or a profile from Releases ›
 * Profiles filtered to this library's media type. Titles in this library
 * rank with it wherever they appear.
 */
function QualityProfileRow({ library, onSaved }: { library: Library; onSaved: () => void }) {
  const { quality } = useQuality();
  const [busy, setBusy] = useState(false);
  const media = library.filter?.type ?? (library.mediaType === "show" ? "show" : "movie");
  const profileMedia = media === "show" ? "tv" : "movie";
  const names = (quality?.profiles ?? [])
    .filter((p) => !p.mediaType || p.mediaType === profileMedia)
    .map((p) => p.name);
  const value = library.qualityProfile ?? "";
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
            onChange={(v) => {
              if (busy || v === current) return;
              setBusy(true);
              librariesApi
                .patch(library.id, { qualityProfile: v })
                .then(() => {
                  toast.success(t("Saved"));
                  onSaved();
                })
                .catch((e) => toast.error(errorMessage(e)))
                .finally(() => setBusy(false));
            }}
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

function FilterSummary({ library }: { library: Library }) {
  const f = library.filter ?? {};
  const parts: string[] = [];
  parts.push(f.type === "show" ? t("TV shows") : t("Movies"));
  if (Array.isArray(f.genres) && f.genres.length) parts.push((f.genres as string[]).join(", "));
  if (f.yearFrom || f.yearTo)
    parts.push(`${Number(f.yearFrom ?? "") || "…"}–${Number(f.yearTo ?? "") || "…"}`);
  if (typeof f.query === "string" && f.query) parts.push(`“${f.query}”`);
  if (f.group) parts.push(t("plus your filter choices"));
  return (
    <SettingsSection title={t("What is in it")}>
      <p className="px-5 pt-4 text-sm">{parts.join(" · ")}</p>
      <p className="px-5 pt-1 pb-4 text-sm text-fg-muted">
        {t(
          "Titles stay in Movies and TV Shows too — this library is another way to reach them. The filter itself cannot be changed yet; delete and recreate the library to change it.",
        )}
      </p>
    </SettingsSection>
  );
}

function ReleaseSection({ mediaType }: { mediaType: ReleaseMediaType }) {
  const availability = useAvailabilityDraft();
  if (!availability.draft) return null;
  return (
    <>
      <NewReleasesSettings
        mediaType={mediaType}
        availability={availability.draft}
        onAvailability={availability.setDraft}
      />
      {availability.saveBar}
    </>
  );
}

/** Shared TMDB catalog thresholds, language/region, and sync health on every TMDB library. */
function TmdbSourceSection({ library }: { library: Library }) {
  const { health, dashboard, refreshDashboard } = useApp();
  const catalogStatus = asObj(dashboard?.sections.catalog);
  const titleCount =
    Number(catalogStatus.browsableMovies ?? catalogStatus.movies ?? 0) +
    Number(catalogStatus.browsableShows ?? catalogStatus.shows ?? 0);
  const catalogHealth = health.find((h) => h.id === "catalog");
  const config = useAsync(() => api.getConfig(), []);
  const stored = config.data ? asObj(config.data.catalog) : null;
  const { draft, setDraft, dirty, reset } = useDraft<Obj>(stored);
  const [error, setError] = useState<unknown>(null);
  const [saving, setSaving] = useState(false);

  const rules = useMemo(() => library.rules ?? [], [library.rules]);
  const fallback = library.visibilityDefault === "hide" ? "hide" : "show";
  const rulesReady = rules.every((g) => g.rules.length > 0 && g.rules.every(isRuleComplete));
  const visibility = useAsync(
    async (signal) => {
      if (!rulesReady) return null;
      return apiFetch<{ total: number; visible: number; hidden: number }>(
        "/catalog/visibility/preview",
        {
          method: "POST",
          body: JSON.stringify({
            libraryId: library.id,
            exclusions: rules,
            visibilityDefault: fallback,
          }),
          signal,
        },
      );
    },
    [library.id, rules, fallback, rulesReady],
  );

  const save = useCallback(async () => {
    if (!draft) return;
    setSaving(true);
    setError(null);
    try {
      await api.putSection("catalog", draft);
      toast.success(t("Settings saved"));
      await config.reload();
      void refreshDashboard();
    } catch (e) {
      setError(e);
      if (!(e instanceof ApiError)) toast.error(errorMessage(e));
    } finally {
      setSaving(false);
    }
  }, [config, draft, refreshDashboard]);

  const groups = [...CATALOG, UpcomingEpisodeSettings].map(briefGroup);

  return (
    <section className="space-y-4 pt-3" aria-label={t("Source: TMDB")}>
      <div>
        <h2 className="text-base font-semibold text-fg">{t("Source: TMDB")}</h2>
        <p className="text-sm text-fg-muted">
          {t("These settings apply to every library that uses TMDB.")}
        </p>
      </div>
      {config.error ? (
        <ErrorPanel error={config.error} onRetry={() => void config.reload()} />
      ) : null}
      {!draft ? (
        <div className="py-10 text-center">
          <Spinner size={20} label={t("Loading settings")} />
        </div>
      ) : (
        <div className="grid gap-5 lg:grid-cols-[minmax(0,1fr)_300px]">
          <div className="space-y-5">
            {groups.map((g, i) => (
              <SettingsSection key={g.id ?? i} title={g.title}>
                {g.id === "browsable" ? <ObscureTitles draft={draft} onChange={setDraft} /> : null}
                {g.fields.map((f, j) =>
                  f.kind === "group" || isCatalogPicker(f.key) ? null : (
                    <SchemaField
                      key={`${f.key}-${j}`}
                      def={f}
                      value={draft[f.key]}
                      path={`catalog.${f.key}`}
                      errors={{}}
                      onChange={(v) => setDraft({ ...draft, [f.key]: v })}
                    />
                  ),
                )}
                {g.fields.some((f) => isCatalogPicker(f.key)) ? (
                  <CatalogPickers
                    language={draft.language}
                    region={draft.region}
                    refreshInterval={draft.refreshInterval}
                    onChange={(key, value) => setDraft({ ...draft, [key]: value })}
                  />
                ) : null}
                {g.more?.length ? (
                  <MoreOptions
                    setting={g.id}
                    label={t("More options: {title}", { title: g.title ?? "" })}
                  >
                    {g.more.map((f, j) => (
                      <SchemaField
                        key={`more-${f.key}-${j}`}
                        def={f}
                        value={draft[f.key]}
                        path={`catalog.${f.key}`}
                        errors={{}}
                        onChange={(v) => setDraft({ ...draft, [f.key]: v })}
                      />
                    ))}
                  </MoreOptions>
                ) : null}
              </SettingsSection>
            ))}
          </div>
          <aside className="space-y-5">
            {catalogHealth ? (
              <section
                className="rounded-xl border border-border bg-surface p-5"
                aria-label={t("Your catalog")}
              >
                <div className="mb-4 flex items-center gap-3">
                  {catalogHealth.level === "ok" ? (
                    <CheckCircle2 size={36} className="text-success" />
                  ) : (
                    <AlertTriangle size={36} className="text-warning" />
                  )}
                  <h3 className="text-lg font-semibold">{t("Your catalog")}</h3>
                </div>
                <p className="text-xs text-fg-muted">{t("Catalog size before rules")}</p>
                <p className="mt-1 text-3xl font-semibold tabular-nums">
                  {tn(visibility.data?.total ?? titleCount, "{count} title", "{count} titles")}
                </p>
                <p className="mt-2 text-sm">
                  {visibility.data
                    ? tn(
                        visibility.data.visible ?? visibility.data.total - visibility.data.hidden,
                        "{count} shown after your rules",
                        "{count} shown after your rules",
                      )
                    : t("Checking your rules…")}
                </p>
                <p className="mt-2 text-sm text-fg-muted">
                  {catalogStatus.lastSync
                    ? t("Synced {when}", { when: relativeTime(String(catalogStatus.lastSync)) })
                    : t("Not synced yet")}
                </p>
                <div className="mt-4">
                  <LevelBadge level={catalogHealth.level} />
                </div>
                {catalogHealth.level === "warn" ? (
                  <p className="mt-3 break-words text-sm text-warning">{catalogHealth.summary}</p>
                ) : null}
              </section>
            ) : null}
            <TierMigrationStatus />
            <CatalogActivity />
            <TrendingStatus />
          </aside>
        </div>
      )}
      <SaveBar
        dirty={dirty}
        saving={saving}
        error={error}
        onSave={() => void save()}
        onDiscard={() => {
          reset();
          setError(null);
        }}
      />
    </section>
  );
}

/** Per-library settings: only the sections that apply to this library (§11). */
export function LibrarySettings({ library, onSaved }: { library: Library; onSaved: () => void }) {
  const navigate = useNavigate();
  const [name, setName] = useState(library.name);
  const [busy, setBusy] = useState(false);
  const [syncing, setSyncing] = useState(false);
  const [wizard, setWizard] = useState(false);
  const setup = useAsync(
    (signal) =>
      library.source === "plugin"
        ? librariesApi.librarySetup(library.id, signal)
        : Promise.resolve(null),
    [library.id, library.source],
  );
  useEffect(() => setName(library.name), [library.name]);

  const patch = async (body: Record<string, unknown>, done: string) => {
    setBusy(true);
    try {
      await librariesApi.patch(library.id, body);
      toast.success(done);
      onSaved();
    } catch (e) {
      toast.error(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  const syncNow = async (fullWalk: boolean) => {
    setSyncing(true);
    try {
      const res = await librariesApi.sync(library.id, fullWalk);
      toast.success(t("Sync finished: {count} updated", { count: res.upserted }));
      onSaved();
    } catch (e) {
      toast.error(errorMessage(e));
    } finally {
      setSyncing(false);
    }
  };

  const blockers = setup.data?.blockers ?? [];
  const canonicalMediaType: ReleaseMediaType =
    defaultPlexBase(library) === "show" ? "show" : "movie";

  return (
    <div className="flex flex-col gap-4">
      {library.source === "plugin" && blockers.length ? (
        <p role="alert" className="library-setup-banner">
          {t("This library is waiting for setup. Finish it and the library can appear in apps.")}{" "}
          <Button size="sm" onClick={() => setWizard(true)}>
            <Wand2 size={14} /> {t("Finish setup")}
          </Button>
        </p>
      ) : null}
      <SettingsSection title={t("Basics")}>
        <SettingsRow
          title={t("Name")}
          description={t("The name viewers see in their apps.")}
          control={
            <span className="library-present-row">
              <input
                value={name}
                onChange={(e) => setName(e.target.value)}
                aria-label={t("Library name")}
              />
              <Button
                size="sm"
                disabled={busy || !name.trim() || name.trim() === library.name}
                onClick={() => void patch({ name: name.trim() }, t("Library renamed"))}
              >
                {t("Save")}
              </Button>
            </span>
          }
        />
        <SettingsRow
          title={t("Hide from apps")}
          description={t("The library stays in this console but disappears from every app.")}
          control={
            <Switch
              aria-label={t("Hide from apps")}
              checked={library.hidden}
              disabled={busy}
              onChange={(e) => void patch({ hidden: e.target.checked }, t("Saved"))}
            />
          }
        />
        <SettingsRow
          title={t("Sharing")}
          description={t("Choose who can open each library on the Viewers page.")}
          control={
            <Link to="/viewers" className="text-sm text-primary underline">
              {t("Open Viewers")}
            </Link>
          }
        />
        <QualityProfileRow library={library} onSaved={onSaved} />
      </SettingsSection>

      {library.source === "builtin" ? (
        <SettingsSection title={t("Show in apps")}>
          <div className="px-5 py-4 text-sm">
            <p className="text-fg-muted">
              {t(
                "Movies and TV Shows appear in every app. Use “Hide from apps” above to hide one.",
              )}
            </p>
            <p className="pt-2">
              <Link to="/metadata" className="text-primary underline">
                {t("Metadata settings")}
              </Link>
            </p>
          </div>
        </SettingsSection>
      ) : (
        <PresentationsEditor library={library} onSaved={onSaved} />
      )}

      {library.source === "filter" ? <FilterSummary library={library} /> : null}

      {library.source !== "plugin" ? (
        <>
          <ReleaseSection mediaType={canonicalMediaType} />
          <TmdbSourceSection library={library} />
        </>
      ) : null}

      {library.source === "plugin" ? (
        <>
          <SettingsSection title={t("Keeping it up to date")}>
            <SettingsRow
              title={t("Last sync")}
              description={
                library.sync?.lastError
                  ? library.sync.lastError
                  : library.sync?.lastOk
                    ? formatDateTime(library.sync.lastOk)
                    : t("Not synced yet")
              }
              control={
                <span className="library-present-row">
                  <Button
                    size="sm"
                    variant="outline"
                    disabled={syncing}
                    onClick={() => void syncNow(true)}
                  >
                    {t("Full re-sync")}
                  </Button>
                  <Button size="sm" disabled={syncing} onClick={() => void syncNow(false)}>
                    <RefreshCw size={14} className={syncing ? "animate-spin" : ""} />{" "}
                    {t("Sync now")}
                  </Button>
                </span>
              }
            />
            <SettingsRow
              title={t("Setup")}
              description={
                blockers.length
                  ? t("Some steps still need finishing.")
                  : t("Re-run the guided setup or check a step any time.")
              }
              control={
                <Button size="sm" variant="outline" onClick={() => setWizard(true)}>
                  <Wand2 size={14} /> {t("Open setup")}
                </Button>
              }
            />
          </SettingsSection>
          <LibraryCacheCard library={library} />
        </>
      ) : null}

      {library.source === "filter" ? (
        <SettingsSection title={t("Remove")}>
          <SettingsRow
            title={t("Delete this library")}
            description={t("Its titles stay in Movies and TV Shows.")}
            control={
              <Button
                size="sm"
                variant="ghost"
                disabled={busy}
                onClick={() => {
                  if (
                    window.confirm(
                      t("Delete the library “{name}”? Its titles stay in Movies and TV Shows.", {
                        name: library.name,
                      }),
                    )
                  )
                    librariesApi
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

      {wizard && library.pluginId ? (
        <SetupWizard
          pluginId={library.pluginId}
          onClose={() => {
            setWizard(false);
            void setup.reload();
            onSaved();
          }}
        />
      ) : null}
    </div>
  );
}
