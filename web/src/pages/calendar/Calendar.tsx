import { Button, Popover, buttonVariants, cn } from "@braedonsaunders/appkit-ui";
import {
  CalendarDays,
  ChevronLeft,
  ChevronRight,
  Clapperboard,
  Film,
  Keyboard,
  Layers,
  ListVideo,
  Sparkles,
  Tv,
  UserRound,
  Users,
} from "lucide-react";
import { useCallback, useEffect, useMemo, useState, type ReactNode } from "react";
import { Link, useNavigate, useParams, useSearchParams } from "react-router-dom";
import { Segmented } from "../../components/fields";
import { ErrorPanel } from "../../components/feedback";
import { ToolbarPicker } from "../../components/ToolbarPicker";
import { Workspace } from "../../components/workspace";
import { t, tn } from "../../i18n";
import { useAsync, useInterval } from "../../lib/hooks";
import { TitleInspector } from "../catalog/TitleInspector";
import { AgendaView } from "./AgendaView";
import { DayList } from "./DayList";
import { MonthView } from "./MonthView";
import { EmptyNote, StateDot } from "./parts";
import { Spotlight } from "./Spotlight";
import { WeekView } from "./WeekView";
import {
  STATE_META,
  addDays,
  calendarApi,
  groupByDay,
  localToday,
  periodRange,
  periodTitle,
  shiftAnchor,
  weekStartDay,
  type CalendarEntry,
  type CalendarViewer,
  type EntryState,
  type Filters,
  type MoviePolicy,
  type Types,
  type View,
} from "./model";

const VIEWS: View[] = ["month", "week", "agenda"];
const DATE = /^\d{4}-\d{2}-\d{2}$/;

function useWide(query = "(min-width: 640px)"): boolean {
  const get = () =>
    typeof window.matchMedia === "function" ? window.matchMedia(query).matches : true;
  const [wide, setWide] = useState(get);
  useEffect(() => {
    if (typeof window.matchMedia !== "function") return;
    const mq = window.matchMedia(query);
    const on = () => setWide(mq.matches);
    mq.addEventListener("change", on);
    return () => mq.removeEventListener("change", on);
  }, [query]);
  return wide;
}

/**
 * The release calendar: what the household follows, assembled by the server
 * from watch history, watchlists and Look Ahead collections, in month, week
 * and agenda views. The view lives in the path, the period and filters in
 * the query, so every calendar is a link.
 */
export default function Calendar() {
  const { view: rawView } = useParams();
  const navigate = useNavigate();
  const [params, setParams] = useSearchParams();
  const wide = useWide();
  const roomy = useWide("(min-width: 1280px)");
  const view: View = VIEWS.includes(rawView as View)
    ? (rawView as View)
    : wide
      ? "month"
      : "agenda";

  const [today, setToday] = useState(localToday);
  useInterval(() => setToday(localToday()), 60_000);

  const rawDate = params.get("date") ?? "";
  const anchor = DATE.test(rawDate) ? rawDate : today;
  const filters: Filters = {
    viewer: params.get("viewer") ?? "all",
    types: (["movie", "show"].includes(params.get("types") ?? "")
      ? params.get("types")
      : "all") as Types,
    movies: (["popular", "watchlist", "hidden"].includes(params.get("movies") ?? "")
      ? params.get("movies")
      : "foryou") as MoviePolicy,
  };
  const titleKey = params.get("title");

  const first = weekStartDay();
  const { from, to } = periodRange(view, anchor, first);

  const update = useCallback(
    (patch: Record<string, string | null>) => {
      setParams(
        (p) => {
          const next = new URLSearchParams(p);
          for (const [k, v] of Object.entries(patch)) {
            if (v === null || v === "") next.delete(k);
            else next.set(k, v);
          }
          return next;
        },
        { replace: "title" in patch ? false : true },
      );
    },
    [setParams],
  );

  const goView = useCallback(
    (v: View) => navigate({ pathname: `/calendar/${v}`, search: params.toString() }),
    [navigate, params],
  );
  const goDate = useCallback(
    (d: string) => update({ date: d === today ? null : d }),
    [update, today],
  );
  const step = useCallback(
    (dir: 1 | -1) => goDate(shiftAnchor(view, anchor, dir)),
    [goDate, view, anchor],
  );

  const state = useAsync(
    (signal) => calendarApi.load(from, to, filters, signal),
    [from, to, filters.viewer, filters.types, filters.movies],
  );
  const upNext = useAsync(
    (signal) => calendarApi.load(today, addDays(today, 13), filters, signal),
    [today, filters.viewer, filters.types, filters.movies],
  );
  const data = state.data;

  const viewers = useMemo(
    () =>
      new Map<number, CalendarViewer>(
        (data?.viewers ?? upNext.data?.viewers ?? []).map((v) => [v.id, v]),
      ),
    [data, upNext.data],
  );
  const byDay = useMemo(() => groupByDay(data?.entries ?? []), [data]);

  // Month keyboard focus: the anchor's day, or today when it is in view.
  const [focusDay, setFocusDay] = useState(anchor);
  const [selectedDay, setSelectedDay] = useState<string | null>(null);
  useEffect(() => {
    setFocusDay((d) => (d >= from && d <= to ? d : anchor >= from && anchor <= to ? anchor : from));
    setSelectedDay(null);
  }, [from, to, anchor]);

  const open = useCallback((e: CalendarEntry) => update({ title: e.open }), [update]);

  // Google-Calendar-style shortcuts.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.defaultPrevented || e.metaKey || e.ctrlKey || e.altKey || titleKey) return;
      const el = e.target as HTMLElement | null;
      if (el && (el.isContentEditable || /^(INPUT|TEXTAREA|SELECT)$/.test(el.tagName))) return;
      if (document.querySelector("[role=dialog]")) return;
      const k = e.key.toLowerCase();
      if (k === "j" || k === "n" || e.key === "PageDown") step(1);
      else if (k === "k" || k === "p" || e.key === "PageUp") step(-1);
      else if (k === "t") goDate(today);
      else if (k === "m") goView("month");
      else if (k === "w") goView("week");
      else if (k === "a") goView("agenda");
      else return;
      e.preventDefault();
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [step, goDate, goView, today, titleKey]);

  const viewerName =
    filters.viewer !== "all" ? viewers.get(Number(filters.viewer))?.name : undefined;
  const subtitle = data
    ? viewerName
      ? tn(data.shows, "{name} follows {count} show", "{name} follows {count} shows", {
          name: viewerName,
        })
      : tn(
          data.shows,
          "Built from {count} show the household follows, plus movie releases",
          "Built from {count} shows the household follows, plus movie releases",
        )
    : undefined;

  const isCurrent = anchor === today;
  const emptyHousehold = data && data.entries.length === 0 && data.shows === 0 && !data.warming;

  let body: ReactNode;
  if (state.error && !data) {
    body = (
      <div className="p-6">
        <ErrorPanel error={state.error} onRetry={() => void state.reload()} />
      </div>
    );
  } else if (!data) {
    body = <Skeleton view={view} />;
  } else if (emptyHousehold && filters.viewer === "all" && filters.types === "all") {
    body = (
      <EmptyNote icon={<CalendarDays size={26} />} title={t("Nothing on the calendar yet")}>
        <div className="flex flex-wrap justify-center gap-2">
          <Link
            to="/collections/new"
            className={buttonVariants({ variant: "outline", size: "sm" })}
          >
            <Layers size={14} /> {t("Create a Look Ahead collection")}
          </Link>
          <Link to="/viewers" className={buttonVariants({ variant: "ghost", size: "sm" })}>
            <Users size={14} /> {t("See viewers")}
          </Link>
        </div>
      </EmptyNote>
    );
  } else if (view === "month") {
    body = (
      <>
        <MonthView
          from={from}
          month={anchor.slice(0, 7)}
          byDay={byDay}
          today={today}
          viewers={viewers}
          focusDay={focusDay}
          onFocusDay={(d, o) => {
            setFocusDay(d);
            if (o?.focus && (d < from || d > to)) goDate(d);
          }}
          selectedDay={selectedDay}
          onSelectDay={setSelectedDay}
          onOpen={open}
          wide={wide}
        />
        {!wide ? (
          <div className="border-t border-border bg-surface">
            <DayList
              day={selectedDay ?? focusDay}
              entries={byDay.get(selectedDay ?? focusDay) ?? []}
              today={today}
              viewers={viewers}
              onOpen={open}
            />
          </div>
        ) : null}
        <Legend />
      </>
    );
  } else if (view === "week") {
    body = <WeekView from={from} byDay={byDay} today={today} viewers={viewers} onOpen={open} />;
  } else {
    body = (
      <AgendaView from={from} to={to} byDay={byDay} today={today} viewers={viewers} onOpen={open} />
    );
  }

  const viewerOptions = [
    {
      value: "all",
      label: t("Everyone"),
      icon: <Users size={14} />,
      hint: t("The whole household"),
    },
    ...(data?.viewers ?? upNext.data?.viewers ?? []).map((v) => ({
      value: String(v.id),
      label: v.name,
      icon: <UserRound size={14} />,
      hint: v.owner ? t("Owner") : undefined,
    })),
  ];

  return (
    <Workspace
      title={t("Calendar")}
      subtitle={subtitle}
      actions={
        <>
          <Segmented
            ariaLabel={t("Calendar view")}
            size="sm"
            value={view}
            onChange={goView}
            options={[
              { value: "month", label: t("Month") },
              { value: "week", label: t("Week") },
              { value: "agenda", label: t("Agenda") },
            ]}
          />
          <ShortcutHelp />
        </>
      }
    >
      <div className="flex min-h-0 flex-1">
        <div className="app-scroll flex min-h-0 min-w-0 flex-1 flex-col overflow-y-auto bg-bg">
          {upNext.data && !emptyHousehold && !roomy ? (
            <Spotlight
              entries={upNext.data.entries}
              today={today}
              viewers={viewers}
              onOpen={open}
            />
          ) : null}
          <div className="sticky top-0 z-20 flex shrink-0 flex-wrap items-center gap-2 border-b border-border bg-surface/95 px-3 py-2 backdrop-blur sm:px-5">
            <div className="mr-auto flex min-w-0 items-center gap-1">
              <Button variant="ghost" size="sm" aria-label={t("Previous")} onClick={() => step(-1)}>
                <ChevronLeft size={16} />
              </Button>
              <Button
                variant="outline"
                size="sm"
                onClick={() => goDate(today)}
                disabled={isCurrent}
              >
                {t("Today")}
              </Button>
              <Button variant="ghost" size="sm" aria-label={t("Next")} onClick={() => step(1)}>
                <ChevronRight size={16} />
              </Button>
              <h2
                className="ml-1.5 truncate text-base font-semibold tracking-tight text-fg"
                aria-live="polite"
              >
                {periodTitle(view, anchor, first)}
              </h2>
              {state.loading && data ? (
                <span
                  className="ml-2 inline-flex size-3.5 animate-spin rounded-full border-2 border-primary/30 border-t-primary"
                  role="status"
                  aria-label={t("Updating…")}
                />
              ) : null}
            </div>
            <ToolbarPicker
              ariaLabel={t("Whose calendar")}
              value={filters.viewer}
              onChange={(v) => update({ viewer: v === "all" ? null : v })}
              options={viewerOptions}
              iconOnly={!wide}
              menuClassName="w-56"
            />
            <ToolbarPicker
              ariaLabel={t("Show")}
              value={filters.types}
              onChange={(v) => update({ types: v === "all" ? null : v })}
              options={[
                { value: "all", label: t("TV & movies"), icon: <CalendarDays size={14} /> },
                { value: "show", label: t("TV only"), icon: <Tv size={14} /> },
                { value: "movie", label: t("Movies only"), icon: <Film size={14} /> },
              ]}
              iconOnly={!wide}
              menuClassName="w-48"
            />
            {filters.types !== "show" ? (
              <ToolbarPicker
                ariaLabel={t("Which movies")}
                value={filters.movies}
                onChange={(v) => update({ movies: v === "foryou" ? null : v })}
                options={[
                  {
                    value: "foryou",
                    label: t("Movies for you"),
                    icon: <Sparkles size={14} />,
                    hint: t(
                      "Watchlists, Look Ahead collections and releases that match the household's taste",
                    ),
                  },
                  {
                    value: "popular",
                    label: t("Popular movies"),
                    icon: <Clapperboard size={14} />,
                    hint: t("The biggest releases, whatever anyone watches"),
                  },
                  {
                    value: "watchlist",
                    label: t("Watchlisted movies"),
                    icon: <ListVideo size={14} />,
                    hint: t("Only movies someone has on a watchlist"),
                  },
                  {
                    value: "hidden",
                    label: t("No movies"),
                    icon: <Film size={14} />,
                    hint: t("Episodes only"),
                  },
                ]}
                iconOnly={!wide}
                menuClassName="w-80"
              />
            ) : null}
          </div>
          {data?.warming ? (
            <p
              className="shrink-0 border-b border-border bg-info/10 px-3 py-2 text-xs text-info sm:px-5"
              role="status"
            >
              {t(
                "Ether is fetching upcoming episodes for the shows your household follows. New dates appear within a few minutes.",
              )}
            </p>
          ) : null}
          {data?.truncated ? (
            <p
              className="shrink-0 border-b border-border bg-warning/10 px-3 py-2 text-xs text-warning sm:px-5"
              role="status"
            >
              {t(
                "This period has more releases than the calendar shows at once. Narrow it with the filters above.",
              )}
            </p>
          ) : null}
          <div
            className={cn(
              "flex flex-1 flex-col transition-opacity",
              state.loading && data ? "opacity-70" : "",
            )}
          >
            {body}
          </div>
        </div>
        {roomy && upNext.data && !emptyHousehold ? (
          <aside
            aria-label={t("Up next")}
            className="app-scroll w-80 shrink-0 overflow-y-auto border-l border-border bg-bg-subtle"
          >
            <Spotlight
              entries={upNext.data.entries}
              today={today}
              viewers={viewers}
              onOpen={open}
              rail
            />
          </aside>
        ) : null}
      </div>
      {titleKey ? (
        <TitleInspector
          key={titleKey}
          titleKey={titleKey}
          onClose={() => update({ title: null })}
        />
      ) : null}
    </Workspace>
  );
}

function Legend() {
  const states: EntryState[] = ["available", "today", "upcoming", "unavailable"];
  return (
    <div className="flex shrink-0 flex-wrap items-center gap-x-4 gap-y-1 border-t border-border bg-surface px-3 py-2 sm:px-5">
      {states.map((s) => (
        <span
          key={s}
          className="flex items-center gap-1.5 text-[11px] text-fg-muted"
          title={STATE_META[s].hint}
        >
          <StateDot state={s} />
          {STATE_META[s].label}
        </span>
      ))}
      <span className="flex items-center gap-1.5 text-[11px] text-fg-muted">
        <span className="size-1.5 rounded-full bg-primary" aria-hidden /> {t("Premiere")}
      </span>
      <span className="flex items-center gap-1.5 text-[11px] text-fg-muted">
        <span className="size-1.5 rounded-full bg-danger" aria-hidden /> {t("Finale")}
      </span>
    </div>
  );
}

function ShortcutHelp() {
  const [open, setOpen] = useState(false);
  const rows: [string, string][] = [
    ["J / N", t("Next period")],
    ["K / P", t("Previous period")],
    ["T", t("Jump to today")],
    ["M · W · A", t("Month, week or agenda")],
    ["← ↑ → ↓", t("Move between days in the month")],
    ["Enter", t("Open a day")],
  ];
  return (
    <Popover
      open={open}
      onOpenChange={setOpen}
      align="end"
      className="w-72 p-3"
      trigger={
        <Button
          variant="ghost"
          size="sm"
          onClick={() => setOpen((o) => !o)}
          aria-label={t("Keyboard shortcuts")}
        >
          <Keyboard size={15} />
        </Button>
      }
    >
      <p className="mb-2 text-xs font-semibold uppercase tracking-wider text-fg-subtle">
        {t("Keyboard shortcuts")}
      </p>
      <dl className="space-y-1.5">
        {rows.map(([k, label]) => (
          <div key={k} className="flex items-center justify-between gap-3 text-sm">
            <dt className="text-fg-muted">{label}</dt>
            <dd>
              <kbd className="rounded border border-border bg-bg-subtle px-1.5 py-0.5 font-mono text-[11px] text-fg">
                {k}
              </kbd>
            </dd>
          </div>
        ))}
      </dl>
    </Popover>
  );
}

function Skeleton({ view }: { view: View }) {
  if (view === "month")
    return (
      <div
        className="grid flex-1 grid-cols-7 grid-rows-[repeat(6,minmax(7.75rem,1fr))] gap-px bg-border/60"
        aria-busy
        role="status"
        aria-label={t("Loading the calendar")}
      >
        {Array.from({ length: 42 }, (_, i) => (
          <div key={i} className="space-y-1.5 bg-bg p-1.5">
            <div className="h-4 w-5 animate-pulse rounded bg-surface-hover" />
            {i % 3 === 0 ? <div className="h-6 animate-pulse rounded-md bg-surface-hover" /> : null}
            {i % 5 === 0 ? <div className="h-6 animate-pulse rounded-md bg-surface-hover" /> : null}
          </div>
        ))}
      </div>
    );
  return (
    <div
      className="mx-auto w-full max-w-5xl space-y-3 p-5"
      aria-busy
      role="status"
      aria-label={t("Loading the calendar")}
    >
      {Array.from({ length: 6 }, (_, i) => (
        <div key={i} className="flex gap-3 rounded-xl bg-surface p-2.5 ring-1 ring-border">
          <div className="aspect-video w-48 animate-pulse rounded-lg bg-surface-hover" />
          <div className="flex-1 space-y-2 py-1">
            <div className="h-4 w-1/3 animate-pulse rounded bg-surface-hover" />
            <div className="h-3 w-1/2 animate-pulse rounded bg-surface-hover" />
          </div>
        </div>
      ))}
    </div>
  );
}
