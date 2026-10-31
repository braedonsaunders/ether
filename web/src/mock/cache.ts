import type { CacheSnapshot } from "../pages/cache/Cache";
import type { EtherConfig } from "../lib/types";

/** Offline-only fixtures used by the preview and UI tests. */
export function createCacheMock(restartRequired = false) {
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
    },
    {
      ReleaseID: "demo-severance-s02e03",
      Title: "Severance · S02 E03",
      Bytes: 2.1 * GiB,
      Size: 3 * GiB,
      Segments: 540,
      Plays: 2,
      LastUsed: new Date(Date.now() - 3600000).toISOString(),
      LastPlayed: new Date().toISOString(),
      Pins: ["in-progress"],
    },
    {
      ReleaseID: "demo-dune-2160",
      Title: "Dune: Part Two",
      Bytes: 7.8 * GiB,
      Size: 22 * GiB,
      Segments: 2000,
      Plays: 3,
      LastUsed: new Date(Date.now() - 7200000).toISOString(),
      LastPlayed: new Date().toISOString(),
      Pins: [],
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
            state: "disabled",
            message: "Enable a preload allocation to build a local shelf",
            title: "",
            fetched: 0,
            size: 0,
            allocated: 0,
            ready: 0,
            failed: 0,
            paused: false,
            items: [],
            viewers: cfg.cache.preloadEnabled ? 3 : 0,
            selection: cfg.cache.preloadEnabled
              ? [
                  {
                    key: 400001,
                    title: "Severance · S02E04 · Woe's Hollow",
                    type: "episode",
                    score: 91,
                    catalogue: 88,
                    household: 96,
                    reason:
                      "Recent viewing from 3 household viewers · next in a household viewing sequence",
                  },
                  {
                    key: 100001,
                    title: "The Wild Robot",
                    type: "movie",
                    score: 85,
                    catalogue: 80,
                    household: 88,
                    reason: "Recent viewing from 2 household viewers",
                  },
                  {
                    key: 100002,
                    title: "Dune: Part Two",
                    type: "movie",
                    score: 72,
                    catalogue: 94,
                    household: 60,
                    reason: "Matches household genre interests",
                  },
                ]
              : [],
          },
          metrics: { hits: 824, misses: 96, servedBytes: 42 * GiB, evictedBytes: 18 * GiB },
          prefetch: { queued: 0, running: 0, completed: 18, failed: 1, dropped: 0 },
        } satisfies CacheSnapshot,
      };
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
