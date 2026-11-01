import { getLocale, localized, t } from "../../i18n";
import { apiFetch } from "../../lib/api";

export type EntryKind = "episode" | "premiere" | "movie";
export type EntryState = "available" | "today" | "upcoming" | "unavailable";
export type Badge = "seriesPremiere" | "seasonPremiere" | "finale";
export type Reason = "watching" | "watchlist" | "lookahead" | "taste" | "popular";

export interface CalendarEntry {
  id: string;
  kind: EntryKind;
  /** The dated title; `open` is what the console opens (the show for episodes). */
  key: string;
  open: string;
  title: string;
  episodeTitle?: string;
  season?: number;
  episode?: number;
  /** YYYY-MM-DD */
  date: string;
  year?: number;
  poster?: string;
  backdrop?: string;
  still?: string;
  overview?: string;
  runtimeMinutes?: number;
  contentRating?: string;
  rating?: number;
  genres?: string[];
  state: EntryState;
  badges?: Badge[];
  reasons: Reason[];
  collections?: string[];
  viewers: number[];
  watched?: number[];
}

export interface CalendarViewer {
  id: number;
  name: string;
  thumb?: string;
  owner?: boolean;
}

export interface CalendarData {
  from: string;
  to: string;
  today: string;
  viewers: CalendarViewer[];
  entries: CalendarEntry[];
  shows: number;
  truncated?: boolean;
  warming?: boolean;
  generatedAt: string;
}

export type View = "month" | "week" | "agenda";
export type Types = "all" | "movie" | "show";
export type MoviePolicy = "foryou" | "popular" | "watchlist" | "hidden";

export interface Filters {
  viewer: string; // "all" or a viewer id
  types: Types;
  movies: MoviePolicy;
}

export const calendarApi = {
  load: (from: string, to: string, f: Filters, signal?: AbortSignal) => {
    const p = new URLSearchParams({ from, to, movies: f.movies });
    if (f.viewer !== "all") p.set("viewer", f.viewer);
    if (f.types !== "all") p.set("types", f.types);
    const tz = Intl.DateTimeFormat().resolvedOptions().timeZone;
    if (tz) p.set("tz", tz);
    return apiFetch<CalendarData>(`/calendar/?${p}`, { signal });
  },
};

// ---- dates (all calendar days are YYYY-MM-DD, computed in UTC) ----------------

const DAY = 86_400_000;

export function parseDay(s: string): Date {
  const [y, m, d] = s.split("-").map(Number);
  return new Date(Date.UTC(y ?? 1970, (m ?? 1) - 1, d ?? 1));
}

export function dayKey(d: Date): string {
  return d.toISOString().slice(0, 10);
}

export function addDays(s: string, n: number): string {
  return dayKey(new Date(parseDay(s).getTime() + n * DAY));
}

export function addMonths(s: string, n: number): string {
  const d = parseDay(s);
  const target = new Date(Date.UTC(d.getUTCFullYear(), d.getUTCMonth() + n, 1));
  const last = new Date(
    Date.UTC(target.getUTCFullYear(), target.getUTCMonth() + 1, 0),
  ).getUTCDate();
  target.setUTCDate(Math.min(d.getUTCDate(), last));
  return dayKey(target);
}

export function daysBetween(a: string, b: string): number {
  return Math.round((parseDay(b).getTime() - parseDay(a).getTime()) / DAY);
}

/** Today in the browser's time zone. */
export function localToday(): string {
  const n = new Date();
  return dayKey(new Date(Date.UTC(n.getFullYear(), n.getMonth(), n.getDate())));
}

/** 0 = Sunday, 1 = Monday: the locale's first day of the week. */
export function weekStartDay(locale = getLocale()): number {
  try {
    const loc = new Intl.Locale(locale) as Intl.Locale & {
      getWeekInfo?: () => { firstDay: number };
      weekInfo?: { firstDay: number };
    };
    const info = loc.getWeekInfo?.() ?? loc.weekInfo;
    if (info?.firstDay) return info.firstDay % 7;
  } catch {
    // older engines: fall through
  }
  return /^en(-US)?$/i.test(locale) || /^(ja|ko|zh-TW|pt-BR)/i.test(locale) ? 0 : 1;
}

export function startOfWeek(s: string, first = weekStartDay()): string {
  const dow = parseDay(s).getUTCDay();
  return addDays(s, -((dow - first + 7) % 7));
}

export function startOfMonth(s: string): string {
  return `${s.slice(0, 7)}-01`;
}

/** The days a period shows: six weeks for a month, seven days for a week, five weeks of agenda. */
export function periodRange(
  view: View,
  anchor: string,
  first = weekStartDay(),
): { from: string; to: string } {
  if (view === "month") {
    const from = startOfWeek(startOfMonth(anchor), first);
    return { from, to: addDays(from, 41) };
  }
  if (view === "week") {
    const from = startOfWeek(anchor, first);
    return { from, to: addDays(from, 6) };
  }
  return { from: anchor, to: addDays(anchor, 34) };
}

export function shiftAnchor(view: View, anchor: string, dir: 1 | -1): string {
  if (view === "month") return addMonths(startOfMonth(anchor), dir);
  if (view === "week") return addDays(anchor, 7 * dir);
  return addDays(anchor, 35 * dir);
}

function fmt(s: string, opts: Intl.DateTimeFormatOptions): string {
  return new Intl.DateTimeFormat(getLocale(), { ...opts, timeZone: "UTC" }).format(parseDay(s));
}

export const formatDay = {
  monthYear: (s: string) => fmt(s, { month: "long", year: "numeric" }),
  long: (s: string) => fmt(s, { weekday: "long", month: "long", day: "numeric" }),
  medium: (s: string) => fmt(s, { weekday: "short", month: "short", day: "numeric" }),
  short: (s: string) => fmt(s, { month: "short", day: "numeric" }),
  monthDay: (s: string) => fmt(s, { month: "long", day: "numeric" }),
  weekday: (s: string) => fmt(s, { weekday: "short" }),
  weekdayLong: (s: string) => fmt(s, { weekday: "long" }),
  number: (s: string) => fmt(s, { day: "numeric" }),
  monthShort: (s: string) => fmt(s, { month: "short" }),
};

/** "Today", "Tomorrow", "Yesterday", "In 3 days", "5 days ago". */
export function relativeDay(s: string, today: string): string {
  const n = daysBetween(today, s);
  if (n === 0) return t("Today");
  if (n === 1) return t("Tomorrow");
  if (n === -1) return t("Yesterday");
  const rtf = new Intl.RelativeTimeFormat(getLocale(), { numeric: "auto" });
  return rtf.format(n, "day");
}

export function periodTitle(view: View, anchor: string, first = weekStartDay()): string {
  if (view === "month") return formatDay.monthYear(anchor);
  const { from, to } = periodRange(view, anchor, first);
  return `${formatDay.short(from)} – ${formatDay.short(to)}`;
}

// ---- entries --------------------------------------------------------------------------

/** "S02E05" */
export function episodeCode(e: Pick<CalendarEntry, "season" | "episode">): string {
  if (e.season == null) return "";
  const s = `S${String(e.season).padStart(2, "0")}`;
  return e.episode ? `${s}E${String(e.episode).padStart(2, "0")}` : s;
}

/** The second line of an entry: episode code and title, a season, or a movie's year. */
export function entrySubtitle(e: CalendarEntry): string {
  if (e.kind === "episode") return [episodeCode(e), e.episodeTitle].filter(Boolean).join(" · ");
  if (e.kind === "premiere") return t("Season {number}", { number: e.season ?? 0 });
  return t("Movie");
}

export function groupByDay(entries: CalendarEntry[]): Map<string, CalendarEntry[]> {
  const m = new Map<string, CalendarEntry[]>();
  for (const e of entries) {
    const list = m.get(e.date);
    if (list) list.push(e);
    else m.set(e.date, [e]);
  }
  return m;
}

/**
 * The state as the owner's browser sees it: the server decides "today" in
 * the time zone the request named, but the page may have stayed open past
 * midnight.
 */
export function stateOn(e: CalendarEntry, today: string): EntryState {
  if (e.date > today) return "upcoming";
  if (e.date === today) return "today";
  if (e.state === "upcoming" || e.state === "today")
    return e.kind === "premiere" ? "unavailable" : "available";
  return e.state;
}

export const STATE_META = localized<
  Record<EntryState, { label: string; hint: string; dot: string; bar: string; chip: string }>
>(() => ({
  available: {
    label: t("Available"),
    hint: t("Aired and ready to play"),
    dot: "bg-success",
    bar: "bg-success",
    chip: "bg-success/15 text-success ring-success/30",
  },
  today: {
    label: t("Airing today"),
    hint: t("Out today"),
    dot: "bg-primary",
    bar: "bg-primary",
    chip: "bg-primary/15 text-primary ring-primary/30",
  },
  upcoming: {
    label: t("Upcoming"),
    hint: t("Not out yet"),
    dot: "bg-info",
    bar: "bg-info",
    chip: "bg-info/15 text-info ring-info/30",
  },
  unavailable: {
    label: t("Not playable yet"),
    hint: t("Released, but not streamable yet"),
    dot: "bg-warning",
    bar: "bg-warning",
    chip: "bg-warning/15 text-warning ring-warning/30",
  },
}));

export const BADGE_LABEL = localized<Record<Badge, string>>(() => ({
  seriesPremiere: t("Series premiere"),
  seasonPremiere: t("Season premiere"),
  finale: t("Finale"),
}));

export const REASON_LABEL = localized<Record<Reason, string>>(() => ({
  watching: t("Watching"),
  watchlist: t("On a watchlist"),
  lookahead: t("Look Ahead collection"),
  taste: t("Matches the household's taste"),
  popular: t("Popular release"),
}));

/** Stable hue per show or movie, for artwork-less fallbacks. */
export function hueOf(s: string): number {
  let h = 0;
  for (let i = 0; i < s.length; i++) h = (h * 31 + s.charCodeAt(i)) >>> 0;
  return h % 360;
}
