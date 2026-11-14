/**
 * Ether Plugin Protocol v1 (EPP/1) types for @ether/plugin-sdk.
 * Mirrors internal/plugins/contract.go and internal/viewspec.
 */

export type MaybePromise<T> = T | Promise<T>;

export type PluginKind =
  | "source"
  | "integration"
  | "notification_channel"
  | "livetv_source"
  | "extension"
  | "content";

export type Capability =
  | "stream_source"
  | "debrid_provider"
  | "list_source"
  | "livetv_channels"
  | "subtitles"
  | "markers"
  | "notify"
  | "events"
  | "ranking_step"
  | "rule_condition"
  | "tools"
  | "ui"
  | "catalog_provider"
  | "kind_resolver"
  | "frontend_adapter"
  | "matcher";

export type EtherEventName =
  | "playback.started"
  | "playback.paused"
  | "playback.resumed"
  | "playback.stopped"
  | "playback.completed"
  | "playback.error"
  | "playback.failover"
  | "title.available"
  | "viewer.added"
  | "viewer.access_changed"
  | "collection.rebuilt"
  | "server.health_changed";

export interface StremioCatalogDecl {
  type: string;
  id: string;
  name?: string;
  extra?: Array<{
    name: string;
    isRequired?: boolean;
    options?: string[];
    optionsLimit?: number;
  }>;
}

export interface StremioResourceDecl {
  name: string;
  types?: string[];
  idPrefixes?: string[];
}

export interface PluginAuthor {
  name?: string;
  url?: string;
}

export interface RuntimeDecl {
  kind: "hosted" | "remote";
  entry?: string;
  module?: string;
}

export interface HealthDecl {
  interval?: string;
}

export interface ScheduleEntry {
  every: string;
  call: string;
}

export interface ViewerPermissions {
  identity?: "none" | "granted_viewers" | "all";
  watch_history?: "none" | "granted_viewers" | "all";
  write_watch_state?: "none" | "granted_viewers";
}

export interface Permissions {
  network?: string[];
  catalog?: "none" | "read";
  viewers?: ViewerPermissions;
  collections?: "none" | "write";
  folders?: "none" | "read";
  secrets?: string[];
  /** Lets a kind resolver ask Ether to find bytes: search to look, acquire to open. */
  sources?: "none" | "search" | "acquire";
}

export type SettingFieldKind =
  | "group"
  | "text"
  | "secret"
  | "duration"
  | "readonly"
  | "number"
  | "bool"
  | "select"
  | "list"
  | "bytes"
  | "hidden"
  | "multiSelect"
  | "orderedList"
  | "language"
  | "viewer"
  | "collection"
  | "folder"
  | "url"
  | "title";

export interface FieldOption {
  value: string;
  label: string;
  hint?: string;
}

export interface SettingField {
  key: string;
  kind: SettingFieldKind;
  label?: string;
  hint?: string;
  required?: boolean;
  default?: unknown;
  options?: FieldOption[];
  fields?: SettingField[];
  min?: number;
  max?: number;
  placeholder?: string;
  /** Search /ether/options/{key}?q=...; selected value persists verbatim. */
  search?: boolean;
}

export interface SettingsSchema {
  $ref?: string;
  fields?: SettingField[];
  groups?: Array<{
    key?: string;
    label?: string;
    hint?: string;
    advanced?: boolean;
    fields: SettingField[];
  }>;
}

export interface SourceMeta {
  class: "debrid" | "usenet" | "indexer" | "direct";
  abilities?: string[];
  limits?: Record<string, unknown>;
}

export interface ChannelsMeta {
  refresh?: string;
  test: true;
}

export interface SubtitlesMeta {
  languages?: "any" | string[];
  formats?: string[];
}

export type MarkerKind = "intro" | "credits" | "recap" | "preview";

export interface MarkersMeta {
  kinds: MarkerKind[];
}

export interface NotificationMeta {
  fields?: SettingField[] | SettingsSchema;
  sendTest: true;
  formats?: string[];
  maxLength?: number;
}

export interface EventsMeta {
  subscribe: EtherEventName[];
}

export interface IntegrationMeta {
  category: string;
  logo?: string;
  adds: string[];
  connect: "fields" | "device_code" | "none";
  test?: boolean;
}

export interface ListSourceMeta {
  key: string;
  label: string;
  hint?: string;
  catalog: string;
  params?: unknown;
  maxItems?: number;
  refresh?: string;
}

export interface RankingStepMeta {
  key: string;
  label: string;
  hint?: string;
  editor?: unknown;
  budget?: unknown;
}

export interface RuleConditionMeta {
  key: string;
  label: string;
  operators?: string[];
  editor?: unknown;
}

export interface ToolMeta {
  name: string;
  title: string;
  description: string;
  inputSchema: Record<string, unknown>;
  readOnly?: boolean;
  destructive?: boolean;
  openWorld?: boolean;
}

export interface PageMeta {
  key: string;
  label: string;
  route: string;
  group?: string;
  icon?: string;
}

export type SlotKind =
  | "page"
  | "title.panel"
  | "viewer.tab"
  | "dashboard.panel"
  | "actionMenu.title"
  | "actionMenu.viewer"
  | "actionMenu.collection"
  | "actionMenu.release"
  | "command";

type ActionSlot =
  | "actionMenu.title"
  | "actionMenu.viewer"
  | "actionMenu.collection"
  | "actionMenu.release";
export type SlotMeta = { key: string; label: string } & (
  | { slot: ActionSlot; action: string; route?: never }
  | { slot: Exclude<SlotKind, ActionSlot>; route: string; action?: never }
);

export interface CommandMeta {
  key: string;
  label: string;
  action: string;
}

export interface Contributes {
  source?: SourceMeta;
  channels?: ChannelsMeta;
  subtitles?: SubtitlesMeta;
  markers?: MarkersMeta;
  notification?: NotificationMeta;
  events?: EventsMeta;
  integration?: IntegrationMeta;
  listSources?: ListSourceMeta[];
  rankingSteps?: RankingStepMeta[];
  ruleConditions?: RuleConditionMeta[];
  tools?: ToolMeta[];
  pages?: PageMeta[];
  slots?: SlotMeta[];
  commands?: CommandMeta[];
  mediaKinds?: MediaKindMeta[];
  frontendAdapters?: FrontendAdapterMeta[];
  setup?: SetupMeta;
}

/** How a library is published per protocol (Ether v0.3.8 content kinds). */
export interface Presentations {
  adapterOnly?: boolean;
  plex?: { base: "movie" | "show" | "artist" | "photo" };
  mediaBrowser?: { collectionType: string; itemTypes?: Record<string, string> };
  stremio?: { type: "movie" | "series" | "channel" | "tv" };
  xtream?: { category: string };
  silo?: { type: string };
}

export interface KindLevel {
  key: string;
  label: string;
  playable?: boolean;
  container?: boolean;
}

export interface KindField {
  key: string;
  type: "string" | "int" | "float" | "bool" | "date" | "duration" | "enum" | "strings" | "people";
  label: string;
  indexed?: boolean;
  enum?: string[];
}

export interface KindArt {
  role: string;
  label: string;
}

/** One declared content kind; each kind becomes its own library. */
export interface MediaKindMeta {
  key: string;
  label: string;
  library: { name: string; icon?: string };
  levels: KindLevel[];
  fields?: KindField[];
  art?: KindArt[];
  present: Presentations;
  browse: {
    sorts?: string[];
    facets?: string[];
    search: boolean;
    searchFields?: string[];
    /** Client searches also call /search live; results are upserted and merged. */
    liveSearch?: boolean;
  };
  sync: { mode: "pull" | "push"; refresh?: string };
  cache?: { maxBytes?: number; ttl?: string };
  match?: { thresholdDefault?: number };
}

export interface FrontendAdapterMeta {
  key: string;
  label: string;
  protocolHint: "opds" | "subsonic" | "custom";
  pathPrefix: string;
  defaultPort?: number;
  clients?: string[];
  auth?: "ether_token" | "basic" | "none_lan";
}

export interface SetupStep {
  key: string;
  label: string;
  hint?: string;
  fields: string[];
  test: string;
  required: boolean;
}

export interface SetupMeta {
  steps: SetupStep[];
}

/** The standardised plugin item envelope. */
export interface KindItem {
  unavailable?: { code: string; detail?: string };
  id: string;
  level: string;
  parentId?: string;
  index?: number;
  title: string;
  sortTitle?: string;
  summary?: string;
  year?: number;
  released?: number;
  rating?: number;
  playable?: boolean;
  childCount?: number;
  leafCount?: number;
  art?: Array<{ role: string; url: string; width?: number; height?: number }>;
  fields?: Record<string, unknown>;
  hints?: Record<string, unknown>;
  etag?: string;
}

export interface KindPage {
  items: KindItem[];
  nextCursor?: string;
  total?: number;
}

export interface KindBrowseRequest {
  kind: string;
  parentId?: string;
  level?: string;
  cursor?: string;
  limit?: number;
  sort?: string;
  facets?: Record<string, string[]>;
  query?: string;
}

export interface KindChangePage {
  upserts?: KindItem[];
  deletes?: string[];
  nextCursor?: string;
  complete: boolean;
}

export interface SourceQuery {
  kind: "indexer_search" | "debrid_check" | "debrid_acquire";
  query?: string;
  /** Comma-separated Newznab category IDs or standard family names; uses generic search. */
  category?: string;
  infoHashes?: string[];
  infoHash?: string;
  magnet?: string;
}

export interface SourceCandidate {
  infoHash?: string;
  magnet?: string;
  nzbUrl?: string;
  /** Zero-based index in the provider torrent file list. Omit for normal video selection. */
  fileIndex?: number;
  title?: string;
  size?: number;
  seeders?: number;
  cached?: boolean;
}

export interface KindMatchRequest {
  kind: string;
  item: KindItem;
  candidates: SourceCandidate[];
  settings?: Record<string, unknown>;
}

export interface KindMatchScore {
  candidateRef: string;
  score: number;
  reasons?: string[];
}

export interface PlayableSource {
  path?: string;
  id: string;
  label?: string;
  pattern: "range" | "download" | "direct" | "container" | "release" | "file";
  mime: string;
  size?: number;
  durationSec?: number;
  url?: string;
  headers?: Record<string, string>;
  blobRef?: string;
  sha256?: string;
  maxBytes?: number;
  expiresAt?: number;
  container?: {
    format?: string;
    volumes: Array<{ url?: string; headers?: Record<string, string>; blobRef?: string; size: number }>;
    member?: string;
    memberIndex?: number;
    passwordSecret?: string;
  };
  release?: {
    nzbUrl?: string;
    magnet?: string;
    infoHash?: string;
    candidate?: SourceCandidate;
    /** Zero-based torrent file index; overrides candidate.fileIndex. Invalid selections fail. */
    fileIndex?: number;
    /** Exact torrent path or unique basename, including non-video files. */
    file?: string;
    /** Archive member name when composing a release with container extraction. */
    fileHint?: string;
    passwordSecret?: string;
  };
}

export interface KindResolveRequest {
  kind: string;
  libraryKey: string;
  item: KindItem;
  partIndex?: number;
  client?: Record<string, unknown>;
}

export interface KindResolveResponse {
  sources: PlayableSource[];
  ttlSec?: number;
}

export interface SetupTestRequest {
  step: string;
  fields?: Record<string, unknown>;
}

export interface SetupTestResult {
  ok: boolean;
  detail?: string;
  fields?: Record<string, string>;
}

export interface EtherManifestBlock {
  protocol: 1;
  kind: PluginKind;
  capabilities?: Capability[];
  summary: string;
  author?: PluginAuthor;
  license?: string;
  repo?: string;
  icon?: string;
  runtime: RuntimeDecl;
  cluster?: "leader" | "every_node";
  health?: HealthDecl;
  permissions?: Permissions;
  settings?: SettingsSchema | Record<string, unknown>;
  strings?: Record<string, string | Record<string, string>>;
  schedule?: ScheduleEntry[];
  contributes?: Contributes;
}

export interface Manifest {
  id: string;
  version: string;
  name: string;
  description?: string;
  logo?: string;
  background?: string;
  types?: string[];
  catalogs?: StremioCatalogDecl[];
  resources?: Array<string | StremioResourceDecl>;
  idPrefixes?: string[];
  behaviorHints?: {
    adult?: boolean;
    p2p?: boolean;
    configurable?: boolean;
    configurationRequired?: boolean;
  };
  ether?: EtherManifestBlock;
}

// --- Route request and response payloads ---

export interface CatalogItem {
  id: string;
  type: string;
  name: string;
  poster?: string;
  posterShape?: "poster" | "landscape" | "square";
  background?: string;
  logo?: string;
  description?: string;
  releaseInfo?: string;
  imdbRating?: string;
  genres?: string[];
}

export interface CatalogRequest {
  type: string;
  id: string;
  extra: Record<string, string>;
  skip: number;
  search?: string;
  genre?: string;
}

export interface CatalogResponse {
  metas: CatalogItem[];
}

export interface MetaVideo {
  id: string;
  title: string;
  released?: string;
  season?: number;
  episode?: number;
  overview?: string;
  thumbnail?: string;
}

export interface MetaItem extends CatalogItem {
  runtime?: string;
  cast?: string[];
  director?: string[];
  videos?: MetaVideo[];
}

export interface MetaRequest {
  type: string;
  id: string;
}

export interface MetaResponse {
  meta: MetaItem;
}

export interface StreamEtherExt {
  id?: string;
  releaseName?: string;
  size?: number;
  languages?: string[];
  lazy?: boolean;
  file?: string;
}

export interface StreamItem {
  name?: string;
  title?: string;
  description?: string;
  url?: string;
  infoHash?: string;
  fileIdx?: number;
  ytId?: string;
  externalUrl?: string;
  behaviorHints?: {
    notWebReady?: boolean;
    bingeGroup?: string;
    countryWhitelist?: string[];
    filename?: string;
    videoSize?: number;
    videoHash?: string;
    proxyHeaders?: {
      request?: Record<string, string>;
      response?: Record<string, string>;
    };
  };
  ether?: StreamEtherExt;
}

export interface StreamRequest {
  type: string;
  id: string;
  extra: Record<string, string>;
}

export interface StreamResponse {
  streams: StreamItem[];
}

export interface OpenStreamRequest {
  streamId: string;
}

export interface OpenStreamResponse {
  url?: string;
  headers?: Record<string, string>;
  expiresAt?: string;
  file?: string;
}

export interface SubtitleItem {
  id: string;
  url: string;
  lang: string;
  label?: string;
  format?: string;
  hearingImpaired?: boolean;
}

export interface SubtitlesRequest {
  type: string;
  id: string;
  extra: Record<string, string>;
  pickToken?: string;
  videoHash?: string;
  videoSize?: number;
  filename?: string;
}

export interface SubtitlesResponse {
  subtitles: SubtitleItem[];
}

export interface MarkerItem {
  kind: MarkerKind;
  start: number;
  end: number;
  label?: string;
}

export interface MarkersRequest {
  type: string;
  id: string;
}

export interface MarkersResponse {
  markers: MarkerItem[];
}

export interface ChannelInput {
  url: string;
  name?: string;
  headers?: Record<string, string>;
  priority?: number;
}

export interface ChannelItem {
  id: string;
  name: string;
  number?: number;
  group?: string;
  logo?: string;
  guideId?: string;
  inputs: ChannelInput[];
}

export interface ChannelsResponse {
  channels: ChannelItem[];
}

export interface GuideRequest {
  channelId: string;
  from?: string;
  to?: string;
}

export interface ProgrammeItem {
  id?: string;
  channelId?: string;
  title: string;
  subtitle?: string;
  description?: string;
  start: string;
  stop: string;
  icon?: string;
  categories?: string[];
}

export interface GuideResponse {
  programmes: ProgrammeItem[];
}

export interface ConfigureResponse {
  errors?: Record<string, string>;
}

export interface TestResponse {
  ok: boolean;
  message?: string;
  details?: string[];
}

export type HealthStatus = "ok" | "attention" | "error";

export interface HealthResponse {
  status: HealthStatus;
  reason?: string;
}

export interface ViewSpecFieldRef {
  $: string;
}

export type ViewSpecValue = string | ViewSpecFieldRef;

export type ViewSpecBlock =
  | {
      kind: "page-header";
      title: ViewSpecValue;
      description?: ViewSpecValue;
      back?: { href: ViewSpecValue; label: ViewSpecValue };
      actions?: Array<{ widget: string; props?: Record<string, unknown> }>;
      when?: ViewSpecFieldRef;
    }
  | {
      kind: "stat-tile";
      iconKey: ViewSpecValue;
      accent: ViewSpecValue;
      label: ViewSpecValue;
      value: ViewSpecValue;
      sub?: ViewSpecValue;
      tone?: "default" | "positive" | "warning" | "negative" | ViewSpecFieldRef;
      when?: ViewSpecFieldRef;
    }
  | {
      kind: "panel";
      title: ViewSpecValue;
      iconKey?: ViewSpecValue;
      hint?: ViewSpecValue;
      blocks: ViewSpecBlock[];
      when?: ViewSpecFieldRef;
    }
  | {
      kind: "text";
      content: ViewSpecValue;
      tone?:
        | "default"
        | "negative"
        | "positive"
        | "warning"
        | "muted"
        | "strong"
        | ViewSpecFieldRef;
      when?: ViewSpecFieldRef;
    }
  | {
      kind: "heading";
      level: 2 | 3;
      content: ViewSpecValue;
      when?: ViewSpecFieldRef;
    }
  | {
      kind: "grid";
      as?: "div" | "section";
      blocks: ViewSpecBlock[];
      when?: ViewSpecFieldRef;
    }
  | {
      kind: "widget";
      widget:
        | "ether.posterGrid"
        | "ether.posterShelf"
        | "ether.titleCard"
        | "ether.liveChart"
        | "ether.settingsForm"
        | "ether.actionButton"
        | "ether.titleMatch"
        | "ether.healthStrip"
        | "ether.channelRow"
        | "ether.badge";
      props?: Record<string, unknown>;
      when?: ViewSpecFieldRef;
    }
  | {
      kind: "frame";
      frame: "ether.section" | "ether.tabs" | "ether.drawer";
      props?: Record<string, unknown>;
      blocks: ViewSpecBlock[];
      when?: ViewSpecFieldRef;
    }
  | {
      kind: "repeat";
      items: ViewSpecFieldRef;
      itemKey: ViewSpecFieldRef;
      unwrapped?: boolean;
      empty?: { text: ViewSpecValue };
      blocks: ViewSpecBlock[];
      when?: ViewSpecFieldRef;
    }
  | Record<string, unknown>;

export interface ViewSpecDocument {
  specVersion: 1;
  layout: "list" | "detail" | "bare";
  route?: string;
  header: ViewSpecBlock[];
  body: ViewSpecBlock[];
}

export interface ViewRequest {
  page: string;
  query: Record<string, string>;
}

export interface ViewResponse {
  spec: ViewSpecDocument;
  data: Record<string, unknown>;
}

export interface ActionProposal {
  category: string;
  description: string;
  input?: Record<string, unknown>;
}

export interface ActionRequest<P = Record<string, unknown>> {
  action: string;
  input: P;
}

export interface ActionResponse {
  toast?: string;
  refresh?: boolean;
  open?: string;
  proposal?: ActionProposal;
}

export interface EtherEvent<P = Record<string, unknown>> {
  id: string;
  name: EtherEventName | string;
  at: string;
  viewerId?: number;
  payload?: P;
}

export interface EventResponse {
  ack: string[];
}

export interface ToolRequest<I = Record<string, unknown>> {
  name: string;
  input: I;
}

export type ToolResponse = Record<string, unknown>;

export interface JobRequest {
  name: string;
}

export interface JobResponse {
  ok?: boolean;
  [key: string]: unknown;
}

export interface NotifyRequest {
  title?: string;
  message: string;
  event?: string;
  settings?: Record<string, unknown>;
}

export interface NotifyResponse {
  ok: boolean;
  message?: string;
}

// --- Hot-path types (SPEC §5.2 & §5.8) ---

export interface ClientCaps {
  maxHeight?: number;
  hdr?: string[];
  codecs?: string[];
}

export interface CandidateFacts {
  releaseId: string;
  name: string;
  size?: number;
  protocol?: "usenet" | "torrent" | "direct" | string;
  sourceClass?: string;
  indexer?: string;
  resolution?: string | number;
  hdr?: string[];
  videoCodec?: string;
  audioCodecs?: string[];
  audioChannels?: string;
  languages?: string[];
  subtitles?: string[];
  releaseType?: string;
  group?: string;
  cached?: boolean;
  verified?: boolean;
  clientCaps?: ClientCaps;
}

export type RankKeyFn<C = Record<string, unknown>> = (
  candidate: CandidateFacts,
  config: C,
) => number;

export type CondMatchFn<V = unknown> = (
  candidate: CandidateFacts,
  operator: string,
  value: V,
) => boolean;

// --- Low-level ABI request/response ---

export interface RawRequest {
  pluginId?: string;
  method?: string;
  path: string;
  query?: Record<string, string | string[]>;
  headers?: Record<string, string | string[]>;
  body?: unknown;
  locale?: string;
}

export interface RawResponse {
  status: number;
  headers?: Record<string, string>;
  body?: unknown;
}

export class PluginHttpError extends Error {
  readonly status: number;
  readonly code: string;

  constructor(status: number, code: string, message: string) {
    super(message);
    this.name = "PluginHttpError";
    this.status = status;
    this.code = code;
  }
}
