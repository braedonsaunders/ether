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
type Programme = { id: string; channelId: string; start: string; end: string; item: Item };

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

function hash(s: string): number {
  let h = 2166136261;
  for (let i = 0; i < s.length; i++) h = Math.imul(h ^ s.charCodeAt(i), 16777619);
  return h >>> 0;
}

export function createLiveTVMock(titles: FakeTitle[]) {
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
  // Two channels have a guide id their own source doesn't carry.
  const unlisted = new Set(["iptv-4", "iptv-10", "iptv-11", "iptv-16"]);

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
      return out;
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
    return {
      ...s,
      channelCount: own.length,
      guideChannelCount: s.id === "regional-epg" ? 42 : own.length - 3,
      programmeCount: s.id === "regional-epg" ? 3120 : 1840,
      guideFrom: new Date(Date.now() - 3 * 3600e3).toISOString(),
      guideUntil: new Date(Date.now() + (s.id === "regional-epg" ? 30 : 5) * 3600e3).toISOString(),
      nextRefresh: new Date(Date.now() + 3 * 3600e3).toISOString(),
    };
  }

  function handle(method: string, seg: string[], q: URLSearchParams, body: unknown) {
    if (seg[0] !== "livetv") return null;
    const route = `${method} /${seg.slice(1).join("/")}`;
    const now = Date.now();
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
      const b = (body ?? {}) as { channelIds?: string[]; dryRun?: boolean };
      const targets = channels.filter(
        (c) =>
          c.kind === "iptv" &&
          c.enabled &&
          unlisted.has(c.id) &&
          (!b.channelIds || b.channelIds.includes(c.id)),
      );
      const changes = targets.map((c) => ({
        channelId: c.id,
        channelName: c.name,
        guideSourceId: "regional-epg",
        guideSourceName: "Regional listings",
        guideId: `${c.name.replace(/[^A-Za-z0-9]+/g, "")}.regional`,
        guideName: `${c.name} HD`,
        method: "name",
      }));
      if (!b.dryRun)
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
          id: "mock-session",
          playlist: "sessions/mock-session/index.m3u8",
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
    if (seg[1] === "policies")
      return {
        body: {
          userId: Number(seg[2]),
          disabled: false,
          channels: [],
          favorites: [],
          maxRating: "",
        },
      };
    return null;
  }
  return { handle };
}
