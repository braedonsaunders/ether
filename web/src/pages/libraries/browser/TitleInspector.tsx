import { WarmingControls } from "../../cache/WarmingControls";
import { PanelSlots, PluginActionMenu } from "../../../viewspec/slots/Slots";
import { Badge, Button, Spinner, SubtabNav } from "@braedonsaunders/appkit-ui";
import { useState, type ReactNode } from "react";
import { Link } from "react-router-dom";
import { ArrowUpRight, Film, Star } from "lucide-react";
import { Artwork } from "../../../components/Artwork";
import { Drawer } from "../../../components/Drawer";
import { ErrorPanel } from "../../../components/feedback";
import { getLocale, t, tn } from "../../../i18n";
import { api, apiFetch } from "../../../lib/api";
import type { TitleDetail, TitleLibraryReason, TitleLibraryStatus } from "../../../lib/contract";
import { formatRating, ratingSource } from "../../../lib/contentRatings";
import { fixed } from "../../../lib/format";
import { useAsync } from "../../../lib/hooks";
import { localToday } from "../../calendar/model";
import { CatalogTags } from "./CatalogTags";
import { TitleAvailability } from "./TitleAvailability";
import { TitleCalendar } from "./TitleCalendar";
import { NextEpisodeCard, TitleEpisodes } from "./TitleEpisodes";
import { TitleReleases, milestone } from "./TitleReleases";
import {
  countryName,
  longDate,
  shortDate,
  titleScheduleApi,
  type MovieReleases,
  type ScheduleEpisode,
} from "./titleSchedule";
import "./catalog.css";

type InspectorTab = "overview" | "episodes" | "calendar" | "releases" | "availability";

/** Where another tab asked the drawer to go: a season, and optionally one episode. */
export interface EpisodeFocus {
  season: number;
  key?: string;
}

export function TitleInspector({
  titleKey,
  onClose,
  watchlistAction,
  watchlistViewers,
  onLibraryChange,
}: {
  titleKey: string;
  onClose: () => void;
  watchlistAction?: ReactNode;
  watchlistViewers?: ReactNode;
  onLibraryChange?: () => void;
}) {
  const state = useAsync(() => api.title(titleKey), [titleKey]);
  const title = state.data;
  const isShow = title?.type === "show";
  // Loaded after title details settle so the server reads the cached row and
  // performs one primary-key lookup against catalog_owner_includes.
  const library = useAsync(
    (signal) => (title ? api.titleLibrary(title.key, signal) : Promise.resolve(null)),
    [title?.key],
  );
  // One schedule read serves Overview's next-episode card, Episodes and Calendar.
  const schedule = useAsync(
    (signal) => (isShow ? titleScheduleApi.show(titleKey, signal) : Promise.resolve(null)),
    [titleKey, isShow],
  );
  // One release-dates read serves the Overview glance and the Releases tab,
  // so both always show the same region-aware dates.
  const isMovie = title?.type === "movie";
  const releases = useAsync(
    (signal) => (isMovie ? titleScheduleApi.releases(titleKey, signal) : Promise.resolve(null)),
    [titleKey, isMovie],
  );
  const [tab, setTab] = useState<InspectorTab>("overview");
  const [focus, setFocus] = useState<EpisodeFocus | null>(null);
  const [availabilityKey, setAvailabilityKey] = useState<string | null>(null);

  const tabs: { key: InspectorTab; label: string }[] = !title
    ? []
    : isShow
      ? [
          { key: "overview", label: t("Overview") },
          { key: "episodes", label: t("Episodes") },
          { key: "calendar", label: t("Calendar") },
          { key: "availability", label: t("Availability") },
        ]
      : [
          { key: "overview", label: t("Overview") },
          { key: "releases", label: t("Releases") },
          { key: "availability", label: t("Availability") },
        ];
  const active = tabs.some((x) => x.key === tab) ? tab : "overview";

  const openEpisode = (e: Pick<ScheduleEpisode, "season" | "key">) => {
    setFocus({ season: e.season, key: e.key });
    setTab("episodes");
  };
  const checkEpisode = (e: Pick<ScheduleEpisode, "key">) => {
    setAvailabilityKey(e.key);
    setTab("availability");
  };

  return (
    <Drawer
      open
      onClose={onClose}
      title={title?.title ?? t("Title details")}
      size="lg"
      bodyClassName="overflow-y-auto p-0"
      subtabs={
        tabs.length ? (
          <SubtabNav
            ariaLabel={t("Title sections")}
            tabs={tabs}
            active={active}
            onSelect={(key) => setTab(key as InspectorTab)}
          />
        ) : undefined
      }
    >
      {state.error ? (
        <ErrorPanel className="m-5" error={state.error} onRetry={() => void state.reload()} />
      ) : !title ? (
        <div className="grid min-h-64 place-items-center">
          <Spinner label={t("Loading title details")} />
        </div>
      ) : active === "episodes" ? (
        <TitleEpisodes title={title} schedule={schedule} focus={focus} onCheck={checkEpisode} />
      ) : active === "calendar" ? (
        <TitleCalendar title={title} schedule={schedule} onOpen={openEpisode} />
      ) : active === "releases" ? (
        <TitleReleases releases={releases} />
      ) : active === "availability" ? (
        <TitleAvailability
          title={title}
          schedule={schedule.data ?? null}
          episodeKey={availabilityKey}
          onEpisodeKey={setAvailabilityKey}
        />
      ) : (
        <Overview
          watchlistAction={watchlistAction}
          watchlistViewers={watchlistViewers}
          title={title}
          library={library.data ?? null}
          onLibraryReload={async () => {
            await library.reload();
            onLibraryChange?.();
          }}
          next={schedule.data?.next ?? null}
          last={schedule.data?.last ?? null}
          scheduleLoading={isShow && schedule.loading}
          onOpenEpisode={openEpisode}
          releases={releases.data ?? null}
          onShowReleases={() => setTab("releases")}
          onReload={() => void state.reload()}
          onClose={onClose}
        />
      )}
    </Drawer>
  );
}

export function libraryReasonLine(item: TitleLibraryReason): string {
  switch (item.code) {
    case "popularity_floor":
      return tn(
        item.votes ?? 0,
        "Below your popularity floor ({count} vote; floor {floor})",
        "Below your popularity floor ({count} votes; floor {floor})",
        { floor: item.minVotes ?? 0 },
      );
    case "popularity_only_floor":
      return t("Below your popularity floor ({popularity} popularity; floor {floor})", {
        popularity: fixed(item.popularity ?? 0, 1),
        floor: fixed(item.minPopularity ?? 0, 1),
      });
    case "search_only":
      return t("In the search-only index");
    case "catalog_rule_named":
      return t("Excluded by your catalogue rule “{rule}”", { rule: item.rule ?? "" });
    case "catalog_rule":
      return t("Excluded by your catalogue rules");
    case "catalog_default_hide":
      return t("Hidden by your default catalogue visibility");
    case "release_gate_movie":
      return t("Waiting for a playable release (new movie)");
    case "release_gate_show":
      return t("Waiting for a playable release");
    case "unreleased":
      return t("Not released yet");
    case "content_policy":
      return t("Excluded by your content rules");
    case "adult":
      return t("Adult titles are off");
    default:
      return item.message;
  }
}

function LibraryStatusSection({
  titleKey,
  status,
  onChanged,
}: {
  titleKey: string;
  status: TitleLibraryStatus | null;
  onChanged: () => Promise<void>;
}) {
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<unknown>(null);
  if (!status || (status.browsable && !status.included)) return null;

  const lines =
    status.items && status.items.length > 0
      ? status.items.map(libraryReasonLine)
      : (status.reasons ?? []);

  const act = async (fn: () => Promise<TitleLibraryStatus>) => {
    setSaving(true);
    setError(null);
    try {
      await fn();
      await onChanged();
    } catch (e) {
      setError(e);
    } finally {
      setSaving(false);
    }
  };

  return (
    <section
      aria-label={status.browsable ? t("Library") : t("Not in your library")}
      className="rounded-lg border border-border bg-elevated/50 p-3.5 space-y-2.5"
    >
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div className="flex flex-wrap items-center gap-2">
          <h3 className="m-0">{status.browsable ? t("Library") : t("Not in your library")}</h3>
          {status.included ? <Badge variant="secondary">{t("Added to library")}</Badge> : null}
        </div>
        {status.canInclude ? (
          <Button
            size="sm"
            disabled={saving}
            onClick={() => void act(() => api.includeTitle(titleKey))}
          >
            {t("Add to library")}
          </Button>
        ) : status.included ? (
          <Button
            size="sm"
            variant="ghost"
            disabled={saving}
            onClick={() => void act(() => api.removeIncludedTitle(titleKey))}
          >
            {t("Remove from library")}
          </Button>
        ) : null}
      </div>
      {error ? <ErrorPanel error={error} onRetry={() => setError(null)} /> : null}
      {!status.browsable && lines.length > 0 ? (
        <ul className="list-disc space-y-1 pl-4 text-sm text-fg-muted">
          {lines.map((line, i) => (
            <li key={`${i}-${line}`}>{line}</li>
          ))}
        </ul>
      ) : null}
      {!status.browsable && status.releaseGated ? (
        <p className="text-xs text-fg-muted">
          {t("This title will appear automatically once a playable release exists.")}
        </p>
      ) : null}
      {status.browsable && status.included ? (
        <p className="text-sm text-fg-muted">{t("Included in your browsable library by you.")}</p>
      ) : null}
    </section>
  );
}

function Overview({
  watchlistAction,
  watchlistViewers,
  title,
  library,
  onLibraryReload,
  next,
  last,
  scheduleLoading,
  onOpenEpisode,
  releases,
  onShowReleases,
  onReload,
  onClose,
}: {
  watchlistAction?: ReactNode;
  watchlistViewers?: ReactNode;
  title: TitleDetail;
  library: TitleLibraryStatus | null;
  onLibraryReload: () => Promise<void>;
  releases: MovieReleases | null;
  next: ScheduleEpisode | null;
  last: ScheduleEpisode | null;
  scheduleLoading: boolean;
  onOpenEpisode: (e: ScheduleEpisode) => void;
  onShowReleases: () => void;
  onReload: () => void;
  onClose: () => void;
}) {
  const isShow = title.type === "show";
  return (
    <>
      <div className="catalog-detail-hero">
        <Artwork src={title.backdrop} alt="" icon={<span />} className="absolute inset-0" eager />
        <div className="catalog-detail-scrim" />
        <div className="catalog-detail-heading">
          <Artwork
            src={title.poster}
            alt={t("{title} poster", { title: title.title })}
            className="aspect-[2/3] w-28 shrink-0 rounded-lg shadow-xl"
            eager
          />
          <div>
            <p className="catalog-eyebrow">{isShow ? t("TELEVISION") : t("MOVIE")}</p>
            <h2>{title.title}</h2>
            <p>
              {[
                title.year,
                title.contentRating ? formatRating(title.contentRating) : null,
                title.runtimeMinutes ? t("{minutes} min", { minutes: title.runtimeMinutes }) : null,
              ]
                .filter(Boolean)
                .join(" · ")}
            </p>
            {title.contentRating && title.contentRatingSource ? (
              <p className="mt-1 text-xs text-fg-muted">
                {t("Content rating from {source}", {
                  source: ratingSource(title.contentRatingSource),
                })}
              </p>
            ) : null}
            {title.rating ? (
              <span className="mt-3 inline-flex items-center gap-1.5 text-sm">
                <Star size={14} />
                {t("{rating} / 10 · TMDB", { rating: fixed(title.rating, 1) })}
              </span>
            ) : null}
          </div>
        </div>
      </div>
      <div className="catalog-detail-body">
        {watchlistAction || isShow || title.type === "movie" ? (
          <div className="flex flex-wrap items-center gap-2">
            {watchlistAction}
            {isShow || title.type === "movie" ? <WarmingControls titleKey={title.key} /> : null}
            {watchlistViewers}
          </div>
        ) : null}
        <LibraryStatusSection titleKey={title.key} status={library} onChanged={onLibraryReload} />
        {isShow ? (
          <NextEpisodeCard
            status={title.status}
            next={next}
            last={last}
            loading={scheduleLoading}
            onOpen={onOpenEpisode}
          />
        ) : (
          <MovieDates title={title} releases={releases} onMore={onShowReleases} />
        )}
        {title.tagline ? <p className="catalog-tagline">“{title.tagline}”</p> : null}
        <div className="flex flex-wrap gap-2">
          {title.genres?.map((g) => (
            <Badge key={g} variant="secondary">
              {g}
            </Badge>
          ))}
        </div>
        <section>
          <h3>{t("Overview")}</h3>
          <p>{title.summary || t("No overview is available for this title yet.")}</p>
        </section>
        <CatalogTags titleKey={title.key} />
        <ReleaseEvidence titleKey={title.key} type={title.type} />
        <PanelSlots
          slot="title.panel"
          recordType="title"
          recordId={title.key}
          tmdbId={title.tmdbId}
          imdbId={title.imdbId}
          titleKey={title.key}
          type={title.type}
        />
        <PluginActionMenu slot="actionMenu.title" recordId={title.key} onRefresh={onReload} />
        <Facts title={title} />
        {title.cast?.length ? (
          <section>
            <h3>{t("Cast")}</h3>
            <div className="catalog-cast">
              {title.cast.map((person) => (
                <div key={`${person.id}-${person.role}`}>
                  <Artwork
                    src={person.image}
                    alt=""
                    title={person.name}
                    className="aspect-[2/3] rounded-md"
                  />
                  <h4>{person.name}</h4>
                  <p>{person.role}</p>
                </div>
              ))}
            </div>
          </section>
        ) : null}
        {title.keywords?.length ? (
          <section>
            <h3>{t("Keywords")}</h3>
            <div className="flex flex-wrap gap-1.5">
              {title.keywords.map((k) => (
                <Badge key={typeof k === "string" ? k : k.id} variant="outline">
                  {typeof k === "string" ? k : k.name}
                </Badge>
              ))}
            </div>
          </section>
        ) : null}
        <div className="catalog-detail-footer">
          <Film size={18} />
          <p>
            {t("Artwork and ratings here come from TMDB. Change how they look under Metadata.")}
          </p>
          <Link to="/metadata" onClick={onClose} aria-label={t("Open Metadata")}>
            {t("Metadata")}
            <ArrowUpRight size={14} />
          </Link>
        </div>
      </div>
    </>
  );
}

/** A TMDB status in the owner's words. */
export function statusLabel(status: string | undefined): string {
  switch ((status ?? "").toLowerCase()) {
    case "returning series":
      return t("Returning series");
    case "ended":
      return t("Ended");
    case "canceled":
    case "cancelled":
      return t("Cancelled");
    case "in production":
      return t("In production");
    case "planned":
      return t("Planned");
    case "pilot":
      return t("Pilot");
    case "released":
      return t("Released");
    case "post production":
      return t("Post-production");
    case "rumored":
      return t("Rumoured");
    default:
      return status ?? "";
  }
}

function Facts({ title }: { title: TitleDetail }) {
  const isShow = title.type === "show";
  const language = title.language
    ? (() => {
        try {
          return new Intl.DisplayNames([getLocale()], { type: "language" }).of(title.language);
        } catch {
          return title.language;
        }
      })()
    : "";
  const rows: [string, string | undefined][] = [
    [t("Status"), statusLabel(title.status)],
    [
      isShow ? t("First aired") : t("Release date"),
      title.releaseDate ? longDate(title.releaseDate) : undefined,
    ],
    [t("Last aired"), isShow && title.lastAirDate ? longDate(title.lastAirDate) : undefined],
    [
      t("Seasons"),
      isShow && title.seasonCount
        ? title.episodeCount
          ? t("{seasons} seasons · {episodes} episodes", {
              seasons: title.seasonCount,
              episodes: title.episodeCount,
            })
          : String(title.seasonCount)
        : undefined,
    ],
    [isShow ? t("Networks & studios") : t("Studios"), title.studios?.slice(0, 4).join(", ")],
    [t("Directed by"), isShow ? undefined : title.directors?.map((p) => p.name).join(", ")],
    [
      t("Country"),
      title.countries?.length
        ? title.countries.map((c) => (c.length === 2 ? countryName(c, getLocale()) : c)).join(", ")
        : undefined,
    ],
    [t("Original language"), language || undefined],
    [t("Original title"), title.originalTitle],
    [t("Collection"), title.collection?.name],
    [t("Media type"), isShow ? t("TV show") : t("Movie")],
  ];
  return (
    <dl className="catalog-detail-facts">
      {rows
        .filter(([, v]) => v)
        .map(([label, value]) => (
          <div key={label}>
            <dt>{label}</dt>
            <dd>{value}</dd>
          </div>
        ))}
      <div>
        <dt>{t("Catalog ID")}</dt>
        <dd className="font-mono">{title.key}</dd>
      </div>
    </dl>
  );
}

/** Theatrical and home release at a glance, in the owner's region when TMDB lists it. */
function MovieDates({
  title,
  releases,
  onMore,
}: {
  title: TitleDetail;
  releases: MovieReleases | null;
  onMore: () => void;
}) {
  const today = localToday();
  const dates = releases?.dates ?? [];
  const region = (releases?.region || "US").toUpperCase();
  const theatres = dates.length
    ? milestone(dates, ["theatrical", "limited"], region)?.date
    : title.releaseDate;
  const home = dates.length
    ? milestone(dates, ["digital", "physical"], region)?.date
    : title.homeReleaseDate;
  if (!theatres && !home) return null;
  return (
    <section className="catalog-glance" aria-label={t("Release dates")}>
      <div>
        <span>{t("In theatres")}</span>
        <strong>{theatres ? shortDate(theatres, today) : t("Not announced")}</strong>
      </div>
      <div>
        <span>{t("At home")}</span>
        <strong>{home ? shortDate(home, today) : t("Not announced")}</strong>
        {title.homeReleaseAgeAdmitted && <p>{t("In cinemas over 120 days ago")}</p>}
      </div>
      <button type="button" onClick={onMore}>
        {t("All release dates")}
        <ArrowUpRight size={13} aria-hidden />
      </button>
    </section>
  );
}

function evidenceLine(code: string) {
  if (code === "playable" || code === "digital") return t("Ready to play");
  if (code === "theatrical") return t("In theatres only");
  if (code === "quality") return t("Hidden by your quality profile");
  if (code === "searching") return t("Waiting for a release");
  return "";
}

export function useReleaseEvidence(titleKey: string, type?: string) {
  return useAsync(async () => {
    if (type && type !== "movie") return null;
    try {
      return await apiFetch<{ code: string; checkedAt?: number; nextCheck?: number }>(
        `/availability/title/${encodeURIComponent(titleKey)}`,
      );
    } catch {
      return null;
    }
  }, [titleKey, type]);
}

export { evidenceLine };

/** Why a new movie is shown or still hidden. Missing evidence stays quiet. */
function ReleaseEvidence({ titleKey, type }: { titleKey: string; type?: string }) {
  const state = useReleaseEvidence(titleKey, type);
  const line = evidenceLine(state.data?.code ?? "");
  if (!line) return null;
  return (
    <section>
      <h3>{t("New release")}</h3>
      <p>{line}</p>
    </section>
  );
}
