import { DashboardPanel } from "@braedonsaunders/appkit-dashboard/primitives";
import {
  Alert,
  AlertDescription,
  AlertTitle,
  Badge,
  Button,
  EmptyState,
  Skeleton,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
  cn,
} from "@braedonsaunders/appkit-ui";
import {
  Activity,
  BarChart3,
  Film,
  HeartPulse,
  MonitorPlay,
  PlayCircle,
  RefreshCw,
  Table2,
  TrendingUp,
  X,
} from "lucide-react";
import { useMemo, useState, type ReactNode } from "react";
import { Link } from "react-router-dom";
import { useApp, useLiveDashboard } from "../app-context";
import { Artwork } from "../components/Artwork";
import { Meter, TimeBars } from "../components/charts";
import { ErrorPanel, LoadingRows } from "../components/feedback";
import { ServerPanel } from "../components/ServerPanel";
import { LevelBadge, StatusDot } from "../components/status";
import { getLocale, localized, t, tn, tx } from "../i18n";
import { api } from "../lib/api";
import type { ActivityDay, RowInsight, Viewer } from "../lib/contract";
import { formatNumber, formatPercent, formatRate, relativeTime } from "../lib/format";
import {
  countsOverall,
  hasExemptDiagnostic,
  normalizeSessions,
  type HealthItem,
  type NowPlaying,
} from "../lib/health";
import { useAsync } from "../lib/hooks";
import { useTitle } from "../lib/titles";
import type { EtherConfig } from "../lib/types";
import { useLocale } from "../i18n/react";
import { connectDevicePath } from "./setup/connect-path";

const GETTING_STARTED_KEY = "ether.getting-started.dismissed";

function sourceReady(cfg: EtherConfig): boolean {
  const debrid = (cfg.debrid?.providers ?? []).some(
    (provider) => provider.enabled && provider.apiKey.trim() !== "",
  );
  const usenet = (cfg.usenet?.providers ?? []).some(
    (provider) => provider.enabled && provider.host.trim() !== "",
  );
  return debrid || usenet;
}

function GettingStarted({ sessions }: { sessions: NowPlaying[] }) {
  const [dismissed, setDismissed] = useState(
    () => localStorage.getItem(GETTING_STARTED_KEY) === "1",
  );
  const config = useAsync(() => api.getConfig(), []);
  const viewers = useAsync(() => api.viewers(), []);
  if (dismissed) return null;
  const cfg = config.data;
  const people = viewers.data;
  if (!cfg || !people) return null;
  const apps = Object.entries(cfg.frontends ?? {})
    .filter(([, row]) => row?.enabled)
    .map(([id]) => id);
  const profile = cfg.quality?.default?.trim() || "1080p";
  const steps = [
    { id: "source", todo: !sourceReady(cfg), label: t("Add a source"), to: "/sources" },
    {
      id: "plex",
      todo: apps.includes("plex") && !(cfg.plex?.claimed || cfg.plex?.token),
      label: t("Connect Plex"),
      to: "/apps/plex/account",
    },
    {
      id: "device",
      todo:
        apps.length > 0 &&
        sessions.length === 0 &&
        !people.some((viewer: Viewer) => !viewer.isOwner && Boolean(viewer.lastActive)),
      label: t("Connect a device"),
      to: connectDevicePath(cfg.frontends),
    },
    {
      id: "viewers",
      todo: !people.some((viewer) => !viewer.isOwner),
      label: t("Add viewers"),
      to: "/viewers",
    },
    {
      id: "quality",
      todo: profile === "1080p",
      label: t("Playback quality: {profile} at home · change", { profile }),
      to: "/releases/quality",
    },
  ].filter((step) => step.todo);
  if (steps.length === 0) return <span hidden data-testid="getting-started-clear" />;
  return (
    <section
      aria-label={t("Next steps")}
      className="flex shrink-0 flex-wrap items-center gap-2 rounded-lg border border-border bg-surface py-1.5 pl-3 pr-1.5"
    >
      <span className="text-xs font-medium text-fg-muted">{t("Next steps")}</span>
      {steps.map((step) => (
        <Link
          key={step.id}
          to={step.to}
          className="rounded-full border border-border px-3 py-1 text-xs font-medium text-fg transition hover:border-primary hover:text-primary focus:outline-none focus-visible:ring-2 focus-visible:ring-accent"
        >
          {step.label}
        </Link>
      ))}
      <Button
        variant="ghost"
        size="icon"
        className="ml-auto size-7"
        aria-label={t("Dismiss")}
        onClick={() => {
          localStorage.setItem(GETTING_STARTED_KEY, "1");
          setDismissed(true);
        }}
      >
        <X size={14} aria-hidden />
      </Button>
    </section>
  );
}

function greeting(now = new Date()): string {
  const h = now.getHours();
  return h < 5
    ? t("Good night")
    : h < 12
      ? t("Good morning")
      : h < 17
        ? t("Good afternoon")
        : t("Good evening");
}

/**
 * The Overview is a cockpit: on a wide, tall enough window every panel is on
 * one screen with no page scroll (panels scroll inside); narrower windows
 * stack the panels and scroll the page.
 */
export default function Home() {
  useLiveDashboard();
  const { boot, dashboard, dashboardError, refreshDashboard, health } = useApp();
  const activity = useAsync((_s) => api.activity(), []);
  const rows = useAsync((_s) => api.rowInsights(), []);
  const { locale } = useLocale();
  const sessions = useMemo(
    () => normalizeSessions(dashboard?.sections.sessions),
    [dashboard, locale],
  );
  const hw = dashboard?.sections.hwaccel as
    { hwaccel?: { name?: string }; ffmpeg?: string; error?: string } | undefined;
  const problems = health.filter((h) => !h.setupGap && (h.level === "error" || h.level === "warn"));
  const setupGaps = health.filter((h) => h.setupGap);

  const status = !dashboard
    ? null
    : problems.length
      ? tn(problems.length, "{count} thing needs attention", "{count} things need attention")
      : setupGaps.length
        ? tn(setupGaps.length, "{count} thing to set up", "{count} things to set up")
        : health.some((h) => h.level === "unknown" && countsOverall(h))
          ? t("Status incomplete")
          : hasExemptDiagnostic(health)
            ? t("Operational")
            : t("Everything is healthy");

  return (
    <div className="@container flex min-h-0 flex-1 flex-col">
      <div className="app-scroll min-h-0 flex-1 overflow-y-auto cockpit:overflow-hidden">
        <div className="reveal mx-auto flex w-full max-w-(--breakpoint-2xl) flex-col gap-4 p-4 sm:p-6 cockpit:h-full cockpit:max-w-none cockpit:min-h-0 cockpit:pb-5">
          <header className="flex shrink-0 flex-wrap items-end justify-between gap-3">
            <div className="min-w-0">
              <h1 className="text-xl font-semibold text-fg">{t("Overview")}</h1>
              <p className="truncate text-sm text-fg-muted">
                {t("{greeting}. {server}", {
                  greeting: greeting(),
                  server: boot.serverName || "Ether",
                })}
                {status ? ` · ${status}` : ""}
                {sessions.length
                  ? ` · ${tn(sessions.length, "{count} stream live", "{count} streams live")}`
                  : ""}
              </p>
            </div>
            <Button
              variant="outline"
              size="sm"
              onClick={() => {
                void refreshDashboard();
                void activity.reload();
                void rows.reload();
              }}
            >
              <RefreshCw size={14} /> {t("Refresh")}
            </Button>
          </header>

          {!boot.setupComplete ? (
            <Alert variant="warning" className="shrink-0">
              <AlertTitle>{t("Setup isn't finished")}</AlertTitle>
              <AlertDescription>
                {tx("Apps can't find this server until setup is complete. {link}", {
                  link: (
                    <Link className="font-medium underline" to="/setup">
                      {t("Continue setup")}
                    </Link>
                  ),
                })}
              </AlertDescription>
            </Alert>
          ) : (
            <GettingStarted sessions={sessions} />
          )}
          {dashboardError && !dashboard ? (
            <div className="shrink-0">
              <ErrorPanel error={dashboardError} onRetry={() => void refreshDashboard()} />
            </div>
          ) : null}

          <div className="grid gap-4 @3xl:grid-cols-2 cockpit:min-h-0 cockpit:flex-1 cockpit:grid-cols-12">
            {/* Each cockpit column is its own stack: the upper panel sizes to
                its content (up to a cap) and the lower one takes the rest. */}
            <div className="flex min-w-0 flex-col gap-4 @3xl:col-span-2 cockpit:col-span-5 cockpit:min-h-0">
              <NowPlayingPanel
                loading={!dashboard}
                sessions={sessions}
                className="shrink-0 cockpit:max-h-[58%]"
              />
              <ActivityPanel state={activity} className="cockpit:min-h-0 cockpit:flex-1" />
            </div>
            <ServerPanel hwaccel={hw?.hwaccel?.name} className="cockpit:col-span-4" />
            <div className="flex min-w-0 flex-col gap-4 cockpit:col-span-3 cockpit:min-h-0">
              <HealthPanel
                loading={!dashboard}
                items={health}
                className="shrink-0 cockpit:max-h-[68%]"
              />
              <RowsPanel state={rows} className="cockpit:min-h-0 cockpit:flex-1" />
            </div>
          </div>
        </div>
      </div>
    </div>
  );
}

/** Lists inside a fixed cockpit cell scroll on their own. */
const scrollBody = "cockpit:overflow-y-auto";

function NowPlayingPanel({
  loading,
  sessions,
  className,
}: {
  loading: boolean;
  sessions: NowPlaying[];
  className?: string;
}) {
  const total = sessions.reduce((n, s) => n + s.speedBps, 0);
  return (
    <DashboardPanel
      title={t("Now playing")}
      icon={<PlayCircle size={16} />}
      className={className}
      bodyClassName={scrollBody}
      actions={
        <span className="flex items-center gap-2">
          {sessions.length ? (
            <Badge variant="info">
              {tn(sessions.length, "{count} stream", "{count} streams")}
              {total > 0 ? ` · ${formatRate(total)}` : ""}
            </Badge>
          ) : null}
          <Link to="/activity/now" className="text-xs font-medium text-primary hover:underline">
            {t("Activity")}
          </Link>
        </span>
      }
    >
      {loading ? (
        <LoadingRows rows={2} />
      ) : sessions.length === 0 ? (
        <EmptyState
          icon={<MonitorPlay />}
          title={t("Nothing playing")}
          description={t("Streams appear here when someone presses play.")}
          className="py-6"
        />
      ) : (
        <ul className="space-y-3">
          {sessions.map((s) => (
            <SessionCard key={s.id || s.key} s={s} />
          ))}
        </ul>
      )}
    </DashboardPanel>
  );
}

function SessionCard({ s }: { s: NowPlaying }) {
  const title = useTitle(s.key);
  return (
    <li className="relative overflow-hidden rounded-lg border border-border">
      <Artwork src={title?.backdrop} alt="" title="" className="absolute inset-0" icon={<span />} />
      <div className="scrim-left absolute inset-0" />
      <div className="relative flex gap-3 p-3">
        <Artwork
          src={title?.poster}
          alt={s.title}
          title={s.title}
          className="aspect-[2/3] w-14 shrink-0 rounded-md shadow-md sm:w-16"
        />
        <div className="min-w-0 flex-1 text-on-scrim">
          <p className="truncate font-semibold">{title?.title ?? s.title}</p>
          <p className="truncate text-sm opacity-80">
            {s.user} · {s.device}
            {s.product && s.product !== s.device ? ` (${s.product})` : ""}
          </p>
          <div className="mt-1.5 flex flex-wrap gap-1.5">
            <Badge variant={s.decision === "transcode" ? "warning" : "success"}>
              {s.decision === "transcode"
                ? t("Transcoding")
                : s.decision === "copy"
                  ? t("Direct stream")
                  : s.decision
                    ? t("Direct play")
                    : t("Starting")}
            </Badge>
            {s.origin ? <Badge variant="secondary">{s.origin}</Badge> : null}
            {s.remote ? <Badge variant="info">{t("Remote")}</Badge> : null}
            {s.failovers ? (
              <Badge variant="warning">
                {tn(s.failovers, "{count} failover", "{count} failovers")}
              </Badge>
            ) : null}
            <Badge variant="secondary">{formatRate(s.speedBps)}</Badge>
          </div>
          <div className="mt-2 max-w-md">
            {s.progress !== null ? (
              <Meter
                onScrim
                value={s.progress}
                label={t("Started {time}", { time: relativeTime(s.startedAt) })}
                valueLabel={t("{percent} watched", { percent: formatPercent(s.progress) })}
              />
            ) : (
              <p className="text-xs opacity-80">{t("Waiting for playback position")}</p>
            )}
          </div>
          {s.decisionReason ? (
            <p className="mt-1.5 text-xs opacity-80">{s.decisionReason}</p>
          ) : null}
          {s.release ? (
            <p className="mt-1.5 truncate font-mono text-[11px] opacity-70" title={s.release}>
              {s.release}
            </p>
          ) : null}
        </div>
      </div>
    </li>
  );
}

/** Health: anything that needs attention first, each one link from its fix. */
function HealthPanel({
  loading,
  items,
  className,
}: {
  loading: boolean;
  items: HealthItem[];
  className?: string;
}) {
  const problems = items.filter((h) => !h.setupGap && (h.level === "error" || h.level === "warn"));
  const setupGaps = items.filter((h) => h.setupGap);
  const rest = items.filter((h) => !problems.includes(h) && !h.setupGap);
  const severe = problems.some((h) => h.level === "error");
  return (
    <DashboardPanel
      title={t("Health")}
      icon={<HeartPulse size={16} />}
      className={className}
      bodyClassName={cn("px-3 py-2", scrollBody)}
      actions={null}
    >
      {loading ? (
        <LoadingRows rows={4} />
      ) : (
        <>
          <div className="mb-2">
            {problems.length ? (
              <Badge variant={severe ? "destructive" : "warning"} className="whitespace-nowrap">
                {tn(problems.length, "{count} to fix", "{count} to fix")}
              </Badge>
            ) : setupGaps.length ? (
              <Badge variant="secondary" className="whitespace-nowrap">
                {tn(setupGaps.length, "{count} thing to set up", "{count} things to set up")}
              </Badge>
            ) : items.some((h) => h.level === "unknown" && countsOverall(h)) ? (
              <LevelBadge level="unknown" label={t("Status incomplete")} />
            ) : hasExemptDiagnostic(items) ? (
              <LevelBadge level="ok" label={t("Operational")} />
            ) : (
              <LevelBadge level="ok" label={t("All healthy")} />
            )}
          </div>
          {problems.length ? (
            <section
              role="region"
              aria-label={t("Needs attention")}
              className={cn(
                "mb-2 rounded-lg p-1",
                severe ? "bg-danger-subtle" : "bg-warning-subtle",
              )}
            >
              <ul className="space-y-0.5">
                {problems.map((h) => (
                  <HealthRow key={h.id} h={h} fix />
                ))}
              </ul>
            </section>
          ) : null}
          {setupGaps.length ? (
            <ul className="mb-2 space-y-0.5">
              {setupGaps.map((h) => (
                <HealthRow key={h.id} h={h} />
              ))}
            </ul>
          ) : null}
          <ul className="space-y-0.5">
            {rest.map((h) => (
              <HealthRow key={h.id} h={h} />
            ))}
          </ul>
        </>
      )}
    </DashboardPanel>
  );
}

const LEVEL_TEXT: Record<HealthItem["level"], string> = localized(() => ({
  ok: t("Healthy"),
  warn: t("Degraded"),
  error: t("Failing"),
  off: t("Off"),
  unknown: t("Unknown"),
}));

const LEVEL_TONE: Record<HealthItem["level"], string> = {
  ok: "text-success",
  warn: "text-warning",
  error: "text-danger",
  off: "text-fg-subtle",
  unknown: "text-fg-subtle",
};

function HealthRow({ h, fix = false }: { h: HealthItem; fix?: boolean }) {
  return (
    <li className="relative flex items-start gap-2.5 rounded-md px-2 py-1.5 hover:bg-surface-hover">
      <StatusDot level={h.level} pulse={fix} className="mt-1.5" />
      <span className="min-w-0 flex-1" title={h.details.join("\n") || undefined}>
        <span className="flex items-center gap-2">
          <Link
            to={h.href}
            className="shrink-0 text-sm font-medium text-fg after:absolute after:inset-0 focus-visible:outline-none focus-visible:after:rounded-md focus-visible:after:ring-2 focus-visible:after:ring-ring/50"
          >
            {h.label}
          </Link>
          <span className="min-w-0 flex-1">
            {h.fill !== undefined ? (
              <Meter
                value={h.fill}
                tone={h.fill > 0.95 && h.id !== "cache" ? "warning" : "primary"}
              />
            ) : null}
          </span>
          {fix ? (
            <Link
              to={h.href}
              className="relative z-10 shrink-0 text-xs font-medium text-primary hover:underline"
            >
              {t("Fix")}
            </Link>
          ) : (
            <span className={cn("shrink-0 text-xs font-medium", LEVEL_TONE[h.level])}>
              {h.statusLabel ?? LEVEL_TEXT[h.level]}
            </span>
          )}
        </span>
        <span className="block text-pretty text-xs text-fg-muted">{h.summary}</span>
        {h.note ? <span className="mt-1 block text-xs text-fg-subtle">{h.note}</span> : null}
      </span>
    </li>
  );
}

/** A headline figure inside a panel: label, value, one line of context. */
function Readout({ label, value, detail }: { label: string; value: ReactNode; detail: ReactNode }) {
  return (
    <div className="min-w-0 border-l border-border-subtle pl-3 first:border-l-0 first:pl-0">
      <dt className="truncate text-[11px] font-medium uppercase tracking-wide text-fg-subtle">
        {label}
      </dt>
      <dd className="truncate text-xl font-semibold text-fg">{value}</dd>
      <dd className="truncate text-xs text-fg-muted">{detail}</dd>
    </div>
  );
}

function ActivityPanel({
  state,
  className,
}: {
  state: ReturnType<typeof useAsync<ActivityDay[]>>;
  className?: string;
}) {
  const [metric, setMetric] = useState<"plays" | "hours">("plays");
  const [asTable, setAsTable] = useState(false);
  const days = state.data ?? [];
  const totals = useMemo(() => {
    const plays = days.reduce((n, x) => n + x.plays, 0);
    const hours = days.reduce((n, x) => n + x.hours, 0);
    const peakViewers = days.reduce((n, x) => Math.max(n, x.uniqueViewers), 0);
    const half = Math.floor(days.length / 2);
    const recent = days.slice(half).reduce((n, x) => n + x.plays, 0);
    const earlier = days.slice(0, half).reduce((n, x) => n + x.plays, 0);
    return { plays, hours, peakViewers, change: earlier > 0 ? (recent - earlier) / earlier : null };
  }, [days]);
  const data = days.map((d) => ({
    key: d.date,
    label: new Date(`${d.date}T12:00:00`).toLocaleDateString(getLocale(), {
      month: "short",
      day: "numeric",
    }),
    value: d[metric],
    detail:
      metric === "plays"
        ? `${t("{hours} h", { hours: formatNumber(d.hours, 1) })} · ${tn(d.uniqueViewers, "{count} viewer", "{count} viewers")}`
        : `${tn(d.plays, "{count} play", "{count} plays")} · ${tn(d.uniqueViewers, "{count} viewer", "{count} viewers")}`,
  }));
  const na = state.error ? "—" : undefined;
  return (
    <DashboardPanel
      title={t("Activity · last 30 days")}
      icon={<BarChart3 size={16} />}
      className={className}
      bodyClassName="flex min-h-0 flex-col gap-3"
      actions={
        <div className="flex items-center gap-1">
          <div
            role="radiogroup"
            aria-label={t("Metric")}
            className="inline-flex rounded-md border border-border bg-bg-subtle p-0.5"
          >
            {(["plays", "hours"] as const).map((m) => (
              <button
                key={m}
                type="button"
                role="radio"
                aria-checked={metric === m}
                onClick={() => setMetric(m)}
                className={`rounded px-2 py-0.5 text-xs font-medium ${metric === m ? "bg-surface text-fg shadow-sm" : "text-fg-muted hover:text-fg"}`}
              >
                {m === "plays" ? t("Plays") : t("Hours")}
              </button>
            ))}
          </div>
          <Button
            variant="ghost"
            size="sm"
            className="h-7 px-2"
            onClick={() => setAsTable((on) => !on)}
            aria-pressed={asTable}
            title={asTable ? t("Show chart") : t("Show as table")}
          >
            {asTable ? <BarChart3 size={14} /> : <Table2 size={14} />}
            <span className="sr-only">{asTable ? t("Show chart") : t("Show as table")}</span>
          </Button>
        </div>
      }
    >
      {state.loading && !state.data ? (
        <Skeleton className="h-full min-h-40 w-full" />
      ) : (
        <>
          <dl className="grid shrink-0 grid-cols-3 gap-3">
            <Readout
              label={t("Plays · 30 days")}
              value={na ?? formatNumber(totals.plays, 0)}
              detail={
                totals.change === null
                  ? t("Across every viewer")
                  : t("{change} vs. prior", {
                      change: `${totals.change >= 0 ? "+" : "−"}${formatPercent(Math.abs(totals.change))}`,
                    })
              }
            />
            <Readout
              label={t("Hours watched")}
              value={na ?? formatNumber(totals.hours, 0)}
              detail={t("Last 30 days")}
            />
            <Readout
              label={t("Busiest day")}
              value={na ?? tn(totals.peakViewers, "{count} viewer", "{count} viewers")}
              detail={t("Peak in one day")}
            />
          </dl>
          {state.error ? (
            <ErrorPanel error={state.error} onRetry={() => void state.reload()} />
          ) : days.length === 0 ? (
            <EmptyState
              icon={<Activity />}
              title={t("No plays yet")}
              description={t("Activity appears after the first title is watched.")}
              className="py-6"
            />
          ) : asTable ? (
            <div className="max-h-56 min-h-0 overflow-auto cockpit:max-h-none cockpit:flex-1">
              <Table containerClassName="border-0 shadow-none">
                <TableHeader>
                  <TableRow>
                    <TableHead>{t("Day")}</TableHead>
                    <TableHead className="text-right">{t("Plays")}</TableHead>
                    <TableHead className="text-right">{t("Hours")}</TableHead>
                    <TableHead className="text-right">{t("Viewers")}</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {[...days].reverse().map((d) => (
                    <TableRow key={d.date}>
                      <TableCell>{d.date}</TableCell>
                      <TableCell className="text-right tabular-nums">{d.plays}</TableCell>
                      <TableCell className="text-right tabular-nums">
                        {formatNumber(d.hours, 1)}
                      </TableCell>
                      <TableCell className="text-right tabular-nums">{d.uniqueViewers}</TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            </div>
          ) : (
            <TimeBars
              fill
              className="h-44 cockpit:h-auto cockpit:flex-1"
              data={data}
              ariaLabel={
                metric === "plays"
                  ? t("Plays per day over the last 30 days")
                  : t("Hours watched per day over the last 30 days")
              }
              valueFormat={(v) => formatNumber(v, metric === "hours" ? 1 : 0)}
            />
          )}
        </>
      )}
    </DashboardPanel>
  );
}

function RowsPanel({
  state,
  className,
}: {
  state: ReturnType<typeof useAsync<RowInsight[]>>;
  className?: string;
}) {
  const rows = [...(state.data ?? [])].sort((a, b) => b.playRate - a.playRate).slice(0, 6);
  const maxRate = Math.max(0.0001, ...rows.map((r) => r.playRate));
  return (
    <DashboardPanel
      title={t("Best rows")}
      icon={<TrendingUp size={16} />}
      className={className}
      bodyClassName={scrollBody}
      actions={
        <Link to="/home" className="text-xs font-medium text-primary hover:underline">
          {t("Home")}
        </Link>
      }
    >
      {state.error ? (
        <ErrorPanel error={state.error} onRetry={() => void state.reload()} />
      ) : state.loading && !state.data ? (
        <LoadingRows rows={4} />
      ) : rows.length === 0 ? (
        <EmptyState
          icon={<Film />}
          title={t("No row data yet")}
          description={t(
            "No plays yet. The library stays empty until a TMDB key syncs the catalogue, and a debrid account or a Usenet provider can play it.",
          )}
          className="py-6"
        />
      ) : (
        <ol className="space-y-2.5">
          {rows.map((r, i) => (
            <li
              key={r.hubId}
              title={tn(
                r.plays7d,
                "{count} play from {impressions} impressions in 7 days",
                "{count} plays from {impressions} impressions in 7 days",
                { impressions: formatNumber(r.impressions7d, 0) },
              )}
            >
              <div className="mb-1 flex items-baseline justify-between gap-2 text-sm">
                <span className="min-w-0 truncate text-fg">
                  <span className="mr-1.5 tabular-nums text-fg-subtle">{i + 1}</span>
                  {r.title}
                </span>
                <span className="shrink-0 tabular-nums text-fg-muted">
                  {formatPercent(r.playRate, 1)}
                </span>
              </div>
              <div className="h-1.5 rounded-full bg-bg-subtle">
                <div
                  className="h-full rounded-full bg-primary"
                  style={{ width: `${(r.playRate / maxRate) * 100}%` }}
                />
              </div>
              <p className="mt-0.5 text-[11px] text-fg-subtle">
                {tn(r.plays7d, "{count} play", "{count} plays")} ·{" "}
                {tn(r.impressions7d, "{count} impression", "{count} impressions")}
              </p>
            </li>
          ))}
        </ol>
      )}
    </DashboardPanel>
  );
}
