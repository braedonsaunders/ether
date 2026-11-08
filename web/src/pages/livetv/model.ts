import type { ScreenStyle } from "./onair/ScreenPicker";
import { personalRecipe } from "./onair/defaults";
import type { PersonalRecipe, ChannelLook, ClockSlot } from "./onair/model";
import { getLocale, localized, t, tn } from "../../i18n";
import { apiFetch } from "../../lib/api";
import { unit } from "../../lib/format";

export type Item = {
  key: number;
  title: string;
  durationMs: number;
  summary?: string;
  rating?: string;
  thumb?: string;
  art?: string;
  showTitle?: string;
  episodeTitle?: string;
  genres?: string[];
  premiere?: boolean;
  showKey?: number;
  season?: number;
  episode?: number;
  releasedMs?: number;
  breakPointsMs?: number[];
  breakPoints?: { atMs: number; source: string; confidence?: number }[];
};
export type ProgramSource = {
  id?: string;
  kind?: "items" | "show" | "movies" | "collection" | "filter" | "personal" | "plugin" | "mix";
  showKey?: number;
  startSeason?: number;
  startEpisode?: number;
  startDate?: string;
  order?: "order" | "shuffle";
  strip?: boolean;
  segmentId?: string;
  personal?: PersonalRecipe;
  sources?: ProgramSource[];
  name?: string;
  weight?: number;
  items?: Item[];
  collection?: number;
  filter?: Record<string, unknown>;
  watched?: "" | "watched" | "unwatched";
};
export type Block = {
  id?: string;
  preset?: string;
  breaks?: ChannelLook;
  start: number;
  end: number;
  /** Weekdays in the channel timezone, 0 Sunday … 6 Saturday. Empty means every day. */
  days?: number[];
  /** Preset key ("Cartoons", "Movie night") or a custom grid label. */
  name?: string;
  items: Item[];
  shuffle: boolean;
  mode?: string;
  align?: "" | "hour" | "half";
  fit?: "" | "cut" | "pad" | "shift";
  repeatDays?: number;
  filler?: Item[];
  marathonKey?: number;
  collection?: number;
  filter?: Record<string, unknown>;
  sources?: ProgramSource[];
  watched?: "" | "watched" | "unwatched";
};
export type Pin = {
  id: string;
  name: string;
  start: number;
  end: number;
  days?: number[];
  date?: string;
  sources?: ProgramSource[];
  items?: Item[];
  mode?: string;
  repeatDays?: number;
  breaks?: ChannelLook;
};
export type Channel = {
  pins?: Pin[];
  personal?: PersonalRecipe;
  look?: ChannelLook;
  screens?: { offAir?: ScreenStyle; noSignal?: ScreenStyle };
  clock?: ClockSlot[];
  health?: "unchecked" | "available" | "unavailable";
  id: string;
  number: string;
  name: string;
  kind: "virtual" | "iptv";
  /** Only viewers given this channel may watch it. */
  private?: boolean;
  transcode?: boolean;
  enabled: boolean;
  favorite: boolean;
  rating?: string;
  logo?: string;
  sourceId?: string;
  guideId?: string;
  /** The provider's own tvg-name label; a hint for guide matching, not a key. */
  tvgName?: string;
  /** The source whose XMLTV guide feeds this channel (blank: its own source). */
  guideSourceId?: string;
  streamUrl?: string;
  /** Ordered stream sources: the first enabled one plays, the rest are backups. */
  streamSources?: StreamSource[];
  group?: string;
  customGroup?: boolean;
  /** The provider's channel number; differs from `number` when the owner renumbered it. */
  providerNumber?: string;
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
  fit?: "" | "cut" | "pad" | "shift";
  repeatDays?: number;
  filler?: Item[];
  fallback?: Item[];
  offAir?: boolean;
  marathonKey?: number;
  /** IANA zone. Empty means block times are UTC, every day, as older channels stored them. */
  timezone?: string;
};
/** The lineup identity the paged console API returns; no stream or programme payload. */
export type ChannelSummary = Omit<Channel, "epoch" | "seed" | "shuffle" | "items"> &
  Partial<Pick<Channel, "epoch" | "seed" | "shuffle" | "items">>;
export type ChannelCounts = {
  all: number;
  live: number;
  favorites: number;
  virtual: number;
  iptv: number;
  off: number;
  missing: number;
};
export type ChannelGroup = {
  key: string;
  sourceId?: string;
  kind: "virtual" | "iptv";
  count: number;
  start: number;
};
export type ChannelPage = {
  channels: Channel[];
  total: number;
  counts: ChannelCounts;
  groups: ChannelGroup[];
  coverage: Record<string, boolean>;
  nextNumber: string;
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
export type PlexLineup = { id: string; title: string; type: string };
/** An Xtream Codes login's account, as the panel reported it at the last import. Read-only. */
export type SourceAccount = {
  status?: string;
  trial?: boolean;
  /** RFC 3339. */
  expiresAt?: string;
  maxConnections?: number;
  activeConnections: number;
  formats?: string[];
  checkedAt: string;
};
/** One of the provider's channel groups, in provider order. Read-only. */
export type SourceGroup = {
  name: string;
  channels: number;
  adult?: boolean;
  /** The owner's rules leave this group out. */
  excluded?: boolean;
};
/** Owner rules that shape a playlist source's lineup; a save re-imports the source. */
export type GroupRules = {
  /** Group names left out; their channels are not imported. */
  excludeGroups?: string[];
  /** "exclude": groups that appear on a later refresh are left out until the owner includes them. */
  newGroups?: "" | "exclude";
  /** Leave out every group flagged adult. */
  hideAdult?: boolean;
  /** Empty: the provider's order and numbers. Otherwise channels are numbered 1… down this group order. */
  groupOrder?: string[];
};
/** How gently Ether uses a provider's streams (see SourceLimits). */
export type SourceLimitRules = {
  /** Fewer connections than the panel allows; empty uses the panel's limit (or none). */
  maxConnections?: number;
  /** Seconds between new provider streams: empty automatic, negative off. */
  tuneGapSeconds?: number;
  /** New provider streams per hour; empty is no cap. */
  tunesPerHour?: number;
};
/** Group names match case-insensitively, ignoring surrounding spaces (as the server does). */
export const normGroup = (name: string) => name.trim().toLowerCase();
export type Source = {
  useLiveTV?: boolean;
  useVOD?: boolean;
  vodStatus?: { movies: number; series: number; updated: number; error?: string };
  lineupId?: string;
  country?: string;
  postalCode?: string;
  kind?: string;
  id: string;
  name: string;
  playlistUrl?: string;
  guideUrl?: string;
  updatedAt: string;
  refreshHours?: number;
  channelCount?: number;
  guideChannelCount?: number;
  programmeCount?: number;
  /** VOD entries the last playlist import left out. */
  skippedVod?: number;
  guideFrom?: string;
  guideUntil?: string;
  lastError?: string;
  nextRefresh?: string;
  hasPlaylist?: boolean;
  hasGuide?: boolean;
  auth?: SourceAuth;
  refreshFailures?: number;
  account?: SourceAccount;
  groups?: SourceGroup[];
} & GroupRules &
  SourceLimitRules;
export type SourceDraft = {
  useLiveTV?: boolean;
  useVOD?: boolean;
  lineupId?: string;
  country?: string;
  postalCode?: string;
  kind?: string;
  id: string;
  name: string;
  playlistUrl: string;
  guideUrl: string;
  playlist: string;
  guide: string;
  refreshHours?: number;
  auth?: SourceAuth;
} & GroupRules &
  SourceLimitRules;
/** Live progress of a manual import, polled while the save request runs. */
export type SourceProgress = {
  phase: "starting" | "playlist" | "guide" | "saving" | string;
  read: number;
  total?: number;
  channels?: number;
  programmes?: number;
  savedChannels?: number;
  savedProgrammes?: number;
  started: string;
  updated: string;
};
export type Programme = {
  id: string;
  channelId: string;
  start: string;
  end: string;
  item: Item;
  reason?: string;
  slate?: { kind: string };
};
export function plexLineupName(title: string): string {
  return title.replace(/\s*\([\d,]+\s+channels?\)$/i, "").trim();
}
export function plexLineupCount(title: string): number | null {
  const match = title.match(/\(([\d,]+)\s+channels?\)$/i);
  return match ? Number(match[1]!.replaceAll(",", "")) : null;
}
export type GuideChannel = {
  sourceKind?: string;
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
  method?: "id" | "name" | "alias" | "ai" | "manual";
  confidence?: number;
  reason?: string;
  proposalKey?: string;
  reviewRegion?: boolean;
};
export type GuideCommitSelection = {
  channelId: string;
  guideSourceId: string;
  guideId: string;
  expectedGuideSourceId: string;
  expectedGuideId: string;
  proposalKey?: string;
};
export type MatchResult = {
  undoToken?: string;
  ai?: {
    configured: boolean;
    estimatedCalls: number;
    inputTokens: number;
    cached: number;
    calls: number;
    deferred: number;
  };
  matched: number;
  unmatched: number;
  skipped?: number;
  ambiguous?: number;
  regionRejected?: number;
  changes: MatchChange[] | null;
};

const enc = encodeURIComponent;
const json = (method: string, body: unknown): RequestInit => ({
  method,
  body: JSON.stringify(body),
});

/**
 * One viewer's Live TV access: the only channel allow-list. The viewer's page
 * and each channel's Viewers tab both edit it. Restricted viewers see only
 * `channels` (none when empty); the rest see every channel, new ones too.
 */
export type LiveTVPolicy = {
  userId: number;
  disabled: boolean;
  restricted: boolean;
  channels?: string[];
  /** Private channels an unrestricted viewer was given. */
  granted?: string[];
  favorites?: string[];
  maxRating?: string;
  personal?: { enabled: boolean | null; name: string };
};

/**
 * Whether a viewer's policy (none: every public channel) lets them watch a
 * channel. A private channel needs to be in a restricted viewer's list, or
 * granted to an unrestricted one.
 */
export function policyPermits(
  p: LiveTVPolicy | undefined,
  channelId: string,
  isPrivate = false,
): boolean {
  if (p?.disabled) return false;
  if (p?.restricted) return (p.channels ?? []).includes(channelId);
  return !isPrivate || (p?.granted ?? []).includes(channelId);
}

/** Live viewing and provider usage (GET /livetv/usage). */
export type LiveUsage = {
  sessions: number;
  viewers: number;
  watching: { channelId: string; viewers: number }[];
  sources: {
    sourceId: string;
    /** Provider connections Ether holds now, and the most it may hold (0: no limit). */
    open: number;
    limit: number;
    /** New provider streams in the last hour; perHour is the owner's cap (0: none). */
    startsLastHour: number;
    perHour: number;
    gapSeconds: number;
  }[];
};
export const CHANNELS_PER_PAGE = 200;

export const tv = {
  usage: async (signal?: AbortSignal): Promise<LiveUsage> => {
    const u = await apiFetch<Partial<LiveUsage> | null>("/livetv/usage", { signal });
    return {
      sessions: u?.sessions ?? 0,
      viewers: u?.viewers ?? 0,
      watching: Array.isArray(u?.watching) ? u.watching : [],
      sources: Array.isArray(u?.sources) ? u.sources : [],
    };
  },
  plexLineups: (country: string, postalCode: string) =>
    apiFetch<PlexLineup[]>(
      `/livetv/plex-guide/lineups?country=${enc(country)}&postalCode=${enc(postalCode)}`,
    ),
  channels: (signal?: AbortSignal) => apiFetch<Channel[]>("/livetv/channels", { signal }),
  channelPage: (
    q: {
      offset?: number;
      limit?: number;
      q?: string;
      filter?: string;
      source?: string;
      group?: string;
    },
    signal?: AbortSignal,
  ) => {
    const p = new URLSearchParams();
    p.set("offset", String(q.offset ?? 0));
    p.set("limit", String(q.limit ?? CHANNELS_PER_PAGE));
    if (q.q?.trim()) p.set("q", q.q.trim());
    if (q.filter) p.set("filter", q.filter);
    if (q.source) p.set("source", q.source);
    if (q.group) p.set("group", q.group);
    return apiFetch<ChannelPage>(`/livetv/channels/page?${p}`, { signal });
  },
  channel: (id: string, signal?: AbortSignal) =>
    apiFetch<Channel>(`/livetv/channels/${enc(id)}`, { signal }),
  channelSummaries: (signal?: AbortSignal) =>
    apiFetch<ChannelSummary[]>("/livetv/channels/summary", { signal }),
  sources: (signal?: AbortSignal) => apiFetch<Source[]>("/livetv/sources", { signal }),
  guide: (from: Date, until: Date, channels?: string[], signal?: AbortSignal) => {
    const p = new URLSearchParams({
      from: from.toISOString(),
      until: until.toISOString(),
    });
    if (channels?.length) p.set("channels", channels.join(","));
    return apiFetch<Programme[]>(`/livetv/guide?${p}`, { signal });
  },
  updateChannels: (
    ids: string[],
    patch: { enabled?: boolean; favorite?: boolean; group?: string },
  ) => apiFetch<{ updated: number }>("/livetv/channels/bulk", json("PATCH", { ids, ...patch })),
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
  play: (id: string, viewer?: string, hevc = false, hevc10 = false) =>
    apiFetch<PreviewSession>(
      `/livetv/channels/${enc(id)}/play?${new URLSearchParams({ ...(viewer ? { viewer } : {}), ...(hevc ? { hevc: "1" } : {}), ...(hevc10 ? { hevc10: "1" } : {}) })}`,
      { method: "POST", body: "{}" },
    ),
  stopPreview: (session: string) =>
    apiFetch(`/livetv/sessions/${enc(session)}`, { method: "DELETE" }),
  policies: (signal?: AbortSignal) => apiFetch<LiveTVPolicy[]>("/livetv/policies", { signal }),
  /** Allow or deny viewers on one channel by editing their policies. */
  setChannelViewers: (id: string, change: { allow: number[]; deny: number[] }) =>
    apiFetch(`/livetv/channels/${enc(id)}/viewers`, json("PUT", change)),
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
  saveSource: (s: SourceDraft, pending = false, progress = "") => {
    const q = new URLSearchParams();
    if (pending) q.set("pending", "1");
    if (progress) q.set("progress", progress);
    const suffix = q.size ? `?${q}` : "";
    return apiFetch<Source>(
      `/livetv/sources${s.id ? `/${enc(s.id)}` : ""}${suffix}`,
      json(s.id ? "PUT" : "POST", s),
    );
  },
  sourceProgress: (id: string) =>
    apiFetch<SourceProgress | null>(`/livetv/sources/progress/${enc(id)}`),
  uploadSource: (id: string, part: "playlist" | "guide", file: File) =>
    apiFetch<Source>(`/livetv/sources/${enc(id)}/${part}`, {
      method: "POST",
      body: file,
      headers: { "Content-Type": "application/octet-stream" },
    }),
  refreshSource: (id: string) =>
    apiFetch<Source>(`/livetv/sources/${enc(id)}/refresh`, { method: "POST" }),
  deleteSource: (id: string) => apiFetch(`/livetv/sources/${enc(id)}`, { method: "DELETE" }),
  guideChannels: (q: string, signal?: AbortSignal, limit = 50, source?: string) =>
    apiFetch<GuideChannel[] | null>(
      `/livetv/guide/channels?q=${enc(q)}&limit=${limit}${source ? `&source=${enc(source)}` : ""}`,
      { signal },
    ),
  coverage: (signal?: AbortSignal) =>
    apiFetch<Coverage[] | null>("/livetv/guide/coverage", { signal }),
  match: (
    body: {
      channelIds?: string[];
      overwrite?: boolean;
      dryRun?: boolean;
      ai?: "estimate" | "propose" | "apply" | "commit" | "undo";
      guideSourceId?: string;
      proposalKeys?: Record<string, string>;
      selections?: GuideCommitSelection[];
      undoToken?: string;
    },
    signal?: AbortSignal,
  ) => apiFetch<MatchResult>("/livetv/guide/match", { ...json("POST", body), signal }),
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

/** The Live TV source kind of an installed add-on's channels. */
export const addonChannelsKind = (pluginId: string) => `plugin:${pluginId}/channels`;

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
    key: "personal",
    name: t("Made for each viewer"),
    tagline: t("Their favourites, their next episodes, their channel"),
    hue: 180,
    apply: (c) => ({ ...c, name: t("Personal TV"), personal: personalRecipe() }),
  },
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
/** IANA zone of this browser. Empty means UTC, which the schedule engine already treats as the default. */
export function browserTimeZone(): string {
  try {
    const zone = Intl.DateTimeFormat().resolvedOptions().timeZone;
    return zone && zone !== "UTC" ? zone : "";
  } catch {
    return "";
  }
}

export function timeZones(): string[] {
  const intl = Intl as typeof Intl & { supportedValuesOf?: (key: string) => string[] };
  const zones = intl.supportedValuesOf?.("timeZone") ?? [];
  return zones.includes("UTC") ? zones : ["UTC", ...zones];
}

/** What a block chip should say before the clock range. Empty when the block has no programming yet. */
export function blockPlays(block: Block): string {
  const name = block.name?.trim();
  if (name === "Cartoons") return t("Cartoons");
  if (name === "Movie night") return t("Movie night");
  if (name) return name;
  const genres = block.filter?.genres;
  if (Array.isArray(genres)) {
    const labels = genres.map((genre) => String(genre).trim()).filter(Boolean);
    if (labels.length) return labels.join(", ");
  }
  if (block.filter?.type === "movie") return t("Movies");
  if (block.filter?.type === "show") return t("TV shows");
  const items = block.items ?? [];
  if (items.length > 1) return tn(items.length, "{count} title", "{count} titles");
  if (items[0]?.title) return items[0].title;
  const source = block.sources?.find((item) => item.name?.trim());
  if (source?.name) return source.name;
  if (block.sources?.length) return t("Mix");
  if (block.collection) return t("Collection");
  return "";
}

export function blockLabel(block: Block): string {
  const end = block.end === 1440 ? "24:00" : timeOfDay(block.end);
  const range = `${timeOfDay(block.start)}–${end}`;
  const what = blockPlays(block);
  return what ? `${what} · ${range}` : range;
}

export const timeOfDay = (minutes: number) =>
  minutes === 1440
    ? "24:00"
    : `${String(Math.floor(minutes / 60)).padStart(2, "0")}:${String(minutes % 60).padStart(2, "0")}`;
export const minutesOfDay = (value: string) => {
  const [h = NaN, m = NaN] = value.split(":").map(Number);
  return h * 60 + m;
};
// Reuse ICU formatters: the guide formats each programme's endpoints several
// times per render. Date.toLocale* constructs a formatter for every call.
const clockFormats = new Map<string, Intl.DateTimeFormat>();
function clockFormat(day: boolean) {
  const locale = getLocale();
  const key = `${locale}:${day}`;
  let format = clockFormats.get(key);
  if (!format) {
    format = new Intl.DateTimeFormat(locale, {
      ...(day ? { weekday: "short" as const } : {}),
      hour: "numeric",
      minute: "2-digit",
    });
    clockFormats.set(key, format);
  }
  return format;
}
export const clock = (d: Date | string) => clockFormat(false).format(new Date(d));
export const dayClock = (d: Date | string) => clockFormat(true).format(new Date(d));
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
  return programmes?.find((p) => !p.slate && Date.parse(p.start) > at);
}

export function progress(p: Programme, at = Date.now()): number {
  const s = Date.parse(p.start);
  const e = Date.parse(p.end);
  return e > s ? Math.min(1, Math.max(0, (at - s) / (e - s))) : 0;
}

const collator = new Intl.Collator();

export function byNumber(a: Channel, b: Channel): number {
  return (
    (parseFloat(a.number) || 0) - (parseFloat(b.number) || 0) ||
    collator.compare(a.number, b.number) ||
    collator.compare(a.name, b.name)
  );
}

/**
 * Channels in lineup order (see byNumber). Each number is parsed once rather
 * than per comparison, which matters for provider lineups of 16,000.
 */
export function sortByNumber(channels: Channel[]): Channel[] {
  const keyed = channels.map((c) => ({ c, n: parseFloat(c.number) || 0 }));
  keyed.sort(
    (a, b) =>
      a.n - b.n || collator.compare(a.c.number, b.c.number) || collator.compare(a.c.name, b.c.name),
  );
  return keyed.map((k) => k.c);
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

/** Sources that supply guide listings: XMLTV guides, Plex lineups, and M3U playlists with a guide. */
export const isGuideSource = (s: Source) =>
  s.kind === "plex" || s.hasGuide === true || Boolean(s.guideUrl) || (s.guideChannelCount ?? 0) > 0;

/** Sources that supply channels; guide-only XMLTV sources and Plex lineups are not. */
export const isChannelSource = (s: Source) =>
  s.kind !== "plex" &&
  s.hasPlaylist !== false &&
  !(!s.playlistUrl && !s.channelCount && (s.guideChannelCount ?? 0) > 0);

/** Catalogue snapshots retain TMDB paths; console images use the image proxy. */
export function programmeArtwork(src?: string): string | undefined {
  return src && /^\/[A-Za-z0-9_-]+\.(jpg|png|webp)$/i.test(src)
    ? `/ether/api/image?path=${encodeURIComponent(src)}&w=342`
    : src;
}
