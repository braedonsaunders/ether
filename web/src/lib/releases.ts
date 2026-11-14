import { localized, t } from "../i18n";
import { apiFetch } from "./api";
import type { QualityProfile, ResolverTrace, ReleasePreferenceResult } from "./types";

/** Release console API (/ether/api/releases): search, pins, blocklist, history. */

export interface ReleaseInfo {
  title?: string;
  edition?: string;
  year?: number;
  seasons?: number[] | null;
  episodes?: number[] | null;
  fullSeason?: boolean;
  height?: number;
  source?: string;
  videoCodec?: string;
  hdr10?: boolean;
  dolbyVision?: boolean;
  audioCodec?: string;
  atmos?: boolean;
  channels?: string;
  group?: string;
  languages?: string[] | null;
  proper?: boolean;
  repack?: boolean;
}

export interface Release {
  id: string;
  guid?: string;
  indexer: string;
  name: string;
  size: number;
  pubDate: string;
  grabs?: number;
  password?: boolean;
  info: ReleaseInfo;
  score: number;
}

interface FormatMatch {
  id: string;
  name: string;
  score: number;
}

export interface HealthResult {
  releaseId: string;
  checkedAt: string;
  healthy: boolean;
  reason?: string;
  duration?: number;
  /** "check" (an owner check), "verified" (passed during playback) or "blocked". */
  source: "check" | "verified" | "blocked";
  blocked?: boolean;
}

export interface ConsoleRelease {
  preferences?: ReleasePreferenceResult;
  release: Release;
  score: number;
  formatScore: number;
  /** Present only when this profile rejected the custom format score. */
  minimumFormatScore?: number;
  customFormats?: FormatMatch[] | null;
  compatibility?: string;
  /** Why source routing moved this release (faster source, or the hedge). */
  route?: string;
  rejected: boolean;
  reason?: string;
  /** Position in the order Ether tries releases (the pin first); 0 when rejected. */
  rank?: number;
  winner?: boolean;
  blocked?: boolean;
  blockReason?: string;
  pinned?: boolean;
  health?: HealthResult | null;
}

type PinStatus = "" | "healthy" | "unhealthy" | "rejected" | "unavailable";

export interface Pin {
  titleKey: string;
  titleName: string;
  release: Release;
  note?: string;
  pinnedAt: string;
  lastStatus?: PinStatus;
  lastReason?: string;
  lastCheckedAt?: string;
}

export interface ConsoleSearch {
  titleKey: string;
  titleName: string;
  profile: string;
  cached: boolean;
  searchedAt: string;
  duration: number;
  winner?: string;
  pin?: Pin | null;
  pinNote?: string;
  releases: ConsoleRelease[] | null;
}

export interface ConsoleSeasonPack {
  id: string;
  name: string;
  indexer: string;
  size: number;
  info: ReleaseInfo;
  score: number;
  formatScore: number;
  episodeReleases: Record<string, string>;
}

export interface ConsoleSeasonEpisode {
  key: string;
  season: number;
  episode: number;
  title: string;
  aired?: string;
  winner?: string;
  pin?: Pin | null;
  releases: ConsoleRelease[] | null;
}

export interface ConsoleSeasonSearch {
  seasonKey: string;
  showKey?: string;
  titleName: string;
  season: number;
  profile: string;
  searchedAt: string;
  duration: number;
  packs: ConsoleSeasonPack[] | null;
  episodes: ConsoleSeasonEpisode[] | null;
}

export interface BlockEntry {
  releaseId: string;
  name?: string;
  indexer?: string;
  titleKey: string;
  titleName?: string;
  reason: string;
  source: "auto" | "owner";
  manual: boolean;
  createdAt: string;
  updatedAt: string;
}

export interface HistoryRecord {
  id: number;
  at: string;
  titleKey: string;
  titleName: string;
  profile?: string;
  /** Max-height ceilings (configured profile / client cap / effective).
   * The configured value is presence-carrying: absent on pre-tracking
   * records (unknown), an explicit 0 means no ceiling ("Any"). */
  profileMaxHeight?: number;
  clientMaxHeight?: number;
  effectiveMaxHeight?: number;
  releaseId?: string;
  releaseName?: string;
  indexer?: string;
  size?: number;
  error?: string;
  failover?: boolean;
  pinned?: boolean;
  /** Who chose the release. Absent on older decisions. */
  chooser?: "auto" | "viewer";
  chosenVersion?: string;
  candidates: number;
  trace?: ResolverTrace | null;
}

export interface ChildTitle {
  key: string;
  type: string;
  title: string;
  season?: number;
  episode?: number;
  aired?: string;
  pinned?: boolean;
}

interface ConsoleProfiles {
  default: string;
  profiles: string[];
  special: string[];
}

export type StarrApp = "radarr" | "sonarr";

/**
 * Media scopes (core.MediaAny/Movie/TV). Every profile serves exactly one
 * media; a client rule may leave it empty to apply to every media.
 */
export const MEDIA_ANY = "";
export const MEDIA_MOVIE = "movie";
export const MEDIA_TV = "tv";
export type ProfileMedia = typeof MEDIA_MOVIE | typeof MEDIA_TV;

/**
 * The media a profile can serve, in console order. Mirrors
 * core.ProfileMediaTypes; every media option in the console comes from
 * this list, so a new media (such as anime) is one more entry here.
 */
export const PROFILE_MEDIA: readonly ProfileMedia[] = [MEDIA_MOVIE, MEDIA_TV];

/** A profile's media. */
export function profileMediaType(profile: { mediaType?: string }): string {
  return profile.mediaType ?? MEDIA_ANY;
}

/** Whether a profile or rule scope applies when choosing for media ("" = unknown). */
export function mediaApplies(scope: string | undefined, media: string | undefined): boolean {
  if (!media) return true;
  return !scope || scope === media;
}

/** "Movies", "Shows", or "Movies & shows" (a rule for every media). */
export function mediaLabel(media: string | undefined): string {
  if (media === MEDIA_MOVIE) return t("Movies");
  if (media === MEDIA_TV) return t("Shows");
  return t("Movies & shows");
}

/**
 * Profile names a client rule for media can pick. A movie and a TV profile
 * may share a name; a rule for every media names one that each media has,
 * and each media uses its own profile of that name.
 */
export function ruleProfileNames(
  profiles: { name: string; mediaType?: string }[] | null | undefined,
  media: string | undefined,
): string[] {
  const list = profiles ?? [];
  const names = [...new Set(list.map((p) => p.name))];
  return names.filter((name) =>
    (media ? [media] : PROFILE_MEDIA).every((m) =>
      list.some((p) => p.name === name && p.mediaType === m),
    ),
  );
}

/** The media an import app's profiles describe. */
export function mediaTypeForApp(app: StarrApp): ProfileMedia {
  return app === "sonarr" ? MEDIA_TV : MEDIA_MOVIE;
}

/** The Starr app whose guide describes this media, or null for both. */
export function appForMedia(media: string | undefined): StarrApp | null {
  if (media === MEDIA_MOVIE) return "radarr";
  if (media === MEDIA_TV) return "sonarr";
  return null;
}

/** Profiles that apply to media, in their original order. */
export function compatibleProfiles<T extends { mediaType?: string }>(
  profiles: T[] | null | undefined,
  media: string | undefined,
): T[] {
  return (profiles ?? []).filter((p) => mediaApplies(p.mediaType, media));
}

/** A TRaSH Guides or Profilarr (Dictionarry) quality profile offered for import. */
export interface TrashProfile {
  /** File/profile slug; what the import endpoints take. */
  id: string;
  trashId?: string;
  name: string;
  /** Plain text; never HTML. */
  description?: string;
  group?: number;
  /** The guide page (https://trash-guides.info or https://dictionarry.dev). */
  url?: string;
  scoreSet?: string;
  /** Formats the profile itself names. */
  formats: number;
  /** Formats its default-on groups add. */
  defaultFormats?: number;
}

/** The import picker's list; `warning` reports guide groups that failed to load. */
export interface TrashProfileList {
  items: TrashProfile[];
  warning?: string;
}

/** present: already saved; add: added by the import. */
type ImportFormatStatus = "present" | "add" | "unsupported" | "missing";

export interface ImportFormat {
  id: string;
  name: string;
  score: number;
  status: ImportFormatStatus;
  reason?: string;
}

/** A previewed import: the profile as it will be saved and its custom formats. */
export interface QualityImportPlan {
  app: StarrApp;
  /** "trash", "dictionarry", or "" for pasted JSON. */
  source?: "trash" | "dictionarry" | "";
  /** Source profile id/slug used to fetch it. */
  profileId?: string;
  trashId?: string;
  /** The media the source app's profile describes. */
  mediaType?: ProfileMedia;
  /** Non-empty when the profile cannot be imported. A score shortfall is a warning. */
  unusable?: string;
  profile: QualityProfile;
  scoreSet?: string;
  formats: ImportFormat[];
  skippedQualities: string[];
  warnings: string[];
}

/** POST /releases/quality/sync: what the automatic guide sync changed. */
export interface QualitySyncReport {
  profilesUpdated: number;
  formatsAdded: number;
  formatsUpdated: number;
  formatsRemoved: number;
  warnings?: string[] | null;
}

/** A TRaSH or Profilarr (Dictionarry) profile by id, or pasted profile JSON. */
export type QualityImportSource = { app: StarrApp; source?: "trash" | "dictionarry" } & (
  { profile: string; json?: undefined } | { json: string; profile?: undefined }
);

/** Profile names the server understands beside the configured ones. */
export const PROFILE_NONE = "none";
export const PROFILE_DOWNLOAD = "download";

function qs(params: Record<string, string | number | undefined | null>): string {
  const q = new URLSearchParams();
  for (const [k, v] of Object.entries(params))
    if (v !== undefined && v !== null && v !== "") q.set(k, String(v));
  const s = q.toString();
  return s ? `?${s}` : "";
}

const base = "/releases";
const post = <T>(path: string, body: unknown) =>
  apiFetch<T>(base + path, { method: "POST", body: JSON.stringify(body) });
const del = (path: string) => apiFetch<null>(base + path, { method: "DELETE" });

export const releaseConsole = {
  profiles: (media?: string) =>
    apiFetch<ConsoleProfiles>(
      `${base}/profiles${media ? `?media=${encodeURIComponent(media)}` : ""}`,
    ),
  search: (key: string, opts: { profile?: string; fresh?: boolean } = {}, signal?: AbortSignal) =>
    apiFetch<ConsoleSearch>(
      `${base}/search${qs({ key, profile: opts.profile, fresh: opts.fresh ? 1 : undefined })}`,
      { signal },
    ),
  season: (
    target: { key?: string; show?: string; season?: number },
    opts: { profile?: string; fresh?: boolean } = {},
    signal?: AbortSignal,
  ) =>
    apiFetch<ConsoleSeasonSearch>(
      `${base}/season${qs({
        key: target.key,
        show: target.show,
        season: target.season,
        profile: opts.profile,
        fresh: opts.fresh ? 1 : undefined,
      })}`,
      { signal },
    ),
  children: async (key: string, signal?: AbortSignal) =>
    (
      await apiFetch<{ items: ChildTitle[] | null }>(
        `${base}/titles/${encodeURIComponent(key)}/children`,
        { signal },
      )
    ).items ?? [],
  checkHealth: (releaseId: string, key?: string) =>
    post<HealthResult>("/health", { releaseId, key: key ?? "" }),
  forget: (key: string) => post<{ ok: boolean }>("/forget", { key }),
  pins: async (signal?: AbortSignal) =>
    (await apiFetch<{ pins: Pin[] | null }>(`${base}/pins`, { signal })).pins ?? [],
  pin: (key: string, releaseId: string, note: string) =>
    post<Pin>("/pins", { key, releaseId, note }),
  pinBatch: async (pins: { key: string; releaseId: string }[], note: string) =>
    (await post<{ pins: Pin[] | null }>("/pins/batch", { pins, note })).pins ?? [],
  unpin: (key: string) => del(`/pins/${encodeURIComponent(key)}`),
  blocklist: async (
    params: { key?: string; source?: string; q?: string; limit?: number; offset?: number },
    signal?: AbortSignal,
  ) => {
    const res = await apiFetch<{ items: BlockEntry[] | null; total: number }>(
      `${base}/blocklist${qs(params)}`,
      { signal },
    );
    return { items: res.items ?? [], total: res.total ?? 0 };
  },
  block: (body: {
    releaseId: string;
    key?: string;
    reason: string;
    name?: string;
    indexer?: string;
  }) => post<BlockEntry>("/blocklist", body),
  unblock: (releaseId: string) => del(`/blocklist/${encodeURIComponent(releaseId)}`),
  history: async (
    params: { key?: string; limit?: number; offset?: number },
    signal?: AbortSignal,
  ) => {
    const res = await apiFetch<{ items: HistoryRecord[] | null; total: number }>(
      `${base}/history${qs(params)}`,
      { signal },
    );
    return { items: res.items ?? [], total: res.total ?? 0 };
  },
  historyEntry: (id: number) => apiFetch<HistoryRecord>(`${base}/history/${id}`),
  trashProfiles: (app: StarrApp, signal?: AbortSignal) =>
    apiFetch<TrashProfileList>(`${base}/trash/${app}/profiles`, { signal }),
  dictionarryProfiles: (app: StarrApp, signal?: AbortSignal) =>
    apiFetch<TrashProfileList>(`${base}/dictionarry/${app}/profiles`, { signal }),
  previewImport: (source: QualityImportSource, signal?: AbortSignal) =>
    apiFetch<QualityImportPlan>(`${base}/quality-import/preview`, {
      method: "POST",
      body: JSON.stringify(source),
      signal,
    }),
  importProfile: (source: QualityImportSource, name: string) =>
    post<{ name: string; formatsAdded: number }>("/quality-import", { ...source, name }),
  /** Run the automatic guide sync once and return what it changed. */
  syncQuality: () => post<QualitySyncReport>("/quality/sync", {}),
};

/** "WEB-DL 1080p" for an imported quality gate such as "webdl-1080"; DVD has no resolution. */
export function qualityGateLabel(gate: string): string {
  const [source = "", height = ""] = gate.split("-");
  const name = SOURCES[source] ?? source;
  return source === "dvd" ? name : `${name} ${height === "2160" ? "4K" : `${height}p`}`;
}

/** "Blu-ray" for a source key. */
export function sourceLabel(source: string): string {
  return SOURCES[source] ?? source;
}

/** "4K", "1080p", or "SD" for unknown heights. */
function resolutionLabel(info: ReleaseInfo | undefined): string {
  const h = info?.height ?? 0;
  if (h >= 2160) return "4K";
  if (h > 0) return `${h}p`;
  return "SD";
}

const SOURCES: Record<string, string> = {
  remux: "Remux",
  bluray: "Blu-ray",
  webdl: "WEB-DL",
  webrip: "WEBRip",
  hdtv: "HDTV",
  dvd: "DVD",
  screener: "Screener",
  cam: "CAM",
};

/** "4K Remux · HEVC · TrueHD Atmos" from parsed release info. */
export function qualityLabel(info: ReleaseInfo | undefined): string {
  if (!info) return t("Unknown");
  const parts = [resolutionLabel(info)];
  if (info.source && SOURCES[info.source]) parts[0] += ` ${SOURCES[info.source]}`;
  if (info.videoCodec) parts.push(info.videoCodec.toUpperCase());
  if (info.audioCodec)
    parts.push(
      `${info.audioCodec.toUpperCase()}${info.channels ? ` ${info.channels}` : ""}${info.atmos ? " Atmos" : ""}`,
    );
  return parts.join(" · ");
}

/** Dynamic range badge text, or "" for SDR. */
export function rangeLabel(info: ReleaseInfo | undefined): string {
  if (info?.dolbyVision && info?.hdr10) return "DV · HDR10";
  if (info?.dolbyVision) return "Dolby Vision";
  if (info?.hdr10) return "HDR10";
  return "";
}

/** Whole days since a release was posted, or null when unknown. */
export function ageDays(pubDate: string | undefined, now: Date = new Date()): number | null {
  if (!pubDate) return null;
  const ms = new Date(pubDate).getTime();
  if (!Number.isFinite(ms) || new Date(pubDate).getUTCFullYear() < 1971) return null;
  return Math.max(0, Math.floor((now.getTime() - ms) / 86400000));
}

export const PIN_STATUS_LABEL = localized<Record<Exclude<PinStatus, "">, string>>(() => ({
  healthy: t("Playing"),
  unhealthy: t("Failing"),
  rejected: t("Not playable here"),
  unavailable: t("Check failed"),
}));
