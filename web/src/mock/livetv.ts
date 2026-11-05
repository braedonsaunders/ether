import { scheduleFixture } from "./schedule-timeline";
import type { ChannelLook } from "../pages/livetv/onair/model";
/**
 * Live TV for the mock backend: a few virtual channels built from the fake
 * catalogue, an IPTV source with a guide, a guide-only source, and channels
 * that still need a guide. Development and tests only; never shipped.
 */
import type { FakeTitle } from "./data";

type Item = {
  key: number;
  title: string;
  durationMs: number;
  summary?: string;
  rating?: string;
  thumb?: string;
};
type Channel = Record<string, unknown> & {
  id: string;
  number: string;
  name: string;
  kind: "virtual" | "iptv";
  enabled: boolean;
  favorite: boolean;
  sourceId?: string;
  guideId?: string;
  guideSourceId?: string;
  items: Item[];
  group?: string;
};
type Source = Record<string, unknown> & { id: string; name: string };
type Programme = {
  id: string;
  channelId: string;
  start: string;
  end: string;
  item: Item;
  reason?: string;
  slate?: { kind: string };
};
/** A viewer's Live TV access: the only channel allow-list (see livetv.Policy). */
type Policy = {
  userId: number;
  disabled: boolean;
  restricted: boolean;
  channels?: string[];
  /** Private channels given to an unrestricted viewer. */
  granted?: string[];
  favorites?: string[];
  maxRating?: string;
  personal?: { enabled: boolean | null; name: string };
};

const NEWS = ["Morning Brief", "World Report", "Market Watch", "Evening News", "Late Edition"];
const SHOWS = [
  "Cooking Live",
  "Home Rescue",
  "Classic Cartoons",
  "Nature Hour",
  "Retro Sitcoms",
  "Game Night",
  "Talk Tonight",
  "Detective Files",
  "Space Frontier",
  "Road Trips",
  "Studio Sessions",
  "Pet Patrol",
];
const IPTV = [
  ["News 24", "News"],
  ["Cartoon Classics", "Kids"],
  ["Retro TV", "Entertainment"],
  ["Food Live", "Lifestyle"],
  ["Nature Channel", "Documentary"],
  ["Sci-Fi Central", "Entertainment"],
  ["Comedy Hits", "Entertainment"],
  ["Crime Files", "Entertainment"],
  ["Weather Now", "News"],
  ["Game Show Network", "Entertainment"],
  ["Travel & Escape", "Lifestyle"],
  ["Music Videos", "Music"],
  ["Sports Classics", "Sports"],
  ["Kids Zone", "Kids"],
  ["Western Channel", "Movies"],
  ["Mystery Theater", "Movies"],
  ["Home & Garden", "Lifestyle"],
  ["Anime World", "Kids"],
];

/** A large Xtream panel's groups, in provider order (events float to the top). */
const PANEL_GROUPS: { name: string; channels: number; adult?: boolean }[] = [
  { name: "EVENTS | PPV", channels: 64 },
  { name: "EVENTS | Live Sports", channels: 120 },
  ...[
    "UK",
    "US",
    "CA",
    "IE",
    "AU",
    "DE",
    "FR",
    "ES",
    "IT",
    "NL",
    "PT",
    "PL",
    "TR",
    "AR",
    "IN",
    "BR",
    "MX",
    "SE",
    "NO",
    "DK",
  ].flatMap((cc, i) =>
    ["Entertainment", "News", "Sports", "Kids", "Movies", "Documentary", "Music"].map(
      (kind, k) => ({ name: `${cc} | ${kind}`, channels: 8 + ((i * 7 + k * 13) % 90) }),
    ),
  ),
  { name: "24/7 Series", channels: 310 },
  { name: "Radio", channels: 75 },
  { name: "XXX | Adult", channels: 140, adult: true },
];

const norm = (name: string) => name.trim().toLowerCase();

function hash(s: string): number {
  let h = 2166136261;
  for (let i = 0; i < s.length; i++) h = Math.imul(h ^ s.charCodeAt(i), 16777619);
  return h >>> 0;
}

export function createLiveTVMock(titles: FakeTitle[], empty = false) {
  const movies = titles.filter((t) => t.type === "movie" && t.runtimeMinutes);
  const item = (t: FakeTitle): Item => ({
    key: Number(t.key),
    title: t.title,
    durationMs: (t.runtimeMinutes ?? 100) * 60000,
    summary: t.summary,
    rating: t.contentRating,
    thumb: t.poster,
  });
  const slice = (seed: number, n: number) =>
    Array.from({ length: n }, (_, i) => movies[(seed * 7 + i * 3) % movies.length]!).map(item);

  function interstitials(programmes: Programme[], look?: ChannelLook) {
    if (!look) return programmes;
    return programmes.flatMap((p, i) => {
      const kinds = [
        ...(i === 0 && look.ident ? ["ident"] : []),
        ...(look.upNext ? ["upnext"] : []),
        ...(look.breaks ? ["break", "back"] : []),
      ];
      if (!kinds.length) return [p];
      const boundary = Date.parse(p.end);
      let at = boundary - kinds.length * 12000;
      return [
        { ...p, end: new Date(at).toISOString() },
        ...kinds.map((kind) => {
          const start = at;
          at += 12000;
          return {
            id: `${p.id}-${kind}`,
            channelId: p.channelId,
            start: new Date(start).toISOString(),
            end: new Date(at).toISOString(),
            item: {
              key: 0,
              title:
                kind === "upnext"
                  ? "Up next"
                  : kind === "back"
                    ? "Back from break"
                    : kind === "break"
                      ? "Break"
                      : "Ident",
              durationMs: 12000,
            },
            slate: { kind },
          };
        }),
      ];
    });
  }
  const sources: Source[] = [
    {
      id: "demo-fast",
      name: "Demo FAST playlist",
      playlistUrl: "https://iptv.example/demo.m3u",
      guideUrl: "https://iptv.example/demo.xml.gz",
      hasPlaylist: true,
      hasGuide: true,
      updatedAt: new Date(Date.now() - 2 * 3600e3).toISOString(),
    },
    {
      id: "demo-xtream",
      name: "Demo Xtream panel",
      auth: {
        type: "xtream",
        server: "http://panel.example:8080",
        username: "demo",
        password: "••••",
        output: "ts",
      },
      hasPlaylist: true,
      hasGuide: true,
      excludeGroups: ["XXX | Adult", "24/7 Series"],
      updatedAt: new Date(Date.now() - 50 * 60e3).toISOString(),
    },
    {
      id: "regional-epg",
      name: "Regional listings",
      guideUrl: "https://epg.example/regional.xml.gz",
      hasPlaylist: false,
      hasGuide: true,
      updatedAt: new Date(Date.now() - 40 * 60e3).toISOString(),
    },
  ];
  const channels: Channel[] = [
    {
      id: "v-movie-night",
      number: "1",
      name: "Movie Night",
      kind: "virtual",
      enabled: true,
      favorite: true,
      epoch: "2026-01-01T00:00:00Z",
      seed: 3,
      shuffle: true,
      items: slice(1, 14),
    },
    {
      id: "v-family",
      number: "2",
      name: "Family Animation",
      kind: "virtual",
      enabled: true,
      favorite: false,
      epoch: "2026-01-01T00:00:00Z",
      seed: 1,
      shuffle: false,
      rating: "PG",
      filter: { type: "movie", genres: ["Animation"], maxContentRating: "PG" },
      items: slice(2, 10),
    },
    {
      id: "v-80s",
      number: "3",
      name: "80s Action 24/7",
      kind: "virtual",
      enabled: true,
      favorite: true,
      epoch: "2026-01-01T00:00:00Z",
      seed: 9,
      shuffle: true,
      filter: { type: "movie", genres: ["Action"], yearFrom: 1980, yearTo: 1989 },
      items: slice(3, 12),
      blocks: [{ start: 1200, end: 1380, items: slice(5, 3), shuffle: false }],
    },
    {
      id: "v-horror",
      number: "4",
      name: "Midnight Horror",
      kind: "virtual",
      enabled: true,
      favorite: false,
      epoch: "2026-01-01T00:00:00Z",
      seed: 4,
      shuffle: true,
      items: slice(4, 9),
    },
    ...IPTV.map(([name, group], i) => ({
      id: `iptv-${i}`,
      number: String(100 + i),
      name: name!,
      group,
      kind: "iptv" as const,
      enabled: i !== 16,
      favorite: i === 0 || i === 5,
      sourceId: "demo-fast",
      guideId: i % 6 === 4 ? "" : `${name!.toLowerCase().replace(/[^a-z0-9]+/g, "")}.us`,
      streamUrl: `https://iptv.example/live/${i}.m3u8`,
      epoch: "1970-01-01T00:00:00Z",
      seed: 1,
      shuffle: false,
      items: [],
    })),
  ];
  channels[0]!.look = {
    lookId: "nocturne",
    ident: true,
    upNext: true,
    offAir: true,
    breaks: "between",
    breakContent: "reel",
    reelId: "saturday",
    breakSeconds: 60,
  };
  channels[1]!.look = {
    lookId: "blorp",
    ident: true,
    upNext: true,
    offAir: true,
    breaks: "between",
    breakContent: "segments",
    segmentIds: ["plugin:weather/forecast"],
    allowUnratedSegments: false,
  };
  channels[1]!.rating = "PG";
  channels[1]!.clock = [
    {
      segmentId: "plugin:weather/forecast",
      minute: 58,
      everyMinutes: 60,
      from: 360,
      until: 1380,
      days: [],
    },
  ];
  channels.push({
    id: "v-personal",
    number: "5",
    name: "Personal TV",
    kind: "virtual",
    enabled: true,
    favorite: false,
    epoch: "2026-01-01T00:00:00Z",
    seed: 1,
    shuffle: false,
    items: [],
    personal: {
      ingredients: [
        "continue",
        "nextEpisodes",
        "picked",
        "watchlist",
        "comfort",
        "newReleases",
        "collections",
      ].map((kind, i) => ({ kind, share: [25, 25, 30, 10, 10, 10, 10][i], enabled: i < 5 })),
      pauseWhenAway: true,
      nameFormat: "{viewer} TV",
      repeatHours: 24,
      maxShowShare: 30,
      nightMaxMinutes: 60,
    },
    look: { lookId: "personal", ident: true, upNext: true, offAir: true, breaks: "between" },
  });
  if (empty) channels.length = 0;
  // Two channels have a guide id their own source doesn't carry.
  const unlisted = new Set(["iptv-4", "iptv-10", "iptv-11", "iptv-16"]);
  // Opt-in 5k review fixture, stripped from production with the mock backend.
  const reviewFixture =
    new URLSearchParams(window.location.search).get("guideReviewFixture") === "5000";
  if (reviewFixture) {
    channels.splice(
      0,
      channels.length,
      ...Array.from({ length: 5000 }, (_, i) => ({
        id: `review-${i}`,
        number: String(i + 1),
        name: `${["US", "CA", "UK", "AU"][i % 4]}: ${["ESPN 2 HD RAW", "BBC News FHD", "Sky Sports Main Event", "Discovery Science", "National Geographic HD", "CNN International", "NBC Boston", "History Channel"][i % 8]} ${String(i + 1).padStart(4, "0")}`,
        kind: "iptv" as const,
        enabled: true,
        favorite: false,
        sourceId: i % 2 ? "demo-fast" : "demo-xtream",
        group: ["Sports", "News", "Documentary", "Entertainment"][i % 4],
        guideId: i % 5 === 0 ? `fixture-${i}` : "",
        items: [],
      })),
    );
    unlisted.clear();
    for (const c of channels) if (!c.guideId) unlisted.add(c.id);
    if (new URLSearchParams(window.location.search).has("guideReviewResults")) {
      const rows = Object.fromEntries(
        channels
          .filter((c) => !c.guideId)
          .map((c, i) => [
            c.id,
            {
              guideId: "",
              guideSourceId: "",
              decision: i % 4 === 0 ? "pending" : "accepted",
              checked: true,
              proposal: {
                channelId: c.id,
                channelName: c.name,
                guideSourceId: "regional-epg",
                guideSourceName: "Regional listings",
                guideId: `fixture-guide-${c.id}`,
                guideName: c.name.replace(/ RAW| FHD| HD/g, ""),
                method: "ai",
                confidence: i % 4 === 0 ? 0.78 : 0.98,
                reason:
                  i % 4 === 0
                    ? "Regional feed needs confirmation; the current programme differs."
                    : "Station identity, region and current programme agree.",
                proposalKey: `mock-${c.id}`,
              },
            },
          ]),
      );
      sessionStorage.setItem("ether-guide-review-v2:all:lineup", JSON.stringify({ rows }));
    }
  }
  const guideUndo = new Map<string, { id: string; guideId: unknown; guideSourceId: unknown }[]>();

  function schedule(c: Channel, from: number, until: number): Programme[] {
    const out: Programme[] = [];
    if (c.kind === "virtual") {
      const items = c.items.length ? c.items : slice(hash(c.id) % 20, 6);
      const loop = items.reduce((n, i) => n + i.durationMs, 0);
      const epoch = Date.parse(String(c.epoch ?? "2026-01-01T00:00:00Z"));
      let t = epoch + Math.floor((from - epoch) / loop) * loop;
      let i = 0;
      while (t < until) {
        const it = items[i % items.length]!;
        if (t + it.durationMs > from)
          out.push({
            id: `${c.id}-${t}`,
            channelId: c.id,
            start: new Date(t).toISOString(),
            end: new Date(t + it.durationMs).toISOString(),
            item: it,
          });
        t += it.durationMs;
        i++;
      }
      return interstitials(out, c.look as ChannelLook | undefined);
    }
    if (!c.enabled || unlisted.has(c.id) || !c.guideId) return out;
    const names = c.group === "News" ? NEWS : SHOWS;
    const slot = 30 * 60000;
    let t = Math.floor(from / slot) * slot;
    while (t < until) {
      const h = hash(`${c.id}${t}`);
      const len = slot * (1 + (h % 3));
      const title = names[h % names.length]!;
      out.push({
        id: `${c.id}-${t}`,
        channelId: c.id,
        start: new Date(t).toISOString(),
        end: new Date(t + len).toISOString(),
        item: {
          key: 0,
          title,
          durationMs: len,
          summary: `${title} on ${c.name}.`,
          rating: "TV-PG",
        },
      });
      t += len;
    }
    return out;
  }

  function source(s: Source) {
    const own = channels.filter((c) => c.sourceId === s.id);
    const out: Record<string, unknown> = { ...s };
    if (s.id === "demo-xtream") {
      const left = new Set(((s.excludeGroups as string[] | undefined) ?? []).map(norm));
      const groups = PANEL_GROUPS.map((g) => ({
        ...g,
        excluded: left.has(norm(g.name)) || (Boolean(s.hideAdult) && Boolean(g.adult)),
      }));
      return {
        ...out,
        groups,
        account: {
          status: "Active",
          trial: true,
          expiresAt: new Date(Date.now() + 20 * 3600e3).toISOString(),
          maxConnections: 1,
          activeConnections: 0,
          formats: ["ts", "m3u8"],
          checkedAt: s.updatedAt,
        },
        channelCount: groups.reduce((n, g) => n + (g.excluded ? 0 : g.channels), 0),
        guideChannelCount: 9800,
        programmeCount: 412000,
        guideFrom: new Date(Date.now() - 3 * 3600e3).toISOString(),
        guideUntil: new Date(Date.now() + 48 * 3600e3).toISOString(),
        nextRefresh: new Date(Date.now() + 5 * 3600e3).toISOString(),
      };
    }
    if (own.length) {
      const left = new Set(((s.excludeGroups as string[] | undefined) ?? []).map(norm));
      const counts = new Map<string, number>();
      for (const c of own) counts.set(c.group ?? "", (counts.get(c.group ?? "") ?? 0) + 1);
      out.groups = [...counts].map(([name, n]) => ({
        name,
        channels: n,
        ...(left.has(norm(name)) ? { excluded: true } : {}),
      }));
    }
    return {
      ...out,
      channelCount: own.length,
      guideChannelCount: s.id === "regional-epg" ? 42 : own.length - 3,
      programmeCount: s.id === "regional-epg" ? 3120 : 1840,
      guideFrom: new Date(Date.now() - 3 * 3600e3).toISOString(),
      guideUntil: new Date(Date.now() + (s.id === "regional-epg" ? 30 : 5) * 3600e3).toISOString(),
      nextRefresh: new Date(Date.now() + 3 * 3600e3).toISOString(),
    };
  }

  const policies = new Map<number, Policy>();
  const policy = (userId: number): Policy =>
    policies.get(userId) ?? { userId, disabled: false, restricted: false };
  const savePolicy = (p: Policy) => {
    const saved = {
      ...p,
      channels: p.restricted ? [...new Set(p.channels ?? [])] : undefined,
      granted: p.restricted ? undefined : [...new Set(p.granted ?? [])],
    };
    policies.set(p.userId, saved);
    return saved;
  };

  function handle(method: string, seg: string[], q: URLSearchParams, body: unknown) {
    if (seg[0] !== "livetv") return null;
    const route = `${method} /${seg.slice(1).join("/")}`;
    const now = Date.now();
    if (route === "GET /usage")
      return { body: { sessions: 0, viewers: 0, watching: [], sources: [] } };
    if (method === "GET" && seg[1] === "channels" && seg[3] === "timeline") {
      const from = Number(q.get("from")) || Date.now();
      const to = Number(q.get("to")) || from + 86400000;
      return { body: scheduleFixture(seg[2]!, from, to) };
    }
    if (method === "GET" && seg[1] === "channels" && seg[3] === "rundown") {
      const viewer = Number(q.get("viewer") || 1);
      const picks = slice(viewer + 3, 14);
      let at = now - 20 * 60000;
      return {
        body: interstitials(
          picks.map((it, i) => {
            const start = at;
            at += it.durationMs;
            return {
              id: `personal-${viewer}-${i}`,
              channelId: seg[2]!,
              start: new Date(start).toISOString(),
              end: new Date(at).toISOString(),
              item: it,
              reason: i % 2 ? "Next episode in your queue" : "Because you rated Annihilation 5★",
            };
          }),
          channels.find((c) => c.id === seg[2])?.look as ChannelLook | undefined,
        ),
      };
    }
    if (route === "GET /channels/summary") return { body: channels };
    if (route === "GET /channels/page") {
      const needle = (q.get("q") || "").toLowerCase();
      const filter = q.get("filter") || "all";
      const matches = (c: Channel) =>
        filter === "all" ||
        (filter === "virtual" || filter === "iptv"
          ? c.kind === filter
          : filter === "favorites"
            ? c.favorite
            : filter === "off"
              ? !c.enabled
              : filter === "missing"
                ? !c.guideId && c.kind === "iptv"
                : c.enabled);
      const all = channels.filter(
        (c) => matches(c) && `${c.name} ${c.number}`.toLowerCase().includes(needle),
      );
      const offset = Number(q.get("offset") || 0),
        limit = Number(q.get("limit") || 200);
      const groups = [...new Set(all.map((c) => c.kind))].map((kind) => ({
        key: kind,
        kind,
        count: all.filter((c) => c.kind === kind).length,
        start: all.findIndex((c) => c.kind === kind),
      }));
      return {
        body: {
          channels: all.slice(offset, offset + limit),
          total: all.length,
          counts: {
            all: channels.length,
            live: channels.filter((c) => c.enabled).length,
            favorites: channels.filter((c) => c.favorite).length,
            virtual: channels.filter((c) => c.kind === "virtual").length,
            iptv: channels.filter((c) => c.kind === "iptv").length,
            off: channels.filter((c) => !c.enabled).length,
            missing: channels.filter((c) => !c.guideId && c.kind === "iptv").length,
          },
          groups,
          coverage: Object.fromEntries(
            channels.map((c) => [c.id, c.kind === "virtual" || Boolean(c.guideId)]),
          ),
          nextNumber: String(Math.max(0, ...channels.map((c) => Number(c.number))) + 1),
        },
      };
    }
    if (method === "GET" && seg[1] === "channels" && seg.length === 3)
      return { body: channels.find((c) => c.id === seg[2]) };
    if (route === "GET /channels") return { body: channels };
    if (route === "GET /sources") return { body: sources.map(source) };
    if (route === "GET /guide") {
      const from = Date.parse(q.get("from") ?? "") || now;
      const until = Date.parse(q.get("until") ?? "") || now + 86400e3;
      return {
        body: channels
          .filter((c) => c.enabled)
          .flatMap((c) => schedule(c, from, until))
          .sort((a, b) => a.start.localeCompare(b.start)),
      };
    }
    if (route === "GET /guide/coverage")
      return {
        body: channels.map((c) => {
          const ps = schedule({ ...c, enabled: true }, now, now + 86400e3);
          return { channelId: c.id, programmes: ps.length, until: ps.at(-1)?.end };
        }),
      };
    if (route === "GET /guide/channels") {
      const needle = (q.get("q") ?? "").toLowerCase();
      const all = [
        ...IPTV.map(([name]) => ({
          sourceId: "demo-fast",
          sourceName: "Demo FAST playlist",
          guideId: `${name!.toLowerCase().replace(/[^a-z0-9]+/g, "")}.us`,
          name: name!,
          programmes: 48,
          until: new Date(now + 5 * 3600e3).toISOString(),
        })),
        ...IPTV.map(([name]) => ({
          sourceId: "regional-epg",
          sourceName: "Regional listings",
          guideId: `${name!.replace(/[^A-Za-z0-9]+/g, "")}.regional`,
          name: `${name} HD`,
          programmes: 96,
          until: new Date(now + 30 * 3600e3).toISOString(),
        })),
      ];
      return {
        body: all
          .filter((g) => `${g.name} ${g.guideId}`.toLowerCase().includes(needle))
          .slice(0, Number(q.get("limit") ?? 50)),
      };
    }
    if (route === "POST /guide/match") {
      const b = (body ?? {}) as {
        channelIds?: string[];
        dryRun?: boolean;
        ai?: string;
        selections?: { channelId: string; guideId: string; guideSourceId: string }[];
        undoToken?: string;
      };
      if (b.ai === "undo") {
        const before = guideUndo.get(b.undoToken ?? "");
        if (!before) return { status: 400, body: { error: "undo expired" } };
        for (const v of before) {
          const c = channels.find((c) => c.id === v.id)!;
          c.guideId = v.guideId as string;
          c.guideSourceId = v.guideSourceId as string;
          unlisted.add(c.id);
        }
        guideUndo.delete(b.undoToken!);
        return { body: { matched: before.length, changes: [] } };
      }
      if (b.ai === "commit") {
        const selections = b.selections ?? [],
          token = `undo-${Date.now()}`;
        const before = selections.map((v) => {
          const c = channels.find((c) => c.id === v.channelId)!;
          return { id: c.id, guideId: c.guideId, guideSourceId: c.guideSourceId };
        });
        for (const v of selections) {
          const c = channels.find((c) => c.id === v.channelId)!;
          c.guideId = v.guideId;
          c.guideSourceId = v.guideSourceId;
          unlisted.delete(c.id);
        }
        guideUndo.set(token, before);
        return { body: { matched: selections.length, undoToken: token, changes: [] } };
      }

      if (b.ai === "estimate")
        return {
          body: {
            matched: 0,
            unmatched: unlisted.size,
            changes: [],
            ai: {
              configured: true,
              estimatedCalls: 1,
              inputTokens: 2400,
              cached: 0,
              calls: 0,
              deferred: 0,
            },
          },
        };
      const targets = channels.filter(
        (c) =>
          c.kind === "iptv" &&
          c.enabled &&
          unlisted.has(c.id) &&
          (!b.channelIds || b.channelIds.includes(c.id)),
      );
      const changes = targets.map((c, i) => ({
        channelId: c.id,
        channelName: c.name,
        guideSourceId: "regional-epg",
        guideSourceName: "Regional listings",
        guideId: `${c.name.replace(/[^A-Za-z0-9]+/g, "")}.regional`,
        guideName: `${c.name} HD`,
        method: b.ai ? "ai" : "name",
        ...(b.ai
          ? {
              confidence: i === 1 ? 0.74 : 0.98,
              reason: "Station identity, region and current programme agree.",
              proposalKey: `mock-${c.id}`,
            }
          : {}),
      }));
      if (!b.dryRun && b.ai !== "propose")
        for (const ch of changes) {
          const c = channels.find((x) => x.id === ch.channelId)!;
          c.guideSourceId = ch.guideSourceId;
          c.guideId = ch.guideId;
          unlisted.delete(c.id);
        }
      return {
        body: {
          matched: changes.length,
          unmatched: 0,
          skipped: 0,
          dryRun: Boolean(b.dryRun),
          changes,
        },
      };
    }
    if (method === "POST" && seg[1] === "channels" && seg[3] === "play")
      return {
        body: {
          id: `mock-session-${seg[2]}`,
          playlist: `sessions/mock-session-${seg[2]}/index.m3u8`,
          activeSourceName: "Primary stream",
          programme: schedule(
            channels.find((c) => c.id === seg[2]) ?? channels[0]!,
            now,
            now + 1,
          )[0],
        },
      };
    if (method === "DELETE" && seg[1] === "sessions") return { body: { ok: true } };
    if (route === "POST /preview") {
      const c = body as Channel;
      return { body: schedule({ ...c, id: c.id || "draft" }, now, now + 6 * 3600e3) };
    }
    if (method === "PUT" && seg[1] === "channels" && seg[2] && seg[3] === "viewers") {
      const id = seg[2];
      if (!channels.some((c) => c.id === id))
        return { body: { error: { code: "livetv", message: "not found" } }, status: 404 };
      const b = (body ?? {}) as { allow?: number[]; deny?: number[] };
      const isPrivate = (cid: string) =>
        Boolean((channels.find((c) => c.id === cid) as { private?: boolean } | undefined)?.private);
      const sees = (p: Policy, cid: string) =>
        p.restricted
          ? (p.channels ?? []).includes(cid)
          : !isPrivate(cid) || (p.granted ?? []).includes(cid);
      for (const user of b.allow ?? []) {
        const p = policy(user);
        if (sees(p, id)) continue;
        if (p.restricted) savePolicy({ ...p, channels: [...(p.channels ?? []), id] });
        else savePolicy({ ...p, granted: [...(p.granted ?? []), id] });
      }
      for (const user of b.deny ?? []) {
        const p = policy(user);
        if (p.restricted)
          savePolicy({ ...p, channels: (p.channels ?? []).filter((c) => c !== id) });
        else if (isPrivate(id))
          savePolicy({ ...p, granted: (p.granted ?? []).filter((c) => c !== id) });
        else
          savePolicy({
            ...p,
            restricted: true,
            channels: channels.map((c) => c.id).filter((c) => c !== id && sees(p, c)),
          });
      }
      return { body: { ok: true } };
    }
    if (
      (method === "POST" && route === "POST /channels") ||
      (method === "PUT" && seg[1] === "channels" && seg[2])
    ) {
      const c = { ...(body as Channel) };
      if (!c.id) c.id = `v-${hash(String(now))}`;
      const i = channels.findIndex((x) => x.id === c.id);
      if (c.guideSourceId && c.guideId) unlisted.delete(c.id);
      if (i >= 0) channels[i] = c;
      else channels.push(c);
      return { body: c };
    }
    if (method === "DELETE" && seg[1] === "channels" && seg[2]) {
      const i = channels.findIndex((x) => x.id === seg[2]);
      if (i >= 0) channels.splice(i, 1);
      return { body: { ok: true } };
    }
    if (method === "POST" && seg[1] === "sources" && seg[3] === "refresh") {
      const s = sources.find((x) => x.id === seg[2]);
      if (s) s.updatedAt = new Date().toISOString();
      return s
        ? { body: source(s) }
        : { body: { error: { code: "livetv", message: "not found" } }, status: 404 };
    }
    if (
      method === "POST" &&
      seg[1] === "sources" &&
      seg[2] &&
      (seg[3] === "playlist" || seg[3] === "guide")
    ) {
      const s = sources.find((x) => x.id === seg[2]);
      if (!s) return { body: { error: { code: "livetv", message: "not found" } }, status: 404 };
      if (seg[3] === "playlist") s.hasPlaylist = true;
      if (seg[3] === "guide") s.hasGuide = true;
      s.updatedAt = new Date().toISOString();
      return { body: source(s) };
    }
    if (
      route === "POST /sources" ||
      (method === "PUT" && seg[1] === "sources" && seg[2] && !seg[3])
    ) {
      const b = body as Source & { playlist?: string; guide?: string; playlistUrl?: string };
      const s: Source = {
        ...b,
        id: b.id || `src-${hash(String(now))}`,
        playlist: undefined,
        guide: undefined,
        hasPlaylist: Boolean(b.playlist || b.playlistUrl),
        hasGuide: true,
        updatedAt: new Date().toISOString(),
      };
      const i = sources.findIndex((x) => x.id === s.id);
      if (i >= 0) sources[i] = s;
      else sources.push(s);
      return { body: source(s) };
    }
    if (method === "DELETE" && seg[1] === "sources" && seg[2]) {
      const i = sources.findIndex((x) => x.id === seg[2]);
      if (i >= 0) sources.splice(i, 1);
      return { body: { ok: true } };
    }
    if (route === "GET /policies") return { body: [...policies.values()] };
    if (seg[1] === "policies" && seg[2]) {
      const userId = Number(seg[2]);
      if (method === "PUT") return { body: savePolicy({ ...(body as Policy), userId }) };
      return { body: policy(userId) };
    }
    return null;
  }
  return { handle };
}
