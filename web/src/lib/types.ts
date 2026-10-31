/** Mirrors of the Go config/API types (internal/config, internal/admin). */

export type Json = null | boolean | number | string | Json[] | { [key: string]: Json };

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
   * "cardigann" for a built-in site, "stremio" for a Stremio stream addon
   * (always protocol "torrent"); empty or "newznab" for a Newznab/Torznab URL.
   */
  kind?: IndexerKind | "";
  /** Installed definition id of a built-in site. */
  definition?: string;
  /** A built-in site's setting values by name. */
  settings?: Record<string, string> | null;
  /** A built-in site's password settings; each set value is redacted to "••••". */
  secretSettings?: Record<string, string> | null;
}

export type IndexerProtocol = "usenet" | "torrent";
export type IndexerKind = "newznab" | "cardigann" | "stremio";

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
  | "realdebrid"
  | "torbox"
  | "alldebrid"
  | "premiumize"
  | "debridlink"
  | "offcloud";

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
}

export interface PosterProviderConfig {
  enabled: boolean;
  source: string;
  baseUrl: string;
  apiKey: string;
  urlTemplate: string;
  profile?: string;
}

export interface EtherConfig {
  frontends?: Record<string, { enabled: boolean; settings?: Record<string, unknown> }>;
  frontendPorts?: Record<string, number>;
  posters?: PosterProviderConfig;
  availability?: AvailabilityConfig;
  server: {
    port: number;
    bindAddress: string;
    friendlyName: string;
    moviesLibraryName: string;
    tvShowsLibraryName: string;
    machineIdentifier: string;
    logLevel: string;
    defaultStreamLimit: number;
    maxStreams: number;
    /** Node-local share weight for account-wide budgets in a cluster; 0/absent = 1. */
    clusterWeight?: number;
  };
  plex: {
    token: string;
    clientIdentifier: string;
    ownerPlexId: number;
    ownerUsername: string;
    claimed: boolean;
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
  indexerSites?: IndexerSitesConfig;
  /** Absent on servers without debrid support. */
  debrid?: DebridConfig;
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
    detailWarmup?: string;
    detailBytes?: number;
    inProgressPinPercent?: number;
    inProgressPinMaxAge?: string;
    dir: string;
    budgetBytes: number;
    prefetchNextEpisode: boolean;
    prefetchBytes: number;
    keepInProgressWarm: boolean;
    nextEpisodeAt: number;
  };
  catalog: {
    tmdbKey: string;
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
    blockedGroups: string[] | null;
    blockedTerms: string[] | null;
    optimisticStart?: boolean;
    healthRace?: number;
    sourcePreference?: string;
    customFormats?: unknown[];
    customFormatScores?: Record<string, number>;
  };
  quality: {
    profiles: QualityProfile[] | null;
    rules: QualityRule[] | null;
    default: string;
    remote: string;
  };
  transcode: {
    ffmpegPath: string;
    ffprobePath: string;
    hwAccel: string;
    vaapiDevice: string;
    tempDir: string;
    maxTranscodes: number;
    remoteMaxBitrateKbps: number;
    rtxVideo?: {
      mode: "off" | "auto" | "on";
      executablePath: string;
      sourceMaxHeight: number;
      targetMaxHeight: number;
      quality: number;
      maxConcurrent: number;
    };
  };
  trakt: { enabled: boolean; clientId: string; clientSecret: string };
  simkl?: { enabled: boolean; clientId: string; clientSecret: string };
  markers?: { theIntroDb: boolean; apiKey: string };
  openSubtitles: {
    enabled: boolean;
    apiKey: string;
    username: string;
    password: string;
  };
  llm: {
    enabled: boolean;
    provider: string;
    baseUrl: string;
    apiKey: string;
    model: string;
  };
  updates: {
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
  };
  personalize?: {
    postPlay: boolean;
    postPlayPrebuffer: boolean;
    microReasons: boolean;
    sessionIntent: boolean;
    clockFormat: string;
  };
  extras?: {
    enabled: boolean;
    resolverCommand?: string;
    resolverArgs?: string[] | null;
    resolverTimeout?: string;
    prewarmBytes?: number;
  };
  setupComplete: boolean;
}

export interface QualityProfile {
  releaseRules?: ReleaseRule[] | null;
  name: string;
  maxHeight: number;
  minHeight: number;
  allowHdr: boolean;
  allowDolbyVision: boolean;
  preferredSources: string[] | null;
  preferredCodecs: string[] | null;
  preferredAudio: string[] | null;
  minSizeMbPerMin: number;
  maxSizeMbPerMin: number;
  episodeMinSizeMbPerMin?: number;
  episodeMaxSizeMbPerMin?: number;
  preferLargest?: boolean;
  preferredLanguages: string[] | null;
  allowCam: boolean;
  /** Imported profiles: the only source-resolution pairs allowed, e.g. "webdl-1080". */
  allowedQualities?: string[] | null;
  /** Imported profiles: custom format scores by trash ID, overriding the defaults. */
  customFormatScores?: Record<string, number> | null;
  minCustomFormatScore?: number | null;
}

export interface QualityRule {
  protocol?: string;
  product?: string;
  platform?: string;
  userId?: number;
  profile: string;
  /** Caps this device below the profile. 0 or absent means the profile's ceiling. */
  maxHeight?: number;
}

export interface SetupStep {
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
}
export interface Bootstrap {
  ok: boolean;
  version: string;
  setupComplete: boolean;
  ownerConfigured?: boolean;
  signInMethods?: ("local" | "plex")[];
  authenticated: boolean;
  authRequired: boolean;
  serverName: string;
}
export interface TestResult {
  ok: boolean;
  error?: string;
  durationMs: number;
}
export interface LogEntry {
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
  streamLimit: number;
  maxContentRating: string;
  subtitleLanguages: string[] | null;
  audioLanguages: string[] | null;
  allowedSections: number[] | null;
}

/** GET /api/updates/ — the server's self-update manager. */
export interface UpdateStatus {
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
  };
  capability: "self-apply" | "notify-only" | "disabled";
  capabilityReason?: string;
  state:
    | "idle"
    | "downloading"
    | "staged"
    | "waiting-for-window"
    | "waiting-for-idle"
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
  id: string;
  name: string;
  version: string;
  addrs: string[] | null;
  caps: { transcode: boolean; hwAccel?: string; weight?: number; egressIp?: string };
  /** Active playback sessions on the node. */
  load: number;
  startedAt: string;
  lastSeen: string;
  leader: boolean;
  live: boolean;
  draining?: boolean;
}

export interface ClusterHealth {
  channel: string;
  busConnected: boolean;
  lastHeartbeat?: string;
  /** This node's clock minus the database server's, in nanoseconds. */
  clockSkewNs: number;
}

export interface ClusterStatus {
  mode: "solo" | "postgres";
  self: ClusterNode;
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
  checkBytes: number;
  reservePercent: number;
}

export interface ReleaseCondition {
  field: string;
  operator: "equals" | "contains" | "regex" | "range";
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
