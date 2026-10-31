/**
 * In-memory fake of the TRaSH Guides and Profilarr (Dictionarry) profile
 * imports (/ether/api/releases/trash, /dictionarry and /quality-import).
 * Used by mock mode and the tests; never shipped.
 */
import type { ImportFormat, QualityImportPlan, StarrApp, TrashProfile } from "../lib/releases";
import type { EtherConfig, QualityProfile } from "../lib/types";

type Reply = { status: number; body: unknown };

const ok = (body: unknown, status = 200): Reply => ({ status, body });
const fail = (status: number, code: string, message: string): Reply => ({
  status,
  body: { error: { code, message } },
});

interface FakeFormat {
  id: string;
  name: string;
  scores: Record<string, number>;
  /** Why Ether can't evaluate it, for formats using indexer flags and the like. */
  unsupported?: string;
}

const FORMATS: Record<StarrApp, FakeFormat[]> = {
  radarr: [
    { id: "ed38b889b31be83fda192888e2286d83", name: "BR-DISK", scores: { default: -10000 } },
    { id: "90a6f9a284dff5103f6346090e6280c8", name: "LQ", scores: { default: -10000 } },
    { id: "dc98083864ea246d05a42df0d05f81cc", name: "x265 (HD)", scores: { default: -10000 } },
    { id: "b8cd450cbfa689c0259a01d9e29ba3d6", name: "3D", scores: { default: -10000 } },
    { id: "e7718d7a3ce595f289bfee26adc178f5", name: "Repack/Proper", scores: { default: 5 } },
    {
      id: "c20f169ef63c5f40c2def54abaf4438e",
      name: "WEB Tier 01",
      scores: { default: 1700, "sqp-1-2160p": 1800 },
    },
    {
      id: "923b6abef9b17f937fab56cfcf89e1f1",
      name: "DV (w/o HDR fallback)",
      scores: { default: -10000 },
    },
    {
      id: "0d91270a7255a1e388fa85e959f359d8",
      name: "FreeLeech",
      scores: { default: 0 },
      unsupported: "Uses indexer flags, which Ether doesn't read",
    },
    {
      id: "dictionarry-radarr-2160p-remux",
      name: "2160p Remux",
      scores: { default: 860000 },
    },
    {
      id: "dictionarry-radarr-remux-tier-1",
      name: "Remux Tier 1",
      scores: { default: 485000 },
    },
    {
      id: "dictionarry-radarr-x265",
      name: "x265",
      scores: { default: 20000 },
    },
  ],
  sonarr: [
    { id: "85c61753df5da1fb2aab6f2a47426b09", name: "BR-DISK", scores: { default: -10000 } },
    { id: "9c11cd3f07101cdba90a2d81cf0e56b4", name: "LQ", scores: { default: -10000 } },
    { id: "47435ece6b99a0b477caf360e79ba0bb", name: "x265 (HD)", scores: { default: -10000 } },
    { id: "e6258996055b9fbab7e9cb2f75819294", name: "WEB Tier 01", scores: { default: 1700 } },
    {
      id: "dictionarry-sonarr-2160p-remux",
      name: "2160p Remux",
      scores: { default: 860000 },
    },
    {
      id: "dictionarry-sonarr-x265",
      name: "x265",
      scores: { default: 25000 },
    },
  ],
};

interface FakeProfile extends TrashProfile {
  items: { name: string; allowed: boolean; items?: string[] }[];
  minFormatScore: number;
  language?: string;
  formatItems: Record<string, string>;
}

function formatItems(app: StarrApp, names: string[]): Record<string, string> {
  return Object.fromEntries(
    names.map((n) => [n, FORMATS[app].find((f) => f.name === n)?.id ?? `missing-${n}`]),
  );
}

const WEB = (h: string) => ({
  name: `WEB ${h}`,
  allowed: true,
  items: [`WEBRip-${h}`, `WEBDL-${h}`],
});

const PROFILES: Record<StarrApp, FakeProfile[]> = {
  radarr: [
    {
      id: "hd-bluray-web",
      name: "HD Bluray + WEB",
      description: "If you prefer 720p/1080p Bluray encodes.\nSize: 6-15 GB for a 1080p movie.",
      group: 1,
      url: "https://trash-guides.info/Radarr/radarr-setup-quality-profiles/",
      formats: 6,
      items: [
        { name: "Bluray-1080p", allowed: true },
        WEB("1080p"),
        { name: "Bluray-720p", allowed: true },
        { name: "Raw-HD", allowed: false },
      ],
      minFormatScore: 0,
      language: "Original",
      formatItems: formatItems("radarr", [
        "BR-DISK",
        "LQ",
        "x265 (HD)",
        "3D",
        "Repack/Proper",
        "FreeLeech",
      ]),
    },
    {
      id: "uhd-bluray-web",
      name: "UHD Bluray + WEB",
      description: "If you prefer 2160p Bluray encodes.",
      group: 1,
      formats: 4,
      items: [{ name: "Bluray-2160p", allowed: true }, WEB("2160p")],
      minFormatScore: 0,
      language: "Original",
      formatItems: formatItems("radarr", ["BR-DISK", "LQ", "3D", "DV (w/o HDR fallback)"]),
    },
    {
      id: "sqp-1-2160p",
      name: "[SQP] SQP-1 (2160p)",
      description:
        "This Special Quality Profile is an advanced Quality Profile designed for a specific use case.",
      group: 98,
      scoreSet: "sqp-1-2160p",
      formats: 3,
      items: [
        { name: "Bluray-2160p", allowed: true },
        WEB("2160p"),
        { name: "SDTV", allowed: true },
      ],
      minFormatScore: 1000,
      language: "Original",
      formatItems: formatItems("radarr", ["BR-DISK", "WEB Tier 01", "DV (w/o HDR fallback)"]),
    },
  ],
  sonarr: [
    {
      id: "web-1080p",
      name: "WEB-1080p",
      description: "720p/1080p WEBDL with a fallback to 720p.",
      group: 1,
      formats: 4,
      items: [WEB("1080p"), WEB("720p")],
      minFormatScore: 0,
      formatItems: formatItems("sonarr", ["BR-DISK", "LQ", "x265 (HD)", "WEB Tier 01"]),
    },
    {
      id: "web-2160p",
      name: "WEB-2160p",
      description: "2160p WEBDL with a fallback to 1080p.",
      group: 1,
      formats: 2,
      items: [WEB("2160p"), WEB("1080p")],
      minFormatScore: 0,
      formatItems: formatItems("sonarr", ["BR-DISK", "LQ"]),
    },
  ],
};

const DICTIONARRY_PROFILES: Record<StarrApp, FakeProfile[]> = {
  radarr: [
    {
      id: "2160p-remux",
      name: "2160p Remux",
      description:
        "2160p Remux utilizes Video / Audio Formats to prioritise high quality lossless copies of UHD Blurays.",
      group: 1,
      url: "https://dictionarry.dev",
      formats: 2,
      items: [
        {
          name: "2160p Remux",
          allowed: true,
          items: ["Remux-2160p", "Bluray-2160p", "WEBDL-2160p", "Remux-1080p", "SDTV"],
        },
      ],
      minFormatScore: 200000,
      formatItems: formatItems("radarr", ["2160p Remux", "Remux Tier 1"]),
    },
    {
      id: "1080p-efficient",
      name: "1080p Efficient",
      description: "1080p Efficient targets high quality x265 Bluray and WEB Encodes.",
      group: 2,
      url: "https://dictionarry.dev",
      formats: 1,
      items: [
        {
          name: "1080p Efficient",
          allowed: true,
          items: ["Bluray-1080p", "WEBDL-1080p", "WEBRip-1080p", "SDTV"],
        },
      ],
      minFormatScore: 200000,
      formatItems: formatItems("radarr", ["x265"]),
    },
  ],
  sonarr: [
    {
      id: "2160p-remux",
      name: "2160p Remux",
      description:
        "2160p Remux utilizes Video / Audio Formats to prioritise high quality lossless copies of UHD Blurays.",
      group: 1,
      url: "https://dictionarry.dev",
      formats: 1,
      items: [
        {
          name: "2160p Remux",
          allowed: true,
          items: ["Bluray-2160p Remux", "Bluray-2160p", "WEBDL-2160p"],
        },
      ],
      minFormatScore: 200000,
      formatItems: formatItems("sonarr", ["2160p Remux"]),
    },
    {
      id: "1080p-efficient",
      name: "1080p Efficient",
      description: "1080p Efficient targets high quality x265 Bluray and WEB Encodes.",
      group: 2,
      url: "https://dictionarry.dev",
      formats: 1,
      items: [
        {
          name: "1080p Efficient",
          allowed: true,
          items: ["Bluray-1080p", "WEBDL-1080p", "WEBRip-1080p"],
        },
      ],
      minFormatScore: 200000,
      formatItems: formatItems("sonarr", ["x265"]),
    },
  ],
};

const SOURCE_ORDER = ["remux", "bluray", "webdl", "webrip", "hdtv", "dvd"];

/** "WEBDL-1080p" → ["webdl", 1080]; null for qualities Ether has no equivalent for. */
function parseQuality(name: string): [string, number] | null {
  let [src = "", res = ""] = name.split("-");
  if (/ remux$/i.test(res)) [src, res] = ["remux", res.replace(/ remux$/i, "")];
  const source = {
    webdl: "webdl",
    webrip: "webrip",
    bluray: "bluray",
    remux: "remux",
    hdtv: "hdtv",
    dvd: "dvd",
  }[src.toLowerCase()];
  if (!source) return null;
  const height = res ? Number.parseInt(res, 10) : 480;
  return Number.isFinite(height) && height > 0 ? [source, height] : null;
}

type Native = Pick<
  FakeProfile,
  "name" | "items" | "minFormatScore" | "language" | "formatItems"
> & {
  trash_score_set?: string;
};

function plan(
  app: StarrApp,
  native: Native,
  scoreSet: string | undefined,
  config: EtherConfig,
): QualityImportPlan | Reply {
  const allowed: string[] = [];
  const sources: string[] = [];
  const skipped: string[] = [];
  let min = 0;
  let max = 0;
  for (const item of native.items ?? []) {
    if (!item.allowed) continue;
    const group: string[] = [];
    for (const q of item.items?.length ? item.items : [item.name]) {
      const parsed = parseQuality(q);
      if (!parsed) {
        skipped.push(q);
        continue;
      }
      const [source, height] = parsed;
      allowed.push(`${source}-${height}`);
      if (!sources.includes(source) && !group.includes(source)) group.push(source);
      min = min ? Math.min(min, height) : height;
      max = Math.max(max, height);
    }
    sources.push(...group.sort((a, b) => SOURCE_ORDER.indexOf(a) - SOURCE_ORDER.indexOf(b)));
  }
  if (!allowed.length)
    return fail(400, "invalid", "None of its enabled qualities can be streamed.");
  const resolver = config.resolver as { customFormats?: { trash_id?: string; name?: string }[] };
  const have = new Set((resolver.customFormats ?? []).map((f) => f.trash_id || f.name));
  const formats: ImportFormat[] = Object.entries(native.formatItems ?? {}).map(([name, id]) => {
    const def = FORMATS[app].find((f) => f.id === id);
    if (!def) return { id, name, score: 0, status: "missing", reason: "Not found in TRaSH Guides" };
    const score = (scoreSet ? def.scores[scoreSet] : undefined) ?? def.scores.default ?? 0;
    if (have.has(id)) return { id, name: def.name, score, status: "present" };
    if (def.unsupported)
      return { id, name: def.name, score, status: "unsupported", reason: def.unsupported };
    return { id, name: def.name, score, status: "add" };
  });
  formats.sort((a, b) => b.score - a.score || a.name.localeCompare(b.name));
  const names = new Set((config.quality.profiles ?? []).map((p) => p.name));
  let name = native.name;
  for (let i = 2; names.has(name); i++) name = `${native.name} (${i})`;
  const uhd = max >= 2160;
  const profile: QualityProfile = {
    name,
    maxHeight: max,
    minHeight: min,
    allowHdr: uhd,
    allowDolbyVision: uhd,
    preferredSources: sources,
    preferredCodecs: uhd ? ["hevc", "h264"] : ["h264", "hevc"],
    preferredAudio: ["eac3", "ac3", "aac"],
    minSizeMbPerMin: 5,
    maxSizeMbPerMin: 0,
    preferredLanguages: null,
    allowCam: false,
    allowedQualities: allowed,
    minCustomFormatScore: native.minFormatScore ?? 0,
    customFormatScores: Object.fromEntries(
      formats
        .filter((f) => f.status === "present" || f.status === "add")
        .map((f) => [f.id, f.score]),
    ),
  };
  const warnings: string[] = [];
  if (skipped.length)
    warnings.push(
      `Left out ${skipped.join(", ")}: Ether has no equivalent for ${skipped.length === 1 ? "this quality" : "these qualities"}.`,
    );
  warnings.push(
    "Upgrade and cutoff settings are left out: Ether picks the best release each time a title plays, so there is nothing to upgrade.",
  );
  if (native.language === "Original")
    warnings.push(
      "Radarr limits this profile to each title's original language. Ether doesn't filter by language; it passes over dubs that don't match the original language, and the profile's language formats still score releases.",
    );
  const lost = formats.filter((f) => f.status === "unsupported" || f.status === "missing").length;
  if (lost)
    warnings.push(
      `${lost} custom ${lost === 1 ? "format" : "formats"} can't be imported and won't score releases for this profile.`,
    );
  return { app, profile, scoreSet, formats, skippedQualities: skipped, warnings };
}

function isReply(v: QualityImportPlan | Reply): v is Reply {
  return "status" in v && "body" in v;
}

function app(v: unknown): StarrApp | null {
  return v === "radarr" || v === "sonarr" ? v : null;
}

export function createTrashMock() {
  function resolve(body: Record<string, unknown>, config: EtherConfig): QualityImportPlan | Reply {
    let a = app(body.app);
    if (!a) return fail(400, "invalid", "app must be radarr or sonarr");
    if (body.source === "dictionarry") {
      const p = DICTIONARRY_PROFILES[a].find((x) => x.id === body.profile);
      if (!p) return fail(404, "not_found", "Profilarr (Dictionarry) has no such profile");
      return plan(a, p, p.scoreSet, config);
    }
    if (typeof body.json === "string" && body.json.trim()) {
      let native: Native;
      try {
        native = JSON.parse(body.json) as Native;
      } catch {
        return fail(400, "invalid", "Not valid profile JSON.");
      }
      if (!native?.name || !Array.isArray(native.items) || !native.items.length)
        return fail(400, "invalid", "Name and items are required.");
      if (JSON.stringify(native.items).includes(" Remux")) a = "sonarr";
      return plan(a, native, native.trash_score_set, config);
    }
    const p = PROFILES[a].find((x) => x.id === body.profile);
    if (!p) return fail(404, "not_found", "TRaSH Guides has no such profile");
    return plan(a, p, p.scoreSet, config);
  }

  function handle(method: string, seg: string[], body: unknown, config: EtherConfig): Reply | null {
    if (seg[0] !== "releases") return null;
    const b = (body ?? {}) as Record<string, unknown>;
    if (method === "GET" && seg[1] === "trash" && seg[3] === "profiles" && seg.length === 4) {
      const a = app(seg[2]);
      if (!a) return fail(400, "invalid", "app must be radarr or sonarr");
      return ok({
        items: PROFILES[a].map(
          ({ items: _i, minFormatScore: _m, language: _l, formatItems: _f, ...summary }) => summary,
        ),
      });
    }
    if (method === "GET" && seg[1] === "dictionarry" && seg[3] === "profiles" && seg.length === 4) {
      const a = app(seg[2]);
      if (!a) return fail(400, "invalid", "app must be radarr or sonarr");
      return ok({
        items: DICTIONARRY_PROFILES[a].map(
          ({ items: _i, minFormatScore: _m, language: _l, formatItems: _f, ...summary }) => summary,
        ),
      });
    }
    const route = `${method} /${seg.slice(1).join("/")}`;
    if (route === "POST /quality-import/preview") {
      const p = resolve(b, config);
      return isReply(p) ? p : ok(p);
    }
    if (route === "POST /quality-import") {
      const p = resolve(b, config);
      if (isReply(p)) return p;
      const name = String(b.name ?? "").trim() || p.profile.name;
      const profiles = config.quality.profiles ?? [];
      if (profiles.some((x) => x.name === name))
        return fail(
          409,
          "exists",
          `A profile named "${name}" already exists; choose another name.`,
        );
      const resolver = config.resolver as { customFormats?: unknown[] };
      const added = p.formats
        .filter((f) => f.status === "add")
        .map((f) => ({
          trash_id: f.id,
          name: f.name,
          trash_scores: { default: f.score },
          specifications: [],
        }));
      resolver.customFormats = [...(resolver.customFormats ?? []), ...added];
      config.quality.profiles = [...profiles, { ...p.profile, name }];
      return ok({ name, formatsAdded: added.length });
    }
    return null;
  }

  return { handle };
}
