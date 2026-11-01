/**
 * Release calendar for the mock backend: a household's followed shows airing
 * weekly around today, season premieres and finales, and movie releases from
 * watchlists, Look Ahead collections, taste and popularity. Anchored to the
 * real today so the calendar always looks live. Development and tests only.
 */
import type { CalendarEntry, CalendarViewer } from "../pages/calendar/model";
import { avatarFor, hash, posterFor } from "./data";

const DAY = 86_400_000;

type Show = {
  id: number;
  title: string;
  network: string;
  weekday: number;
  season: number;
  episodes: number;
  /** Days from today of the season's first episode. */
  premiere: number;
  rating: string;
  viewers: number[];
  reason: "watching" | "watchlist";
  genres: string[];
  runtime: number;
};

const SHOWS: Show[] = [
  {
    id: 101,
    title: "Night Desk",
    network: "Atlas TV",
    weekday: 0,
    season: 3,
    episodes: 10,
    premiere: -36,
    rating: "TV-MA",
    viewers: [1, 4],
    reason: "watching",
    genres: ["Crime", "Drama"],
    runtime: 52,
  },
  {
    id: 102,
    title: "Little Engines",
    network: "Sundial",
    weekday: 6,
    season: 2,
    episodes: 12,
    premiere: -20,
    rating: "TV-Y7",
    viewers: [3],
    reason: "watching",
    genres: ["Animation", "Family"],
    runtime: 22,
  },
  {
    id: 103,
    title: "The Salt Empire",
    network: "Channel Nine",
    weekday: 4,
    season: 1,
    episodes: 8,
    premiere: -2,
    rating: "TV-14",
    viewers: [1, 2],
    reason: "watching",
    genres: ["Drama", "History"],
    runtime: 58,
  },
  {
    id: 104,
    title: "Harbor Lights",
    network: "Atlas TV",
    weekday: 2,
    season: 5,
    episodes: 10,
    premiere: -58,
    rating: "TV-PG",
    viewers: [2, 3],
    reason: "watching",
    genres: ["Comedy", "Family"],
    runtime: 30,
  },
  {
    id: 105,
    title: "Velvet Hours",
    network: "Sundial",
    weekday: 1,
    season: 2,
    episodes: 8,
    premiere: 9,
    rating: "TV-14",
    viewers: [2],
    reason: "watchlist",
    genres: ["Romance", "Drama"],
    runtime: 45,
  },
  {
    id: 106,
    title: "Comet Road",
    network: "Channel Nine",
    weekday: 3,
    season: 4,
    episodes: 10,
    premiere: -30,
    rating: "TV-14",
    viewers: [1],
    reason: "watching",
    genres: ["Science Fiction", "Adventure"],
    runtime: 48,
  },
  {
    id: 107,
    title: "Paper Kingdom",
    network: "Atlas TV",
    weekday: 5,
    season: 1,
    episodes: 6,
    premiere: -12,
    rating: "TV-PG",
    viewers: [1, 2, 3],
    reason: "watching",
    genres: ["Fantasy", "Adventure"],
    runtime: 40,
  },
  {
    id: 108,
    title: "Northern Tide",
    network: "Sundial",
    weekday: 0,
    season: 6,
    episodes: 10,
    premiere: -64,
    rating: "TV-MA",
    viewers: [4],
    reason: "watching",
    genres: ["Thriller"],
    runtime: 55,
  },
  {
    id: 109,
    title: "Glass Archive",
    network: "Channel Nine",
    weekday: 2,
    season: 1,
    episodes: 9,
    premiere: 2,
    rating: "TV-14",
    viewers: [1, 4],
    reason: "watchlist",
    genres: ["Mystery", "Science Fiction"],
    runtime: 50,
  },
  {
    id: 110,
    title: "Kitchen Frontier",
    network: "Atlas TV",
    weekday: 1,
    season: 8,
    episodes: 14,
    premiere: -41,
    rating: "TV-PG",
    viewers: [2],
    reason: "watching",
    genres: ["Documentary"],
    runtime: 42,
  },
  {
    id: 111,
    title: "Midnight Machine",
    network: "Sundial",
    weekday: 4,
    season: 3,
    episodes: 8,
    premiere: 23,
    rating: "TV-MA",
    viewers: [1],
    reason: "watching",
    genres: ["Horror", "Thriller"],
    runtime: 47,
  },
  {
    id: 112,
    title: "Iron Signal",
    network: "Channel Nine",
    weekday: 3,
    season: 2,
    episodes: 10,
    premiere: -5,
    rating: "TV-14",
    viewers: [1, 4],
    reason: "watching",
    genres: ["Action", "Thriller"],
    runtime: 44,
  },
];

type Movie = {
  id: number;
  title: string;
  offset: number;
  rating: string;
  reason: "watchlist" | "lookahead" | "taste" | "popular";
  viewers?: number[];
  popularity: number;
  genres: string[];
  inCinemas?: boolean;
  collection?: string;
};

const MOVIES: Movie[] = [
  {
    id: 201,
    title: "Golden Comet",
    offset: -9,
    rating: "PG-13",
    reason: "watchlist",
    viewers: [1],
    popularity: 180,
    genres: ["Science Fiction"],
  },
  {
    id: 202,
    title: "The Hidden Garden",
    offset: 4,
    rating: "PG",
    reason: "lookahead",
    popularity: 120,
    genres: ["Family", "Fantasy"],
    collection: "Coming soon for the family",
  },
  {
    id: 203,
    title: "Electric Orchard",
    offset: -16,
    rating: "R",
    reason: "taste",
    popularity: 95,
    genres: ["Thriller"],
    inCinemas: true,
  },
  {
    id: 204,
    title: "Crimson Passage",
    offset: 11,
    rating: "PG-13",
    reason: "popular",
    popularity: 260,
    genres: ["Action"],
  },
  {
    id: 205,
    title: "Wild Atlas",
    offset: 0,
    rating: "PG",
    reason: "watchlist",
    viewers: [2, 3],
    popularity: 140,
    genres: ["Adventure", "Family"],
  },
  {
    id: 206,
    title: "Lost Lantern",
    offset: 18,
    rating: "PG-13",
    reason: "lookahead",
    popularity: 90,
    genres: ["Mystery"],
    collection: "Most anticipated",
  },
  {
    id: 207,
    title: "Summer Witness",
    offset: -3,
    rating: "R",
    reason: "taste",
    popularity: 75,
    genres: ["Crime", "Drama"],
    inCinemas: true,
  },
  {
    id: 208,
    title: "The Quiet Echo",
    offset: 26,
    rating: "PG-13",
    reason: "popular",
    popularity: 210,
    genres: ["Drama"],
  },
  {
    id: 209,
    title: "Bright Circus",
    offset: 7,
    rating: "G",
    reason: "lookahead",
    popularity: 110,
    genres: ["Animation", "Family"],
    collection: "Coming soon for the family",
  },
  {
    id: 210,
    title: "Hollow Empire",
    offset: 33,
    rating: "R",
    reason: "watchlist",
    viewers: [4],
    popularity: 160,
    genres: ["War", "History"],
  },
  {
    id: 211,
    title: "Distant Hours",
    offset: -24,
    rating: "PG-13",
    reason: "taste",
    popularity: 85,
    genres: ["Science Fiction", "Drama"],
  },
  {
    id: 212,
    title: "Salt & Signal",
    offset: 46,
    rating: "PG-13",
    reason: "popular",
    popularity: 240,
    genres: ["Action", "Adventure"],
  },
];

const VIEWERS: CalendarViewer[] = [
  { id: 1, name: "Alex Rivera", owner: true },
  { id: 2, name: "Sam Rivera" },
  { id: 3, name: "The Kids" },
  { id: 4, name: "Jordan Lee" },
].map((v) => ({ ...v, thumb: avatarFor(v.name) }));

const EPISODE_NAMES = [
  "Pilot",
  "The Long Night",
  "Undertow",
  "Paper Trail",
  "Static",
  "Open Water",
  "Glasshouse",
  "The Return",
  "Low Tide",
  "Fault Lines",
  "Homecoming",
  "First Light",
  "Ashes",
  "The Turn",
];

/** A textless episode still: light, haze and a horizon, seeded by the key. */
function stillFor(key: string): string {
  const h = hash(key) % 360;
  const x = 120 + (hash(key + "x") % 400);
  const y = 60 + (hash(key + "y") % 140);
  const svg = `<svg xmlns="http://www.w3.org/2000/svg" width="640" height="360" viewBox="0 0 640 360"><defs><linearGradient id="s" x1="0" y1="0" x2="0" y2="1"><stop offset="0" stop-color="hsl(${h},55%,58%)"/><stop offset="0.55" stop-color="hsl(${(h + 25) % 360},45%,30%)"/><stop offset="1" stop-color="hsl(${(h + 60) % 360},50%,10%)"/></linearGradient><radialGradient id="l" cx="${x / 640}" cy="${y / 360}" r="0.45"><stop offset="0" stop-color="white" stop-opacity="0.75"/><stop offset="1" stop-color="white" stop-opacity="0"/></radialGradient></defs><rect width="640" height="360" fill="url(#s)"/><rect width="640" height="360" fill="url(#l)"/><path d="M0 ${250 + (hash(key) % 40)} Q160 ${200 + (hash(key + "a") % 60)} 320 ${240 + (hash(key + "b") % 40)} T640 ${230 + (hash(key + "c") % 50)} V360 H0Z" fill="hsl(${(h + 60) % 360},45%,8%)" fill-opacity="0.85"/></svg>`;
  return `data:image/svg+xml;charset=utf-8,${encodeURIComponent(svg)}`;
}

function dayKey(ms: number): string {
  return new Date(ms).toISOString().slice(0, 10);
}

function localToday(): number {
  const n = new Date();
  return Date.UTC(n.getFullYear(), n.getMonth(), n.getDate());
}

/** Snap a date offset to the show's weekday on or after it. */
function onWeekday(base: number, offset: number, weekday: number): number {
  const d = base + offset * DAY;
  const dow = new Date(d).getUTCDay();
  return d + ((weekday - dow + 7) % 7) * DAY;
}

function buildEntries(): CalendarEntry[] {
  const today = localToday();
  const todayKey = dayKey(today);
  const out: CalendarEntry[] = [];
  for (const s of SHOWS) {
    const showKey = String(2_000_000_000_000_000 + s.id);
    const poster = posterFor(showKey, s.title, 2026);
    const backdrop = stillFor(showKey);
    const first = onWeekday(today, s.premiere, s.weekday);
    const announcedOnly = s.premiere > 20;
    if (announcedOnly) {
      out.push({
        id: `premiere:${s.id}`,
        kind: "premiere",
        key: String(3_000_000_000_000_000 + s.id * 10_000 + s.season),
        open: showKey,
        title: s.title,
        episodeTitle: `Season ${s.season}`,
        season: s.season,
        date: dayKey(first),
        poster,
        backdrop,
        overview: `${s.title} returns to ${s.network} for season ${s.season}.`,
        contentRating: s.rating,
        state: "upcoming",
        badges: ["seasonPremiere"],
        reasons: [s.reason],
        viewers: s.viewers,
      });
      continue;
    }
    for (let e = 1; e <= s.episodes; e++) {
      const at = first + (e - 1) * 7 * DAY;
      const key = String(4_000_000_000_000_000 + s.id * 10_000_000 + s.season * 10_000 + e);
      const name = EPISODE_NAMES[(hash(key) % EPISODE_NAMES.length) | 0] as string;
      const date = dayKey(at);
      const badges: CalendarEntry["badges"] = [];
      if (e === 1) badges.push(s.season === 1 ? "seriesPremiere" : "seasonPremiere");
      if (e === s.episodes) badges.push("finale");
      const watched =
        date < todayKey
          ? s.viewers.filter((v) => hash(key + v) % 10 < (date < dayKey(today - 10 * DAY) ? 9 : 4))
          : [];
      out.push({
        id: key,
        kind: "episode",
        key,
        open: showKey,
        title: s.title,
        episodeTitle: e === 1 && s.season === 1 ? "Pilot" : name,
        season: s.season,
        episode: e,
        date,
        poster,
        backdrop,
        still: stillFor(key),
        overview: `${name}: a new chapter of ${s.title}, airing on ${s.network}.`,
        runtimeMinutes: s.runtime,
        contentRating: s.rating,
        rating: Math.round((7 + (hash(key) % 20) / 10) * 10) / 10,
        genres: s.genres,
        state: date > todayKey ? "upcoming" : date === todayKey ? "today" : "available",
        badges,
        reasons: [s.reason],
        viewers: s.viewers,
        watched,
      });
    }
  }
  for (const m of MOVIES) {
    const key = String(1_000_000_000_000_000 + m.id);
    const at = today + m.offset * DAY;
    const date = dayKey(at);
    out.push({
      id: key,
      kind: "movie",
      key,
      open: key,
      title: m.title,
      date,
      year: new Date(at).getUTCFullYear(),
      poster: posterFor(key, m.title, new Date(at).getUTCFullYear()),
      backdrop: stillFor(key),
      overview: `${m.title}: a ${m.genres.join(" and ").toLowerCase()} release.`,
      runtimeMinutes: 95 + (m.id % 40),
      contentRating: m.rating,
      rating: Math.round((6.5 + (hash(key) % 25) / 10) * 10) / 10,
      genres: m.genres,
      state:
        date > todayKey
          ? "upcoming"
          : date === todayKey
            ? "today"
            : m.inCinemas
              ? "unavailable"
              : "available",
      reasons: [m.reason],
      collections: m.collection ? [m.collection] : undefined,
      viewers:
        m.viewers ??
        VIEWERS.filter((v) => v.id !== 3 || ["G", "PG"].includes(m.rating)).map((v) => v.id),
      watched: date < todayKey && !m.inCinemas && m.viewers ? m.viewers.slice(0, 1) : undefined,
    });
  }
  out.sort((a, b) =>
    a.date === b.date
      ? (a.kind === "movie" ? 1 : 0) - (b.kind === "movie" ? 1 : 0) ||
        a.title.localeCompare(b.title)
      : a.date < b.date
        ? -1
        : 1,
  );
  return out;
}

export function createCalendarMock() {
  return {
    /** Title detail for the shows and movies on the calendar. */
    title(key: string): Record<string, unknown> | null {
      const e = buildEntries().find((x) => x.open === key);
      if (!e) return null;
      const show = e.kind !== "movie";
      return {
        key,
        type: show ? "show" : "movie",
        title: e.title,
        year: e.year ?? new Date().getFullYear(),
        summary: show
          ? `${e.title} follows a cast of unlikely allies through one long season of surprises.`
          : e.overview,
        poster: e.poster,
        backdrop: e.backdrop,
        rating: e.rating,
        contentRating: e.contentRating,
        runtimeMinutes: e.runtimeMinutes,
        genres: e.genres ?? [],
        cast: [],
        directors: [],
        keywords: [],
        collection: null,
      };
    },
    handle(
      method: string,
      seg: string[],
      q: URLSearchParams,
    ): { status: number; body: unknown } | null {
      if (method !== "GET" || seg[0] !== "calendar" || seg.length > 1) return null;
      const today = dayKey(localToday());
      const from = q.get("from") ?? today;
      const to = q.get("to") ?? dayKey(localToday() + 41 * DAY);
      const viewer = Number(q.get("viewer") ?? 0);
      const types = q.get("types") ?? "";
      const movies = q.get("movies") ?? "foryou";
      const all = buildEntries();
      const entries = all
        .filter((e) => e.date >= from && e.date <= to)
        .filter((e) => !viewer || e.viewers.includes(viewer))
        .filter((e) =>
          types === "movie" ? e.kind === "movie" : types === "show" ? e.kind !== "movie" : true,
        )
        .filter((e) => {
          if (e.kind !== "movie") return true;
          const r = e.reasons[0];
          if (movies === "hidden") return false;
          if (movies === "watchlist") return r === "watchlist";
          if (movies === "popular") return r === "popular" || r === "taste";
          return r !== "popular";
        })
        .map((e) =>
          viewer ? { ...e, viewers: [viewer], watched: e.watched?.filter((w) => w === viewer) } : e,
        );
      const shows = new Set(
        SHOWS.filter((s) => !viewer || s.viewers.includes(viewer)).map((s) => s.id),
      ).size;
      return {
        status: 200,
        body: {
          from,
          to,
          today,
          viewers: VIEWERS,
          entries,
          shows,
          generatedAt: new Date().toISOString(),
        },
      };
    },
  };
}
