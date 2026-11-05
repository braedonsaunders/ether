import { t } from "../../i18n";
export const HOUR = 3600000;
export type TimelineKind =
  | "programme"
  | "pod"
  | "spot"
  | "ident"
  | "upnext"
  | "break"
  | "back"
  | "stationId"
  | "plugin"
  | "special"
  | "gap"
  | "offair";
export interface TimelineElement {
  children?: TimelineElement[];
  id: string;
  kind: TimelineKind;
  start: number;
  end: number;
  title: string;
  subtitle?: string;
  mediaType?: "episode" | "movie" | "clip" | "segment";
  blockId?: string;
  sourceId?: string;
  pinned?: boolean;
  breakPoints?: { atMs: number; source: string; confidence: number }[];
  art?: string;
  rule: string;
}
export interface TimelineResponse {
  channelId: string;
  timezone: string;
  now: number;
  dayparts: { id: string; name: string; start: number; end: number; days: number[] }[];
  elements: TimelineElement[];
}
export type TimelineRange = { from: number; to: number };
export function kindLabel(kind: TimelineKind): string {
  return {
    programme: t("Programme"),
    pod: t("Break pod"),
    spot: t("Spot"),
    ident: t("Ident"),
    upnext: t("Up next"),
    break: t("Break"),
    back: t("Back"),
    stationId: t("Station ID"),
    plugin: t("Plugin segment"),
    special: t("Special"),
    gap: t("Gap"),
    offair: t("Off air"),
  }[kind];
}
export function timeLabel(ms: number, timezone: string, locale?: string) {
  return new Intl.DateTimeFormat(locale, {
    timeZone: timezone,
    weekday: "short",
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
  }).format(ms);
}
export function tooltipContent(
  el: TimelineElement,
  timezone: string,
  children: TimelineElement[] = [],
  locale?: string,
) {
  return [
    el.title,
    el.subtitle,
    kindLabel(el.kind),
    `${timeLabel(el.start, timezone, locale)} – ${timeLabel(el.end, timezone, locale)}`,
    timezone,
    el.pinned ? t("Pinned") : "",
    el.rule,
    ...children.map(
      (s) =>
        `${s.title} · ${kindLabel(s.kind)} · ${timeLabel(s.start, timezone, locale)} – ${timeLabel(s.end, timezone, locale)} · ${s.rule}`,
    ),
  ]
    .filter(Boolean)
    .join("\n");
}
export function labelDetail(width: number) {
  return width >= 170 ? "subtitle" : width >= 64 ? "title" : "compact";
}
export function flattenElements(elements: TimelineElement[]): TimelineElement[] {
  return elements.flatMap((el) => [el, ...flattenElements(el.children ?? [])]);
}
export function layoutElements(
  elements: TimelineElement[],
  range: TimelineRange,
  width: number,
  viewportHours: number,
) {
  elements = flattenElements(elements);
  const scale = width / (viewportHours * HOUR);
  const pods = elements.filter((e) => e.kind === "pod");
  const grouped = new Set(
    elements
      .filter(
        (e) =>
          !["programme", "special", "pod", "gap", "offair"].includes(e.kind) &&
          pods.some((p) => e.start >= p.start && e.end <= p.end && (p.end - p.start) * scale < 24),
      )
      .map((e) => e.id),
  );
  return elements
    .filter((e) => !grouped.has(e.id) && e.end > range.from && e.start < range.to)
    .map((el) => {
      const left = Math.max(0, (el.start - range.from) * scale);
      const w = Math.max(3, (Math.min(el.end, range.to) - Math.max(el.start, range.from)) * scale);
      return {
        el,
        left,
        width: w,
        detail: labelDetail(w),
        lane: ["programme", "special", "gap", "offair"].includes(el.kind)
          ? 0
          : el.kind === "spot"
            ? 2
            : 1,
      };
    });
}
// Iterate real instants, then read channel-local clock parts. This also handles DST folds/gaps.
export function daypartBands(data: TimelineResponse, range: TimelineRange) {
  const fmt = new Intl.DateTimeFormat("en-US", {
    timeZone: data.timezone,
    weekday: "short",
    hour: "numeric",
    minute: "numeric",
    hourCycle: "h23",
  });
  const result: { start: number; end: number; name: string; id: string }[] = [];
  for (let at = Math.floor(range.from / 60000) * 60000; at < range.to; at += 60000) {
    const parts = Object.fromEntries(fmt.formatToParts(at).map((p) => [p.type, p.value]));
    const minute = Number(parts.hour) * 60 + Number(parts.minute),
      day = ["Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"].indexOf(parts.weekday!);
    for (const band of data.dayparts) {
      const overnight = band.start > band.end;
      const match = overnight
        ? minute >= band.start || minute < band.end
        : minute >= band.start && minute < band.end;
      const ownerDay = overnight && minute < band.end ? (day + 6) % 7 : day;
      if (!match || (band.days?.length && !band.days.includes(ownerDay))) continue;
      const last = result[result.length - 1];
      if (last?.id === band.id && last.end === at) last.end = Math.min(range.to, at + 60000);
      else
        result.push({
          start: Math.max(range.from, at),
          end: Math.min(range.to, at + 60000),
          name: band.name,
          id: band.id,
        });
    }
  }
  return result;
}
