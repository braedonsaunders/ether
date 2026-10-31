import { getLocale, localized, t } from "../../i18n";
import { apiFetch } from "../../lib/api";
import { unit } from "../../lib/format";

export type Item = {
  key: number;
  title: string;
  durationMs: number;
  summary?: string;
  rating?: string;
  thumb?: string;
  showKey?: number;
  season?: number;
  episode?: number;
  releasedMs?: number;
};
export type ProgramSource = {
  name?: string;
  weight?: number;
  items?: Item[];
  collection?: number;
  filter?: Record<string, unknown>;
  watched?: "" | "watched" | "unwatched";
};
export type Block = {
  start: number;
  end: number;
  /** Weekdays in the channel timezone, 0 Sunday … 6 Saturday. Empty means every day. */
  days?: number[];
  items: Item[];
  shuffle: boolean;
  mode?: string;
  align?: "" | "hour" | "half";
  fit?: "" | "cut" | "pad";
  repeatDays?: number;
  filler?: Item[];
  marathonKey?: number;
  collection?: number;
  filter?: Record<string, unknown>;
  sources?: ProgramSource[];
  watched?: "" | "watched" | "unwatched";
};
export type Channel = {
  id: string;
  number: string;
  name: string;
  kind: "virtual" | "iptv";
  transcode?: boolean;
  enabled: boolean;
  favorite: boolean;
  rating?: string;
  logo?: string;
  allowedUsers?: number[];
  sourceId?: string;
  guideId?: string;
  /** The source whose XMLTV guide feeds this channel (blank: its own source). */
  guideSourceId?: string;
  streamUrl?: string;
  /** Ordered stream sources: the first enabled one plays, the rest are backups. */
  streamSources?: StreamSource[];
  group?: string;
  epoch: string;
  seed: number;
  shuffle: boolean;
  items: Item[];
  blocks?: Block[];
  collection?: number;
  filter?: Record<string, unknown>;
  sources?: ProgramSource[];
  watched?: "" | "watched" | "unwatched";
  mode?: string;
  align?: "" | "hour" | "half";
  fit?: "" | "cut" | "pad";
  repeatDays?: number;
  filler?: Item[];
  fallback?: Item[];
  offAir?: boolean;
  marathonKey?: number;
  /** IANA zone. Empty means block times are UTC, every day, as older channels stored them. */
  timezone?: string;
};
export type StreamSource = {
  id: string;
  name: string;
  url: string;
  enabled: boolean;
  priority: number;
  /** The source whose provider login this input uses (blank: the channel's own). */
  authSourceId?: string;
};
export type AuthType = "" | "basic" | "bearer" | "headers" | "xtream";
/** Provider login. Secrets come back as "••••"; sending that back keeps them. */
export type SourceAuth = {
  type?: AuthType;
  username?: string;
  password?: string;
  token?: string;
  userAgent?: string;
  referer?: string;
  headers?: Record<string, string>;
  server?: string;
  output?: "ts" | "m3u8";
};
export type SourceStatus = {
  id: string;
  state: "unchecked" | "available" | "cooling_down" | "retry_ready" | "disabled" | string;
  failures?: number;
  lastChecked?: string;
  lastPlayed?: string;
  retryAfter?: string;
  lastError?: string;
};
export type PreviewSession = {
  id: string;
  /** Relative to the livetv API, e.g. "sessions/{id}/index.m3u8". */
  playlist: string;
  activeSourceId?: string;
  activeSourceName?: string;
  programme?: Programme;
};
export type Source = {
  id: string;
  name: string;
  playlistUrl?: string;
  guideUrl?: string;
  updatedAt: string;
  refreshHours?: number;
  channelCount?: number;
  guideChannelCount?: number;
  programmeCount?: number;
  guideFrom?: string;
  guideUntil?: string;
  lastError?: string;
  nextRefresh?: string;
  hasPlaylist?: boolean;
  hasGuide?: boolean;
  auth?: SourceAuth;
  refreshFailures?: number;
};
export type SourceDraft = {
  id: string;
  name: string;
  playlistUrl: string;
  guideUrl: string;
  playlist: string;
  guide: string;
  refreshHours?: number;
  auth?: SourceAuth;
};
export type Programme = { id: string; channelId: string; start: string; end: string; item: Item };
export type GuideChannel = {
  sourceId: string;
  sourceName: string;
  guideId: string;
  name: string;
  logo?: string;
  programmes: number;
  until?: string;
};
export type Coverage = { channelId: string; programmes: number; until?: string };
export type MatchChange = {
  channelId: string;
  channelName?: string;
  guideSourceId: string;
  guideSourceName?: string;
  guideId: string;
  guideName: string;
  method?: "id" | "name";
};
export type MatchResult = {
  matched: number;
  unmatched: number;
  skipped?: number;
  changes: MatchChange[] | null;
};

const enc = encodeURIComponent;
const json = (method: string, body: unknown): RequestInit => ({
  method,
  body: JSON.stringify(body),
});

export const tv = {
  channels: (signal?: AbortSignal) => apiFetch<Channel[]>("/livetv/channels", { signal }),
  sources: (signal?: AbortSignal) => apiFetch<Source[]>("/livetv/sources", { signal }),
  guide: (from: Date, until: Date, signal?: AbortSignal) =>
    apiFetch<Programme[]>(
      `/livetv/guide?from=${enc(from.toISOString())}&until=${enc(until.toISOString())}`,
      { signal },
    ),
  saveChannel: (c: Channel) =>
    apiFetch<Channel>(
      `/livetv/channels${c.id ? `/${enc(c.id)}` : ""}`,
      json(c.id ? "PUT" : "POST", c),
    ),
  sourceStatus: async (id: string, signal?: AbortSignal): Promise<SourceStatus[]> => {
    const r = await apiFetch<SourceStatus[] | { sources?: SourceStatus[] } | null>(
      `/livetv/channels/${enc(id)}/sources/status`,
      { signal },
    );
    return Array.isArray(r) ? r : (r?.sources ?? []);
  },
  play: (id: string) =>
    apiFetch<PreviewSession>(`/livetv/channels/${enc(id)}/play`, { method: "POST", body: "{}" }),
  stopPreview: (session: string) =>
    apiFetch(`/livetv/sessions/${enc(session)}`, { method: "DELETE" }),
  deleteChannel: (id: string) => apiFetch(`/livetv/channels/${enc(id)}`, { method: "DELETE" }),
  preview: (c: Channel, signal?: AbortSignal, window?: { from: Date; until: Date }) => {
    const q = new URLSearchParams();
    if (window) {
      q.set("from", window.from.toISOString());
      q.set("until", window.until.toISOString());
    }
    const suffix = q.size ? `?${q}` : "";
    return apiFetch<Programme[]>(`/livetv/preview${suffix}`, { ...json("POST", c), signal });
  },
  saveSource: (s: SourceDraft, pending = false) =>
    apiFetch<Source>(
      `/livetv/sources${s.id ? `/${enc(s.id)}` : ""}${pending ? "?pending=1" : ""}`,
      json(s.id ? "PUT" : "POST", s),
    ),
  uploadSource: (id: string, part: "playlist" | "guide", file: File) =>
    apiFetch<Source>(`/livetv/sources/${enc(id)}/${part}`, {
      method: "POST",
      body: file,
      headers: { "Content-Type": "application/octet-stream" },
    }),
  refreshSource: (id: string) =>
    apiFetch<Source>(`/livetv/sources/${enc(id)}/refresh`, { method: "POST" }),
  deleteSource: (id: string) => apiFetch(`/livetv/sources/${enc(id)}`, { method: "DELETE" }),
  guideChannels: (q: string, signal?: AbortSignal, limit = 50) =>
    apiFetch<GuideChannel[] | null>(`/livetv/guide/channels?q=${enc(q)}&limit=${limit}`, {
      signal,
    }),
  coverage: (signal?: AbortSignal) =>
    apiFetch<Coverage[] | null>("/livetv/guide/coverage", { signal }),
  match: (body: { channelIds?: string[]; overwrite?: boolean; dryRun?: boolean }) =>
    apiFetch<MatchResult>("/livetv/guide/match", json("POST", body)),
};

export const emptyChannel = (): Channel => ({
  id: "",
  number: "",
  name: "",
  kind: "virtual",
  enabled: true,
  favorite: false,
  epoch: "1970-01-01T00:00:00Z",
  seed: 1,
  shuffle: false,
  items: [],
});

export const emptySource = (): SourceDraft => ({
  id: "",
  name: "",
  playlistUrl: "",
  guideUrl: "",
  playlist: "",
  guide: "",
});

export type Template = {
  key: string;
  name: string;
  tagline: string;
  hue: number;
  apply: (c: Channel) => Channel;
};

/** Starting points for a virtual channel; each is an ordinary editable draft. */
export const TEMPLATES: Template[] = localized<Template[]>(() => [
  {
    key: "blank",
    name: t("Blank channel"),
    tagline: t("Hand-pick every title"),
    hue: 220,
    apply: (c) => c,
  },
  {
    key: "80s-action",
    name: t("80s Action 24/7"),
    tagline: t("Action movies, 1980–1989, shuffled"),
    hue: 12,
    apply: (c) => ({
      ...c,
      name: t("80s Action 24/7"),
      filter: { type: "movie", genres: ["Action"], yearFrom: 1980, yearTo: 1989 },
      shuffle: true,
    }),
  },
  {
    key: "family",
    name: t("Family Animation"),
    tagline: t("Animated movies rated PG and under"),
    hue: 160,
    apply: (c) => ({
      ...c,
      name: t("Family Animation"),
      filter: { type: "movie", genres: ["Animation"], maxContentRating: "PG" },
      rating: "PG",
    }),
  },
  {
    key: "scifi",
    name: t("Sci-Fi Channel"),
    tagline: t("Science fiction around the clock"),
    hue: 265,
    apply: (c) => ({
      ...c,
      name: t("Sci-Fi Channel"),
      filter: { type: "movie", genres: ["Science Fiction"] },
      shuffle: true,
    }),
  },
  {
    key: "horror",
    name: t("Midnight Horror"),
    tagline: t("Horror movies, shuffled"),
    hue: 350,
    apply: (c) => ({
      ...c,
      name: t("Midnight Horror"),
      filter: { type: "movie", genres: ["Horror"] },
      shuffle: true,
    }),
  },
  {
    key: "sitcoms",
    name: t("Sitcom Marathon"),
    tagline: t("Comedy series, every episode in order"),
    hue: 45,
    apply: (c) => ({
      ...c,
      name: t("Sitcom Marathon"),
      filter: { type: "show", genres: ["Comedy"] },
    }),
  },
]);

// ---- time ------------------------------------------------------------------------

export const MINUTE = 60_000;
export const timeOfDay = (minutes: number) =>
  minutes === 1440
    ? "24:00"
    : `${String(Math.floor(minutes / 60)).padStart(2, "0")}:${String(minutes % 60).padStart(2, "0")}`;
export const minutesOfDay = (value: string) => {
  const [h = NaN, m = NaN] = value.split(":").map(Number);
  return h * 60 + m;
};
export const clock = (d: Date | string) =>
  new Date(d).toLocaleTimeString(getLocale(), { hour: "numeric", minute: "2-digit" });
export const dayClock = (d: Date | string) =>
  new Date(d).toLocaleString(getLocale(), { weekday: "short", hour: "numeric", minute: "2-digit" });
export function duration(ms: number): string {
  const m = Math.max(0, Math.round(ms / MINUTE));
  const h = Math.floor(m / 60);
  const r = m % 60;
  return h ? (r ? `${unit(h, "hour")} ${unit(r, "minute")}` : unit(h, "hour")) : unit(r, "minute");
}
/** UTC block minutes rendered in the owner's local time. */
export function localBlockTime(utcMinutes: number): string {
  const d = new Date();
  d.setUTCHours(0, 0, 0, 0);
  d.setUTCMinutes(utcMinutes);
  return clock(d);
}

// ---- channel helpers ------------------------------------------------------------------

/** A stable hue per channel so generated logos and guide accents stay put. */
export function hueOf(s: string): number {
  let h = 2166136261;
  for (let i = 0; i < s.length; i++) h = Math.imul(h ^ s.charCodeAt(i), 16777619);
  h ^= h >>> 13;
  h = Math.imul(h, 0x5bd1e995);
  return ((h ^ (h >>> 15)) >>> 0) % 360;
}

export function onNow(programmes: Programme[] | undefined, at = Date.now()): Programme | undefined {
  return programmes?.find((p) => Date.parse(p.start) <= at && Date.parse(p.end) > at);
}

export function upNext(
  programmes: Programme[] | undefined,
  at = Date.now(),
): Programme | undefined {
  return programmes?.find((p) => Date.parse(p.start) > at);
}

export function progress(p: Programme, at = Date.now()): number {
  const s = Date.parse(p.start);
  const e = Date.parse(p.end);
  return e > s ? Math.min(1, Math.max(0, (at - s) / (e - s))) : 0;
}

export function byNumber(a: Channel, b: Channel): number {
  return (
    (parseFloat(a.number) || 0) - (parseFloat(b.number) || 0) ||
    a.number.localeCompare(b.number) ||
    a.name.localeCompare(b.name)
  );
}

export function groupBy<T>(items: T[], key: (t: T) => string): Map<string, T[]> {
  const out = new Map<string, T[]>();
  for (const t of items) {
    const k = key(t);
    const list = out.get(k);
    if (list) list.push(t);
    else out.set(k, [t]);
  }
  return out;
}

export function nextNumber(channels: Channel[]): string {
  const max = channels.reduce((n, c) => Math.max(n, Math.floor(parseFloat(c.number) || 0)), 0);
  return String(max + 1);
}

export function programmingMode(c: Channel): "movies" | "collection" | "genre" | "mix" {
  if (c.sources?.length) return "mix";
  return c.collection ? "collection" : c.filter ? "genre" : "movies";
}

/** The playout the owner sees. Order and shuffle stay on the original engine until a newer mode is chosen. */
export function playoutValue(mode: string | undefined, shuffle: boolean): string {
  if (mode === "random" || mode === "marathon" || mode === "newest") return mode;
  return shuffle ? "shuffle" : "order";
}

export function playoutPatch(value: string): { mode?: string; shuffle: boolean } {
  if (value === "shuffle") return { mode: undefined, shuffle: true };
  if (value === "order") return { mode: undefined, shuffle: false };
  return { mode: value, shuffle: false };
}

const WEEKDAYS = ["Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"];

/** Move UTC minute-of-day blocks onto today's offset in `timeZone`, splitting any block that crosses local midnight. */
export function blocksInZone(blocks: Block[], timeZone: string): Block[] {
  const probe = new Date();
  probe.setSeconds(0, 0);
  const utcMidnight = Date.UTC(probe.getUTCFullYear(), probe.getUTCMonth(), probe.getUTCDate());
  const out: Block[] = [];
  for (const block of blocks) {
    const startAt = utcMidnight + block.start * MINUTE;
    const endAt = utcMidnight + block.end * MINUTE;
    const start = zonedMinutes(startAt, timeZone);
    const end = zonedMinutes(endAt, timeZone);
    const span = block.end - block.start;
    if (end.day === start.day || span <= 0) {
      const endMin = end.day === start.day ? end.minute : 1440;
      out.push({ ...block, start: start.minute, end: Math.max(start.minute + 30, endMin) });
      continue;
    }
    out.push({ ...block, start: start.minute, end: 1440 });
    if (end.minute > 0) out.push({ ...block, start: 0, end: end.minute });
  }
  return out;
}

function zonedMinutes(ms: number, timeZone: string): { day: number; minute: number } {
  const parts = new Intl.DateTimeFormat("en-US", {
    timeZone,
    hourCycle: "h23",
    weekday: "short",
    hour: "2-digit",
    minute: "2-digit",
  }).formatToParts(new Date(ms));
  const get = (type: string) => parts.find((p) => p.type === type)?.value ?? "";
  const hour = Number(get("hour"));
  const minute = Number(get("minute"));
  const day = WEEKDAYS.indexOf(get("weekday"));
  return { day: day < 0 ? 0 : day, minute: (hour % 24) * 60 + minute };
}

export function loopLength(items: Item[]): number {
  return items.reduce((n, i) => n + (i.durationMs || 0), 0);
}
