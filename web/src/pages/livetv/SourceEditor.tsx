import {
  Badge,
  Switch,
  Button,
  Input,
  SearchSelect,
  confirmDialog,
  toast,
} from "@braedonsaunders/appkit-ui";
import {
  AlertTriangle,
  CalendarRange,
  FileUp,
  KeyRound,
  Link2,
  ListVideo,
  Plus,
  RefreshCw,
  Trash2,
  X,
} from "lucide-react";
import { useCallback, useEffect, useMemo, useRef, useState, type ComponentProps } from "react";
import { Link } from "react-router-dom";
import { Field, SecretInput, Segmented } from "../../components/fields";
import { SubtabNav } from "../../components/layout";
import { localized, t, tn } from "../../i18n";
import { useLocale } from "../../i18n/react";
import { searchSelectLabels } from "../../i18n/appkit-labels";
import { REGION_CODES } from "../libraries/browser/CatalogPickers";
import { browserLanguageList, regionFromLocales, regionFromTimeZone } from "../setup/language";
import { api, errorMessage } from "../../lib/api";
import { isPluginDescriptor, textOf, useRegistry, type Descriptor } from "../../lib/extend";
import type { ParamField } from "../../lib/rules";
import { formatBytes, formatNumber, relativeTime } from "../../lib/format";
import { ChannelGroups, ProviderAccountCard } from "./SourceGroups";
import { sourceImportError } from "./sourceErrors";
import { SourceLimits } from "./SourceLimits";
import {
  dayClock,
  duration,
  emptySource,
  normGroup,
  plexLineupCount,
  plexLineupName,
  tv,
  type AuthType,
  type GroupRules,
  type Source,
  type SourceAuth,
  type SourceDraft,
  type PlexLineup,
  type SourceProgress,
} from "./model";

export type SourceKind = "playlist" | "guide";

/** Mirrors internal/extend.LiveTVSourceMeta. */
interface LiveTVSourceMeta {
  params?: ParamField[];
  editor?: string;
  yieldsChannels?: boolean;
  yieldsGuide?: boolean;
  test?: boolean;
}

type LiveTVDescriptor = Descriptor<LiveTVSourceMeta>;

function kindIcon(d: LiveTVDescriptor | undefined, size = 13) {
  switch (d?.icon) {
    case "calendar-range":
      return <CalendarRange size={size} />;
    case "key-round":
      return <KeyRound size={size} />;
    default:
      return <ListVideo size={size} />;
  }
}

const playlistCap = 64 * 1024 * 1024;
const guideCap = 2 * 1024 * 1024 * 1024;

/** Reject a file the server will not accept. Gzip stays compressed; the server streams it. */
function uploadError(file: File, key: "playlist" | "guide"): string {
  const gzip = /\.gz$/i.test(file.name);
  if (key === "playlist" && file.size > playlistCap)
    return t("This playlist is larger than 64 MB.");
  if (key === "guide" && !gzip && file.size > guideCap) return t("This guide is larger than 2 GB.");
  return "";
}

/** Create or edit an M3U playlist source or a guide-only XMLTV source, in place. */
export function SourceEditor({
  source,
  initialXtream = false,
  kind: initialKind,
  guideOnly = false,
  onGuideKindChange,
  onKindChange,
  onSaved,
  onDeleted,
}: {
  source: Source | null;
  initialXtream?: boolean;
  kind: SourceKind;
  guideOnly?: boolean;
  onGuideKindChange?: (kind: "xmltv" | "plex") => void;
  onKindChange?: (kind: SourceKind) => void;
  onSaved: (s: Source) => Promise<void> | void;
  onDeleted: () => Promise<void> | void;
}) {
  const [draft, setDraft] = useState<SourceDraft>(emptySource());
  const automaticInterval = useRef(0);
  const [kind, setKind] = useState<SourceKind>(initialKind);
  const [busy, setBusy] = useState<"" | "save" | "refresh" | "delete">("");
  const [error, setError] = useState("");
  const [guideKind, setGuideKind] = useState<"xmltv" | "plex">("xmltv");
  const [lineups, setLineups] = useState<PlexLineup[] | null>(null);
  const [looking, setLooking] = useState(false);
  const [tab, setTab] = useState<"connection" | "groups" | "limits">("connection");
  const { locale } = useLocale();
  const browserRegion = useMemo(() => {
    const languageRegion = regionFromLocales(browserLanguageList(), "");
    const zoneRegion = regionFromTimeZone(Intl.DateTimeFormat().resolvedOptions().timeZone);
    return languageRegion && languageRegion !== "US"
      ? languageRegion
      : zoneRegion || languageRegion;
  }, []);
  const [serverRegion, setServerRegion] = useState("");
  // The catalogue's US/en-US defaults do not establish the server's country.
  useEffect(() => {
    void api
      .getConfig()
      .then((cfg) => {
        const code = cfg.catalog.region?.toUpperCase() ?? "";
        const isDefault = code === "US" && cfg.catalog.language === "en-US";
        setServerRegion(REGION_CODES.includes(code) && !isDefault ? code : "");
      })
      .catch(() => {});
  }, []);
  const region = serverRegion || browserRegion;
  const countries = useMemo(() => {
    const names = new Intl.DisplayNames([locale], { type: "region" });
    return REGION_CODES.map((code) => ({ value: code, label: names.of(code) ?? code })).sort(
      (a, b) => a.label.localeCompare(b.label, locale),
    );
  }, [locale]);
  const [progress, setProgress] = useState<SourceProgress | null>(null);
  const [progressRate, setProgressRate] = useState(0);
  const [progressUnavailable, setProgressUnavailable] = useState(false);
  const [savedImport, setSavedImport] = useState<Source | null>(null);
  // First sample of the current import step, for a stable rows-or-bytes rate.
  const rateBase = useRef<{ stage: string; at: number; value: number } | null>(null);
  const [files, setFiles] = useState<{ playlist: File | null; guide: File | null }>({
    playlist: null,
    guide: null,
  });
  // Source kinds come from the descriptor registry; the editor families
  // (playlist, guide, xtream) are named by each kind's meta.editor.
  const registry = useRegistry<LiveTVSourceMeta>("liveTVSource");
  const kindDescriptors: LiveTVDescriptor[] = registry.descriptors;
  const byEditor = (editor: string) => kindDescriptors.find((d) => d.meta?.editor === editor);
  const typeDescriptors = kindDescriptors.filter(
    (d) => (d.meta?.editor === "playlist" || d.meta?.editor === "guide") && d.id !== "plex",
  );
  const pluginKinds = kindDescriptors.filter(
    (d) =>
      isPluginDescriptor(d) &&
      d.meta?.editor !== "playlist" &&
      d.meta?.editor !== "guide" &&
      d.meta?.editor !== "xtream",
  );
  const selectedPlugin = pluginKinds.find(
    (d) => d.id === draft.playlistUrl || (!!draft.kind && d.id === draft.kind),
  );
  // A source whose channels come from a plugin or add-on (installed or not):
  // the plugin owns its links, so the editor shows none.
  const pluginBacked =
    Boolean(selectedPlugin) ||
    Boolean(draft.kind?.startsWith("plugin:")) ||
    draft.playlistUrl.startsWith("plugin:");
  useEffect(() => {
    automaticInterval.current = Math.max(0, source?.refreshHours ?? 0);
    setDraft(
      source
        ? {
            ...emptySource(),
            id: source.id,
            kind: source.kind,
            name: source.kind === "plex" ? plexLineupName(source.name) : source.name,
            lineupId: source.lineupId,
            country: source.country,
            postalCode: source.postalCode,
            playlistUrl: source.playlistUrl ?? "",
            guideUrl: source.guideUrl ?? "",
            refreshHours: source.refreshHours,
            activityRefreshMinutes: source.activityRefreshMinutes,
            useLiveTV: source.useLiveTV,
            useVOD: source.useVOD,
            auth: source.auth ? structuredClone(source.auth) : undefined,
            ...groupRules(source),
            maxConnections: source.maxConnections,
            tuneGapSeconds: source.tuneGapSeconds,
            tunesPerHour: source.tunesPerHour,
          }
        : {
            ...emptySource(),
            ...(initialXtream ? { auth: { type: "xtream" as const, output: "ts" } } : {}),
          },
    );
    setTab("connection");
    setKind(
      source
        ? source.auth?.type === "xtream"
          ? "playlist"
          : source.hasPlaylist === false ||
              (!source.playlistUrl && !source.channelCount && source.hasGuide)
            ? "guide"
            : "playlist"
        : initialKind,
    );
    setGuideKind(source?.kind === "plex" ? "plex" : "xmltv");
    setLineups(null);
    setFiles({ playlist: null, guide: null });
    setError("");
    setProgress(null);
    setProgressUnavailable(false);
    setSavedImport(null);
    setProgressRate(0);
    rateBase.current = null;
  }, [source, initialKind, initialXtream]);

  const upload = (file: File | undefined, key: "playlist" | "guide") => {
    if (!file) return;
    const message = uploadError(file, key);
    if (message) {
      setError(message);
      return;
    }
    setError("");
    setFiles((current) => ({ ...current, [key]: file }));
    setDraft((s) => ({
      ...s,
      [key]: "",
      [key === "playlist" ? "playlistUrl" : "guideUrl"]: "",
    }));
  };

  const auth = draft.auth ?? {};
  const xtream = kind === "playlist" && auth.type === "xtream";
  const setAuth = (patch: SourceAuth) => setDraft((d) => ({ ...d, auth: { ...d.auth, ...patch } }));
  const plex = kind === "guide" && guideKind === "plex";
  const canSave =
    draft.name.trim() !== "" &&
    (xtream
      ? Boolean(auth.server?.trim() && auth.username?.trim() && auth.password)
      : kind === "playlist"
        ? Boolean(files.playlist || draft.playlist || draft.playlistUrl)
        : plex
          ? Boolean(draft.lineupId && (draft.country || region) && draft.postalCode)
          : Boolean(files.guide || draft.guide || draft.guideUrl));

  const save = async () => {
    setBusy("save");
    setError("");
    setProgressUnavailable(false);
    setSavedImport(null);
    setProgress(null);
    setProgressRate(0);
    rateBase.current = null;
    const progressId = `import-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 8)}`;
    let polling = false;
    const poll = window.setInterval(() => {
      if (polling) return;
      polling = true;
      void tv
        .sourceProgress(progressId)
        .then((p) => {
          if (!p) throw new Error("progress unavailable");
          setProgressUnavailable(false);
          const stage = progressStage(p);
          const value = progressValue(p, stage);
          const now = Date.now();
          const base = rateBase.current;
          if (!base || base.stage !== stage) {
            rateBase.current = { stage, at: now, value };
            setProgressRate(0);
          } else if (now - base.at >= 2000) {
            const delta = value - base.value;
            if (delta > 0) setProgressRate(delta / ((now - base.at) / 1000));
          }
          setProgress(p);
        })
        .catch(async () => {
          setProgressUnavailable(true);
          // Progress is local to the importing node. A load-balanced poll may
          // reach another node: saved source state is shared and authoritative.
          if (source) {
            try {
              setSavedImport((await tv.sources()).find((s) => s.id === source.id) ?? null);
            } catch {
              /* The import request still reports its final result. */
            }
          }
        })
        .finally(() => {
          polling = false;
        });
    }, 750);
    try {
      const body: SourceDraft =
        kind === "guide" || xtream ? { ...draft, playlist: "", playlistUrl: "" } : { ...draft };
      body.kind = plex ? "plex" : pluginBacked ? (selectedPlugin?.id ?? draft.kind) : "";
      if (plex) body.country = (draft.country || region).toUpperCase();
      body.auth = pluginBacked ? undefined : cleanAuth(draft.auth, kind);
      const playlistFile = kind === "playlist" && !xtream ? files.playlist : null;
      const guideFile = files.guide;
      if (playlistFile && source?.playlistUrl) body.playlistUrl = source.playlistUrl;
      if (guideFile && source?.guideUrl) body.guideUrl = source.guideUrl;
      const pending =
        !body.id &&
        !body.playlist &&
        !body.playlistUrl &&
        !body.guide &&
        !body.guideUrl &&
        Boolean(playlistFile || guideFile);
      let saved = await tv.saveSource(body, pending, progressId);
      if (playlistFile) saved = await tv.uploadSource(saved.id, "playlist", playlistFile);
      if (guideFile) saved = await tv.uploadSource(saved.id, "guide", guideFile);
      toast.success(source ? t("Source re-imported") : t("Source imported"), {
        description: t("Channel numbers, mappings and access rules are kept."),
      });
      await onSaved(saved);
    } catch (e) {
      setError(sourceImportError(e, Boolean(source)));
    } finally {
      window.clearInterval(poll);
      setProgress(null);
      setProgressRate(0);
      rateBase.current = null;
      setBusy("");
    }
  };

  const refresh = async () => {
    if (!source) return;
    setBusy("refresh");
    setError("");
    try {
      const s = await tv.refreshSource(source.id);
      toast.success(t("Source refreshed"), {
        description: t("Fresh listings are in the guide."),
      });
      await onSaved(s);
    } catch (e) {
      setError(sourceImportError(e, Boolean(source)));
    } finally {
      setBusy("");
    }
  };

  const remove = async () => {
    if (!source) return;
    const ok = await confirmDialog({
      title: t("Delete {name}?", { name: source.name }),
      message:
        kind === "playlist"
          ? t("Its channels and their listings are removed from Live TV.")
          : t("Channels mapped to this guide lose their listings."),
      confirmLabel: t("Delete source"),
      tone: "danger",
    });
    if (!ok) return;
    setBusy("delete");
    try {
      await tv.deleteSource(source.id);
      toast.success(t("Source deleted"));
      await onDeleted();
    } catch (e) {
      setError(errorMessage(e));
      setBusy("");
    }
  };

  const auto = (draft.refreshHours ?? 0) >= 0;
  const groups = kind === "playlist" && !pluginBacked ? source?.groups : undefined;
  const showGroups = Boolean(groups?.length);
  // Channel sources get Connection, Channel groups (once a lineup is in) and Limits.
  const showTabs = kind === "playlist" && !pluginBacked && !plex;
  const onTab = !showTabs || (tab === "groups" && !showGroups) ? "connection" : tab;
  const rulesChanged = showGroups && source ? rulesKey(draft) !== rulesKey(source) : false;
  const setRules = useCallback<ComponentProps<typeof ChannelGroups>["onChange"]>(
    (update) =>
      setDraft((d) => ({
        ...d,
        ...update({ ...d, excludeGroups: d.excludeGroups ?? [], groupOrder: d.groupOrder ?? [] }),
      })),
    [],
  );
  const urlBacked = plex || xtream || Boolean(draft.playlistUrl || draft.guideUrl);

  return (
    <div className="mx-auto max-w-3xl space-y-5 p-3 sm:p-6">
      <div className="flex items-start gap-4">
        <span
          className={
            kind === "guide"
              ? "grid size-12 shrink-0 place-items-center rounded-xl bg-info/15 text-info"
              : "grid size-12 shrink-0 place-items-center rounded-xl bg-primary-subtle text-primary"
          }
        >
          {kind === "guide" ? <CalendarRange size={22} /> : <ListVideo size={22} />}
        </span>
        <div className="min-w-0 flex-1">
          <h2 className="truncate text-xl font-semibold tracking-tight text-fg">
            {source
              ? source.kind === "plex"
                ? plexLineupName(source.name)
                : source.name
              : kind === "guide"
                ? t("New guide source")
                : t("New IPTV source")}
          </h2>
          <p className="text-sm text-fg-muted">
            {plex
              ? t("Plex's TV listings for your provider, by postal code.")
              : textOf(
                  (
                    selectedPlugin ??
                    byEditor(kind === "guide" ? "guide" : xtream ? "xtream" : "playlist")
                  )?.hint,
                ) ||
                (kind === "guide"
                  ? t("An XMLTV guide on its own. Map any channel to it for listings.")
                  : t("An M3U playlist of live streams, with an optional XMLTV guide."))}
          </p>
        </div>
        {source ? (
          <div className="flex shrink-0 gap-2">
            <Button
              variant="outline"
              size="sm"
              disabled={Boolean(busy) || !urlBacked}
              onClick={() => void refresh()}
              title={urlBacked ? undefined : t("Uploaded files can't be refreshed; upload again")}
            >
              <RefreshCw size={13} className={busy === "refresh" ? "animate-spin" : ""} />{" "}
              {t("Refresh now")}
            </Button>
          </div>
        ) : null}
      </div>

      {source ? (
        <div className="grid grid-cols-2 gap-2 sm:grid-cols-4">
          {[
            kind === "guide"
              ? {
                  key: "coverage",
                  label: t("Coverage"),
                  value: source.guideFrom
                    ? `${duration(Date.parse(source.guideUntil ?? source.guideFrom) - Date.parse(source.guideFrom))}`
                    : "—",
                }
              : {
                  key: "channels",
                  label: t("Channels"),
                  value: formatNumber(source.channelCount ?? 0),
                },
            {
              key: "guideChannels",
              label: t("Guide channels"),
              value: formatNumber(source.guideChannelCount ?? 0),
            },
            {
              key: "listings",
              label: t("Listings"),
              value: formatNumber(source.programmeCount ?? 0),
            },
            {
              key: "until",
              label: t("Guide runs to"),
              value: source.guideUntil ? dayClock(source.guideUntil) : "—",
            },
          ].map(({ key, label, value: v }) => (
            <div key={key} className="rounded-xl border border-border bg-surface px-3 py-2.5">
              <p className="text-[11px] font-medium uppercase tracking-wider text-fg-subtle">
                {label}
              </p>
              <p className="mt-0.5 truncate text-lg font-semibold tabular-nums text-fg">{v}</p>
            </div>
          ))}
        </div>
      ) : null}

      {source?.lastError ? (
        <div className="flex items-start gap-2 rounded-xl border border-danger/40 bg-danger/10 p-3 text-sm">
          <AlertTriangle size={15} className="mt-0.5 shrink-0 text-danger" />
          <div>
            <p className="font-medium text-fg">{t("The last automatic refresh failed")}</p>
            <p className="text-fg-muted">{sourceImportError(source.lastError)}</p>
          </div>
        </div>
      ) : null}

      {xtream && source?.account ? <ProviderAccountCard account={source.account} /> : null}

      {showTabs ? (
        <SubtabNav
          ariaLabel={t("Source settings")}
          active={onTab}
          onSelect={(key) =>
            setTab(key === "groups" ? "groups" : key === "limits" ? "limits" : "connection")
          }
          tabs={[
            { key: "connection", label: t("Connection") },
            ...(showGroups
              ? [{ key: "groups", label: t("Channel groups"), count: groups?.length }]
              : []),
            { key: "limits", label: t("Limits") },
          ]}
        />
      ) : null}

      {onTab === "limits" ? (
        <section className="rounded-2xl border border-border bg-surface p-5">
          <SourceLimits
            rules={draft}
            account={xtream ? source?.account : undefined}
            onChange={(next) =>
              setDraft((d) => ({
                ...d,
                maxConnections: next.maxConnections,
                tuneGapSeconds: next.tuneGapSeconds,
                tunesPerHour: next.tunesPerHour,
              }))
            }
          />
        </section>
      ) : null}

      {onTab === "groups" && groups ? (
        <section className="rounded-2xl border border-border bg-surface p-5">
          <ChannelGroups groups={groups} rules={draft} onChange={setRules} />
        </section>
      ) : null}

      <section
        hidden={onTab !== "connection"}
        className="space-y-4 rounded-2xl border border-border bg-surface p-5"
      >
        {!source && !guideOnly ? (
          <div className="space-y-2">
            <Segmented
              ariaLabel={t("Source type")}
              value={kind}
              onChange={(next) => {
                setKind(next);
                onKindChange?.(next);
                if (selectedPlugin) setDraft((d) => ({ ...d, playlistUrl: "" }));
              }}
              options={typeDescriptors.map((d) => ({
                value: (d.meta?.editor === "guide" ? "guide" : "playlist") as SourceKind,
                label: textOf(d.label),
                icon: kindIcon(d),
              }))}
            />
            {registry.error ? (
              <p role="alert" className="text-sm text-danger">
                {t("Source kinds could not be loaded.")}{" "}
                <Button type="button" variant="ghost" size="sm" onClick={registry.reload}>
                  {t("Try again")}
                </Button>
              </p>
            ) : registry.loading ? (
              <p className="text-sm text-fg-muted">{t("Loading source kinds…")}</p>
            ) : null}
            {pluginKinds.length ? (
              <div className="flex flex-wrap items-center gap-1.5">
                {pluginKinds.map((d) => (
                  <button
                    key={d.id}
                    type="button"
                    onClick={() => {
                      setKind("playlist");
                      onKindChange?.("playlist");
                      setDraft((s) => ({
                        ...s,
                        playlistUrl: d.id,
                        playlist: "",
                        auth: undefined,
                        name: s.name || textOf(d.label),
                      }));
                    }}
                  >
                    <Badge variant={draft.playlistUrl === d.id ? "default" : "secondary"}>
                      {t("{label} (from {owner})", { label: textOf(d.label), owner: d.owner })}
                    </Badge>
                  </button>
                ))}
              </div>
            ) : null}
          </div>
        ) : null}
        {kind === "guide" && !source ? (
          <Segmented
            ariaLabel={t("Guide type")}
            value={guideKind}
            onChange={(v) => {
              setGuideKind(v);
              onGuideKindChange?.(v);
              setDraft((d) => ({ ...d, lineupId: "" }));
              setLineups(null);
            }}
            options={[
              { value: "xmltv", label: t("XMLTV guide") },
              { value: "plex", label: t("Plex guide") },
            ]}
          />
        ) : null}
        {plex ? (
          <div className="space-y-3">
            <div className="grid grid-cols-2 gap-3">
              <Field label={t("Country")}>
                <SearchSelect
                  value={draft.country ?? region}
                  onChange={(country) => {
                    setDraft({ ...draft, country, lineupId: "" });
                    setLineups(null);
                  }}
                  options={countries}
                  ariaLabel={t("Country")}
                  searchable
                  {...searchSelectLabels()}
                />
              </Field>
              <Field label={t("Postal code")} htmlFor="plex-postal">
                <Input
                  id="plex-postal"
                  value={draft.postalCode ?? ""}
                  onChange={(e) => {
                    setDraft({ ...draft, postalCode: e.currentTarget.value, lineupId: "" });
                    setLineups(null);
                  }}
                />
              </Field>
            </div>
            <Button
              variant="outline"
              size="sm"
              disabled={looking || !(draft.country || region) || !draft.postalCode}
              onClick={() => {
                setLooking(true);
                setError("");
                void tv
                  .plexLineups(draft.country || region, draft.postalCode || "")
                  .then(setLineups)
                  .catch((e) => setError(errorMessage(e)))
                  .finally(() => setLooking(false));
              }}
            >
              {looking ? t("Finding lineups…") : t("Find lineups")}
            </Button>
            {lineups ? (
              <div className="space-y-1">
                <p className="text-xs font-semibold text-fg-muted">{t("Choose a lineup")}</p>
                {lineups.length ? (
                  [...lineups]
                    .sort((a, b) => Number(b.type === "ota") - Number(a.type === "ota"))
                    .map((l) => {
                      const count = plexLineupCount(l.title);
                      const type =
                        l.type === "ota"
                          ? t("Antenna (over the air)")
                          : l.type === "cable"
                            ? t("Cable")
                            : l.type === "satellite"
                              ? t("Satellite")
                              : l.type;
                      return (
                        <label
                          key={l.id}
                          className="flex cursor-pointer items-center gap-2 rounded-lg border border-border p-2 text-sm"
                        >
                          <input
                            type="radio"
                            name="plex-lineup"
                            checked={draft.lineupId === l.id}
                            onChange={() =>
                              setDraft({ ...draft, lineupId: l.id, name: plexLineupName(l.title) })
                            }
                          />
                          <span>
                            <span className="block">{plexLineupName(l.title)}</span>
                            <span className="text-xs text-fg-muted">
                              {count === null
                                ? type
                                : tn(
                                    count,
                                    "{type} · {count} channel",
                                    "{type} · {count} channels",
                                    { type },
                                  )}
                            </span>
                          </span>
                        </label>
                      );
                    })
                ) : (
                  <p className="text-sm text-fg-muted">
                    {t("No lineups found for this postal code.")}
                  </p>
                )}
              </div>
            ) : null}
          </div>
        ) : null}
        <Field label={t("Source name")} htmlFor="src-name">
          <Input
            id="src-name"
            value={draft.name}
            onChange={(e) => setDraft({ ...draft, name: e.currentTarget.value })}
            placeholder={
              kind === "guide" ? t("e.g. Regional listings") : t("e.g. My IPTV provider")
            }
          />
        </Field>
        {source && pluginBacked ? (
          <div className="flex flex-wrap items-center justify-between gap-2 rounded-xl border border-border bg-bg-subtle/60 px-4 py-3 text-sm">
            <span className="text-fg-muted">
              {selectedPlugin
                ? t("Channels come from {name}.", { name: textOf(selectedPlugin.label) })
                : t("The plugin behind this source is off or removed.")}
            </span>
            <Link to="/plugins/installed" className="font-medium text-primary hover:underline">
              {t("Manage in Plugins")}
            </Link>
          </div>
        ) : null}
        {kind === "playlist" && !pluginBacked ? (
          <Field label={t("Connect with")}>
            <Segmented
              ariaLabel={t("Connect with")}
              size="sm"
              value={xtream ? "xtream" : "url"}
              onChange={(v) =>
                setDraft((d) => ({
                  ...d,
                  auth:
                    v === "xtream"
                      ? { ...d.auth, type: "xtream" }
                      : { ...d.auth, type: d.auth?.type === "xtream" ? "" : d.auth?.type },
                }))
              }
              options={[
                { value: "url", label: t("Playlist URL or file"), icon: <Link2 size={13} /> },
                {
                  value: "xtream",
                  label: textOf(byEditor("xtream")?.label) || t("Xtream Codes login"),
                  icon: <KeyRound size={13} />,
                },
              ]}
            />
          </Field>
        ) : null}
        {xtream ? (
          <div className="space-y-4 rounded-xl border border-border bg-bg-subtle/60 p-4">
            <label className="flex items-center justify-between gap-3 text-sm text-fg">
              {t("Use for Live TV")}
              <Switch
                aria-label={t("Use for Live TV")}
                checked={draft.useLiveTV !== false}
                onChange={(e) => {
                  const checked = e.currentTarget.checked;
                  setDraft((d) => ({ ...d, useLiveTV: checked }));
                }}
              />
            </label>
            <label className="flex items-center justify-between gap-3 text-sm text-fg">
              {t("Use for movies & shows")}
              <Switch
                aria-label={t("Use for movies & shows")}
                checked={draft.useVOD ?? false}
                onChange={(e) => {
                  const checked = e.currentTarget.checked;
                  setDraft((d) => ({ ...d, useVOD: checked }));
                }}
              />
            </label>
            {draft.useVOD ? (
              <div
                className="rounded-lg border border-border p-3 text-sm text-fg-muted"
                role="status"
              >
                <p className="font-medium text-fg">{t("Movies & shows index")}</p>
                {source?.vodStatus?.updated ? (
                  <>
                    <p>
                      {tn(source.vodStatus.movies, "{count} movie", "{count} movies")} ·{" "}
                      {tn(source.vodStatus.series, "{count} show", "{count} shows")}
                    </p>
                    <p>{relativeTime(new Date(source.vodStatus.updated * 1000).toISOString())}</p>
                  </>
                ) : (
                  <p>{t("Waiting to index")}</p>
                )}
                {source?.vodStatus?.error ? (
                  <p>{t("Provider unavailable; retry scheduled")}</p>
                ) : null}
              </div>
            ) : null}
            <Field
              label={t("Server")}
              htmlFor="xt-server"
              hint={t("The panel address your provider gave you, e.g. {example}", {
                example: "http://provider.example:8080",
              })}
            >
              <Input
                id="xt-server"
                value={auth.server ?? ""}
                onChange={(e) => setAuth({ server: e.currentTarget.value })}
                placeholder="http://provider.example:8080"
              />
            </Field>
            <div className="grid gap-4 sm:grid-cols-2">
              <Field label={t("Username")} htmlFor="xt-user">
                <Input
                  id="xt-user"
                  value={auth.username ?? ""}
                  autoComplete="off"
                  onChange={(e) => setAuth({ username: e.currentTarget.value })}
                />
              </Field>
              <Field label={t("Password")} htmlFor="xt-pass">
                <SecretInput
                  id="xt-pass"
                  value={auth.password ?? ""}
                  onChange={(v) => setAuth({ password: v })}
                />
              </Field>
            </div>
            <Field
              label={t("Stream format")}
              hint={t("TS works with almost every panel; pick HLS if yours prefers it.")}
            >
              <Segmented
                ariaLabel={t("Stream format")}
                size="sm"
                value={auth.output ?? "ts"}
                onChange={(v) => setAuth({ output: v })}
                options={[
                  { value: "ts", label: "MPEG-TS" },
                  { value: "m3u8", label: "HLS" },
                ]}
              />
            </Field>
          </div>
        ) : null}
        {kind === "playlist" && !xtream && !pluginBacked ? (
          <UrlOrFile
            id="src-m3u"
            label={t("M3U playlist")}
            url={draft.playlistUrl}
            fileReady={Boolean(files.playlist)}
            accept=".m3u,.m3u8,text/plain"
            placeholder="https://…/playlist.m3u"
            onUrl={(v) => setDraft({ ...draft, playlistUrl: v, playlist: "" })}
            onFile={(f) => void upload(f, "playlist")}
          />
        ) : null}
        {!pluginBacked && !plex ? (
          <UrlOrFile
            id="src-xmltv"
            label={
              xtream
                ? t("XMLTV guide override (optional)")
                : kind === "playlist"
                  ? t("XMLTV guide (optional)")
                  : t("XMLTV guide")
            }
            url={draft.guideUrl}
            fileReady={Boolean(files.guide)}
            accept=".xml,.gz,application/xml,text/xml,application/gzip"
            placeholder={
              xtream
                ? t("Leave blank to use the panel's guide")
                : t("https://…/guide.xml or .xml.gz")
            }
            hint={
              xtream
                ? t("Ether uses the panel's own XMLTV export unless you set one here.")
                : t("Gzipped guides (.xml.gz) are fine.")
            }
            onUrl={(v) => setDraft({ ...draft, guideUrl: v, guide: "" })}
            onFile={(f) => void upload(f, "guide")}
          />
        ) : null}
        {!xtream && !pluginBacked && !plex ? <SignIn auth={auth} onChange={setAuth} /> : null}
        {!pluginBacked && !plex ? <ClientIdentity auth={auth} onChange={setAuth} /> : null}
        <Field
          label={t("Keep it fresh")}
          hint={
            !urlBacked
              ? t("Uploaded files stay as they are until you upload again.")
              : auto
                ? source?.nextRefresh
                  ? t("Refreshes on its own before the listings run out · next {when}.", {
                      when: relativeTime(source.nextRefresh),
                    })
                  : t("Refreshes on its own before the listings run out.")
                : t("Only refreshes when you press Refresh now.")
          }
        >
          <Segmented
            ariaLabel={t("Refresh")}
            size="sm"
            value={auto ? "auto" : "manual"}
            onChange={(v) => {
              if (v === "manual") automaticInterval.current = Math.max(0, draft.refreshHours ?? 0);
              setDraft({ ...draft, refreshHours: v === "auto" ? automaticInterval.current : -1 });
            }}
            options={[
              { value: "auto", label: t("Automatic") },
              { value: "manual", label: t("Manual") },
            ]}
          />
        </Field>
        {urlBacked && auto ? (
          <div className="grid gap-4 sm:grid-cols-2">
            <Field label={t("Maximum refresh interval (hours)")}>
              <Input
                type="number"
                min={1}
                max={8760}
                aria-label={t("Maximum refresh interval (hours)")}
                placeholder="6"
                value={draft.refreshHours || ""}
                onChange={(e) =>
                  setDraft({
                    ...draft,
                    refreshHours: e.currentTarget.value
                      ? Math.max(1, Math.min(8760, Number(e.currentTarget.value)))
                      : undefined,
                  })
                }
                onBlur={(e) => {
                  if (!e.currentTarget.value) setDraft({ ...draft, refreshHours: 6 });
                }}
              />
            </Field>
            <Field
              label={t("Refresh after inactivity")}
              hint={t(
                "Refreshes in the background when an app returns after five idle minutes. Apps keep using the last good playlist and guide.",
              )}
            >
              <select
                className="w-full rounded-lg border border-border bg-surface p-2 text-sm"
                aria-label={t("Refresh after inactivity")}
                value={draft.activityRefreshMinutes ?? 0}
                onChange={(e) =>
                  setDraft({ ...draft, activityRefreshMinutes: Number(e.currentTarget.value) })
                }
              >
                <option value={0}>{t("Off")}</option>
                {[...new Set([30, 60, 120, 360, draft.activityRefreshMinutes ?? 0])]
                  .filter((minutes) => minutes > 0)
                  .sort((a, b) => a - b)
                  .map((minutes) => (
                    <option key={minutes} value={minutes}>
                      {tn(minutes, "{count} minute", "{count} minutes")}
                    </option>
                  ))}
              </select>
            </Field>
          </div>
        ) : null}
      </section>
      <div className="space-y-3">
        {error ? (
          <p role="alert" className="text-sm text-danger">
            {error}
          </p>
        ) : null}
        {rulesChanged ? (
          <p className="flex items-center gap-2 text-xs text-fg-muted">
            <Badge variant="warning">{t("Unsaved changes")}</Badge>
            {t("Saving re-imports this source with your channel group choices.")}
          </p>
        ) : null}
        <div className="flex flex-wrap items-center gap-2">
          <Button disabled={!canSave || Boolean(busy)} onClick={() => void save()}>
            {busy === "save"
              ? t("Importing…")
              : source
                ? t("Save and re-import")
                : t("Import source")}
          </Button>
          {source ? (
            <span className="text-xs text-fg-muted">
              {source.guideFrom && source.guideUntil
                ? t("Last imported {when} · {span} of listings", {
                    when: relativeTime(source.updatedAt),
                    span: duration(Date.parse(source.guideUntil) - Date.parse(source.guideFrom)),
                  })
                : t("Last imported {when}", { when: relativeTime(source.updatedAt) })}
            </span>
          ) : null}
          {source?.skippedVod ? (
            <span className="text-xs text-fg-muted">
              {tn(source.skippedVod, "{count} VOD entry skipped", "{count} VOD entries skipped")}
            </span>
          ) : null}
          {source ? (
            <Button
              variant="ghost"
              className="ml-auto text-danger hover:bg-danger/10"
              disabled={Boolean(busy)}
              onClick={() => void remove()}
            >
              <Trash2 size={14} /> {t("Delete source")}
            </Button>
          ) : null}
        </div>
        {busy === "save" ? (
          <p role="status" className="text-xs text-fg-muted">
            {progressUnavailable
              ? t("Import in progress. Detailed progress is unavailable on this server.")
              : progress
                ? importProgressText(progress, progressRate)
                : t("Starting import…")}
            {progressUnavailable && savedImport ? (
              <span className="block text-xs text-fg-muted">
                {tn(
                  savedImport.channelCount ?? 0,
                  "{count} channel in the last saved import",
                  "{count} channels in the last saved import",
                )}
              </span>
            ) : null}
          </p>
        ) : null}
      </div>
    </div>
  );
}

/** The owner's group rules, normalised so an unchanged draft compares equal to its source. */
function groupRules(r: GroupRules): GroupRules {
  return {
    excludeGroups: r.excludeGroups?.length ? [...r.excludeGroups] : [],
    newGroups: r.newGroups === "exclude" ? "exclude" : "",
    hideAdult: Boolean(r.hideAdult),
    groupOrder: r.groupOrder?.length ? [...r.groupOrder] : [],
  };
}

/** Compares group rules; the excluded set's order does not matter. */
function rulesKey(r: GroupRules): string {
  const g = groupRules(r);
  return JSON.stringify({ ...g, excludeGroups: g.excludeGroups!.map(normGroup).sort() });
}

/** The import step whose counter is moving; the rate is measured per step. */
function progressStage(p: SourceProgress): string {
  if (p.phase === "saving") {
    return p.channels && (p.savedChannels ?? 0) < p.channels ? "channels" : "listings";
  }
  return p.phase;
}

function progressValue(p: SourceProgress, stage: string): number {
  if (stage === "channels") return p.savedChannels ?? 0;
  if (stage === "listings") return p.savedProgrammes ?? 0;
  return p.read;
}

function etaText(seconds: number): string {
  if (!Number.isFinite(seconds) || seconds <= 0) return "";
  if (seconds < 1) return t("almost done");
  if (seconds < 60) return t("~{seconds}s left", { seconds: Math.round(seconds) });
  const minutes = Math.round(seconds / 60);
  if (minutes < 60) return t("~{minutes}m left", { minutes });
  return t("~{hours}h {minutes}m left", {
    hours: Math.floor(minutes / 60),
    minutes: minutes % 60,
  });
}

/** "Fetching guide · 1.2 GiB / 4.1 GiB · ~2m left" while an import runs. */
function importProgressText(p: SourceProgress, rate: number): string {
  let text: string;
  let remaining = 0;
  if (p.phase === "saving") {
    if (p.channels && (p.savedChannels ?? 0) < p.channels) {
      text = t("Saving channels · {saved} of {total}", {
        saved: formatNumber(p.savedChannels ?? 0),
        total: formatNumber(p.channels),
      });
      remaining = p.channels - (p.savedChannels ?? 0);
    } else if (p.programmes) {
      text = t("Saving listings · {saved} of {total}", {
        saved: formatNumber(p.savedProgrammes ?? 0),
        total: formatNumber(p.programmes),
      });
      remaining = p.programmes - (p.savedProgrammes ?? 0);
    } else {
      return t("Saving channels and listings");
    }
  } else {
    const phase =
      p.phase === "playlist"
        ? t("Fetching playlist")
        : p.phase === "guide"
          ? t("Fetching guide")
          : t("Starting import");
    const size =
      p.total && p.total > 0
        ? `${formatBytes(p.read)} / ${formatBytes(p.total)}`
        : p.read > 0
          ? formatBytes(p.read)
          : "";
    const count =
      p.phase === "guide" && p.programmes
        ? t("{programmes} programmes", { programmes: formatNumber(p.programmes) })
        : "";
    text = [phase, size, count].filter(Boolean).join(" · ");
    if (p.total && p.total > p.read) remaining = p.total - p.read;
  }
  const eta = rate > 0 && remaining > 0 ? etaText(remaining / rate) : "";
  return eta ? `${text} · ${eta}` : text;
}

/** Drop an empty login so plain sources stay plain. */
function cleanAuth(a: SourceAuth | undefined, kind: SourceKind): SourceAuth | undefined {
  if (!a) return undefined;
  const type = kind === "guide" && a.type === "xtream" ? "" : (a.type ?? "");
  const headers = Object.fromEntries(
    Object.entries(a.headers ?? {}).filter(([k]) => k.trim() !== ""),
  );
  const out: SourceAuth = {
    type,
    ...(type === "basic" || type === "xtream"
      ? { username: a.username?.trim(), password: a.password }
      : {}),
    ...(type === "bearer" ? { token: a.token } : {}),
    ...(type === "xtream" ? { server: a.server?.trim(), output: a.output ?? "ts" } : {}),
    ...(Object.keys(headers).length ? { headers } : {}),
    ...(a.userAgent?.trim() ? { userAgent: a.userAgent.trim() } : {}),
    ...(a.referer?.trim() ? { referer: a.referer.trim() } : {}),
  };
  if (type === "headers" && !out.headers) out.type = "";
  if (!out.type && !out.headers && !out.userAgent && !out.referer) return undefined;
  return out;
}

const SIGN_IN = localized<{ value: Exclude<AuthType, "xtream">; label: string }[]>(() => [
  { value: "", label: t("None") },
  { value: "basic", label: t("Username & password") },
  { value: "bearer", label: t("Token") },
  { value: "headers", label: t("Custom headers") },
]);

/** How Ether signs in to a provider that protects its playlist, guide and streams. */
function SignIn({ auth, onChange }: { auth: SourceAuth; onChange: (p: SourceAuth) => void }) {
  const type = auth.type === "xtream" ? "" : (auth.type ?? "");
  return (
    <Field
      label={t("Sign-in")}
      hint={
        type
          ? t(
              "Used for the playlist, the guide and this source's streams. Never sent to other providers.",
            )
          : t("Choose a sign-in if the provider protects its URLs.")
      }
    >
      <div className="space-y-3">
        <Segmented
          ariaLabel={t("Sign-in")}
          size="sm"
          value={type}
          onChange={(v) => onChange({ type: v })}
          options={SIGN_IN}
        />
        {type === "basic" ? (
          <div className="grid gap-3 sm:grid-cols-2">
            <Input
              aria-label={t("Username")}
              placeholder={t("Username")}
              autoComplete="off"
              value={auth.username ?? ""}
              onChange={(e) => onChange({ username: e.currentTarget.value })}
            />
            <SecretInput
              ariaLabel={t("Password")}
              placeholder={t("Password")}
              value={auth.password ?? ""}
              onChange={(v) => onChange({ password: v })}
            />
          </div>
        ) : null}
        {type === "bearer" ? (
          <SecretInput
            ariaLabel={t("Token")}
            placeholder={t("Bearer token")}
            value={auth.token ?? ""}
            onChange={(v) => onChange({ token: v })}
          />
        ) : null}
        {type === "headers" ? (
          <HeaderRows headers={auth.headers ?? {}} onChange={(headers) => onChange({ headers })} />
        ) : null}
      </div>
    </Field>
  );
}

function HeaderRows({
  headers,
  onChange,
}: {
  headers: Record<string, string>;
  onChange: (h: Record<string, string>) => void;
}) {
  // Rows live locally so blank and in-progress rows survive; the source only
  // receives named headers.
  const [list, setList] = useState<[string, string][]>(() => {
    const rows = Object.entries(headers);
    return rows.length ? rows : [["", ""]];
  });
  const commit = (next: [string, string][]) => {
    setList(next.length ? next : [["", ""]]);
    onChange(Object.fromEntries(next.filter(([k]) => k.trim() !== "")));
  };
  return (
    <div className="space-y-2">
      {list.map(([k, v], i) => (
        <div key={i} className="flex items-start gap-2">
          <Input
            aria-label={t("Header {number} name", { number: i + 1 })}
            placeholder="X-Api-Key"
            value={k}
            className="w-44 shrink-0 font-mono text-xs"
            onChange={(e) =>
              commit(list.map((r, n) => (n === i ? [e.currentTarget.value, r[1]] : r)))
            }
          />
          <div className="min-w-0 flex-1">
            <SecretInput
              ariaLabel={t("Header {number} value", { number: i + 1 })}
              placeholder={t("Value")}
              value={v}
              onChange={(val) => commit(list.map((r, n) => (n === i ? [r[0], val] : r)))}
            />
          </div>
          <Button
            variant="ghost"
            size="sm"
            className="size-9 shrink-0 p-0 text-fg-subtle hover:text-danger"
            aria-label={t("Remove header {number}", { number: i + 1 })}
            onClick={() => commit(list.filter((_, n) => n !== i))}
          >
            <X size={14} />
          </Button>
        </div>
      ))}
      <Button variant="ghost" size="sm" onClick={() => commit([...list, ["", ""]])}>
        <Plus size={13} /> {t("Add header")}
      </Button>
    </div>
  );
}

/** User agent and referer: many providers only answer known players. */
function ClientIdentity({
  auth,
  onChange,
}: {
  auth: SourceAuth;
  onChange: (p: SourceAuth) => void;
}) {
  const set = Boolean(auth.userAgent || auth.referer);
  return (
    <details open={set} className="rounded-xl border border-border bg-bg-subtle/40 px-4 py-3">
      <summary className="cursor-pointer text-sm font-medium text-fg">
        {t("Player identity")}{" "}
        <span className="font-normal text-fg-muted">
          {set ? `· ${t("custom")}` : `· ${t("for providers that only answer known players")}`}
        </span>
      </summary>
      <div className="mt-3 grid gap-3 sm:grid-cols-2">
        <Field label={t("User agent")} htmlFor="src-ua">
          <Input
            id="src-ua"
            placeholder={t("e.g. {example}", { example: "VLC/3.0.20 LibVLC/3.0.20" })}
            value={auth.userAgent ?? ""}
            onChange={(e) => onChange({ userAgent: e.currentTarget.value })}
          />
        </Field>
        <Field label={t("Referer")} htmlFor="src-referer">
          <Input
            id="src-referer"
            placeholder="https://…"
            value={auth.referer ?? ""}
            onChange={(e) => onChange({ referer: e.currentTarget.value })}
          />
        </Field>
      </div>
    </details>
  );
}

function UrlOrFile({
  id,
  label,
  url,
  fileReady,
  accept,
  placeholder,
  hint,
  onUrl,
  onFile,
}: {
  id: string;
  label: string;
  url: string;
  fileReady: boolean;
  accept: string;
  placeholder: string;
  hint?: string;
  onUrl: (v: string) => void;
  onFile: (f: File | undefined) => void;
}) {
  return (
    <Field label={label} htmlFor={id} hint={hint}>
      <div className="flex gap-2">
        <Input
          id={id}
          value={url}
          onChange={(e) => onUrl(e.currentTarget.value)}
          placeholder={fileReady ? t("Using the uploaded file") : placeholder}
          className="min-w-0 flex-1"
        />
        <label className="relative inline-flex h-9 shrink-0 cursor-pointer items-center gap-1.5 rounded-md border border-border bg-surface px-3 text-sm font-medium text-fg hover:bg-surface-hover">
          <FileUp size={14} />
          {t("Upload")}
          <input
            type="file"
            accept={accept}
            className="sr-only"
            aria-label={t("Upload {name}", { name: label })}
            onChange={(e) => onFile(e.target.files?.[0])}
          />
        </label>
      </div>
      {fileReady ? (
        <Badge variant="success" className="mt-1.5">
          {t("File ready to import")}
        </Badge>
      ) : null}
    </Field>
  );
}
