import type { CacheSnapshot } from "../pages/cache/Cache";
import type { EtherConfig } from "../lib/types";

/** Offline-only fixtures used by the preview and UI tests. */
export function createCacheMock(
  restartRequired = false,
  titleName?: (key: number) => string | undefined,
) {
  const GiB = 1024 ** 3;
  const releases: CacheSnapshot["releases"] = [
    {
      ReleaseID: "demo-arrival-1080",
      Title: "Arrival",
      Bytes: 5.3 * GiB,
      Size: 8 * GiB,
      Segments: 1400,
      Plays: 4,
      LastUsed: new Date(Date.now() - 1200000).toISOString(),
      LastPlayed: new Date().toISOString(),
      Pins: ["owner"],
      reason: {
        causes: [{ code: "playing", firstSeen: new Date(Date.now() - 3600000).toISOString() }],
        retention: { kind: "pinned", pins: ["owner"] },
        file: "Arrival.2016.1080p.mkv",
        source: "Arrival.2016.1080p.BluRay",
      },
    },
    {
      ReleaseID: "demo-severance-s02e03",
      Title: "Severance · S02E03 · Who Is Alive?",
      Bytes: 2.1 * GiB,
      Size: 3 * GiB,
      Segments: 540,
      Plays: 2,
      LastUsed: new Date(Date.now() - 3600000).toISOString(),
      LastPlayed: new Date().toISOString(),
      Pins: ["playback:inprogress"],
      reason: {
        causes: [
          { code: "unfinished", firstSeen: new Date(Date.now() - 7200000).toISOString() },
          { code: "next-episode", firstSeen: new Date(Date.now() - 7200000).toISOString() },
        ],
        retention: { kind: "pinned", pins: ["playback:inprogress"] },
        file: "Severance.S02E03.1080p.mkv",
        source: "Severance.S02E03.1080p.WEB-DL",
      },
    },
    {
      ReleaseID: "demo-dune-2160",
      Title: "Dune: Part Two",
      Bytes: 7.8 * GiB,
      Size: 22 * GiB,
      Segments: 2000,
      Plays: 3,
      LastUsed: new Date(Date.now() - 7200000).toISOString(),
      LastPlayed: new Date(Date.now() - 86400000).toISOString(),
      Pins: ["preloaded"],
      reason: {
        causes: [
          {
            code: "shelf",
            firstSeen: new Date(Date.now() - 86400000).toISOString(),
            shelf: { reasonCodes: ["household-viewing"], score: 85, state: "fetched" },
          },
        ],
        retention: { kind: "pinned", pins: ["preloaded"] },
        file: "Dune.Part.Two.2024.2160p.mkv",
        source: "Dune.Part.Two.2024.2160p.UHD",
      },
    },
    {
      ReleaseID: "demo-interstellar-1080",
      Title: "Interstellar",
      Bytes: 32 * 1024 ** 2,
      Size: 12 * GiB,
      Segments: 8,
      Plays: 0,
      LastUsed: new Date(Date.now() - 100000).toISOString(),
      LastPlayed: "0001-01-01T00:00:00Z",
      Pins: [],
      reason: {
        causes: [{ code: "unrecorded" }],
        retention: {
          kind: "expiry",
          days: 30,
          until: new Date(Date.now() + 86400000).toISOString(),
        },
        file: "Interstellar.2014.1080p.mkv",
        source: "",
      },
    },
    {
      ReleaseID: "bd959c0598e2818d872b8f0e2a1c9d44",
      Title: "bd959c0598e2818d872b8f0e2a1c9d44",
      Bytes: 10.2 * 1024 ** 2,
      Size: 0,
      Segments: 21,
      Plays: 0,
      LastUsed: new Date(Date.now() - 300000).toISOString(),
      LastPlayed: "0001-01-01T00:00:00Z",
      Pins: [],
      reason: {
        causes: [{ code: "quality-check", firstSeen: new Date(Date.now() - 300000).toISOString() }],
        retention: { kind: "lru" },
        file: "bd959c0598e2818d872b8f0e2a1c9d44.mkv",
        source: "The.Americans.2013.S03E01.1080p.BRRip.x264-HiQVE",
      },
    },
  ];
  return (method: string, seg: string[], body: unknown, cfg: EtherConfig, clustered = false) => {
    if (seg[0] !== "cache") return undefined;
    if (method === "GET" && seg.length === 1)
      return {
        status: 200,
        body: {
          clustered,
          restartRequired,
          shared: cfg.cache.storage === "shared",
          used: releases.reduce((sum, r) => sum + r.Bytes, 0),
          budget: cfg.cache.budgetBytes,
          directory: cfg.cache.dir,
          protectedBytes: releases
            .filter((r) => r.Pins?.length)
            .reduce((sum, r) => sum + r.Bytes, 0),
          releases,
          preload: {
            state: "ready",
            message: "Shelf selection is ready; the next pass starts in 15 minutes",
            title: "",
            fetched: 0,
            size: 0,
            allocated: 9 * GiB,
            ready: 1,
            failed: 1,
            paused: false,
            items: [
              {
                key: 400001,
                title: "Severance · S02E04 · Woe's Hollow",
                releaseId: "demo-severance-s02e04",
                size: 3 * GiB,
                complete: true,
                state: "cached",
              },
              {
                key: 100001,
                title: "The Wild Robot",
                releaseId: "demo-wild-robot",
                size: 4 * GiB,
                complete: false,
                state: "queued",
              },
              {
                key: 100002,
                title: "Dune: Part Two",
                releaseId: "demo-dune-2160",
                size: 22 * GiB,
                complete: false,
                state: "failed",
              },
            ],
            viewers: 3,
            selection: [
              {
                key: 400001,
                title: "Severance · S02E04 · Woe's Hollow",
                type: "episode",
                score: 91,
                catalogue: 88,
                household: 96,
                reason:
                  "Recent viewing from 3 household viewers · next in a household viewing sequence",
                reasonCodes: ["household-viewing", "next-in-sequence"],
                reasonArgs: { viewers: 3 },
                state: "cached",
              },
              {
                key: 100001,
                title: "The Wild Robot",
                type: "movie",
                score: 85,
                catalogue: 80,
                household: 88,
                reason: "Recent viewing from 2 household viewers",
                reasonCodes: ["household-viewing"],
                reasonArgs: { viewers: 2 },
                state: "queued",
              },
              {
                key: 100002,
                title: "Dune: Part Two",
                type: "movie",
                score: 72,
                catalogue: 94,
                household: 60,
                reason: "Matches household genre interests",
                reasonCodes: ["household-genres"],
                state: "failed",
              },
            ],
          },
          metrics: { hits: 824, misses: 96, servedBytes: 42 * GiB, evictedBytes: 18 * GiB },
          prefetch: {
            queued: 1,
            running: 1,
            completed: 18,
            failed: 1,
            dropped: 0,
            current: { title: "Severance · S02E04 · Woe's Hollow", cause: "next-episode" },
          },
        } satisfies CacheSnapshot,
      };
    if (seg[1] === "warming") {
      if (method === "GET")
        return {
          status: 200,
          body: {
            titles: (cfg.cache.keepWarmTitles ?? []).map((key) => ({
              key,
              title: titleName?.(key) ?? "Arrival",
            })),
            speculative: cfg.cache.speculativeWarming !== "off",
          },
        };
      if (method === "POST") {
        const b = body as { key?: number; pinned?: boolean; speculative?: boolean };
        if (b.key && b.pinned !== undefined)
          cfg.cache.keepWarmTitles = b.pinned
            ? [...new Set([...(cfg.cache.keepWarmTitles ?? []), b.key])]
            : (cfg.cache.keepWarmTitles ?? []).filter((k) => k !== b.key);
        if (b.speculative !== undefined)
          cfg.cache.speculativeWarming = b.speculative ? "on" : "off";
        return { status: 204, body: null };
      }
    }
    if (method === "POST" && seg[1] === "pin") {
      const b = body as { releaseId: string; pinned: boolean };
      const r = releases.find((r) => r.ReleaseID === b.releaseId);
      if (r)
        r.Pins = b.pinned
          ? [...new Set([...(r.Pins ?? []), "owner"])]
          : (r.Pins?.filter((p) => p !== "owner") ?? []);
      return { status: 204, body: null };
    }
    if (method === "POST" && seg[1] === "preload") return { status: 202, body: null };
    if (method === "POST" && seg[1] === "warm") return { status: 202, body: null };
    return { status: 404, body: null };
  };
}
