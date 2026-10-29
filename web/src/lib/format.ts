import { getLocale, localized, t } from "../i18n";

export function formatBytes(n: unknown): string {
  const v = typeof n === "number" ? n : Number(n);
  if (!Number.isFinite(v) || v < 0) return "—";
  if (v === 0) return "0 B";
  const units = ["B", "KiB", "MiB", "GiB", "TiB", "PiB"];
  const i = Math.min(units.length - 1, Math.floor(Math.log(v) / Math.log(1024)));
  return `${fixed(v / 1024 ** i, i === 0 ? 0 : 1)} ${units[i]}`;
}

/** `n.toFixed(digits)` with the active language's decimal separator. */
export function fixed(n: number, digits: number): string {
  return new Intl.NumberFormat(getLocale(), {
    minimumFractionDigits: digits,
    maximumFractionDigits: digits,
    useGrouping: false,
  }).format(n);
}

export function formatRate(bytesPerSecond: number): string {
  if (!Number.isFinite(bytesPerSecond) || bytesPerSecond <= 0) return "—";
  const mbit = (bytesPerSecond * 8) / 1e6;
  return `${fixed(mbit, mbit >= 10 ? 0 : 1)} Mbit/s`;
}

export function humanizeKey(key: string): string {
  return key
    .replace(/([a-z0-9])([A-Z])/g, "$1 $2")
    .replace(/[_-]+/g, " ")
    .replace(/\b\w/g, (c) => c.toUpperCase());
}

export function formatNumber(n: number, maxFractionDigits = 2): string {
  return new Intl.NumberFormat(getLocale(), { maximumFractionDigits: maxFractionDigits }).format(n);
}

export function formatPercent(fraction: number, digits = 0): string {
  if (!Number.isFinite(fraction)) return "—";
  return new Intl.NumberFormat(getLocale(), {
    style: "percent",
    minimumFractionDigits: digits,
    maximumFractionDigits: digits,
  }).format(fraction);
}

export function isPlainObject(v: unknown): v is Record<string, unknown> {
  return typeof v === "object" && v !== null && !Array.isArray(v);
}

/** Go's zero time marshals as 0001-01-01; treat it (and junk) as "never". */
export function parseTime(s: unknown): Date | null {
  if (typeof s !== "string" || !s) return null;
  const d = new Date(s);
  if (Number.isNaN(d.getTime()) || d.getUTCFullYear() < 1971) return null;
  return d;
}

/** "3 minutes ago", "in 2 days"; "never" for missing times. */
export function relativeTime(s: unknown, now: Date = new Date()): string {
  const d = s instanceof Date ? s : parseTime(s);
  if (!d) return t("never");
  const diff = (d.getTime() - now.getTime()) / 1000;
  const abs = Math.abs(diff);
  const units: [Intl.RelativeTimeFormatUnit, number][] = [
    ["year", 31536000],
    ["month", 2592000],
    ["week", 604800],
    ["day", 86400],
    ["hour", 3600],
    ["minute", 60],
  ];
  for (const [unit, secs] of units) {
    if (abs >= secs) {
      const v = Math.round(diff / secs);
      return new Intl.RelativeTimeFormat(getLocale(), { numeric: "auto" }).format(v, unit);
    }
  }
  return t("just now");
}

export function formatDateTime(s: unknown): string {
  const d = s instanceof Date ? s : parseTime(s);
  if (!d) return "—";
  return d.toLocaleString(getLocale(), { dateStyle: "medium", timeStyle: "short" });
}

export function formatDate(s: unknown): string {
  const d = s instanceof Date ? s : parseTime(s);
  if (!d) return "—";
  return d.toLocaleDateString(getLocale(), { dateStyle: "medium" });
}

/** Go time.Duration values arrive as nanoseconds. */
export function formatNanos(ns: unknown): string {
  const v = typeof ns === "number" ? ns : Number(ns);
  if (!Number.isFinite(v) || v <= 0) return "—";
  const ms = v / 1e6;
  if (ms < 1000) return `${fixed(ms, 0)} ms`;
  const s = ms / 1000;
  if (s < 60) return `${fixed(s, 1)} s`;
  const m = Math.floor(s / 60);
  return `${unit(m, "minute")} ${unit(Math.round(s % 60), "second")}`;
}

/** "5m", "2h": a number with a compact, localised time unit. */
export function unit(n: number, u: "hour" | "minute" | "second"): string {
  return new Intl.NumberFormat(getLocale(), {
    style: "unit",
    unit: u,
    unitDisplay: "narrow",
  }).format(n);
}

export function formatMinutes(min: number): string {
  if (!Number.isFinite(min) || min <= 0) return "—";
  const h = Math.floor(min / 60);
  const m = Math.round(min % 60);
  return h ? (m ? `${unit(h, "hour")} ${unit(m, "minute")}` : unit(h, "hour")) : unit(m, "minute");
}

/** Weekday names, Sunday first, in the active language. */
function weekdays(width: "short" | "long"): string[] {
  const fmt = new Intl.DateTimeFormat(getLocale(), { weekday: width, timeZone: "UTC" });
  // 4 January 1970 was a Sunday.
  return Array.from({ length: 7 }, (_, i) => fmt.format(Date.UTC(1970, 0, 4 + i)));
}

export const WEEKDAYS = localized(() => weekdays("short"));
export const WEEKDAYS_LONG = localized(() => weekdays("long"));

export function formatHour(h: number): string {
  const hh = ((h % 24) + 24) % 24;
  return new Intl.DateTimeFormat(getLocale(), { hour: "numeric", timeZone: "UTC" }).format(
    Date.UTC(1970, 0, 1, hh),
  );
}

export function initials(name: string): string {
  const parts = name.trim().split(/\s+/).filter(Boolean);
  if (parts.length === 0) return "?";
  return (
    (parts[0]?.[0] ?? "") + (parts.length > 1 ? (parts[parts.length - 1]?.[0] ?? "") : "")
  ).toUpperCase();
}
