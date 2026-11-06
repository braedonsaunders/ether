import specimenFont from "../../../internal/onair/fonts/data/fraunces.ttf?url";
import clipPreview from "./clip-preview.mp4?url";
import type { LibraryAsset } from "../pages/livetv/onair/studio-model";
const decorate = (a: LibraryAsset) => ({
  ...a,
  url:
    a.kind === "video" || a.kind === "audio"
      ? clipPreview
      : a.kind === "image"
        ? "data:image/svg+xml," +
          encodeURIComponent(
            '<svg xmlns="http://www.w3.org/2000/svg" width="480" height="270"><rect width="480" height="270" fill="#132c44"/><circle cx="240" cy="120" r="65" fill="#55ddce"/><text x="240" y="232" text-anchor="middle" fill="white" font-size="28">MERIDIAN</text></svg>',
          )
        : a.kind === "font"
          ? specimenFont
          : undefined,
  thumbnail:
    a.kind === "video"
      ? "data:image/jpeg;base64,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"
      : a.kind === "image"
        ? "data:image/svg+xml," +
          encodeURIComponent(
            '<svg xmlns="http://www.w3.org/2000/svg" width="480" height="270"><rect width="480" height="270" fill="#132c44"/><circle cx="240" cy="110" r="65" fill="#55ddce"/><text x="240" y="235" text-anchor="middle" fill="white" font-size="28">MERIDIAN</text></svg>',
          )
        : undefined,
});
let serial = 0;
let assets: LibraryAsset[] = [
  {
    digest: "library-logo",
    name: "Meridian station mark",
    kind: "image",
    size: 18400,
    mime: "image/png",
    createdAt: 1793800000,
    usedBy: [{ kind: "style", id: "meridian", name: "Meridian" }],
  },
  {
    digest: "library-bumper",
    name: "Midnight station bumper",
    kind: "video",
    size: 2480000,
    mime: "video/mp4",
    durationMs: 15000,
    width: 1920,
    height: 1080,
    createdAt: 1793800200,
    usedBy: [],
  },
  {
    digest: "library-sting",
    name: "After hours sting",
    kind: "audio",
    size: 284000,
    mime: "audio/mp4",
    durationMs: 4800,
    createdAt: 1793800300,
    usedBy: [],
  },
  {
    digest: "library-font",
    name: "Station display",
    kind: "font",
    size: 92000,
    mime: "font/ttf",
    license: "OFL",
    createdAt: 1793800400,
    usedBy: [],
  },
];
export function libraryMock(
  method: string,
  seg: string[],
  body: unknown,
): { body: unknown; status?: number } | null {
  if (seg[0] !== "onair" || seg[1] !== "assets") return null;
  const id = seg[2],
    action = seg[3];
  let a = assets.find((x) => x.digest === id);
  if (method === "GET")
    return {
      body:
        action === "waveform"
          ? {
              peaks: Array.from(
                { length: 1000 },
                (_, i) => Math.abs(Math.sin(i * 0.17) * Math.cos(i * 0.031)) * 0.8,
              ),
              durationMs: a?.durationMs,
            }
          : id
            ? a && decorate(a)
            : assets.map(decorate),
    };
  if (method === "DELETE") {
    if (a?.usedBy.length) return { body: { error: "Asset is in use" }, status: 409 };
    assets = assets.filter((x) => x.digest !== id);
    return { body: null };
  }
  if (method === "PATCH" && a) {
    Object.assign(a, body);
    return { body: a };
  }
  if (method === "POST") {
    if (action === "trim" && a) {
      a = {
        ...a,
        digest: `library-${++serial}`,
        name: (body as { name: string }).name,
        usedBy: [],
      };
      assets.push(a);
      return { body: a };
    }
    const data = body instanceof FormData ? body : null;
    const file = data?.get("file") as File | null;
    if (action === "replace" && a) {
      assets = assets.filter((x) => x.digest !== id);
      a = { ...a, digest: `library-${++serial}`, name: file?.name ?? a.name };
      assets.push(a);
      return { body: a };
    }
    const kind = (data?.get("kind") ?? "video") as LibraryAsset["kind"];
    a = {
      digest: `library-${++serial}`,
      name: file?.name ?? "Imported media",
      kind,
      size: file?.size ?? 125000,
      mime: file?.type ?? "video/mp4",
      createdAt: Date.now() / 1000,
      usedBy: [],
      license: String(data?.get("license") ?? ""),
    };
    assets.push(a);
    return { body: a };
  }
  return null;
}
