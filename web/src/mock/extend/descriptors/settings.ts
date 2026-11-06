/** Built-in settings descriptors the mock server serves. Generated mirror of
 * internal/config/schema built-ins; settings drift from Go fails
 * settings-schema tests (testdata/builtins.json is the shared golden). */
import type { Descriptor } from "../../../lib/extend";
import type { SettingsSchemaMeta } from "../../../pages/settings/SchemaForm";

export const descriptors: Descriptor<SettingsSchemaMeta>[] = [
  {
    kind: "settingsSchema",
    id: "server",
    owner: "builtin",
    label: {
      source: "What viewers see",
    },
    order: 0,
    meta: {
      configKey: "server",
      schema: {
        groups: [
          {
            id: "identity",
            title: {
              source: "What viewers see",
            },
            description: {
              source: "The names apps show for this server and its libraries.",
            },
            fields: [
              {
                key: "friendlyName",
                label: {
                  source: "Server name",
                },
                help: {
                  source: "How the server appears in apps.",
                },
                kind: "text",
              },
              {
                key: "moviesLibraryName",
                label: {
                  source: "Movies library name",
                },
                help: {
                  source: "Name shown in Plex. A rename inside Plex does not stick.",
                },
                kind: "text",
              },
              {
                key: "tvShowsLibraryName",
                label: {
                  source: "TV Shows library name",
                },
                help: {
                  source: "Name shown in Plex. A rename inside Plex does not stick.",
                },
                kind: "text",
              },
            ],
          },
          {
            id: "connect",
            title: {
              source: "How apps connect",
            },
            description: {
              source: "The address apps use to reach this computer.",
            },
            fields: [
              {
                key: "port",
                label: {
                  source: "Port",
                },
                help: {
                  source: "Plex apps expect 32400. A restart is needed after changing it.",
                },
                kind: "number",
                min: 1,
                max: 65535,
                step: 1,
              },
            ],
            more: [
              {
                key: "bindAddress",
                label: {
                  source: "Listen address",
                },
                help: {
                  source: "0.0.0.0 listens on every network interface.",
                },
                kind: "text",
              },
              {
                key: "machineIdentifier",
                label: {
                  source: "Machine identifier",
                },
                help: {
                  source: "Generated on first run; never changes.",
                },
                kind: "readonly",
              },
              {
                key: "webFrameOrigins",
                label: {
                  source: "Sites that may embed this page",
                },
                help: {
                  source: "Exact addresses allowed to frame the owner page. Empty means nowhere.",
                },
                kind: "list",
                placeholder: "https://home.example.com",
              },
              {
                key: "trustedProxyCIDRs",
                label: {
                  source: "Trusted proxy networks",
                },
                help: {
                  source: "Networks whose forwarding headers Ether trusts. Empty trusts none.",
                },
                kind: "list",
                placeholder: "10.0.0.0/8",
              },
              {
                key: "logLevel",
                label: {
                  source: "Log level",
                },
                kind: "select",
                options: [
                  {
                    value: "debug",
                    label: {
                      source: "Debug",
                    },
                  },
                  {
                    value: "info",
                    label: {
                      source: "Info",
                    },
                  },
                  {
                    value: "warn",
                    label: {
                      source: "Warnings",
                    },
                  },
                  {
                    value: "error",
                    label: {
                      source: "Errors only",
                    },
                  },
                ],
              },
            ],
          },
          {
            id: "streams",
            title: {
              source: "Who can watch at once",
            },
            description: {
              source:
                "Leave these at the defaults unless too many people are watching at the same time.",
            },
            fields: [],
            more: [
              {
                key: "defaultStreamLimit",
                label: {
                  source: "Streams per viewer",
                },
                help: {
                  source: "Default simultaneous streams for each viewer. 0 = unlimited.",
                },
                kind: "number",
                min: 0,
                step: 1,
              },
              {
                key: "maxStreams",
                label: {
                  source: "Server-wide limit",
                },
                help: {
                  source: "0 derives the limit from your Usenet connections.",
                },
                kind: "number",
                min: 0,
                step: 1,
              },
            ],
          },
        ],
      },
    },
  },
  {
    kind: "settingsSchema",
    id: "plex",
    owner: "builtin",
    label: {
      source: "Remote access",
    },
    order: 1,
    meta: {
      configKey: "plex",
      schema: {
        groups: [
          {
            id: "remote",
            title: {
              source: "Remote access",
            },
            description: {
              source: "Let Plex apps reach Ether from outside your network.",
            },
            fields: [
              {
                key: "remoteAccess",
                label: {
                  source: "Allow remote access",
                },
                help: {
                  source: "Publishes your public address to plex.tv.",
                },
                kind: "bool",
              },
            ],
            more: [
              {
                key: "useUpnp",
                label: {
                  source: "Map the port automatically (UPnP)",
                },
                help: {
                  source: "Asks your router to forward the port.",
                },
                kind: "bool",
              },
              {
                key: "publicPort",
                label: {
                  source: "Public port",
                },
                help: {
                  source: "The port forwarded on your router. 0 uses the server port.",
                },
                kind: "number",
                min: 0,
                max: 65535,
                step: 1,
              },
              {
                key: "publicHost",
                label: {
                  source: "Public host name",
                },
                help: {
                  source: "Optional. Leave empty to detect your public address.",
                },
                kind: "text",
              },
              {
                key: "customUrls",
                label: {
                  source: "Custom server URLs",
                },
                help: {
                  source: "Extra addresses Plex apps should try, comma-separated.",
                },
                kind: "list",
                placeholder: "https://ether.example.com:443",
              },
            ],
          },
          {
            id: "secure",
            title: {
              source: "Secure connections",
            },
            description: {
              source: "How Plex apps encrypt the connection to this server.",
            },
            fields: [
              {
                key: "secureConnections",
                label: {
                  source: "Secure connections",
                },
                kind: "select",
                options: [
                  {
                    value: "required",
                    label: {
                      source: "Required",
                    },
                  },
                  {
                    value: "preferred",
                    label: {
                      source: "Preferred",
                    },
                  },
                  {
                    value: "disabled",
                    label: {
                      source: "Disabled",
                    },
                  },
                ],
              },
            ],
            more: [
              {
                key: "certFile",
                label: {
                  source: "Certificate file",
                },
                help: {
                  source: "Leave empty to use the plex.direct certificate Ether manages.",
                },
                kind: "text",
                wide: true,
              },
              {
                key: "keyFile",
                label: {
                  source: "Key file",
                },
                help: {
                  source: "The private key that matches the certificate file.",
                },
                kind: "text",
                wide: true,
              },
            ],
          },
          {
            id: "compatibility",
            title: {
              source: "Compatibility",
            },
            fields: [
              {
                key: "trackLatestVersion",
                label: {
                  source: "Report the latest public PMS version",
                },
                help: {
                  source:
                    "Off reports the version Ether is verified against. Plex enables features by server version.",
                },
                kind: "bool",
              },
              {
                key: "latestCompatibilityVersion",
                label: {
                  source: "Latest public PMS version",
                },
                kind: "readonly",
              },
            ],
            more: [
              {
                key: "compatibilityVersion",
                label: {
                  source: "Compatibility version override",
                },
                help: {
                  source:
                    "Leave empty to use the verified version, or the latest public version when tracking is on.",
                },
                kind: "text",
              },
            ],
          },
        ],
      },
    },
  },
  {
    kind: "settingsSchema",
    id: "cache",
    owner: "builtin",
    label: {
      source: "Disk cache",
    },
    order: 2,
    meta: {
      configKey: "cache",
      schema: {
        groups: [
          {
            title: {
              source: "Disk cache",
            },
            description: {
              source: "Segments from Usenet are cached on disk so seeks and rewatches are instant.",
            },
            fields: [
              {
                key: "dir",
                label: {
                  source: "Cache folder",
                },
                kind: "text",
                wide: true,
              },
              {
                key: "budgetBytes",
                label: {
                  source: "Disk budget",
                },
                help: {
                  source: "The oldest segments are evicted when the cache reaches this size.",
                },
                kind: "bytes",
                unit: {
                  source: "GiB",
                },
              },
            ],
          },
          {
            title: {
              source: "Prefetch",
            },
            fields: [
              {
                key: "prefetchNextEpisode",
                label: {
                  source: "Prefetch the next episode",
                },
                help: {
                  source: "Warms the opening of the next episode so it starts instantly.",
                },
                kind: "bool",
              },
              {
                key: "nextEpisodeAt",
                label: {
                  source: "Start prefetching at",
                },
                help: {
                  source: "Fraction of the current episode watched (0–1).",
                },
                kind: "number",
                min: 0,
                max: 1,
                step: 0.05,
              },
              {
                key: "prefetchBytes",
                label: {
                  source: "Prefetch size",
                },
                help: {
                  source: "How much of the next episode to pull ahead.",
                },
                kind: "bytes",
                unit: {
                  source: "MiB",
                },
              },
              {
                key: "keepInProgressWarm",
                label: {
                  source: "Keep in-progress titles warm",
                },
                help: {
                  source: "Keeps the resume point of unfinished titles cached.",
                },
                kind: "bool",
              },
              {
                key: "inProgressPinMaxAge",
                label: {
                  source: "Stop keeping a title warm after",
                },
                help: {
                  source:
                    "An unfinished title nobody has watched for this long loses its cache pin. Empty uses 30 days.",
                },
                kind: "duration",
                placeholder: "720h",
              },
              {
                key: "inProgressPinPercent",
                label: {
                  source: "Share of the cache for in-progress titles",
                },
                help: {
                  source:
                    "Percent of the disk budget unfinished titles may keep pinned; the least recently watched lose their pin first. 0 uses 50.",
                },
                kind: "number",
                min: 0,
                max: 100,
                step: 5,
              },
            ],
          },
        ],
      },
    },
  },
  {
    kind: "settingsSchema",
    id: "catalog",
    owner: "builtin",
    label: {
      source: "Names and dates",
    },
    order: 3,
    meta: {
      configKey: "catalog",
      schema: {
        groups: [
          {
            title: {
              source: "Names and dates",
            },
            description: {
              source:
                "Language and region choose title names, descriptions, release dates and which titles count as available.",
            },
            fields: [
              {
                key: "language",
                label: {
                  source: "Language",
                },
                help: {
                  source: "Metadata language, e.g. en-US or fr-FR.",
                },
                kind: "text",
              },
              {
                key: "region",
                label: {
                  source: "Region",
                },
                help: {
                  source: "Two-letter region for release dates and streaming availability.",
                },
                kind: "text",
              },
              {
                key: "refreshInterval",
                label: {
                  source: "Refresh every",
                },
                kind: "duration",
              },
            ],
          },
          {
            title: {
              source: "What's browsable",
            },
            description: {
              source: "Hide obscure titles from libraries. They stay searchable.",
            },
            fields: [
              {
                key: "minPopularity",
                label: {
                  source: "Minimum popularity",
                },
                kind: "number",
                min: 0,
                step: 1,
              },
              {
                key: "minVotes",
                label: {
                  source: "Minimum votes",
                },
                kind: "number",
                min: 0,
                step: 10,
              },
              {
                key: "includeAdult",
                label: {
                  source: "Include adult titles",
                },
                kind: "bool",
              },
              {
                key: "hideFranchiseCollections",
                label: {
                  source: "Hide automatic franchise collections",
                },
                help: {
                  source: "Hide automatic franchise groups, such as Zootopia.",
                },
                kind: "bool",
              },
            ],
          },
        ],
      },
    },
  },
  {
    kind: "settingsSchema",
    id: "playVersions",
    owner: "builtin",
    label: {
      source: "Versions in the app",
    },
    order: 4,
    meta: {
      configKey: "resolver",
      schema: {
        groups: [
          {
            title: {
              source: "Versions in the app",
            },
            description: {
              source: "Extra copies of the same picture, after Auto.",
            },
            fields: [
              {
                key: "sameQualityAlternatives",
                label: {
                  source: "Same-quality versions to offer",
                },
                help: {
                  source: "Listed after Auto in the app's version menu.",
                },
                kind: "number",
                min: 0,
                max: 3,
                step: 1,
              },
              {
                key: "offerHigherVersions",
                label: {
                  source: "Offer higher versions",
                },
                help: {
                  source:
                    "Version menus can list releases taller than the profile. Off keeps the ceiling you set. HDR and file-size limits still apply.",
                },
                kind: "bool",
              },
            ],
          },
        ],
      },
    },
  },
  {
    kind: "settingsSchema",
    id: "releaseSearch",
    owner: "builtin",
    label: {
      source: "How Ether searches",
    },
    order: 5,
    meta: {
      configKey: "resolver",
      schema: {
        groups: [
          {
            title: {
              source: "How Ether searches",
            },
            description: {
              source:
                "How many releases Ether keeps ready, and how long it remembers a search. The defaults are enough for a household.",
            },
            fields: [
              {
                key: "maxCandidates",
                label: {
                  source: "Releases to keep ready",
                },
                help: {
                  source: "Fallback releases kept for instant failover.",
                },
                kind: "number",
                min: 0,
                step: 1,
              },
              {
                key: "healthSamples",
                label: {
                  source: "Health check samples",
                },
                help: {
                  source: "Articles checked before a release is trusted.",
                },
                kind: "number",
                min: 0,
                step: 1,
              },
              {
                key: "searchCacheTtl",
                label: {
                  source: "Remember searches for",
                },
                kind: "duration",
              },
              {
                key: "unavailableCooloff",
                label: {
                  source: "Retry unavailable titles after",
                },
                kind: "duration",
              },
            ],
          },
        ],
      },
    },
  },
  {
    kind: "settingsSchema",
    id: "releaseBlocks",
    owner: "builtin",
    label: {
      source: "Words and groups",
    },
    order: 6,
    meta: {
      configKey: "resolver",
      schema: {
        groups: [
          {
            title: {
              source: "Words and groups",
            },
            description: {
              source:
                "Blocked terms match whole tokens. sub matches SUB, not Subtitles or SUBPACK. Subtitle language tags alone do not prove burned-in subtitles.",
            },
            fields: [
              {
                key: "hardSubtitles",
                label: { source: "Burned-in subtitles" },
                kind: "select",
                help: {
                  source:
                    "Video checks run after playback starts. A newly discovered verdict applies to later selections.",
                },
                options: [
                  { value: "", label: { source: "Prefer clean releases" } },
                  { value: "exclude", label: { source: "Exclude known hardsubs" } },
                  { value: "allow", label: { source: "Allow" } },
                ],
              },
              {
                key: "blockedGroups",
                label: {
                  source: "Blocked release groups",
                },
                kind: "list",
                placeholder: "GROUP1, GROUP2",
              },
              {
                key: "blockedTerms",
                label: {
                  source: "Blocked terms",
                },
                kind: "list",
                placeholder: "hardcoded, korsub",
              },
            ],
          },
        ],
      },
    },
  },
  {
    kind: "settingsSchema",
    id: "transcode",
    owner: "builtin",
    label: {
      source: "Converting for the app",
    },
    order: 7,
    meta: {
      configKey: "transcode",
      schema: {
        groups: [
          {
            id: "convert",
            title: {
              source: "Converting for the app",
            },
            description: {
              source:
                "Ether plays the file directly when the app can. It converts the video on this computer only when it has to.",
            },
            fields: [
              {
                key: "hwAccel",
                label: {
                  source: "Hardware acceleration",
                },
                help: {
                  source:
                    "Automatic uses this computer's graphics chip when an app can't play the file as-is. Choose Off only if converted video looks wrong.",
                },
                kind: "select",
                options: [
                  {
                    value: "auto",
                    label: {
                      source: "Automatic",
                    },
                  },
                  {
                    value: "none",
                    label: {
                      source: "Off (software)",
                    },
                  },
                  {
                    value: "videotoolbox",
                    label: {
                      source: "VideoToolbox (Apple)",
                    },
                  },
                  {
                    value: "nvenc",
                    label: {
                      source: "NVENC (NVIDIA)",
                    },
                  },
                  {
                    value: "qsv",
                    label: {
                      source: "Quick Sync (Intel)",
                    },
                  },
                  {
                    value: "vaapi",
                    label: {
                      source: "VA-API (Linux)",
                    },
                  },
                ],
              },
            ],
            more: [
              {
                key: "vaapiDevice",
                label: {
                  source: "VA-API device",
                },
                help: {
                  source:
                    "Linux graphics device, for example /dev/dri/renderD128. Leave empty unless converted video fails to start.",
                },
                kind: "text",
              },
              {
                key: "maxTranscodes",
                label: {
                  source: "Simultaneous transcodes",
                },
                help: {
                  source:
                    "How many videos can be converted at once. Leave the default unless several people watch together and the computer struggles.",
                },
                kind: "number",
                min: 0,
                step: 1,
              },
              {
                key: "remoteMaxBitrateKbps",
                label: {
                  source: "Remote bitrate cap",
                },
                help: {
                  source:
                    "Limits picture quality for people watching away from home, so the stream fits their connection. 0 means no limit.",
                },
                kind: "number",
                min: 0,
                step: 500,
                unit: {
                  source: "kbps",
                },
              },
            ],
          },
          {
            id: "enhance",
            title: {
              source: "Sharper picture",
            },
            description: {
              source:
                "On a supported NVIDIA graphics card, Ether can clean up a soft video before sending it. This uses extra graphics power, and Ether installs what it needs on a supported Windows computer.",
            },
            fields: [
              {
                key: "rtxVideo",
                label: {
                  source: "RTX Video Super Resolution",
                },
                kind: "group",
                fields: [
                  {
                    key: "mode",
                    label: {
                      source: "RTX Video Super Resolution",
                    },
                    help: {
                      source:
                        "Cleans up a soft picture before it is sent. The viewer's resolution and bitrate limits still apply. Unsupported videos play normally.",
                    },
                    kind: "select",
                    options: [
                      {
                        value: "off",
                        label: {
                          source: "Off",
                        },
                      },
                      {
                        value: "auto",
                        label: {
                          source: "Automatic · local streams",
                        },
                      },
                      {
                        value: "on",
                        label: {
                          source: "On · eligible streams",
                        },
                      },
                    ],
                  },
                ],
              },
            ],
            more: [
              {
                key: "rtxVideo",
                label: {
                  source: "RTX Video Super Resolution",
                },
                kind: "group",
                fields: [
                  {
                    key: "sourceMaxHeight",
                    label: {
                      source: "Enhance sources up to",
                    },
                    help: {
                      source: "Videos taller than this play normally, without enhancement.",
                    },
                    kind: "number",
                    min: 144,
                    max: 2160,
                    step: 1,
                    unit: {
                      source: "p",
                    },
                  },
                  {
                    key: "targetMaxHeight",
                    label: {
                      source: "Enhanced resolution limit",
                    },
                    help: {
                      source: "The tallest picture enhancement is allowed to produce.",
                    },
                    kind: "number",
                    min: 144,
                    max: 2160,
                    step: 1,
                    unit: {
                      source: "p",
                    },
                  },
                  {
                    key: "quality",
                    label: {
                      source: "Enhancement quality",
                    },
                    help: {
                      source:
                        "Higher quality uses more of the graphics card. Streaming choices need a recent NVIDIA card. Medium is a good start.",
                    },
                    kind: "select",
                    numeric: true,
                    options: [
                      {
                        value: "1",
                        label: {
                          source: "Low",
                        },
                      },
                      {
                        value: "2",
                        label: {
                          source: "Medium",
                        },
                      },
                      {
                        value: "3",
                        label: {
                          source: "High",
                        },
                      },
                      {
                        value: "4",
                        label: {
                          source: "Ultra",
                        },
                      },
                      {
                        value: "21",
                        label: {
                          source: "Streaming medium",
                        },
                      },
                      {
                        value: "23",
                        label: {
                          source: "Streaming ultra",
                        },
                      },
                    ],
                  },
                  {
                    key: "maxConcurrent",
                    label: {
                      source: "Simultaneous enhancements",
                    },
                    help: {
                      source:
                        "Additional streams play normally once this many are already being enhanced.",
                    },
                    kind: "number",
                    min: 1,
                    max: 8,
                    step: 1,
                  },
                  {
                    key: "executablePath",
                    label: {
                      source: "NVEnc executable path",
                    },
                    help: {
                      source:
                        "Leave empty and Ether sets this up itself. A path you fill in is never replaced.",
                    },
                    kind: "text",
                  },
                ],
              },
            ],
          },
          {
            id: "programs",
            title: {
              source: "Converter programs",
            },
            description: {
              source:
                "Ether includes the programs it needs. Change a path only if you installed your own copy.",
            },
            fields: [],
            more: [
              {
                key: "ffmpegPath",
                label: {
                  source: "ffmpeg path",
                },
                help: {
                  source: "Leave empty to use the bundled ffmpeg.",
                },
                kind: "text",
                wide: true,
              },
              {
                key: "ffprobePath",
                label: {
                  source: "ffprobe path",
                },
                help: {
                  source: "Leave empty to use the program Ether includes.",
                },
                kind: "text",
                wide: true,
              },
              {
                key: "tempDir",
                label: {
                  source: "Temporary folder",
                },
                help: {
                  source:
                    "Where converted video is written while it plays. Empty uses the normal temporary folder.",
                },
                kind: "text",
                wide: true,
              },
            ],
          },
        ],
      },
    },
  },
  {
    kind: "settingsSchema",
    id: "trakt",
    owner: "builtin",
    label: {
      source: "Trakt",
    },
    order: 8,
    meta: {
      configKey: "trakt",
      schema: {
        groups: [
          {
            title: {
              source: "Trakt",
            },
            description: {
              source: "Sync watch history and lists with Trakt.",
            },
            fields: [
              {
                key: "enabled",
                label: {
                  source: "Enabled",
                },
                kind: "bool",
              },
              {
                key: "clientId",
                label: {
                  source: "Client ID",
                },
                kind: "text",
                wide: true,
              },
              {
                key: "clientSecret",
                label: {
                  source: "Client secret",
                },
                kind: "secret",
              },
            ],
          },
        ],
      },
    },
  },
  {
    kind: "settingsSchema",
    id: "openSubtitles",
    owner: "builtin",
    label: {
      source: "OpenSubtitles",
    },
    order: 9,
    meta: {
      configKey: "openSubtitles",
      schema: {
        groups: [
          {
            title: {
              source: "OpenSubtitles",
            },
            description: {
              source: "Fetch subtitles when a release doesn't include them.",
            },
            fields: [
              {
                key: "enabled",
                label: {
                  source: "Enabled",
                },
                kind: "bool",
              },
              {
                key: "apiKey",
                label: {
                  source: "API key",
                },
                kind: "secret",
              },
              {
                key: "username",
                label: {
                  source: "Username",
                },
                kind: "text",
              },
              {
                key: "password",
                label: {
                  source: "Password",
                },
                kind: "secret",
              },
            ],
          },
        ],
      },
    },
  },
  {
    kind: "settingsSchema",
    id: "updates",
    owner: "builtin",
    label: {
      source: "Release channel",
    },
    order: 10,
    meta: {
      configKey: "updates",
      schema: {
        groups: [
          {
            title: {
              source: "Release channel",
            },
            fields: [
              {
                key: "enabled",
                label: {
                  source: "Check for updates",
                },
                help: {
                  source: "Checks the signed release feed once a day.",
                },
                kind: "bool",
              },
              {
                key: "channel",
                label: {
                  source: "Channel",
                },
                kind: "select",
                options: [
                  {
                    value: "stable",
                    label: {
                      source: "Stable",
                    },
                  },
                  {
                    value: "beta",
                    label: {
                      source: "Beta",
                    },
                  },
                ],
              },
              {
                key: "autoUpdate",
                label: {
                  source: "Install updates automatically",
                },
                help: {
                  source: "Installs during the maintenance window, once nobody is watching.",
                },
                kind: "bool",
              },
            ],
            more: [
              {
                key: "maintenanceStart",
                label: {
                  source: "Maintenance window starts",
                },
                help: {
                  source: "24-hour local time.",
                },
                kind: "text",
                placeholder: "03:00",
              },
              {
                key: "maintenanceEnd",
                label: {
                  source: "Maintenance window ends",
                },
                help: {
                  source: "May wrap past midnight.",
                },
                kind: "text",
                placeholder: "06:00",
              },
              {
                key: "feedUrl",
                label: {
                  source: "Update feed",
                },
                help: {
                  source: "Leave empty for the official feed.",
                },
                kind: "text",
                wide: true,
              },
              {
                key: "maxDrain",
                label: {
                  source: "Wait for playback to finish",
                },
                help: {
                  source:
                    "How long a clustered node waits for current playback to end before installing. Leave empty to wait 30 minutes. A server on its own waits until nobody is watching.",
                },
                kind: "duration",
              },
            ],
          },
        ],
      },
    },
  },
  {
    kind: "settingsSchema",
    id: "spike",
    owner: "builtin",
    label: {
      source: "Test playback with a local file",
    },
    order: 11,
    meta: {
      configKey: "spike",
      schema: {
        groups: [
          {
            title: {
              source: "Test playback with a local file",
            },
            description: {
              source: "Play a local file in Plex to see if the problem is the app or the stream.",
            },
            fields: [
              {
                key: "enabled",
                label: {
                  source: "Serve test files",
                },
                kind: "bool",
              },
              {
                key: "localFiles",
                label: {
                  source: "Test files",
                },
                help: {
                  source: "Full paths to video files on this computer.",
                },
                kind: "list",
                placeholder: "/path/to/movie.mkv",
              },
            ],
          },
        ],
      },
    },
  },
  {
    kind: "settingsSchema",
    id: "markers",
    owner: "builtin",
    label: {
      source: "Chapters and skip markers",
    },
    order: 12,
    meta: {
      configKey: "markers",
      schema: {
        groups: [
          {
            title: {
              source: "Chapters and skip markers",
            },
            fields: [
              {
                key: "theIntroDb",
                label: {
                  source: "Look up intro and credits markers",
                },
                help: {
                  source: "Use the public TheIntroDB lookup, cached per episode and runtime.",
                },
                kind: "bool",
              },
              {
                key: "apiKey",
                label: {
                  source: "Optional TheIntroDB key",
                },
                kind: "secret",
              },
            ],
          },
        ],
      },
    },
  },
  {
    kind: "settingsSchema",
    id: "debrid",
    owner: "builtin",
    label: {
      source: "Torrent handling",
    },
    order: 13,
    meta: {
      configKey: "debrid",
      schema: {
        groups: [
          {
            title: {
              source: "Torrent handling",
            },
            fields: [
              {
                key: "cachedOnly",
                label: {
                  source: "Cached torrents only",
                },
                help: {
                  source:
                    "Only use torrents your debrid service already holds. Nothing is downloaded and playback never waits. Turn off to have Ether send the best uncached release of a title to your first debrid service when someone presses Play; it plays once the download finishes (the app says so meanwhile).",
                },
                kind: "bool",
              },
              {
                key: "removeUncached",
                label: {
                  source: "Remove uncached torrents",
                },
                help: {
                  source:
                    "Remove torrents Ether added that turned out not to be cached. Cached torrents stay on your account so the next play starts instantly and they are reused instead of re-added.",
                },
                kind: "bool",
              },
            ],
          },
          {
            title: {
              source: "Advanced",
            },
            description: {
              source: "Leave at 0 for the built-in behaviour.",
            },
            fields: [
              {
                key: "chunkBytes",
                label: {
                  source: "Largest range request",
                },
                help: {
                  source:
                    "Requests start at 512 KiB for a fast first byte and grow up to this size. 0 = 8 MiB; at most 64 MiB.",
                },
                kind: "bytes",
                unit: {
                  source: "MiB",
                },
              },
              {
                key: "readAheadChunks",
                label: {
                  source: "Fixed read-ahead",
                },
                help: {
                  source:
                    "0 adapts read-ahead to the measured download speed. Set up to 32 chunks to fix the window; -1 turns read-ahead off.",
                },
                kind: "number",
                min: -1,
                max: 32,
                step: 1,
                unit: {
                  source: "chunks",
                },
              },
              {
                key: "maxConnections",
                label: {
                  source: "Connections per file",
                },
                help: {
                  source: "Concurrent range requests per open file. 0 = 4; at most 16.",
                },
                kind: "number",
                min: 0,
                max: 16,
                step: 1,
              },
            ],
          },
        ],
      },
    },
  },
  {
    kind: "settingsSchema",
    id: "downloads",
    owner: "builtin",
    label: {
      source: "Downloads",
    },
    order: 14,
    meta: {
      configKey: "downloads",
      schema: {
        groups: [
          {
            title: {
              source: "Downloads",
            },
            description: {
              source:
                "Lets Plex apps save titles for offline viewing. Ether remuxes each one on demand.",
            },
            fields: [
              {
                key: "enabled",
                label: {
                  source: "Allow downloads",
                },
                kind: "bool",
              },
              {
                key: "allowFriends",
                label: {
                  source: "Friends can download",
                },
                help: {
                  source:
                    "Off lets only you download. A viewer's access policy can still allow or deny it.",
                },
                kind: "bool",
              },
            ],
            more: [
              {
                key: "maxHeight",
                label: {
                  source: "Highest resolution",
                },
                help: {
                  source: "Downloads are capped at this resolution. 0 uses 1080p.",
                },
                kind: "number",
                min: 0,
                step: 1,
                unit: {
                  source: "p",
                },
              },
              {
                key: "maxConcurrent",
                label: {
                  source: "Downloads at once",
                },
                help: {
                  source: "Across the whole server. 0 uses 2.",
                },
                kind: "number",
                min: 0,
                step: 1,
              },
              {
                key: "maxConcurrentPerUser",
                label: {
                  source: "Downloads at once per viewer",
                },
                help: {
                  source: "0 uses 1.",
                },
                kind: "number",
                min: 0,
                step: 1,
              },
              {
                key: "diskBudgetBytes",
                label: {
                  source: "Temporary disk space",
                },
                help: {
                  source:
                    "Maximum space for prepared downloads. Set to 0 to use the default limit of 20 GiB.",
                },
                kind: "bytes",
                unit: {
                  source: "GiB",
                },
              },
              {
                key: "expiry",
                label: {
                  source: "Keep prepared files for",
                },
                help: {
                  source:
                    "A finished download is deleted after this long if the app hasn't fetched it.",
                },
                kind: "duration",
              },
            ],
          },
        ],
      },
    },
  },
  {
    kind: "settingsSchema",
    id: "personalize",
    owner: "builtin",
    label: {
      source: "Personalization",
    },
    order: 15,
    meta: {
      configKey: "personalize",
      schema: {
        groups: [
          {
            title: {
              source: "Personalization",
            },
            description: {
              source: "Extras for each viewer. Turn any off for the plain layout.",
            },
            fields: [
              {
                key: "postPlay",
                label: {
                  source: "Personal “up next” picks",
                },
                help: {
                  source:
                    "After a movie or finale, suggest what this viewer is most likely to watch next.",
                },
                kind: "bool",
              },
              {
                key: "postPlayPrebuffer",
                label: {
                  source: "Pre-buffer the top pick",
                },
                help: {
                  source: "Warm the first suggestion in the last minutes so it starts instantly.",
                },
                kind: "bool",
              },
              {
                key: "microReasons",
                label: {
                  source: "Match and finish time on details",
                },
                help: {
                  source: "Show “96% Match · Ends at 10:45 PM” on a title's detail page.",
                },
                kind: "bool",
              },
              {
                key: "sessionIntent",
                label: {
                  source: "Adapt Home to tonight's browsing",
                },
                help: {
                  source: "Titles a viewer opened but didn't play steer the rows they see next.",
                },
                kind: "bool",
              },
            ],
            more: [
              {
                key: "clockFormat",
                label: {
                  source: "Finish time format",
                },
                kind: "select",
                options: [
                  {
                    value: "",
                    label: {
                      source: "From the language",
                    },
                  },
                  {
                    value: "12h",
                    label: {
                      source: "12-hour",
                    },
                  },
                  {
                    value: "24h",
                    label: {
                      source: "24-hour",
                    },
                  },
                ],
              },
            ],
          },
        ],
      },
    },
  },
  {
    kind: "settingsSchema",
    id: "extras",
    owner: "builtin",
    label: {
      source: "Trailers & extras",
    },
    order: 16,
    meta: {
      configKey: "extras",
      schema: {
        groups: [
          {
            title: {
              source: "Trailers & extras",
            },
            description: {
              source: "Play a trailer before a movie or episode.",
            },
            fields: [
              {
                key: "enabled",
                label: {
                  source: "Trailers and Cinema Trailers",
                },
                help: {
                  source: "Offer trailers in apps.",
                },
                kind: "bool",
              },
            ],
            more: [
              {
                key: "resolverCommand",
                label: {
                  source: "Resolver command",
                },
                help: {
                  source: "Command that turns a trailer page into a stream.",
                },
                kind: "text",
                placeholder: "yt-dlp",
                wide: true,
              },
              {
                key: "resolverArgs",
                label: {
                  source: "Arguments",
                },
                help: {
                  source:
                    "Leave empty for the yt-dlp default. {url}, {site}, {key} and {id} are filled in; no shell is used.",
                },
                kind: "list",
                placeholder: "-g, -f, b[ext=mp4]/b, --no-playlist, {url}",
              },
              {
                key: "resolverTimeout",
                label: {
                  source: "Give up after",
                },
                kind: "duration",
              },
              {
                key: "prewarmBytes",
                label: {
                  source: "Pre-buffer the feature",
                },
                help: {
                  source: "Buffered while the trailer plays, 32–64 MiB. 0 uses 48 MiB.",
                },
                kind: "bytes",
                unit: {
                  source: "MiB",
                },
              },
            ],
          },
        ],
      },
    },
  },
  {
    kind: "settingsSchema",
    id: "serverCluster",
    owner: "builtin",
    label: {
      source: "What viewers see",
    },
    order: 17,
    meta: {
      configKey: "server",
      schema: {
        groups: [
          {
            id: "identity",
            title: {
              source: "What viewers see",
            },
            description: {
              source: "The names apps show for this server and its libraries.",
            },
            fields: [
              {
                key: "friendlyName",
                label: {
                  source: "Server name",
                },
                help: {
                  source: "How the server appears in apps.",
                },
                kind: "text",
              },
              {
                key: "moviesLibraryName",
                label: {
                  source: "Movies library name",
                },
                help: {
                  source: "Name shown in Plex. A rename inside Plex does not stick.",
                },
                kind: "text",
              },
              {
                key: "tvShowsLibraryName",
                label: {
                  source: "TV Shows library name",
                },
                help: {
                  source: "Name shown in Plex. A rename inside Plex does not stick.",
                },
                kind: "text",
              },
            ],
          },
          {
            id: "connect",
            title: {
              source: "How apps connect",
            },
            description: {
              source: "The address apps use to reach this computer.",
            },
            fields: [],
            more: [
              {
                key: "webFrameOrigins",
                label: {
                  source: "Sites that may embed this page",
                },
                help: {
                  source: "Exact addresses allowed to frame the owner page. Empty means nowhere.",
                },
                kind: "list",
                placeholder: "https://home.example.com",
              },
            ],
          },
          {
            id: "streams",
            title: {
              source: "Who can watch at once",
            },
            description: {
              source:
                "Leave these at the defaults unless too many people are watching at the same time.",
            },
            fields: [],
            more: [
              {
                key: "defaultStreamLimit",
                label: {
                  source: "Streams per viewer",
                },
                help: {
                  source: "Default simultaneous streams for each viewer. 0 = unlimited.",
                },
                kind: "number",
                min: 0,
                step: 1,
              },
              {
                key: "maxStreams",
                label: {
                  source: "Server-wide limit",
                },
                help: {
                  source: "0 derives the limit from your Usenet connections.",
                },
                kind: "number",
                min: 0,
                step: 1,
              },
            ],
          },
          {
            id: "node",
            title: {
              source: "This computer",
            },
            description: {
              source:
                "These apply only to this computer. Every other setting is shared by the cluster.",
            },
            fields: [
              {
                key: "port",
                label: {
                  source: "Port",
                },
                help: {
                  source: "Plex apps expect 32400. A restart is needed after changing it.",
                },
                kind: "number",
                min: 1,
                max: 65535,
                step: 1,
              },
            ],
            more: [
              {
                key: "bindAddress",
                label: {
                  source: "Listen address",
                },
                help: {
                  source: "0.0.0.0 listens on every network interface.",
                },
                kind: "text",
              },
              {
                key: "machineIdentifier",
                label: {
                  source: "Machine identifier",
                },
                help: {
                  source: "Generated on first run; never changes.",
                },
                kind: "readonly",
              },
              {
                key: "trustedProxyCIDRs",
                label: {
                  source: "Trusted proxy networks",
                },
                help: {
                  source: "Networks whose forwarding headers Ether trusts. Empty trusts none.",
                },
                kind: "list",
                placeholder: "10.0.0.0/8",
              },
              {
                key: "logLevel",
                label: {
                  source: "Log level",
                },
                kind: "select",
                options: [
                  {
                    value: "debug",
                    label: {
                      source: "Debug",
                    },
                  },
                  {
                    value: "info",
                    label: {
                      source: "Info",
                    },
                  },
                  {
                    value: "warn",
                    label: {
                      source: "Warnings",
                    },
                  },
                  {
                    value: "error",
                    label: {
                      source: "Errors only",
                    },
                  },
                ],
              },
              {
                key: "clusterWeight",
                label: {
                  source: "Node weight",
                },
                help: {
                  source:
                    "This computer's share of each provider's connections and request limits, relative to the others. 2 gets twice the share of a computer at 1; 0 means 1.",
                },
                kind: "number",
                min: 0,
                max: 100,
                step: 1,
              },
            ],
          },
        ],
      },
    },
  },
];
