import type { Style, Template, StyleControl } from "../pages/livetv/onair/studio-model";
import type { Scene } from "../pages/livetv/onair/scene-renderer";
const documents = import.meta.glob("../../../internal/onair/families/documents/*.json", {
  eager: true,
  import: "default",
}) as Record<string, Scene | StyleControl[]>;
const names: Record<string, string> = {
  clean: "Clean",
  kids: "Kids",
  "late-night": "Late night",
  cinema: "Cinema",
  retro: "Retro 80",
  personal: "Personal",
  grunge: "Grunge 90",
  aqua: "Aqua 00",
  flat: "Flat 10",
};
const palettes: Record<string, string[]> = {
  clean: ["#0b1320", "#78b9e8", "#f2f5fa", "#17254a"],
  kids: ["#6b2bd9", "#ff7a00", "#ffffff", "#7ce03a"],
  "late-night": ["#060508", "#c8102e", "#e9e3ce", "#17254a"],
  cinema: ["#070d1d", "#d8b26e", "#f4eedf", "#17254a"],
  retro: ["#12002b", "#ff4fd8", "#ffffff", "#22e6ff"],
  personal: ["#100b23", "#9b85ff", "#ffffff", "#ff5ca8"],
  grunge: ["#141021", "#edff38", "#ffffff", "#ed4c99"],
  aqua: ["#031d37", "#009edb", "#ffffff", "#8cf5ff"],
  flat: ["#102a35", "#f15440", "#ffffff", "#ffd66e"],
};
const display: Record<string, string> = {
  clean: "inter",
  kids: "bungee",
  "late-night": "playfairdisplay",
  cinema: "fraunces",
  retro: "orbitron",
  personal: "spacegrotesk",
  grunge: "archivoblack",
  aqua: "spacegrotesk",
  flat: "inter",
};
const templates: Template[] = Object.entries(names).map(([id, name]) => {
  const [background, accent, text, glow] = palettes[id]! as [string, string, string, string];
  const scenes = Object.entries(documents)
    .filter(([p]) =>
      p
        .split("/")
        .pop()
        ?.startsWith(id + "-"),
    )
    .map(([path, raw]) => {
      const document = JSON.parse(
        JSON.stringify(raw).replace(
          /\{words\.(\w+)\}/g,
          (_, word) =>
            (
              ({
                upNext: "UP NEXT",
                tonight: "TONIGHT",
                now: "NOW PLAYING",
                break: "INTERMISSION",
                back: "NOW RESUMING",
                offair: "OFF AIR",
              }) as Record<string, string>
            )[word] ?? "",
        ),
      ) as Scene;
      return {
        id: path.split("/").pop()!.replace(".json", ""),
        role: document.moment as Style["scenes"][number]["role"],
        name: document.moment,
        enabled: true,
        weight: 1,
        document,
        sounds: [],
      };
    });
  return {
    id,
    name,
    blurb: "A complete starting point for your channel.",
    tags: [name],
    style: {
      id: "",
      name,
      template: id,
      brand: {
        palette: { background, accent, text, glow },
        type: { display: display[id]!, body: id === "retro" ? "vt323" : "inter" },
        tagline: "",
        energy: 0.5,
        textures: ["grain"],
        language: "en",
      },
      roles: Object.fromEntries(scenes.map((s) => [s.role, { duration: s.document.duration }])),
      scenes,
      controls:
        (Object.entries(documents).find(([p]) =>
          p.endsWith("/" + id + ".controls.json"),
        )?.[1] as StyleControl[]) ?? [],
      channelIds: [],
    },
  };
});
let styles: Style[] = templates
  .slice(0, 4)
  .map((t, i) => ({ ...structuredClone(t.style), id: "studio-" + i, channelIds: [] }));
export function remixMock(
  method: string,
  seg: string[],
  body: unknown,
): { body: unknown; status?: number } | null {
  if (seg[0] !== "onair") return null;
  const [, kind, id, action] = seg;
  if (kind === "templates") return { body: templates };
  if (kind === "packs" && method === "POST") {
    const file = body instanceof FormData ? body.get("file") : null;
    if (!(file instanceof File) || !file.name.toLowerCase().endsWith(".etherpkg"))
      return { status: 400, body: { message: "Choose an .etherpkg style pack." } };
    // Offline fixture: model the server's imported owned-style response, without
    // executing or decoding uploaded plugin packages in the mock browser.
    const template = templates.find((entry) => entry.id === "retro")!;
    const style = structuredClone(template.style);
    style.id = crypto.randomUUID();
    style.name =
      file.name
        .replace(/\.etherpkg$/i, "")
        .replace(/[_-]+/g, " ")
        .trim() || "Imported style";
    style.brand.tagline = "Stay tuned to the future";
    style.channelIds = [];
    style.channelCount = 0;
    style.scenes = style.scenes.map((scene) => ({ ...scene, id: crypto.randomUUID() }));
    styles.push(style);
    return { status: 201, body: style };
  }

  if (kind === "looks" && !["duplicate", "detach", "pack"].includes(action ?? "")) {
    if (method === "GET") return { body: id ? styles.find((s) => s.id === id) : styles };
    if (method === "DELETE") {
      styles = styles.filter((s) => s.id !== id);
      return { body: null };
    }
    const style = structuredClone(body) as Style;
    style.id = id ?? crypto.randomUUID();
    styles = styles.filter((s) => s.id !== style.id).concat(style);
    return { body: style };
  }
  if (kind === "draft") {
    const request = body as { style: Style; prompt: string; sceneId?: string };
    const style = structuredClone(request.style);
    for (const scene of style.scenes.filter((s) => !request.sceneId || s.id === request.sceneId)) {
      scene.document.layers.push({
        id: crypto.randomUUID(),
        type: "text",
        text: request.prompt.slice(0, 100),
        x: 960,
        y: 900,
        size: 42,
        font: "{type.body}",
        fill: "{text}",
        align: "center",
        maxWidth: 1600,
        fit: "shrink",
        opacity: {
          keys: [
            [0, 0],
            [1, 1, "out"],
          ],
        },
      });
    }
    return {
      body: {
        style,
        summary: "Added your requested message with a gentle reveal. Preview it before accepting.",
      },
    };
  }
  if (kind === "scenes" && id === "validate") return { body };
  return null;
}
