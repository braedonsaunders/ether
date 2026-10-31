import {
  Avatar,
  Badge,
  Button,
  DetailPageLayout,
  EmptyState,
  Spinner,
  SubtabNav,
  toast,
} from "@braedonsaunders/appkit-ui";
import { ArrowLeft, CheckCircle2, History, LayoutTemplate, RefreshCw } from "lucide-react";
import { useCallback, useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { Artwork } from "../../components/Artwork";
import { Meter } from "../../components/charts";
import { ErrorPanel, LoadingRows } from "../../components/feedback";
import { titleSubtitle } from "../../components/Poster";
import { viewerName } from "../../components/pickers";
import { t, tn } from "../../i18n";
import SignInMethods from "../me/SignInMethods";
import { FavoritesMirror } from "../me/FavoritesMirror";
import { ConnectApps } from "../me/ConnectApps";
import { FavoriteEpisodeAlerts } from "../me/FavoriteEpisodeAlerts";
import { NotInterested, type NotInterestedEntry } from "../me/NotInterested";
import { getToken } from "../../lib/api";
import { api, apiFetch, errorMessage } from "../../lib/api";
import type { Viewer, ViewerDetail } from "../../lib/contract";
import { formatDateTime, formatNumber, formatPercent, relativeTime } from "../../lib/format";
import { useAsync } from "../../lib/hooks";
import { EMPTY_LAYOUT } from "../../lib/layout";
import { ContextBar, type ViewContext } from "../designer/ContextBar";
import { HomeCanvas } from "../designer/HomeCanvas";
import { ViewerWatchlist } from "./sections/ViewerWatchlist";
import { TasteView } from "./TasteView";
import { ViewerAccess } from "./ViewerAccess";
import { ViewerPlayback } from "./ViewerPlayback";
import { Membership } from "./ManageViewers";
import { PlexAccessNote, ViewerStatus, viewerStatus } from "./ViewerStatus";

const TABS = [
  "taste",
  "activity",
  "home",
  "access",
  "playback",
  "watchlist",
  "alerts",
  "sign-in",
] as const;
type Tab = (typeof TABS)[number];
function asTab(raw: string | undefined): Tab | undefined {
  return raw && (TABS as readonly string[]).includes(raw) ? (raw as Tab) : undefined;
}

export default function ViewerProfile() {
  const { id: raw, tab: routeTab } = useParams();
  const id = Number(raw);
  const tab: Tab = asTab(routeTab) ?? "taste";
  const navigate = useNavigate();

  const viewer = useAsync<ViewerDetail | null>(() => api.viewer(id), [id]);
  const taste = useAsync(() => api.taste(id), [id]);
  const [refreshing, setRefreshing] = useState(false);
  const loadFavorites = useCallback(
    () => apiFetch<{ enabled: boolean }>(`/viewers/${id}/favorites-mirror`),
    [id],
  );
  const saveFavorites = useCallback(
    (enabled: boolean) =>
      apiFetch(`/viewers/${id}/favorites-mirror`, {
        method: "POST",
        body: JSON.stringify({ enabled }),
      }),
    [id],
  );
  const loadAlerts = useCallback(
    () => apiFetch<{ enabled: boolean }>(`/viewers/${id}/favorite-episode-alert`),
    [id],
  );
  const loadHidden = useCallback(async (): Promise<NotInterestedEntry[]> => {
    const result = await apiFetch<{ items: { handle: string; title?: string }[] }>(
      `/viewers/${id}/not-interested`,
    );
    return (result.items ?? []).map((item) => ({
      handle: item.handle,
      title: item.title,
      unavailable: !item.title,
    }));
  }, [id]);
  const restoreHidden = useCallback(
    async (handle: string) => {
      const result = await apiFetch<{ restored: boolean }>(
        `/viewers/${id}/not-interested/restore`,
        { method: "POST", body: JSON.stringify({ handle }) },
      );
      if (!result.restored) throw new Error("restore failed");
    },
    [id],
  );

  const v = viewer.data;
  const setTab = (next: string) => navigate(`/viewers/${id}/${next}`);

  const refresh = async () => {
    setRefreshing(true);
    try {
      await api.refreshViewer(id);
      toast.success(t("Rows rebuilt"), { description: t("Their Plex Home is up to date.") });
    } catch (e) {
      toast.error(t("Couldn't rebuild rows"), { description: errorMessage(e) });
    } finally {
      setRefreshing(false);
    }
  };

  if (viewer.error)
    return (
      <div className="p-6">
        <BackLink />
        <ErrorPanel error={viewer.error} onRetry={() => void viewer.reload()} />
      </div>
    );
  if (!v)
    return (
      <div className="grid flex-1 place-items-center">
        <Spinner size={22} label={t("Loading viewer")} />
      </div>
    );
  const name = viewerName(v);

  return (
    <DetailPageLayout
      header={
        <div>
          <BackLink />
          <div className="mt-2 flex flex-wrap items-center gap-4">
            <Avatar src={v.thumb} name={name} size={56} />
            <div className="min-w-0 flex-1">
              <h1 className="flex flex-wrap items-center gap-2 text-xl font-semibold text-fg">
                {name} <ViewerStatus viewer={v} />
              </h1>
              <p className="text-sm text-fg-muted">
                {v.username} · {t("active {time}", { time: relativeTime(v.lastActive) })} ·{" "}
                {tn(
                  v.plays30d,
                  "{count} play and {hours} hours in 30 days",
                  "{count} plays and {hours} hours in 30 days",
                  { hours: formatNumber(v.hoursWatched30d, 1) },
                )}
              </p>
            </div>
            <div className="flex flex-wrap gap-2">
              <Button
                variant="outline"
                size="sm"
                onClick={() => void refresh()}
                disabled={refreshing}
              >
                <RefreshCw size={14} className={refreshing ? "animate-spin" : ""} />{" "}
                {t("Rebuild rows")}
              </Button>
              <Button variant="outline" size="sm" onClick={() => navigate("/home")}>
                <LayoutTemplate size={14} /> {t("Home")}
              </Button>
            </div>
          </div>
          <div className="mt-3">
            <PlexAccessNote viewer={v} />
          </div>
        </div>
      }
      subtabs={
        <SubtabNav
          ariaLabel={t("Viewer sections")}
          active={tab}
          onSelect={setTab}
          tabs={[
            { key: "taste", label: t("Taste") },
            { key: "activity", label: t("Activity"), count: v.recent?.length },
            { key: "home", label: t("Their Home") },
            { key: "access", label: t("Limits & preferences") },
            { key: "playback", label: t("Audio & subtitles") },
            { key: "watchlist", label: t("Watchlist") },
            { key: "alerts", label: t("Alerts") },
            { key: "sign-in", label: t("Sign-in methods & devices") },
          ]}
        />
      }
    >
      {tab === "taste" ? (
        taste.error ? (
          <ErrorPanel error={taste.error} onRetry={() => void taste.reload()} />
        ) : !taste.data ? (
          <LoadingRows rows={4} />
        ) : (
          <TasteView userId={id} taste={taste.data} onChange={(next) => taste.setData(next)} />
        )
      ) : tab === "activity" ? (
        <Activity viewer={v} />
      ) : tab === "home" ? (
        <TheirHome viewer={v} />
      ) : tab === "access" ? (
        <ViewerAccess key={id} viewer={v} />
      ) : tab === "playback" ? (
        <ViewerPlayback key={id} viewer={v} />
      ) : tab === "alerts" ? (
        <div className="space-y-6">
          <FavoriteEpisodeAlerts load={loadAlerts} />
          <NotInterested load={loadHidden} restore={restoreHidden} />
        </div>
      ) : tab === "sign-in" ? (
        <div className="space-y-6">
          {!v.isOwner ? (
            <Membership
              id={id}
              active={viewerStatus(v) === "active"}
              onChange={() => void viewer.reload()}
            />
          ) : null}
          <SignInMethods base={`/users/${id}`} token={getToken()} owner />
          <ConnectApps base={`/viewers/${id}`} token={getToken()} />
        </div>
      ) : (
        <div className="space-y-6">
          <FavoritesMirror load={loadFavorites} save={saveFavorites} />
          <ViewerWatchlist viewerId={id} />
        </div>
      )}
    </DetailPageLayout>
  );
}

function BackLink() {
  return (
    <Link
      to="/viewers"
      className="inline-flex items-center gap-1 text-sm text-fg-muted hover:text-fg"
    >
      <ArrowLeft size={14} /> {t("Viewers")}
    </Link>
  );
}

function Activity({ viewer }: { viewer: ViewerDetail }) {
  const recent = viewer.recent ?? [];
  if (recent.length === 0)
    return (
      <EmptyState
        icon={<History />}
        title={t("Nothing watched recently")}
        description={t("Plays, finishes and abandons appear here as they happen.")}
      />
    );
  return (
    <ul className="divide-y divide-border overflow-hidden rounded-xl border border-border bg-surface">
      {recent.map((r, i) => (
        <li key={`${r.title.key}-${i}`} className="flex items-center gap-4 p-3">
          <Artwork
            src={r.title.poster}
            alt=""
            title={r.title.title}
            className="aspect-[2/3] w-12 shrink-0 rounded"
          />
          <div className="min-w-0 flex-1">
            <p className="truncate text-sm font-medium text-fg">{r.title.title}</p>
            <p className="truncate text-xs text-fg-muted">{titleSubtitle(r.title)}</p>
            {!r.completed ? (
              <div className="mt-1.5 max-w-xs">
                <Meter
                  value={r.progress}
                  valueLabel={formatPercent(r.progress)}
                  label={t("Stopped at")}
                />
              </div>
            ) : null}
          </div>
          <div className="shrink-0 text-right">
            {r.completed ? (
              <Badge variant="success">
                <CheckCircle2 size={11} /> {t("Finished")}
              </Badge>
            ) : (
              <Badge variant="warning">{t("In progress")}</Badge>
            )}
            <p className="mt-1 text-xs text-fg-subtle" title={formatDateTime(r.at)}>
              {relativeTime(r.at)}
            </p>
          </div>
        </li>
      ))}
    </ul>
  );
}

function TheirHome({ viewer }: { viewer: Viewer }) {
  const now = new Date();
  const [ctx, setCtx] = useState<ViewContext>({
    device: "tv",
    day: now.getDay(),
    hour: now.getHours(),
    section: "home",
  });
  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="w-full max-w-md">
          <ContextBar ctx={ctx} onChange={setCtx} />
        </div>
        <Link to="/home" className="text-sm font-medium text-primary hover:underline">
          {t("Shape it in Home →")}
        </Link>
      </div>
      <div className="rounded-xl bg-bg p-2 sm:p-4">
        <HomeCanvas
          readOnly
          draft={EMPTY_LAYOUT}
          saved={EMPTY_LAYOUT}
          dirty={false}
          layer={viewer.id}
          layerName={viewerName(viewer)}
          ctx={ctx}
          viewers={[viewer]}
          viewerId={viewer.id}
          onSpotCheck={() => undefined}
          refreshNonce={0}
          onChange={() => undefined}
          onEditRow={() => undefined}
        />
      </div>
    </div>
  );
}
