import { apiFetch } from "../../../lib/api";
export type Moment =
  "ident" | "tonight" | "upnext" | "break" | "back" | "offair" | "welcome" | "bug" | "lowerthird";
export const MOMENTS: Moment[] = [
  "ident",
  "tonight",
  "upnext",
  "break",
  "back",
  "offair",
  "welcome",
  "bug",
  "lowerthird",
];
export type BrandTokens = {
  palette: { background: string; accent: string; text: string; glow: string };
  type: { display: string; body: string };
  energy: number;
  textures: string[];
  sting: string;
  volume: number;
  seed: number;
  tagline: string;
  words: Partial<Record<Moment | "now", string>>;
  language: string;
  logo?: string;
};
export type StyleScreens = {
  offAir: import("./ScreenPicker").ScreenStyle;
  noSignal: import("./ScreenPicker").ScreenStyle;
  stationId: boolean;
  applyTo?: "converted" | "all";
};
export type BugSettings = {
  position?: "topLeft" | "topRight" | "bottomLeft" | "bottomRight";
  opacity?: number;
  style?: "monochrome" | "original";
  size?: "small" | "medium" | "large";
  source?: "logo" | "wordmark";
  fadeBreaks?: boolean;
  fadeCredits?: boolean;
};
export type Look = {
  channelCount?: number;
  bug?: BugSettings;
  screens?: StyleScreens;
  id: string;
  name: string;
  family: string;
  brand: BrandTokens;
  moments: Partial<
    Record<Moment, { duration: number; poster: number; assetDigest?: string; crossfade?: boolean }>
  >;
  layers?: Record<
    string,
    {
      text?: string;
      size?: number;
      opacity?: number;
      x?: number;
      y?: number;
      fit?: string;
      behaviour?: string;
      hidden?: boolean;
      locked?: boolean;
    }
  >;
  channelIds: string[];
  pack?: { id: string; name: string };
  render?: { done: number; total: number };
};
export type Family = { id: string; name: string; look: Look };
export type ReelClip = {
  id: string;
  name: string;
  durationMs: number;
  sourceDigest?: string;
  status: "ready" | "converting" | "error" | "queued" | "downloading";
  source?: string;
  attribution?: string;
  error?: string;
  progress?: number;
  size?: number;
  preview?: string;
  thumbnail?: string;
};
export type Reel = {
  id: string;
  name: string;
  order: "shuffle" | "sequence";
  repeatHours: number;
  loudnessMatch: boolean;
  trimSilence: boolean;
  clips: ReelClip[];
  channelIds: string[];
};
export type BreakPattern = {
  preset: string;
  seconds: number;
  minSeconds: number;
  maxSeconds: number;
  spots: number;
  minSpots: number;
  maxSpots: number;
  podsPerHalfHour: number;
  inside: boolean;
  noRepeatMinutes: number;
  stationId: boolean;
  bumperStyle: string;
  clockAlignment: string;
};
export type ChannelLook = {
  momentAssets?: Record<string, string>;
  breakPlacement?: "natural" | "even" | "between";
  tonight?: boolean;
  bug?: BugSettings;
  breakPattern?: BreakPattern;
  lookId: string;
  ident: boolean;
  upNext: boolean;
  placement?: "gap" | "insert";
  breaks?: "" | "between" | "inside";
  breakEvery?: number;
  breakContent?: "look" | "reel" | "segments";
  reelId?: string;
  segmentIds?: string[];
  breakSeconds?: number;
  offAir: boolean;
  overlays?: "" | "whenTranscoding" | "always";
  allowUnratedSegments?: boolean;
  tagline?: string;
  accent?: string;
};
export type ClockSlot = {
  segmentId: string;
  minute: number;
  everyMinutes: number;
  from: number;
  until: number;
  days: number[];
};
export type IngredientKind =
  "continue" | "nextEpisodes" | "picked" | "watchlist" | "comfort" | "newReleases" | "collections";
export type PersonalRecipe = {
  ingredients: { kind: IngredientKind; share: number; enabled: boolean; collections?: number[] }[];
  pauseWhenAway: boolean;
  nameFormat: string;
  repeatHours: number;
  maxShowShare: number;
  nightMaxMinutes: number;
};
export type Bindings = {
  channel?: { name: string; logo?: string };
  viewer?: { name: string; greeting?: string };
  now?: { title: string };
  next?: {
    title: string;
    time?: string;
    reason?: string;
    meta?: string;
    logline?: string;
    backdrop?: string;
  };
  offair?: { message: string };
  list?: { title: string; time: string; reason?: string; meta?: string; poster?: string }[];
  greeting?: string;
};
export type OnAirAsset = {
  source?: string;
  attribution?: string;
  status?: string;
  progress?: number;
  error?: string;
  digest: string;
  name: string;
  kind: string;
  size: number;
  license?: string;
  usedBy: string[];
  url?: string;
};
const enc = encodeURIComponent;
const write = <T>(path: string, method: string, body?: unknown) =>
  apiFetch<T>(path, { method, body: body instanceof FormData ? body : JSON.stringify(body) });
export const onair = {
  template: (prompt: string, base: Look) =>
    write<Look>("/onair/template", "POST", { prompt, base }),
  looks: () => apiFetch<Look[]>("/onair/looks"),
  look: (id: string) => apiFetch<Look>(`/onair/looks/${enc(id)}`),
  saveLook: (look: Look) =>
    write<Look>(`/onair/looks${look.id ? `/${enc(look.id)}` : ""}`, look.id ? "PUT" : "POST", look),
  deleteLook: (id: string) => write<void>(`/onair/looks/${enc(id)}`, "DELETE"),
  duplicateLook: (id: string) => write<Look>(`/onair/looks/${enc(id)}/duplicate`, "POST"),
  detachLook: (id: string) => write<Look>(`/onair/looks/${enc(id)}/detach`, "POST"),
  families: () => apiFetch<Family[]>("/onair/families"),
  bindings: (q: { channel?: string; at?: string; viewer?: string; moment?: string }) =>
    apiFetch<Bindings>(
      `/onair/bindings?${new URLSearchParams(Object.entries(q).filter((x): x is [string, string] => !!x[1]))}`,
    ),
  reels: () => apiFetch<Reel[]>("/onair/reels"),
  reel: (id: string) => apiFetch<Reel>(`/onair/reels/${enc(id)}`),
  saveReel: (reel: Reel) =>
    write<Reel>(`/onair/reels${reel.id ? `/${enc(reel.id)}` : ""}`, reel.id ? "PUT" : "POST", reel),
  deleteReel: (id: string) => write<void>(`/onair/reels/${enc(id)}`, "DELETE"),
  uploadClip: (id: string, file: File) => {
    const data = new FormData();
    data.append("file", file);
    return write<ReelClip>(`/onair/reels/${enc(id)}/clips`, "POST", data);
  },
  updateClip: (id: string, clip: ReelClip) =>
    write<ReelClip>(`/onair/reels/${enc(id)}/clips/${enc(clip.id)}`, "PUT", clip),
  deleteClip: (id: string, clip: string) =>
    write<void>(`/onair/reels/${enc(id)}/clips/${enc(clip)}`, "DELETE"),
  clipPreview: (id: string, clip: string) =>
    `/ether/api/onair/reels/${enc(id)}/clips/${enc(clip)}/preview`,
  assets: () => apiFetch<OnAirAsset[]>("/onair/assets"),
  uploadAsset: (file: File, kind: string, license?: string) => {
    const data = new FormData();
    data.append("file", file);
    data.append("kind", kind);
    if (license) data.append("license", license);
    return write<OnAirAsset>("/onair/assets", "POST", data);
  },
  deleteAsset: (id: string) => write<void>(`/onair/assets/${enc(id)}`, "DELETE"),
  rundown: (id: string, viewer?: string) =>
    apiFetch<{ id: string; title: string; start: string; end: string; reason: string }[]>(
      `/livetv/channels/${enc(id)}/rundown?viewer=${enc(viewer ?? "")}`,
    ),
};
