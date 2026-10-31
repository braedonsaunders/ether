import { localized, t } from "../../i18n";
import type { FieldGroup } from "./SchemaForm";

export const SERVER: FieldGroup[] = localized(() => [
  {
    title: t("Server"),
    fields: [
      {
        key: "friendlyName",
        label: t("Server name"),
        kind: "text",
        help: t("How the server appears in Plex apps."),
      },
      {
        key: "moviesLibraryName",
        label: t("Movies library name"),
        kind: "text",
        help: t(
          "How the Movies library appears in Plex apps. Ether sets this; Plex's own rename does not stick.",
        ),
      },
      {
        key: "tvShowsLibraryName",
        label: t("TV Shows library name"),
        kind: "text",
        help: t(
          "How the TV Shows library appears in Plex apps. Ether sets this; Plex's own rename does not stick.",
        ),
      },
      {
        key: "port",
        label: t("Port"),
        kind: "number",
        min: 1,
        max: 65535,
        step: 1,
        help: t("Plex apps expect 32400. A restart is needed after changing it."),
      },
      {
        key: "bindAddress",
        label: t("Listen address"),
        kind: "text",
        help: t("0.0.0.0 listens on every network interface."),
      },
      {
        key: "machineIdentifier",
        label: t("Machine identifier"),
        kind: "readonly",
        help: t("Generated on first run; never changes."),
      },
    ],
  },
  {
    title: t("Streams"),
    fields: [
      {
        key: "defaultStreamLimit",
        label: t("Streams per viewer"),
        kind: "number",
        min: 0,
        step: 1,
        help: t("Default simultaneous streams for each viewer. 0 = unlimited."),
      },
      {
        key: "maxStreams",
        label: t("Server-wide limit"),
        kind: "number",
        min: 0,
        step: 1,
        help: t("0 derives the limit from your Usenet connections."),
      },
      {
        key: "logLevel",
        label: t("Log level"),
        kind: "select",
        options: [
          { value: "debug", label: t("Debug") },
          { value: "info", label: t("Info") },
          { value: "warn", label: t("Warnings") },
          { value: "error", label: t("Errors only") },
        ],
      },
    ],
  },
]);

/** server.* keys each cluster node keeps for itself (internal/config/scope.go). */
const NODE_LOCAL_SERVER_KEYS = new Set(["port", "bindAddress", "logLevel"]);

/**
 * General's server groups on a cluster node: settings shared by every node
 * stay in their groups, and this node's own settings (listener, logging and
 * its weight) gather under "This node".
 */
export function clusterServerGroups(): FieldGroup[] {
  const shared = SERVER.map((g) => ({
    ...g,
    fields: g.fields.filter((f) => !NODE_LOCAL_SERVER_KEYS.has(f.key)),
  }));
  const local = SERVER.flatMap((g) => g.fields.filter((f) => NODE_LOCAL_SERVER_KEYS.has(f.key)));
  return [
    ...shared,
    {
      title: t("This node"),
      description: t(
        "These apply only to this node; every other setting is shared by the cluster.",
      ),
      fields: [
        ...local,
        {
          key: "clusterWeight",
          label: t("Node weight"),
          kind: "number",
          min: 0,
          max: 100,
          step: 1,
          help: t(
            "This node's share of each provider's connections and request limits, relative to the other nodes. 2 gets twice the share of a node at 1; 0 means 1.",
          ),
        },
      ],
    },
  ];
}

export const PLEX: FieldGroup[] = localized(() => [
  {
    id: "remote",
    title: t("Remote access"),
    description: t("Let Plex apps reach Ether from outside your network."),
    fields: [
      {
        key: "remoteAccess",
        label: t("Allow remote access"),
        kind: "bool",
        help: t("Publishes your public address to plex.tv."),
      },
    ],
    more: [
      {
        key: "useUpnp",
        label: t("Map the port automatically (UPnP)"),
        kind: "bool",
        help: t("Asks your router to forward the port."),
      },
      {
        key: "publicPort",
        label: t("Public port"),
        kind: "number",
        min: 0,
        max: 65535,
        step: 1,
        help: t("The port forwarded on your router. 0 uses the server port."),
      },
      {
        key: "publicHost",
        label: t("Public host name"),
        kind: "text",
        help: t("Optional. Leave empty to detect your public address."),
      },
      {
        key: "customUrls",
        label: t("Custom server URLs"),
        kind: "list",
        placeholder: "https://ether.example.com:443",
        help: t("Extra addresses Plex apps should try, comma-separated."),
      },
    ],
  },
  {
    id: "secure",
    title: t("Secure connections"),
    description: t("How Plex apps encrypt the connection to this server."),
    fields: [
      {
        key: "secureConnections",
        label: t("Secure connections"),
        kind: "select",
        options: [
          { value: "required", label: t("Required") },
          { value: "preferred", label: t("Preferred") },
          { value: "disabled", label: t("Disabled") },
        ],
      },
    ],
    more: [
      {
        key: "certFile",
        label: t("Certificate file"),
        kind: "text",
        wide: true,
        help: t("Leave empty to use the plex.direct certificate Ether manages."),
      },
      {
        key: "keyFile",
        label: t("Key file"),
        kind: "text",
        wide: true,
        help: t("The private key that matches the certificate file."),
      },
    ],
  },
  {
    id: "compatibility",
    title: t("Compatibility"),
    fields: [
      {
        key: "trackLatestVersion",
        label: t("Report the latest public PMS version"),
        kind: "bool",
        help: t(
          "Off reports the version Ether is verified against. Plex apps turn on features by server version, so tracking can expose features Ether doesn't support.",
        ),
      },
      {
        key: "latestCompatibilityVersion",
        label: t("Latest public PMS version"),
        kind: "readonly",
      },
    ],
    more: [
      {
        key: "compatibilityVersion",
        label: t("Compatibility version override"),
        kind: "text",
        help: t(
          "Leave empty to use the verified version, or the latest public version when tracking is on.",
        ),
      },
    ],
  },
]);

export const CACHE: FieldGroup[] = localized(() => [
  {
    title: t("Disk cache"),
    description: t("Segments from Usenet are cached on disk so seeks and rewatches are instant."),
    fields: [
      { key: "dir", label: t("Cache folder"), kind: "text", wide: true },
      {
        key: "budgetBytes",
        label: t("Disk budget"),
        kind: "bytes",
        unit: "GiB",
        help: t("The oldest segments are evicted when the cache reaches this size."),
      },
    ],
  },
  {
    title: t("Prefetch"),
    fields: [
      {
        key: "prefetchNextEpisode",
        label: t("Prefetch the next episode"),
        kind: "bool",
        help: t("Warms the opening of the next episode so it starts instantly."),
      },
      {
        key: "nextEpisodeAt",
        label: t("Start prefetching at"),
        kind: "number",
        min: 0,
        max: 1,
        step: 0.05,
        help: t("Fraction of the current episode watched (0–1)."),
      },
      {
        key: "prefetchBytes",
        label: t("Prefetch size"),
        kind: "bytes",
        unit: "MiB",
        help: t("How much of the next episode to pull ahead."),
      },
      {
        key: "keepInProgressWarm",
        label: t("Keep in-progress titles warm"),
        kind: "bool",
        help: t("Keeps the resume point of unfinished titles cached."),
      },
      {
        key: "inProgressPinMaxAge",
        label: t("Stop keeping a title warm after"),
        kind: "duration",
        placeholder: "720h",
        help: t(
          "An unfinished title nobody has watched for this long loses its cache pin. Empty uses 30 days.",
        ),
      },
      {
        key: "inProgressPinPercent",
        label: t("Share of the cache for in-progress titles"),
        kind: "number",
        min: 0,
        max: 100,
        step: 5,
        help: t(
          "Percent of the disk budget unfinished titles may keep pinned; the least recently watched lose their pin first. 0 uses 50.",
        ),
      },
    ],
  },
]);

export const CATALOG: FieldGroup[] = localized(() => [
  {
    title: "TMDB",
    fields: [
      {
        key: "tmdbKey",
        label: t("Your TMDB API key"),
        kind: "secret",
        help: t("Your personal TMDB v3 API key or v4 read-access token."),
      },
      {
        key: "language",
        label: t("Language"),
        kind: "text",
        help: t("Metadata language, e.g. en-US or fr-FR."),
      },
      {
        key: "region",
        label: t("Region"),
        kind: "text",
        help: t("Two-letter region for release dates and streaming availability."),
      },
      { key: "refreshInterval", label: t("Refresh every"), kind: "duration" },
    ],
  },
  {
    title: t("What's browsable"),
    description: t("Thresholds keep obscure titles out of Plex libraries; they remain searchable."),
    fields: [
      { key: "minPopularity", label: t("Minimum popularity"), kind: "number", min: 0, step: 1 },
      { key: "minVotes", label: t("Minimum votes"), kind: "number", min: 0, step: 10 },
      { key: "includeAdult", label: t("Include adult titles"), kind: "bool" },
      {
        key: "hideFranchiseCollections",
        label: t("Hide automatic franchise collections"),
        kind: "bool",
        help: t(
          "Hide TMDB franchise groups such as Zootopia from Plex's Collections tab. Your curated collections and Home rows stay available. Applies after saving; refresh the Collections tab in Plex.",
        ),
      },
    ],
  },
]);

export const RESOLVER: FieldGroup[] = localized(() => [
  {
    title: t("Release selection"),
    fields: [
      {
        key: "allowCam",
        label: t("Allow CAM and telesync releases"),
        kind: "bool",
        help: t("Off by default: these are recorded in cinemas."),
      },
      {
        key: "maxCandidates",
        label: t("Releases to keep ready"),
        kind: "number",
        min: 0,
        step: 1,
        help: t("Fallback releases kept for instant failover."),
      },
      {
        key: "sameQualityAlternatives",
        label: t("Same-quality versions to offer"),
        kind: "number",
        min: 0,
        max: 3,
        step: 1,
        help: t(
          "Other releases with the same cut, resolution and HDR type, listed after Auto in Plex's Play Version menu so you can switch if one fails.",
        ),
      },
      {
        key: "healthSamples",
        label: t("Health check samples"),
        kind: "number",
        min: 0,
        step: 1,
        help: t("Articles checked before a release is trusted."),
      },
      { key: "searchCacheTtl", label: t("Remember searches for"), kind: "duration" },
      { key: "unavailableCooloff", label: t("Retry unavailable titles after"), kind: "duration" },
    ],
  },
  {
    title: t("Blocklists"),
    fields: [
      {
        key: "blockedGroups",
        label: t("Blocked release groups"),
        kind: "list",
        placeholder: "GROUP1, GROUP2",
      },
      {
        key: "blockedTerms",
        label: t("Blocked terms"),
        kind: "list",
        placeholder: "hardcoded, korsub",
      },
    ],
  },
]);

export const TRANSCODE: FieldGroup[] = localized(() => {
  const HW_ACCEL = {
    key: "hwAccel",
    label: t("Hardware acceleration"),
    kind: "select" as const,
    help: t(
      "Automatic uses this computer's graphics chip when an app can't play the file as-is. Choose Off only if converted video looks wrong.",
    ),
    options: [
      { value: "auto", label: t("Automatic") },
      { value: "none", label: t("Off (software)") },
      { value: "videotoolbox", label: "VideoToolbox (Apple)" },
      { value: "nvenc", label: "NVENC (NVIDIA)" },
      { value: "qsv", label: "Quick Sync (Intel)" },
      { value: "vaapi", label: "VA-API (Linux)" },
    ],
  };

  const RTX_MODE = {
    key: "mode",
    label: "RTX Video Super Resolution",
    kind: "select" as const,
    options: [
      { value: "off", label: t("Off") },
      { value: "auto", label: t("Automatic · local streams") },
      { value: "on", label: t("On · eligible streams") },
    ],
    help: t(
      "Cleans up a soft picture before it is sent. The viewer's resolution and bitrate limits still apply. Unsupported videos play normally.",
    ),
  };

  const RTX_DETAILS = [
    {
      key: "sourceMaxHeight",
      label: t("Enhance sources up to"),
      kind: "number" as const,
      min: 144,
      max: 2160,
      step: 1,
      unit: "p",
      help: t("Videos taller than this play normally, without enhancement."),
    },
    {
      key: "targetMaxHeight",
      label: t("Enhanced resolution limit"),
      kind: "number" as const,
      min: 144,
      max: 2160,
      step: 1,
      unit: "p",
      help: t("The tallest picture enhancement is allowed to produce."),
    },
    {
      key: "quality",
      label: t("Enhancement quality"),
      kind: "select" as const,
      numeric: true,
      options: [
        { value: "1", label: t("Low") },
        { value: "2", label: t("Medium") },
        { value: "3", label: t("High") },
        { value: "4", label: t("Ultra") },
        { value: "21", label: t("Streaming medium") },
        { value: "23", label: t("Streaming ultra") },
      ],
      help: t(
        "Higher quality uses more of the graphics card. Streaming choices need a recent NVIDIA card. Medium is a good start.",
      ),
    },
    {
      key: "maxConcurrent",
      label: t("Simultaneous enhancements"),
      kind: "number" as const,
      min: 1,
      max: 8,
      step: 1,
      help: t("Additional streams play normally once this many are already being enhanced."),
    },
    {
      key: "executablePath",
      label: t("NVEnc executable path"),
      kind: "text" as const,
      help: t("Leave empty and Ether sets this up itself. A path you fill in is never replaced."),
    },
  ];

  return [
    {
      id: "convert",
      title: t("Converting for the app"),
      description: t(
        "Ether plays the file directly when the app can. It converts the video on this computer only when it has to.",
      ),
      fields: [HW_ACCEL],
      more: [
        {
          key: "vaapiDevice",
          label: t("VA-API device"),
          kind: "text",
          help: t(
            "Linux graphics device, for example /dev/dri/renderD128. Leave empty unless converted video fails to start.",
          ),
        },
        {
          key: "maxTranscodes",
          label: t("Simultaneous transcodes"),
          kind: "number",
          min: 0,
          step: 1,
          help: t(
            "How many videos can be converted at once. Leave the default unless several people watch together and the computer struggles.",
          ),
        },
        {
          key: "remoteMaxBitrateKbps",
          label: t("Remote bitrate cap"),
          kind: "number",
          min: 0,
          step: 500,
          unit: "kbps",
          help: t(
            "Limits picture quality for people watching away from home, so the stream fits their connection. 0 means no limit.",
          ),
        },
      ],
    },
    {
      id: "enhance",
      title: t("Sharper picture"),
      description: t(
        "On a supported NVIDIA graphics card, Ether can clean up a soft video before sending it. This uses extra graphics power, and Ether installs what it needs on a supported Windows computer.",
      ),
      fields: [
        {
          key: "rtxVideo",
          label: "RTX Video Super Resolution",
          kind: "group",
          fields: [RTX_MODE],
        },
      ],
      more: [
        {
          key: "rtxVideo",
          label: "RTX Video Super Resolution",
          kind: "group",
          fields: RTX_DETAILS,
        },
      ],
    },
    {
      id: "programs",
      title: t("Converter programs"),
      description: t(
        "Ether includes the programs it needs. Change a path only if you installed your own copy.",
      ),
      fields: [],
      more: [
        {
          key: "ffmpegPath",
          label: t("ffmpeg path"),
          kind: "text",
          wide: true,
          help: t("Leave empty to use the bundled ffmpeg."),
        },
        {
          key: "ffprobePath",
          label: t("ffprobe path"),
          kind: "text",
          wide: true,
          help: t("Leave empty to use the program Ether includes."),
        },
        {
          key: "tempDir",
          label: t("Temporary folder"),
          kind: "text",
          wide: true,
          help: t(
            "Where converted video is written while it plays. Empty uses the normal temporary folder.",
          ),
        },
      ],
    },
  ];
});

export const TRAKT: FieldGroup[] = localized(() => [
  {
    title: "Trakt",
    description: t("Sync watch history and lists with Trakt."),
    fields: [
      { key: "enabled", label: t("Enabled"), kind: "bool" },
      { key: "clientId", label: t("Client ID"), kind: "text", wide: true },
      { key: "clientSecret", label: t("Client secret"), kind: "secret" },
    ],
  },
]);

export const OPENSUBTITLES: FieldGroup[] = localized(() => [
  {
    title: "OpenSubtitles",
    description: t("Fetch subtitles when a release doesn't include them."),
    fields: [
      { key: "enabled", label: t("Enabled"), kind: "bool" },
      { key: "apiKey", label: t("API key"), kind: "secret" },
      { key: "username", label: t("Username"), kind: "text" },
      { key: "password", label: t("Password"), kind: "secret" },
    ],
  },
]);

export const UPDATES: FieldGroup[] = localized(() => [
  {
    title: t("Release channel"),
    fields: [
      {
        key: "channel",
        label: t("Channel"),
        kind: "select",
        options: [
          { value: "stable", label: t("Stable") },
          { value: "beta", label: t("Beta") },
        ],
      },
      {
        key: "autoUpdate",
        label: t("Install updates automatically"),
        kind: "bool",
        help: t("Installs during the maintenance window, once nobody is watching."),
      },
      {
        key: "maintenanceStart",
        label: t("Maintenance window starts"),
        kind: "text",
        placeholder: "03:00",
        help: t("24-hour local time."),
      },
      {
        key: "maintenanceEnd",
        label: t("Maintenance window ends"),
        kind: "text",
        placeholder: "06:00",
        help: t("May wrap past midnight."),
      },
      {
        key: "feedUrl",
        label: t("Update feed"),
        kind: "text",
        wide: true,
        help: t("Leave empty for the official feed."),
      },
    ],
  },
]);

export const SPIKE: FieldGroup[] = localized(() => [
  {
    title: t("Test playback with a local file"),
    description: t(
      "Serve a file from this computer to your Plex apps to tell whether a playback problem is the app or the stream. This is a diagnostic, not a library; turn it off when you're done.",
    ),
    fields: [
      { key: "enabled", label: t("Serve test files"), kind: "bool" },
      {
        key: "localFiles",
        label: t("Test files"),
        kind: "list",
        placeholder: "/path/to/movie.mkv",
        help: t("Full paths to video files on this computer."),
      },
    ],
  },
]);

export const QUALITY_SOURCES = [
  "remux",
  "bluray",
  "webdl",
  "webrip",
  "hdtv",
  "dvd",
  "screener",
  "cam",
];
export const QUALITY_CODECS = ["av1", "hevc", "h264", "vp9", "mpeg2"];
export const QUALITY_AUDIO = ["truehd", "dtshd", "eac3", "ac3", "dts", "aac", "flac", "opus"];

export const MARKERS: FieldGroup[] = localized(() => [
  {
    title: t("Chapters and skip markers"),
    fields: [
      {
        key: "theIntroDb",
        label: t("Look up intro and credits markers"),
        kind: "bool",
        help: t("Use the public TheIntroDB lookup, cached per episode and runtime."),
      },
      { key: "apiKey", label: t("Optional TheIntroDB key"), kind: "secret" },
    ],
  },
]);

/** Debrid section settings, edited in place under the provider list (Sources › Debrid). */
export const DEBRID: FieldGroup[] = localized(() => [
  {
    title: t("Torrent handling"),
    fields: [
      {
        key: "cachedOnly",
        label: t("Cached torrents only"),
        kind: "bool",
        help: t(
          "Only use torrents your debrid service already holds. Nothing is downloaded and playback never waits. Turn off to have Ether send the best uncached release of a title to your first debrid service when someone presses Play; it plays once the download finishes (the app says so meanwhile).",
        ),
      },
      {
        key: "removeUncached",
        label: t("Remove uncached torrents"),
        kind: "bool",
        help: t(
          "Remove torrents Ether added that turned out not to be cached. Cached torrents stay on your account so the next play starts instantly and they are reused instead of re-added.",
        ),
      },
    ],
  },
  {
    title: t("Advanced"),
    description: t("Leave at 0 for the built-in behaviour."),
    fields: [
      {
        key: "chunkBytes",
        label: t("Largest range request"),
        kind: "bytes",
        unit: "MiB",
        help: t(
          "Requests start at 512 KiB for a fast first byte and grow up to this size. 0 = 8 MiB; at most 64 MiB.",
        ),
      },
      {
        key: "readAheadChunks",
        label: t("Fixed read-ahead"),
        kind: "number",
        min: -1,
        max: 32,
        step: 1,
        unit: t("chunks"),
        help: t(
          "0 adapts read-ahead to the measured download speed. Set up to 32 chunks to fix the window; -1 turns read-ahead off.",
        ),
      },
      {
        key: "maxConnections",
        label: t("Connections per file"),
        kind: "number",
        min: 0,
        max: 16,
        step: 1,
        help: t("Concurrent range requests per open file. 0 = 4; at most 16."),
      },
    ],
  },
]);

/** Plex offline downloads (General › Downloads). */
export const DOWNLOADS: FieldGroup[] = localized(() => [
  {
    title: t("Downloads"),
    description: t(
      "Lets Plex apps save titles for offline viewing. Ether remuxes each one on demand.",
    ),
    fields: [
      { key: "enabled", label: t("Allow downloads"), kind: "bool" },
      {
        key: "allowFriends",
        label: t("Friends can download"),
        kind: "bool",
        help: t("Off lets only you download. A viewer's access policy can still allow or deny it."),
      },
      {
        key: "maxHeight",
        label: t("Highest resolution"),
        kind: "number",
        min: 0,
        step: 1,
        unit: "p",
        help: t("Downloads are capped at this height. 0 uses 1080p."),
      },
    ],
  },
  {
    title: t("Limits"),
    fields: [
      {
        key: "maxConcurrent",
        label: t("Downloads at once"),
        kind: "number",
        min: 0,
        step: 1,
        help: t("Across the whole server. 0 uses 2."),
      },
      {
        key: "maxConcurrentPerUser",
        label: t("Downloads at once per viewer"),
        kind: "number",
        min: 0,
        step: 1,
        help: t("0 uses 1."),
      },
      {
        key: "diskBudgetBytes",
        label: t("Temporary disk space"),
        kind: "bytes",
        unit: "GiB",
        help: t(
          "Maximum space for prepared downloads. Set to 0 to use the default limit of 20 GiB.",
        ),
      },
      {
        key: "expiry",
        label: t("Keep prepared files for"),
        kind: "duration",
        help: t("A finished download is deleted after this long if the app hasn't fetched it."),
      },
    ],
  },
]);

/** Per-viewer touches in Plex apps (General › Personalization). */
export const PERSONALIZE: FieldGroup[] = localized(() => [
  {
    title: t("Personalization"),
    description: t("Per-viewer touches in Plex apps. Turn any off for plain Plex behaviour."),
    fields: [
      {
        key: "postPlay",
        label: t("Personal “up next” picks"),
        kind: "bool",
        help: t("After a movie or finale, suggest what this viewer is most likely to watch next."),
      },
      {
        key: "postPlayPrebuffer",
        label: t("Pre-buffer the top pick"),
        kind: "bool",
        help: t("Warm the first suggestion in the last minutes so it starts instantly."),
      },
      {
        key: "microReasons",
        label: t("Match and finish time on details"),
        kind: "bool",
        help: t("Show “96% Match · Ends at 10:45 PM” on a title's detail page."),
      },
      {
        key: "sessionIntent",
        label: t("Adapt Home to tonight's browsing"),
        kind: "bool",
        help: t("Titles a viewer opened but didn't play steer the rows they see next."),
      },
      {
        key: "clockFormat",
        label: t("Finish time format"),
        kind: "select",
        options: [
          { value: "", label: t("From the language") },
          { value: "12h", label: t("12-hour") },
          { value: "24h", label: t("24-hour") },
        ],
      },
    ],
  },
]);

/** Trailers and extras (General › Trailers & extras). */
export const EXTRAS: FieldGroup[] = localized(() => [
  {
    title: t("Trailers & extras"),
    description: t(
      "TMDB only lists YouTube and Vimeo trailers, which Plex apps can't play. Point Ether at a yt-dlp-compatible tool you install to turn them into playable streams.",
    ),
    fields: [
      {
        key: "enabled",
        label: t("Trailers and Cinema Trailers"),
        kind: "bool",
        help: t("Without a working tool no extras are offered."),
      },
      {
        key: "resolverCommand",
        label: t("Resolver command"),
        kind: "text",
        wide: true,
        placeholder: "yt-dlp",
        help: t("Path or command name. Ether doesn't ship one."),
      },
      {
        key: "resolverArgs",
        label: t("Arguments"),
        kind: "list",
        placeholder: "-g, -f, b[ext=mp4]/b, --no-playlist, {url}",
        help: t(
          "Leave empty for the yt-dlp default. {url}, {site}, {key} and {id} are filled in; no shell is used.",
        ),
      },
      { key: "resolverTimeout", label: t("Give up after"), kind: "duration" },
      {
        key: "prewarmBytes",
        label: t("Pre-buffer the feature"),
        kind: "bytes",
        unit: "MiB",
        help: t("Buffered while the trailer plays, 32–64 MiB. 0 uses 48 MiB."),
      },
    ],
  },
]);
