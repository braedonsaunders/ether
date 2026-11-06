import { remixMock } from "./onair-remix";
import { libraryMock } from "./onair-library";
import type { Programme } from "../pages/livetv/model";
import clipPreview from "./clip-preview.mp4?url";
import { referenceLooks, templates } from "../pages/livetv/onair/fixtures";
import type { Look, Reel, OnAirAsset } from "../pages/livetv/onair/model";
export function createOnAirMock(
  tv: {
    handle: (
      method: string,
      seg: string[],
      q: URLSearchParams,
      body: unknown,
    ) => { body: unknown } | null;
  },
  layout = "",
) {
  let looks = structuredClone(referenceLooks);
  let serial = 0;
  let reels: Reel[] = [
    {
      id: "saturday",
      name: "Saturday morning ads",
      order: "shuffle",
      repeatHours: 1,
      loudnessMatch: true,
      trimSilence: true,
      channelIds: ["v-movie-night"],
      clips: ["Turbo Crunch ’87", "Station promo", "Laser tag arena", "Mall opening"].map(
        (name, i) => ({
          id: "sat-" + i,
          name,
          durationMs: [30000, 15000, 30000, 60000][i]!,
          status: "ready",
        }),
      ),
    },
    {
      id: "promos",
      name: "Station promos",
      order: "sequence",
      repeatHours: 1,
      loudnessMatch: true,
      trimSilence: true,
      channelIds: [],
      clips: ["Tonight on Meridian", "The midnight movie"].map((name, i) => ({
        id: "promo-" + i,
        name,
        durationMs: 15000,
        status: "ready",
      })),
    },
  ];
  let assets: OnAirAsset[] = [
    {
      digest: "wordmark",
      name: "Station wordmark.svg",
      kind: "image",
      size: 18400,
      usedBy: ["nocturne", "meridian"],
    },
    {
      digest: "sting",
      name: "Midnight sting.wav",
      kind: "audio",
      size: 284000,
      usedBy: ["nocturne"],
    },
  ];
  // Deterministic layout states for the On Air visual acceptance sweep.
  if (layout === "empty") {
    looks = [];
    reels = [];
    assets = [];
  }
  if (layout === "one") {
    looks = looks.slice(0, 1);
    reels = reels.slice(0, 1).map((r) => ({ ...r, clips: r.clips.slice(0, 1) }));
    assets = assets.slice(0, 1);
  }
  if (layout === "long" || layout === "many") {
    looks = Array.from({ length: layout === "many" ? 32 : 5 }, (_, i) => ({
      ...structuredClone(referenceLooks[i % referenceLooks.length]!),
      id: i < referenceLooks.length ? referenceLooks[i]!.id : `look-${i}`,
      name:
        i % 2
          ? `TheEveningCollectionOfRemarkableAdventuresAndFamiliarFavourites${i + 1}`
          : `The evening collection of remarkable adventures and familiar favourites ${i + 1}`,
    }));
    reels = reels.map((r) => ({
      ...r,
      name: "The nostalgic evening station promos and family favourites reel",
      clips: Array.from({ length: layout === "many" ? 24 : 4 }, (_, i) => ({
        ...structuredClone(r.clips[i % r.clips.length]!),
        id: `clip-${i}`,
        name: `A wonderfully long station promotion and intermission title number ${i + 1}`,
      })),
    }));
    assets = Array.from({ length: layout === "many" ? 24 : 2 }, (_, i) => ({
      ...structuredClone(assets[i % assets.length]!),
      digest: `asset-${i}`,
      name: `The very long original station artwork and music asset title ${i + 1}`,
    }));
  }
  const renders = new Map<string, number>();
  for (const reel of reels)
    for (const [i, clip] of reel.clips.entries()) {
      clip.preview = clipPreview;
      clip.thumbnail =
        "data:image/svg+xml," +
        encodeURIComponent(
          `<svg xmlns="http://www.w3.org/2000/svg" width="320" height="180"><defs><linearGradient id="g"><stop stop-color="${["#33295e", "#143e47", "#582338", "#153858"][i % 4]}"/><stop offset="1" stop-color="#101827"/></linearGradient></defs><rect width="320" height="180" fill="url(#g)"/><circle cx="270" cy="35" r="80" fill="white" opacity=".07"/><text x="24" y="90" fill="white" font-family="sans-serif" font-size="24" font-weight="bold">${clip.name.replace(/&/g, "&amp;")}</text><text x="24" y="130" fill="#aeb7c9" font-family="sans-serif" font-size="14">STATION PROMO</text></svg>`,
        );
    }
  const conversions = new Map<string, number>();
  return {
    handle(
      method: string,
      seg: string[],
      q: URLSearchParams,
      body: unknown,
    ): { body: unknown; status?: number } | null {
      const library = libraryMock(method, seg, body);
      if (library) return library;
      if (seg[0] === "onair" && seg[1] === "scenes" && seg[2] === "validate" && method === "POST") {
        const sc = body as import("../pages/livetv/onair/studio-model").StyleScene;
        const valid =
          sc.document?.v === 1 &&
          sc.document.duration >= 4 &&
          sc.document.duration <= 30 &&
          sc.document.duration % 4 === 0 &&
          sc.sounds.length <= 4;
        return valid
          ? { body: structuredClone(sc) }
          : {
              body: {
                error:
                  "Scene duration must be a multiple of four seconds and use at most four sounds",
              },
              status: 400,
            };
      }
      if (seg[0] === "livetv" && seg[3] === "rundown") {
        const result = tv.handle(method, seg, q, body);
        if (!result || !Array.isArray(result.body)) return result;
        const rows = result.body as Programme[];
        if (layout === "empty") return { body: [] };
        if (layout === "one") return { body: rows.slice(0, 1) };
        if (layout === "long" || layout === "many")
          return {
            body: rows.map((p, i) => ({
              ...p,
              item: {
                ...p.item,
                title: `The extraordinarily long evening programme and familiar favourite number ${i + 1}`,
              },
            })),
          };
        return result;
      }
      if (seg[0] !== "onair") return null;
      const remix = remixMock(method, seg, body);
      if (remix) return remix;
      const [, kind, id, action, clipId] = seg;
      if (kind === "clips" && id === "render")
        return {
          body: {
            status: "prototype",
            url: null,
            message: "Real clips arrive with the Go renderer",
          },
        };
      if (kind === "packs")
        return { body: { message: "Channel packs arrive in a later build" }, status: 501 };
      const data = body as Look & Reel;
      if (kind === "templates") return { body: templates };
      if (kind === "bindings") {
        const channels = tv.handle("GET", ["livetv", "channels"], q, null)?.body as {
          id: string;
          name: string;
        }[];
        const channel = channels?.find((c) => c.id === q.get("channel")) || channels?.[0];
        const qs = new URLSearchParams({
          from: q.get("at") || new Date().toISOString(),
          until: new Date(
            Date.parse(q.get("at") || "") + 86400000 || Date.now() + 86400000,
          ).toISOString(),
        });
        const guide = (
          (tv.handle("GET", ["livetv", "guide"], qs, null)?.body as {
            channelId: string;
            start: string;
            item: { title: string; key?: number; slate?: string };
          }[]) || []
        )
          .filter((p) => p.channelId === channel?.id && p.item.key !== 0 && !p.item.slate)
          .slice(0, 4);
        return {
          body: {
            channel: { name: channel?.name || "Your TV" },
            viewer: { name: q.get("viewer") === "2" ? "Alex" : "Braedon" },
            now: { title: guide[0]?.item.title || "Severance" },
            next: {
              title: guide[1]?.item.title || "Arrival",
              reason: "Because you enjoyed Severance",
              time: guide[1]
                ? new Date(guide[1].start).toLocaleTimeString("en", {
                    hour: "numeric",
                    minute: "2-digit",
                  })
                : "8:30 PM",
              meta: "2024 · Drama · 1h 48m",
              logline: "A familiar place. An unexpected journey.",
            },
            list: guide.map((p, i) => ({
              meta: [
                "The evening feature",
                "A new adventure",
                "Something unexpected",
                "One more story",
              ][i],
              title: p.item.title,
              time: new Date(p.start).toLocaleTimeString("en", {
                hour: "numeric",
                minute: "2-digit",
              }),
            })),
          },
        };
      }
      if (kind === "looks") {
        if (action === "duplicate" || action === "detach") {
          const l = looks.find((x) => x.id === id);
          if (!l) return { body: null, status: 404 };
          const copy = {
            ...structuredClone(l),
            id: "look-" + ++serial,
            name: l.name + " copy",
            pack: undefined,
            channelIds: [],
          };
          looks.push(copy);
          return { body: copy };
        }
        if (method === "GET") {
          const result = id ? looks.find((x) => x.id === id) : looks;
          for (const l of looks) {
            const start = renders.get(l.id);
            if (start)
              l.render = { done: Math.min(7, Math.floor((Date.now() - start) / 600)), total: 7 };
          }
          return { body: result };
        }
        if (method === "DELETE") {
          looks = looks.filter((x) => x.id !== id);
          return { body: null };
        }
        const l = { ...data, id: id || "look-" + ++serial };
        looks = looks.filter((x) => x.id !== l.id);
        looks.push(l);
        renders.set(l.id, Date.now());
        return { body: l };
      }
      if (kind === "reels") {
        const reel = reels.find((r) => r.id === id);
        if (action === "clips" && reel) {
          if (method === "POST") {
            const file = body instanceof FormData ? (body.get("file") as File) : null;
            const clip = {
              id: "clip-" + ++serial,
              name: file?.name || "Uploaded clip",
              durationMs: 30000,
              status: "converting" as const,
              progress: 0,
            };
            reel.clips.push(clip);
            conversions.set(clip.id, Date.now());
            return { body: clip };
          }
          if (method === "DELETE") {
            reel.clips = reel.clips.filter((c) => c.id !== clipId);
            return { body: null };
          }
          if (method === "PUT") {
            Object.assign(
              reel.clips.find((c) => c.id === clipId)!,
              body,
            );
            return { body: reel.clips.find((c) => c.id === clipId) };
          }
          return { body: { url: "/mock/clip.mp4" } };
        }
        for (const r of reels)
          for (const c of r.clips) {
            const start = conversions.get(c.id);
            if (start) {
              c.progress = Math.min(100, Math.floor((Date.now() - start) / 60));
              c.status = c.progress === 100 ? "ready" : "converting";
            }
          }
        if (method === "GET") return { body: id ? reel : reels };
        if (method === "DELETE") {
          reels = reels.filter((r) => r.id !== id);
          return { body: null };
        }
        const r = { ...data, id: id || "reel-" + ++serial };
        reels = reels.filter((x) => x.id !== r.id);
        reels.push(r);
        return { body: r };
      }
      if (kind === "assets") {
        if (method === "GET") return { body: id ? assets.find((a) => a.digest === id) : assets };
        if (method === "DELETE") {
          assets = assets.filter((a) => a.digest !== id);
          return { body: null };
        }
        const file = body instanceof FormData ? (body.get("file") as File) : null;
        const a = {
          digest: "asset-" + ++serial,
          name: file?.name || "Upload",
          kind: body instanceof FormData ? String(body.get("kind")) : "image",
          size: file?.size || 0,
          usedBy: [],
        };
        assets.push(a);
        return { body: a };
      }
      return { body: null, status: 404 };
    },
  };
}

// Demo installation only; production segments come from installed plugins.
export const mockOnAirSegments = [
  {
    kind: "onairSegment" as const,
    id: "plugin:weather/forecast",
    owner: "weather",
    label: { resolved: "Local forecast" },
    group: "Weather",
    meta: { duration: 120, feeds: ["now"] },
  },
];
