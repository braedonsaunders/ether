// Style Studio contract (docs/style-studio.md). Every worker builds against
// these shapes. Edits must stay additive unless the orchestrator agrees.
import { apiFetch } from "../../../lib/api";
import type { Scene } from "./scene-renderer";
import type { BugSettings, StyleScreens } from "./model";

/** Playout roles. A style holds any number of scenes per role. */
export type Role =
  "ident" | "tonight" | "upnext" | "break" | "back" | "offair" | "welcome" | "bug" | "lowerthird";
export const ROLES: Role[] = [
  "ident",
  "upnext",
  "tonight",
  "break",
  "back",
  "offair",
  "welcome",
  "lowerthird",
  "bug",
];

/** Brand kit: style-wide tokens every scene can bind to. */
export type Brand = {
  palette: { background: string; accent: string; text: string; glow: string };
  type: { display: string; body: string };
  /** Asset digest of an image, or up to 3 characters drawn as a monogram. */
  logo?: string;
  tagline: string;
  /** 0..1, scales behaviour amplitude/speed through the {energy} token. */
  energy: number;
  /** Lowercase post textures: grain, flicker, scanlines, vignette, chroma. */
  textures: string[];
  language: string;
};

export type SceneSound = {
  id: string;
  assetDigest: string;
  /** Seconds into the scene when the sound starts. */
  at: number;
  /** Trim window inside the source asset, seconds. end omitted = to the end. */
  start: number;
  end?: number;
  volume: number;
  fadeIn: number;
  fadeOut: number;
};

export type SceneVideo = {
  assetDigest: string;
  /** background: graphics composite over the video; replace: the video is the scene. */
  mode: "background" | "replace";
  fit: "cover" | "contain";
  /** Seconds into the source asset to start from. */
  start: number;
  crossfade?: boolean;
};

export type StyleScene = {
  id: string;
  role: Role;
  name: string;
  enabled: boolean;
  /** Relative rotation weight among enabled scenes of the same role, 1..10. */
  weight: number;
  /** Full scene document v1. duration always equals the role's duration. */
  document: Scene;
  sounds: SceneSound[];
  video?: SceneVideo;
};

export type ControlKind = "color" | "font" | "text" | "number" | "toggle" | "image";
/** Remix control. Layers bind to it with the token "{control.<id>}". */
export type StyleControl = {
  id: string;
  label: string;
  kind: ControlKind;
  value: string | number | boolean;
  min?: number;
  max?: number;
  step?: number;
  group?: string;
};

export type Style = {
  id: string;
  name: string;
  /** Template the style was created from. Informational; never read at render time. */
  template: string;
  brand: Brand;
  /** Duration per role in seconds: a multiple of 4 from 4 to 30. */
  roles: Partial<Record<Role, { duration: number }>>;
  scenes: StyleScene[];
  controls: StyleControl[];
  channelIds: string[];
  channelCount?: number;
  bug?: BugSettings;
  screens?: StyleScreens;
  pack?: { id: string; name: string };
  /** Server render progress across every enabled scene. */
  render?: { done: number; total: number };
};

export type Template = {
  id: string;
  name: string;
  /** Short mood line for the gallery, already translated by the server. */
  blurb: string;
  tags: string[];
  /** A complete, owned style (id ""), ready to POST as a new style. */
  style: Style;
};

export type AssetKind = "image" | "font" | "audio" | "video";
export type LibraryAsset = {
  digest: string;
  name: string;
  kind: AssetKind;
  size: number;
  mime: string;
  license?: string;
  source?: string;
  attribution?: string;
  status?: "ready" | "queued" | "downloading" | "converting" | "error";
  progress?: number;
  error?: string;
  /** Media facts filled by the server once known. */
  durationMs?: number;
  width?: number;
  height?: number;
  /** Styles, scenes and channels that reference the asset. */
  usedBy: { kind: "style" | "channel" | "reel"; id: string; name: string; scene?: string }[];
  createdAt: number;
};

const enc = encodeURIComponent;
const send = <T>(path: string, method: string, body?: unknown) =>
  apiFetch<T>(path, { method, body: body instanceof FormData ? body : JSON.stringify(body) });

/** Style Studio API. Paths are relative to /ether/api. */
export const studio = {
  templates: () => apiFetch<Template[]>("/onair/templates"),
  styles: () => apiFetch<Style[]>("/onair/looks"),
  style: (id: string) => apiFetch<Style>(`/onair/looks/${enc(id)}`),
  createStyle: (style: Style) => send<Style>("/onair/looks", "POST", style),
  saveStyle: (style: Style) => send<Style>(`/onair/looks/${enc(style.id)}`, "PUT", style),
  /** Validates one scene document and returns the server's normalized copy or a 400 message. */
  validateScene: (scene: StyleScene) => send<StyleScene>("/onair/scenes/validate", "POST", scene),
  /** AI: draft or revise a style (whole) or one scene. Returns a proposal, never saves. */
  draft: (body: { prompt: string; style: Style; sceneId?: string }) =>
    send<{ style: Style; summary: string }>("/onair/draft", "POST", body),
  exportPack: (id: string) => `/ether/api/onair/looks/${enc(id)}/pack`,
  importPack: (file: File) => {
    const data = new FormData();
    data.append("file", file);
    return send<Style>("/onair/packs", "POST", data);
  },
  library: () => apiFetch<LibraryAsset[]>("/onair/assets"),
  asset: (digest: string) => apiFetch<LibraryAsset>(`/onair/assets/${enc(digest)}/info`),
  renameAsset: (digest: string, name: string) =>
    send<LibraryAsset>(`/onair/assets/${enc(digest)}`, "PATCH", { name }),
  replaceAsset: (digest: string, file: File) => {
    const data = new FormData();
    data.append("file", file);
    return send<LibraryAsset>(`/onair/assets/${enc(digest)}/replace`, "POST", data);
  },
  /** Raw bytes with Range support (audio/video/image/font). */
  media: (digest: string) => `/ether/api/onair/assets/${enc(digest)}`,
  /** 480px-wide JPEG: first frame of video, image downscale, font specimen. */
  thumb: (digest: string) => `/ether/api/onair/assets/${enc(digest)}/thumb`,
  /** Audio or video peaks: { peaks: number[] } with 1000 buckets in 0..1. */
  waveform: (digest: string) =>
    apiFetch<{ peaks: number[]; durationMs: number }>(`/onair/assets/${enc(digest)}/waveform`),
};
