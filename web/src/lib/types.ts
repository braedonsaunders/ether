/** Mirrors of the Go config/API types (internal/config, internal/admin). */

import type { ViewerLifecycle } from "./contract";

export interface UsenetProvider {
  id: string;
  name: string;
  host: string;
  port: number;
  tls: boolean;
  username: string;
  password: string;
  connections: number;
  priority: number;
  backfill: boolean;
  enabled: boolean;
  /** Article store shared with other accounts; "" or absent = its own. */
  backbone?: string;
}

export interface Indexer {
  id: string;
  name: string;
  url: string;
  apiKey: string;
  dailyLimit: number;
  dailyGrabs: number;
  priority: number;
  enabled: boolean;
  movieCategories: number[] | null;
  tvCategories: number[] | null;
  /** "usenet" (Newznab, the default when empty or absent) or "torrent" (Torznab). */
  protocol?: IndexerProtocol | "";
  /**
   * "cardigann" for a built-in site, "stremio" for a Stremio stream addon,
   * "zilean" for a self-hosted Zilean instance (always protocol "torrent");
   * empty or "newznab" for a Newznab/Torznab URL.
   */
  kind?: IndexerKind | "";
  /** Keep a Stremio add-on's own result order instead of profile re-ranking. */
  useAddonOrder?: boolean;
  /** Installed definition id of a built-in site. */
  definition?: string;
  /** A built-in site's setting values by name. */
  settings?: Record<string, string> | null;
  /** A built-in site's password settings; each set value is redacted to "••••". */
  secretSettings?: Record<string, string> | null;
  /**
   * The audio language of the releases this indexer returns, as a BCP 47
   * tag ("pt-BR"). It is evidence for a bare DUAL/MULTI whose name names no
   * dub; an explicit language in a release name always wins. Overrides a
   * built-in site's definition language when set.
   */
  language?: string;
}

/** What an identity rule applies to. */
export type IdentityTargetKind =
  "all_searches" | "indexer" | "host" | "nzb_downloads" | "torrent_downloads";

/** Optional search/download narrowing on an indexer or host rule. */
export type IdentityScope = "" | "search" | "download";

/** Optional media-type narrowing on an identity rule. Empty ("") matches any media type. */
export type IdentityMediaType = "" | "movie" | "tv" | "music" | "books";

/** One header or query-parameter override in an identity rule. */
export interface KVOverride {
  name: string;
  value?: string;
  remove?: boolean;
}

/** One ordered request-identity rule (config.IdentityRule). */
export interface IdentityRule {
  id: string;
  target: IdentityTargetKind;
  indexerId?: string;
  host?: string;
  scope?: IdentityScope;
  mediaType?: IdentityMediaType;
  profile: string;
  headers?: KVOverride[] | null;
  params?: KVOverride[] | null;
}

/**
 * Input.clientIdentity: ordered server-wide request identity rules.
 * The implicit last rule is always honest Ether.
 */
export interface ClientIdentityConfig {
  rules?: IdentityRule[] | null;
}

/** One masked HTTP request preview (search or download). */
export interface RequestPreviewCall {
  requestLine: string;
  headers: { name: string; value: string }[];
  profile?: string;
  matchedRuleId?: string;
}

/** Masked sample requests from POST /indexer/preview. */
export interface RequestPreview {
  profile: string;
  matchedRuleId?: string;
  search: RequestPreviewCall;
  download: RequestPreviewCall;
  downloadKind: "nzb" | "torrent";
  profiles: { id: string; userAgent: string }[];
}

/** Request payload for POST /indexer/preview. */
export interface IndexerPreviewRequest {
  rule?: IdentityRule;
  rules?: IdentityRule[];
  indexer?: Indexer;
}

/** Result from POST /test/indexer-identity. */
export interface IdentityTestResult extends TestResult {
  profile?: string;
  ruleId?: string;
  userAgent?: string;
}

export type IndexerProtocol = "usenet" | "torrent";
type IndexerKind = "newznab" | "cardigann" | "stremio" | "zilean";

/** Definitions source and FlareSolverr for built-in sites (config.indexerSites). */
export interface IndexerSitesConfig {
  definitionsUrl?: string;
  flareSolverrUrl?: string;
}

/** One setting a site definition declares. */
export interface SiteSetting {
  name: string;
  /** text, password, checkbox, select, multi-select, or info* (read-only guidance). */
  type: string;
  label?: string;
  default?: string;
  defaults?: string[];
  options?: { value: string; label: string }[];
}

/** An installed site definition (GET /indexer-sites/definitions). */
export interface SiteDefinition {
  id: string;
  name: string;
  description?: string;
  language?: string;
  /** public, semi-private or private. */
  type?: string;
  protocol: IndexerProtocol;
  links: string[];
  settings: SiteSetting[];
  movies: boolean;
  tv: boolean;
  /** Login method when the site needs an account. */
  login?: string;
  /** "upload" or "source". */
  source: string;
  warnings?: string[];
  installedAt: string;
  updatedAt: string;
}

/** A definition offered by the owner's definitions source. */
export interface CatalogEntry {
  id: string;
  file: string;
  installed: boolean;
  updateAvailable?: boolean;
}

/** POST /indexer-sites/test. */
export interface SiteTestResult {
  ok: boolean;
  results: number;
  titles?: string[] | null;
  error?: string;
  durationMs: number;
}

/** A built-in indexer's last test (GET /indexer-sites/status). */
export interface SiteTestStatus {
  indexerId: string;
  ok: boolean;
  message?: string;
  results: number;
  testedAt: string;
}

/** POST /indexer-sites/update. */
export interface SiteUpdateReport {
  updated: string[];
  unchanged: number;
  failed: { id: string; error: string }[];
}

export type DebridKind =
  "realdebrid" | "torbox" | "alldebrid" | "premiumize" | "debridlink" | "offcloud";

/** One owner-supplied debrid account. `apiKey` is redacted to "••••" when set. */
export interface DebridProvider {
  id: string;
  name: string;
  kind: DebridKind;
  apiKey: string;
  /** Lower is tried first. */
  priority: number;
  enabled: boolean;
}

export interface DebridConfig {
  providers: DebridProvider[] | null;
  cachedOnly: boolean;
  removeUncached: boolean;
  /** Largest HTTP range request; requests start at 512 KiB and grow. 0 = 8 MiB; max 64 MiB. */
  chunkBytes?: number;
  /** Fixed read-ahead window in chunks; 0 = adaptive to throughput, negative disables; max 32. */
  readAheadChunks?: number;
  /** 0 = 4; max 16. */
  maxConnections?: number;
}

/** GET /status/debrid. */
export interface DebridStatus {
  enabled: boolean;
  cachedOnly: boolean;
  providers: DebridProviderStatus[] | null;
  /** Streaming engine counters; absent until the engine is wired. */
  engine?: DebridEngineStatus;
}

/** One sibling Plex server sharing the owner's internet upload link. */
export interface UploadBudgetExternalServer {
  machineId?: string;
  name: string;
  source: string;
  remoteStreams: number;
  bandwidthKbps: number;
  reachable: boolean;
  lastChecked?: string;
}

/** GET /status/upload-budget. */
export interface UploadBudgetStatus {
  enabled: boolean;
  source: "override" | "measured" | "learning";
  overrideKbps: number;
  measuredKbps: number;
  measuredSamples: number;
  effectiveKbps: number;
  usableKbps: number;
  etherRemoteKbps: number;
  etherRemoteStreams: number;
  budgetCappedStreams: number;
  otherPmsKbps: number;
  otherPmsStreams: number;
  totalUsedKbps: number;
  otherServers?: UploadBudgetExternalServer[];
}

/** Moving averages (EWMA) of one provider's timings; all 0 until `samples` > 0. */
export interface DebridLatency {
  resolveMs?: number;
  linkMs?: number;
  connectMs?: number;
  ttfbMs?: number;
  throughputBps?: number;
  samples?: number;
}

export interface DebridEngineStatus {
  openFiles?: number;
  blockBytes?: number;
  memoryHits?: number;
  diskHits?: number;
  blocksFetched?: number;
  bytesFetched?: number;
  hedges?: number;
  hedgeWins?: number;
  linkRefreshes?: number;
  errors?: number;
}

export interface DebridProviderStatus {
  id: string;
  name: string;
  kind: string;
  enabled: boolean;
  priority: number;
  state: "ok" | "backoff" | "auth_failed" | "disabled" | string;
  lastError?: string;
  backoffUntil?: string;
  rateLimitedUntil?: string;
  api?: {
    attempts: number;
    endpoints: Record<string, number> | null;
    retries: number;
    limited: number;
    http429: number;
    listHits: number;
    reuseHits: number;
    rememberedHits: number;
    firstPlays: number;
    firstPlayCalls: number;
  };
  requests?: number;
  failures?: number;
  cached?: number;
  notCached?: number;
  /** Cached files the provider refused to serve (Real-Debrid infringing_file). */
  blocked?: number;
  linksRefreshed?: number;
  /** Torrents reused from the account instead of re-added. */
  reused?: number;
  latency?: DebridLatency;
  lastSuccess?: string;
  /** API calls this node makes at once: the account's limit, or this node's share in a cluster. */
  slots?: number;
  /** The account-wide concurrent API call limit. */
  accountSlots?: number;
  /** Active queued downloads tracked on this provider. */
  queuedDownloads?: number;
  /** Provider queue reason, e.g. "queued at TorBox behind 3 active downloads". */
  queuedDetail?: string;
}

export interface PosterProviderConfig {
  assetFolder?: string;
  ownerEnabled?: boolean;
  version?: number;
  generation?: number;
  accounts?: Record<string, PosterProviderConfig>;
  enabled: boolean;
  source: string;
  baseUrl: string;
  apiKey: string;
  urlTemplate: string;
  profile?: string;
}

export interface ConsoleListenerStatus {
  configuredPort: number;
  activePort: number;
  mainPort: number;
  onMainPort: boolean;
  fallback?: boolean;
  error?: string;
}

export interface EtherConfig {
  frontends?: Record<
    string,
    {
      enabled: boolean;
      publishedAddresses?: string[];
      publicUrl?: string;
      settings?: Record<string, unknown>;
    }
  >;
  frontendPorts?: Record<string, number>;
  posters?: PosterProviderConfig;
  availability?: AvailabilityConfig;
  server: {
    startupAnimation?: boolean;
    port: number;
    consolePort?: number;
    disableConsoleOnMainPort?: boolean;
    bindAddress: string;
    friendlyName: string;
    resumeAfterSeconds?: number;
    pausedStreamMinutes?: number;
    machineIdentifier: string;
    logLevel: string;
    defaultStreamLimit: number;
    maxStreams: number;
    /** Node-local share weight for account-wide budgets in a cluster; 0/absent = 1. */
    clusterWeight?: number;
    /** Reverse proxies (IP, CIDR or host/container name) whose forwarded client address is trusted. */
    trustedProxyCIDRs?: string[] | null;
  };
  plex: {
    token: string;
    clientIdentifier: string;
    ownerPlexId: number;
    ownerUsername: string;
    claimed: boolean;
    usePlexMetadata?: boolean | null;
    remoteAccess: boolean;
    useUpnp: boolean;
    publicPort: number;
    publicHost: string;
    customUrls: string[] | null;
    certFile: string;
    keyFile: string;
    certHash: string;
    secureConnections: string;
  };
  usenet: {
    providers: UsenetProvider[] | null;
    /** How primary (non-backfill) providers share requests; "" is inOrder. */
    loadMode?: "" | "inOrder" | "balanced";
    repair?: {
      enabled: boolean;
      wholeSlices?: boolean;
      summaryRows: number;
      adaptiveSummaries?: boolean;
      maxDownloadBytes: number;
      liveWait: string;
      aheadWait: string;
    };
  };
  indexers: Indexer[] | null;
  /** Client Ether presents to Newznab/Torznab indexers. */
  clientIdentity?: ClientIdentityConfig;
  indexerSites?: IndexerSitesConfig;
  /** Absent on servers without debrid support. */
  debrid?: DebridConfig;
  /** Node-local outbound proxy. Absent until the server sends the section. */
  outbound?: {
    enabled: boolean;
    preset?: string;
    url: string;
    username?: string;
    password?: string;
    indexers: boolean;
    stremio: boolean;
    liveTV: boolean;
    debrid: boolean;
    usenet: boolean;
    metadata: boolean;
    extras?: boolean;
    bypass?: string[] | null;
  };
  sharedCache?: EtherConfig["cache"];
  cache: {
    storage?: "local" | "shared";
    preloadEnabled?: boolean;
    preloadBudgetBytes?: number;
    preloadMaxTitles?: number;
    preloadRateMiB?: number;
    preloadDuringPlayback?: boolean;
    preloadMinFreeBytes?: number;
    preloadSource?: string;
    preloadContent?: string;
    preloadHouseholdWeight?: number;
    preloadHouseholdScope?: string;
    preloadEpisodesAhead?: number;
    preloadHistoryDays?: number;
    mode?: string;
    retentionDays?: number;
    replayBoostHours?: number;
    disableSpeculation?: boolean;
    speculativeWarming?: "on" | "off";
    keepWarmTitles?: number[];
    detailWarmup?: string;
    detailBytes?: number;
    playbackReadAheadMinutes?: number;
    inProgressPinPercent?: number;
    inProgressPinMaxAge?: string;
    dir: string;
    budgetBytes: number;
    libraryBudgetBytes?: number;
    prefetchNextEpisode: boolean;
    prefetchBytes: number;
    keepInProgressWarm: boolean;
    nextEpisodeAt: number;
  };
  catalog: {
    tmdbKey: string;
    tmdbDisabled?: boolean;
    tvdbKey: string;
    language: string;
    region: string;
    minPopularity: number;
    minVotes: number;
    includeAdult: boolean;
    hideFranchiseCollections?: boolean;
    refreshInterval: string;
  };
  resolver: {
    healthSamples: number;
    searchCacheTtl: string;
    unavailableCooloff: string;
    maxCandidates: number;
    sameQualityAlternatives: number;
    alternativesPerResolution?: boolean;
    alternativesPerSource?: boolean;
    offerHigherVersions?: boolean;
    blockedGroups: string[] | null;
    blockedTerms: string[] | null;
    hardSubtitles?: "" | "exclude" | "allow";
    optimisticStart?: boolean;
    healthRace?: number;
    sourcePreference?: string;
    sourceAlwaysFirst?: boolean;
    /** Play a file a source plugin serves from a granted folder before any fetched release. */
    preferLocalFiles?: boolean;
    customFormats?: CustomFormatDefinition[] | null;
    /** Let a detail page's file-availability check open on-demand add-on streams. */
    openOnDemandAddonStreams?: boolean;
  };
  quality: {
    profiles: QualityProfile[] | null;
    /** The ordered rule list: the only place that decides who gets which profile. */
    rules: QualityRule[] | null;
    /** The profile when no rule picks one. */
    default: string;
    /** Limits for plays that reach the at-home fallback row. */
    defaultLimits?: QualityLimits | null;
    /**
     * The profile away from home when no rule picks one. "" is "Same as at
     * home": the at-home profile and its limits.
     */
    defaultAway?: string;
    /** Limits for plays that reach the away fallback row (only with defaultAway set). */
    defaultAwayLimits?: QualityLimits | null;
    databases?: ProfileDatabase[] | null;
    sync?: QualitySync;
    migratedRules?: number;
    /** Set by the one-time move of 0.3.20 settings into rules; shown on Rules until dismissed. */
    migrationNote?: { at: string; dismissed: boolean } | null;
  };
  transcode: {
    videoTranscoding?: "on" | "off";
    ffmpegPath: string;
    ffprobePath: string;
    hwAccel: string;
    vaapiDevice: string;
    tempDir: string;
    maxTranscodes: number;
    throttleSeconds?: number;
    remoteMaxBitrateKbps: number;
    remoteUploadBudgetKbps?: number;
    rtxVideo?: {
      mode: "off" | "auto" | "on";
      executablePath: string;
      sourceMaxHeight: number;
      targetMaxHeight: number;
      quality: number;
      maxConcurrent: number;
    };
  };
  clients?: {
    overrides?: {
      id?: string;
      product?: string;
      productMatch?: string;
      platform?: string;
      device?: string;
      alwaysEncodeVideo?: boolean;
    }[];
  };
  trakt: { enabled: boolean; clientId: string; clientSecret: string };
  simkl?: { enabled: boolean; clientId: string; clientSecret: string };
  oidc?: {
    enabled: boolean;
    displayName?: string;
    issuer: string;
    clientId: string;
    clientSecret: string;
    autoLinkEmail?: "" | "verified" | "any";
    autoLinkUsername?: boolean;
    autoProvision?: boolean;
    requiredGroup?: string;
  };
  mdblist?: { enabled: boolean };
  plexServers?: { enabled: boolean };
  markers?: {
    theIntroDb: boolean;
    apiKey: string;
    skipDb?: boolean;
    skipDbApiKey?: string;
    skipIntro?: boolean;
    skipCredits?: boolean;
    sourceOrder?: string[];
  };
  liveTV?: {
    timeshiftMinutes?: number;
    latencyProfile?: "auto" | "low" | "stable";
    logoIndexUrl?: string;
    healthSweepMinutes?: number;
    stability?: "fastest" | "balanced" | "rocksolid" | "";
  };
  subtitles?: {
    providers?: { id: string; kind: string; name?: string; enabled: boolean }[];
  };
  openSubtitles: {
    enabled: boolean;
    apiKey: string;
    username: string;
    password: string;
    automaticPreferred?: boolean;
  };
  subdl: {
    enabled: boolean;
    apiKey: string;
  };
  subsource: {
    enabled: boolean;
    apiKey: string;
  };
  llm: {
    enabled: boolean;
    provider: string;
    baseUrl: string;
    apiKey: string;
    model: string;
    speechBaseUrl?: string;
    speechApiKey?: string;
    speechModel?: string;
  };
  updates: {
    enabled?: boolean;
    channel: string;
    autoUpdate: boolean;
    feedUrl: string;
    maintenanceStart?: string;
    maintenanceEnd?: string;
    maxDrain?: string;
  };
  spike: { enabled: boolean; localFiles: string[] | null };
  downloads?: {
    enabled: boolean;
    allowFriends: boolean;
    maxConcurrent: number;
    maxConcurrentPerUser: number;
    diskBudgetBytes: number;
    expiry: string;
    maxHeight: number;
    limitUnit?: string;
    limitAmount?: number;
    limitPeriod?: string;
    noTranscode?: boolean;
  };
  personalize?: {
    postPlay: boolean;
    postPlayPrebuffer: boolean;
    microReasons: boolean;
    sessionIntent: boolean;
    clockFormat: string;
  };
  extras?: {
    themeMusic?: boolean;
    enabled: boolean;
    defaultOnMigrated?: boolean;
    resolverCommand?: string;
    resolverArgs?: string[] | null;
    resolverTimeout?: string;
    prewarmBytes?: number;
  };
  setupComplete: boolean;
}

/** A custom format definition (internal/core.CustomFormat). */
export interface CustomFormatDefinition {
  /** TRaSH Guides trash_id; the default key for an imported format. */
  trash_id?: string;
  id?: string;
  name?: string;
  /** "radarr" or "sonarr"; empty means Radarr. */
  app?: string;
  [key: string]: unknown;
}

/** Where an imported profile came from and when it last synced. */
export interface ProfileDatabase {
  id: string;
  name: string;
  repo: string;
  branch?: string;
}

export interface ProfileImport {
  source: string;
  app: "radarr" | "sonarr" | string;
  profile: string;
  database?: string;
  noSync?: boolean;
  trashId?: string;
  scoreSet?: string;
  groups?: string[] | null;
  syncedAt?: string;
}

/** Automatic guide sync (config.QualitySync). */
export interface QualitySync {
  enabled: boolean;
  /** Go duration string, e.g. "24h". */
  interval?: string;
  lastRun?: string;
  lastError?: string;
}

export interface QualityProfile {
  /** Set on imported profiles that automatic sync updates in place. */
  import?: ProfileImport;
  releaseRules?: ReleaseRule[] | null;
  /** Plugin ranking steps the profile uses, with their settings; each adds its points to the score. */
  pluginScores?: Record<string, Record<string, unknown>> | null;
  /** Qualities best first, as in Radarr/Sonarr; qualities in one item rank equally. */
  qualityOrder?: QualityItem[] | null;
  name: string;
  maxHeight: number;
  minHeight: number;
  fallbackBelowMin?: boolean;
  allowHdr: boolean;
  allowDolbyVision: boolean;
  minSizeMbPerMin: number;
  maxSizeMbPerMin: number;
  episodeMinSizeMbPerMin?: number;
  episodeMaxSizeMbPerMin?: number;
  preferredLanguages: string[] | null;
  /**
   * The profile's audio language, as in Radarr: "original" (or unset) is
   * each title's own language, "any" is every language, and any other
   * value is a specific language whose strictness is set by audioLanguageMode.
   */
  language?: string | null;
  /**
   * How a specific profile language applies: "preferred" ranks matching
   * releases first with fallback to viewer languages then original language;
   * "required" (or unset on existing profiles) strictly rejects other audio.
   */
  audioLanguageMode?: "required" | "preferred" | null;
  allowCam: boolean;
  /** Imported profiles: the only source-resolution pairs allowed, e.g. "webdl-1080". */
  allowedQualities?: string[] | null;
  /**
   * Every format the profile scores: custom formats by trash ID or name,
   * Ether's built-in formats ("ether:…") and Prefer rules ("rule:<id>").
   * A format not listed scores 0.
   */
  customFormatScores?: Record<string, number> | null;
  /** Releases whose custom formats score lower aren't played; null sets none. */
  minCustomFormatScore?: number | null;
}

/** One entry of a quality order: a quality, or a named group ranking equally. */
export interface QualityItem {
  name?: string;
  /** "<source>-<height>", e.g. "webdl-1080"; source "unknown" names no source. */
  qualities: string[];
}

/**
 * One device condition of a rule. Exactly one of group, type, app, client or
 * contains is set, holding an id from GET /quality/devices.
 */
export interface DeviceMatch {
  /** "tv", "mobile" or "computer". */
  group?: string;
  /** A device type, e.g. Apple TV. */
  type?: string;
  /** An app, e.g. Infuse. */
  app?: string;
  /** One exact device the server has seen. */
  client?: string;
  /** Product or platform contains this text (migrated rules). */
  contains?: string;
  /** The name shown to the owner, captured when picked. */
  label?: string;
}

/**
 * One rule of the ordered list. Every condition is optional: an empty list
 * means any, and its except flag inverts the list. The first matching rule
 * with a profile picks the profile; every matching rule's limits apply.
 */
/** The limits a rule or a fallback row sets. They only ever lower quality. */
export interface QualityLimits {
  maxHeight?: number;
  maxBitrateKbps?: number;
  stereoAudio?: boolean;
}

/** Which 0.3.20 setting a migrated rule came from. Set by the server only. */
export interface QualityRuleOrigin {
  kind: "awayLimit" | "viewerLimit" | "rule" | "awayProfile" | "library" | "tvDefault";
  /** The library's, viewer's or old rule's name, when there is one. */
  name?: string;
  /**
   * Why the upgrade shaped the rule this way: "awayByKind" (movies and TV had
   * different away profiles, so it stays an Away rule) or "awayDefaultWins"
   * (the away profile outranked library profiles, so this rule is At home).
   */
  reason?: "awayByKind" | "awayDefaultWins";
}

export interface QualityRule {
  /** Assigned by the server; "" on a rule not saved yet. */
  id: string;
  name?: string;
  libraries?: number[] | null;
  exceptLibraries?: boolean;
  viewers?: number[] | null;
  exceptViewers?: boolean;
  devices?: DeviceMatch[] | null;
  exceptDevices?: boolean;
  where?: "" | "home" | "away";
  kind?: "" | "movie" | "episode";
  /** "" or absent: no change, the rule only sets limits. */
  profile?: string;
  maxHeight?: number;
  maxBitrateKbps?: number;
  stereoAudio?: boolean;
  /** Where the upgrade from 0.3.20 took this rule from. Read-only: never sent back. */
  from?: QualityRuleOrigin;
}

interface SetupStep {
  id: string;
  title: string;
  complete: boolean;
  required: boolean;
  detail?: string;
}
export interface SetupStatus {
  setupComplete: boolean;
  requiredComplete: boolean;
  steps: SetupStep[];
  cacheRestartRequired?: boolean;
  /** Set when a /cache volume is mounted. */
  cacheVolume?: string;
}
export interface Bootstrap {
  ok: boolean;
  version: string;
  setupComplete: boolean;
  ownerConfigured?: boolean;
  signInMethods?: ("local" | "plex" | "oidc")[];
  /** Label for the single sign-on button ("Pocket ID"). */
  oidcName?: string;
  authenticated: boolean;
  authRequired: boolean;
  serverName: string;
  startupAnimation?: boolean;
  /** False when the Plex app is off. Omitted means Plex is on. */
  plexEnabled?: boolean;
}
export interface ProviderSpeedResult {
  status: string;
  at?: string;
  seconds?: number;
  mbps?: number;
  mbpsPerConnection?: number;
  streamMbps?: number;
  typicalMbps?: number;
  connections?: number;
  opened?: number;
  refused?: number;
  misses?: number;
  spareOnly?: boolean;
  streamsPlaying?: number;
}

export interface TestResult {
  ok: boolean;
  error?: string;
  durationMs: number;
}
export interface LogEntry {
  id: number;
  time: string;
  level: string;
  message: string;
  attrs?: Record<string, string>;
}
export interface DiskInfo {
  path: string;
  probedPath: string;
  totalBytes: number;
  freeBytes: number;
  usedBytes: number;
  cacheBudgetBytes: number;
}

export interface EtherUser {
  id: number;
  plexId: number;
  uuid?: string;
  username: string;
  title: string;
  email?: string;
  thumb?: string;
  isOwner: boolean;
  lifecycle?: ViewerLifecycle;
  /** Streams at once; 0 uses server.defaultStreamLimit. */
  streamLimit: number;
  /** Content rating limit; "" is unrestricted. */
  maxContentRating: string;
  allowedLibraries: number[] | null;
}

export interface UpdateNotes {
  version: string;
  markdown?: string;
  notesUrl?: string;
  unseen?: boolean;
  installed?: UpdateStatus["installed"];
}

/** GET /api/updates/ — the server's self-update manager. */
export interface UpdateStatus {
  checking?: boolean;
  enabled: boolean;
  disabledReason?: string;
  current: string;
  available?: {
    version: string;
    kind: string;
    notesUrl?: string;
    notes?: string;
    publishedAt?: string;
    staged: boolean;
    /** A security fix: installs at the next quiet moment. */
    critical?: boolean;
    /** Staged rollout: installs on its own after this time. */
    installAfter?: string;
    /** Missed several maintenance windows: installs at the next long quiet moment. */
    catchUp?: boolean;
    /** Notify-only installs: the file to download by hand. */
    downloadUrl?: string;
    downloadName?: string;
  };
  capability: "self-apply" | "notify-only" | "disabled";
  capabilityReason?: string;
  /** How a notify-only install is updated (internal/updater NotifyKind*). */
  notifyKind?:
    | "dev"
    | "macos"
    | "container"
    | "deb"
    | "rpm"
    | "package"
    | "unsupervised"
    | "unwritable"
    | "unknown"
    | "installer";
  channel?: string;
  arch?: string;
  catchUpQuietSeconds?: number;
  /** The last update Ether installed itself, until dismissed. */
  installed?: { from: string; to: string; at: string };
  state:
    | "idle"
    | "downloading"
    | "staged"
    | "waiting-for-window"
    | "waiting-for-idle"
    | "waiting-for-rollout"
    | "waiting-for-turn"
    | "draining"
    | "applying"
    | "rolled-back";
  autoUpdate: boolean;
  requested: boolean;
  force: boolean;
  lastCheck?: string;
  nextCheck?: string;
  lastError?: string;
  window: { start: string; end: string; open: boolean };
  activeSessions: number;
  quietPeriodSeconds: number;
  confirming: boolean;
  failedVersion?: string;
  failedReason?: string;
  note?: string;
}

export interface FieldError {
  field: string;
  message: string;
}

export const REDACTED = "••••";

/** A language-model service from GET /llm/providers (internal/config/llm.go). */
export interface LLMProvider {
  id: string;
  label: string;
  api: "openai" | "anthropic" | "google";
  /** Default endpoint; empty for "custom". */
  baseUrl: string;
  needsKey: boolean;
  /** Runs on the owner's network; the endpoint is editable. */
  local: boolean;
  requiresBaseUrl: boolean;
  keyHint?: string;
  modelHint?: string;
}

export interface LLMModel {
  id: string;
  label?: string;
}

/** GET /dashboard: one JSON blob per wired status provider. */
export interface DashboardData {
  sections: Record<string, unknown>;
  names: string[];
  setupComplete: boolean;
  setupSteps?: SetupStep[];
  version: string;
  /** False when the Plex app is off. Omitted means Plex is on. */
  plexEnabled?: boolean;
  serverName?: string;
}

/** One resolver decision (internal/resolver.Trace). Durations are nanoseconds. */
export interface CandidateTrace {
  releaseId: string;
  name: string;
  indexer: string;
  score: number;
  rejected: boolean;
  reason?: string;
  healthChecked: boolean;
  healthy: boolean;
  fetchDuration: number;
  healthDuration: number;
  size?: number;
  /** The owner's pin for the title. */
  pinned?: boolean;
  customFormats?: { id: string; name: string; score: number }[] | null;
  compatibilityReason?: string;
  /** Why source routing moved this candidate. */
  route?: string;
}
export interface ResolverTrace {
  pluginKind?: boolean;
  downgrade?: string;
  discovery?: { path: string; count: number; rescue?: boolean }[];
  limits?: {
    profileMinHeight: number;
    profileMaxHeight: number;
    profileConfiguredMaxHeight?: number;
    clientMaxHeight: number;
    clientMaxBitrateKbps: number;
  };
  titleKey: string;
  titleName: string;
  profile: string;
  /** Max-height ceilings behind the profile: as configured, the client's
   * cap and the effective value in force. For the configured value,
   * absent = never recorded (unknown, legacy traces) and an explicit
   * 0 = no ceiling ("Any"); client/effective keep 0 for no ceiling. */
  profileMaxHeight?: number;
  clientMaxHeight?: number;
  effectiveMaxHeight?: number;
  at: string;
  candidates: CandidateTrace[] | null;
  winner?: string;
  err?: string;
  searchDuration: number;
  /** The resolution excluded releases already tried (mid-play failover). */
  failover?: boolean;
  /** Pinned release id and what became of it ("used", "unhealthy: …"). */
  pinned?: string;
  pinStatus?: string;
  /** Candidates left out of a stored trace. */
  omitted?: number;
  /** The source-routing decision, e.g. "debrid first: popular title". */
  routing?: string;
  /** Who chose the winner. Absent on decisions recorded before it existed. */
  chooser?: "auto" | "viewer";
  /** Release name when chooser is "viewer". */
  chosenVersion?: string;
}

/** A source's measured startup (NZB or payload fetch plus a real health check). */
export interface SourceStartup {
  protocol: "usenet" | "torrent";
  /** Exponentially weighted average, nanoseconds. */
  average: number;
  last: number;
  samples: number;
}

// ---- Cluster (GET /cluster/, /cluster/settings) ------------------------------------------

/** One Ether node (internal/cluster.Node). */
export interface ClusterNode {
  /** Exact process boot; absent on older servers. */
  incarnation?: string;
  id: string;
  name: string;
  version: string;
  addrs: string[] | null;
  caps: {
    /** Empty or absent preserves the full-server responsibilities. */
    roles?: string[] | null;
    diagnostics?: ClusterDiagnostics;
    transcode: boolean;
    videoTranscoding?: string;
    hwAccel?: string;
    weight?: number;
    egressIp?: string;
  };
  /** Active playback sessions on the node. */
  load: number;
  startedAt: string;
  lastSeen: string;
  leader: boolean;
  live: boolean;
  draining?: boolean;
}

interface ClusterHealth {
  channel: string;
  busConnected: boolean;
  lastHeartbeat?: string;
  /** This node's clock minus the database server's, in nanoseconds. */
  clockSkewNs: number;
}

/** Local process observation only. Quota/resources are deliberately not consumed here. */
export interface ClusterSelfRuntime {
  nodeId: string;
  boot: string;
  runtimeSampledAt: string;
  protocol: number;
  started: boolean;
  readyExisting: boolean;
  acceptingNew: boolean;
  draining: boolean;
}

export interface ClusterStatus {
  /** Gallery fixture only; never consumed outside the mock build. Not a live API contract. */
  mockConsole?: {
    health: Record<
      string,
      { nodeId: string; boot: string; sampledAt: string; level: "ok" | "error" }
    >;
    metrics: Record<string, { transcodes: number; capacity: number; load: number }>;
    events: { node: string; kind: "join" | "drain" | "failover"; at: string }[];
    transition: { node: string; role: string; phase: "drain" | "restart" };
  };
  mode: "solo" | "postgres";
  self: ClusterNode;
  selfRuntime?: ClusterSelfRuntime;
  leader?: string;
  epoch: number;
  nodes: ClusterNode[] | null;
  health?: ClusterHealth;
  /** Membership could not be read (usually the database). */
  error?: string;
  warnings?: string[] | null;
}

export interface JoinTokenInfo {
  id: string;
  createdAt: string;
  expiresAt: string;
  usedAt?: string;
  usedBy?: string;
}

export interface ClusterSettings {
  clustered: boolean;
  keyFingerprint: string;
  sharedVersion?: number;
  joinTokens?: JoinTokenInfo[] | null;
}

/** POST /cluster/join-tokens: the token is returned once and never again. */
export interface CreatedJoinToken {
  token: string;
  id: string;
  expiresAt: string;
  command: string;
  warning: string;
}

/** One machine-load reading (internal/sysload). Percentages cover all cores. */
export interface LoadSample {
  at: string;
  cpu: number;
  memUsed: number;
  netRxBps: number;
  netTxBps: number;
  procCpu: number;
  procRss: number;
  gpu?: number;
}

export interface LoadGPU {
  name: string;
  util?: number;
  encoder?: number;
  decoder?: number;
  memUsed?: number;
  memTotal?: number;
}

/** GET /system/load: this server's load now and over the last few minutes. */
export interface SystemLoad {
  cores: number;
  memTotal: number;
  swapUsed: number;
  swapTotal: number;
  /** 1, 5 and 15-minute load averages; absent on Windows. */
  load?: number[];
  intervalSeconds: number;
  current?: LoadSample;
  history: LoadSample[];
  gpus: LoadGPU[];
  error?: string;
}

export interface ClusterSetupRequest {
  mode: "create" | "join";
  databaseUrl: string;
  token: string;
  replace: boolean;
}
export interface ClusterSetupStatus {
  available: boolean;
  state: "idle" | "checking" | "restarting" | "applying" | "complete" | "failed";
  message?: string;
}

export interface AvailabilityConfig {
  disableMovieEvidenceGate?: boolean;
  movieEvidenceDays?: number;
  /** playable, digital, or immediate. Empty is derived from the gate flags. */
  mode?: string;
  movieMode?: string;
  showMode?: string;
  /** gentle, normal, unlimited, or custom. Empty is treated as normal. */
  effort?: string;
  catchUp?: boolean;
  digitalDateEvidence?: boolean;
  homeReleaseOffsetDays?: number;
  monitor: boolean;
  verify: boolean;
  preferVerified: boolean;
  newlyAvailable: boolean;
  indexerIds: string[] | null;
  debrid: boolean;
  pollMinutes: number;
  maxPages: number;
  feedRequests: number;
  checkRequests: number;
  grabs: number;
  checks: number;
  pluginUsenetChecks?: number;
  checkBytes: number;
  reservePercent: number;
}

export interface ReleaseCondition {
  field: string;
  operator: "equals" | "contains" | "regex" | "range" | "in";
  value?: string;
  min?: number;
  max?: number;
}
export interface ReleaseRule {
  id: string;
  name: string;
  action: "require" | "prefer" | "avoid";
  match?: "all" | "any";
  conditions: ReleaseCondition[];
  unless?: ReleaseCondition[];
  fallback?: ReleaseCondition[];
  disabled?: boolean;
}
export interface ReleasePreferenceResult {
  rejected: boolean;
  unknown: boolean;
  fallback: number;
  preferences: number[];
  explanations: { id: string; name: string; action: string; state: string; outcome: string }[];
}

// ---- Quality: "Who gets what" ------------------------------------------------

/** A known app the Releases page can check (GET /quality/clients). */
export interface QualityClient {
  id: string;
  name: string;
  product: string;
  platform: string;
  device?: string;
}

/**
 * What a rule's Device condition can match (GET /quality/devices). Ids are
 * opaque server strings; a rule stores the id and the name shown when picked.
 */
export interface QualityDevices {
  /** Kinds of device: "tv", "mobile", "computer". `aliases` are extra search words ("iPad", "Shield"). */
  groups: { id: string; name: string; aliases?: string[] }[] | null;
  types: { id: string; name: string; group: string; aliases?: string[] }[] | null;
  /**
   * devices: the device types the app runs on. aliases: those devices and
   * their other names, for search ("iPad" finds Swiftfin).
   */
  apps:
    | { id: string; name: string; protocol?: string; devices?: string[]; aliases?: string[] }[]
    | null;
  /** Devices the server has seen. */
  clients:
    | {
        id: string;
        name: string;
        group?: string;
        type?: string;
        app?: string;
        viewer?: string;
        lastSeen?: string;
      }[]
    | null;
}

export type QualityDecisionSource = "rule" | "default" | "defaultAway" | "plugin";

export interface QualitySummaryCell {
  profile: string;
  source: QualityDecisionSource;
  profileRuleId?: string;
  maxHeight: number;
  maxBitrateKbps: number;
  stereo: boolean;
  limitRuleIds: string[];
  partialRuleIds: string[];
  /**
   * The filtered device's own ceiling (after a rule lifts an Android phone's
   * 1080p), only when the summary is filtered to one device or app; absent =
   * none. The play gets the lower of this and maxHeight / maxBitrateKbps.
   */
  deviceMaxHeight?: number;
  deviceMaxBitrateKbps?: number;
}

export interface QualitySummaryCellPair {
  kind: "" | "movie" | "episode";
  home: QualitySummaryCell;
  away: QualitySummaryCell;
}

export interface QualitySummaryException {
  ruleId: string;
  profile: string;
  maxHeight: number;
  maxBitrateKbps: number;
  stereo: boolean;
}

export interface QualitySummaryRow {
  libraryId: number;
  library: string;
  plugin: string;
  chosenByPlugin: boolean;
  cells: QualitySummaryCellPair[];
  exceptions: QualitySummaryException[];
}

/** Symbolic per-library rule evaluation (GET /quality/summary). */
export interface QualitySummary {
  rows: QualitySummaryRow[];
}

/** One viewer on one device or app, at home or away (POST /quality/explain). */
export interface QualityQuestion {
  /** Left out for "anyone": rules for particular viewers don't apply. */
  userId?: number;
  clientId?: string;
  libraryId?: number;
  titleKey?: string;
  /** "movie" or "episode"; sent when no title is picked. */
  kind?: "movie" | "episode" | "";
  client?: { product: string; platform?: string; device?: string };
  remote: boolean;
}

export type QualityLayerKind =
  "profile" | "rule-limit" | "device" | "android-lift" | "remote-blocked";

/** One layer that took part in a quality answer, in the order playback applies them. */
export interface QualityLayer {
  kind: QualityLayerKind;
  /** Plain English from the server; the console renders its own translated copy. */
  reason: string;
  profile?: string;
  source?: QualityDecisionSource;
  /** 1-based quality rule number. */
  rule?: number;
  ruleId?: string;
  ruleName?: string;
  /** Ceiling this layer sets; absent or 0 = none. */
  maxHeight?: number;
  hdr?: boolean;
  dolbyVision?: boolean;
  bitrateKbps?: number;
  stereo?: boolean;
  /** An App compatibility edit changed this device. */
  edited?: boolean;
  /** The layer had a setting that did not apply. */
  ignored?: boolean;
  /** Set the final resolution ceiling, the final bitrate cap, stereo, or blocked playback. */
  decisive: boolean;
}

export interface QualityAnswer {
  profile: string;
  /** The kind the play was evaluated for. Send `kind` in the question to choose it. */
  kind: "movie" | "episode";
  /** Nothing in the question named the kind (no title, no Movies/Shows library, no `kind`), so it is "movie". */
  kindAssumed?: boolean;
  profileSource: QualityDecisionSource;
  profileRuleId?: string;
  limitRuleIds: string[];
  /** 0 = no ceiling. */
  maxHeight: number;
  hdr: boolean;
  dolbyVision: boolean;
  /** 0 = no cap. */
  bitrateKbps: number;
  stereo?: boolean;
  blocked: boolean;
  layers: QualityLayer[];
}

/** Authoritative nodecontrol observation. Publication is never an acknowledgement. */
export interface ClusterControl {
  nodeId: string;
  boot: string;
  requestedVersion: number;
  appliedVersion: number;
  state: "idle" | "pending" | "applied" | "failed";
  requestId?: string;
  errorCode?: string;
  updatedAt: string;
  observed: { weight: number; draining: boolean };
  capabilities: { key: string; scope: "node" | "fleet"; supported: boolean; reason: string }[];
}
export interface ClusterControlRequest {
  requestId: string;
  boot: string;
  expectedVersion: number;
  patch: { weight?: number; draining?: boolean };
}

/** Read-only refusal evidence; there is no role mutation executor. */
export interface ClusterRolePreflight {
  nodeId: string;
  boot: string;
  observedAt: string;
  membershipSampledAt: string;
  eligible: false;
  executor: "none";
  reasons: string[];
  safety: {
    membership: "observed";
    ownershipRetirement: "unknown";
    activeSessions: "unknown";
    restart: "unsupported";
  };
  guidance: {
    scope: "deployment_pool";
    deploymentOwnership: "unknown";
    verifiedAgainst: string[];
    exampleValues: string;
    impact: string;
    execution: string;
  };
}

export interface ClusterDiagnostics {
  clock?: {
    clockSkewNs: number;
    rttNs: number;
    uncertaintyNs: number;
    boundNs: number;
    sampledAt: string;
    ready: boolean;
    reason?: string;
    lastStepNs: number;
  };
  sampledAt: string;
  readyExisting: boolean;
  acceptingNew: boolean;
  draining: boolean;
  readinessReason?: string;
  transcodeCapacity?: number;
  transcodes?: number;
  sourceConnections?: number;
  sourceConnectionCapacity?: number;
  deliveryBytesPerSecond?: number;
  codecs?: string[];
}
export interface ClusterConsole {
  sampledAt: string;
  truncated: boolean;
  assignments: {
    playback?: {
      protocol?: string;
      decision?: string;
      videoCopy?: boolean;
      audioCopy?: boolean;
      title?: string;
      titleKey?: string;
      viewer?: string;
      device?: string;
      app?: string;
      video?: string;
      bitrateKbps?: number;
    };
    sessionId: string;
    playId?: string;
    encoder?: string;
    sourceNodeId?: string;
    encoderNodeId: string;
    controllerNodeId: string;
    deliveryNodeId?: string;
    generation: number;
  }[];
  events: {
    sessionId: string;
    role: string;
    failedNodeId: string;
    replacementNodeId?: string;
    generation: number;
    startedAt: string;
    completedAt?: string;
    status: string;
    interruptionMs?: number;
    clientStalls?: number;
  }[];
}
