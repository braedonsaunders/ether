import type { Look } from "./model";
import type { Template, StyleControl, Role } from "./studio-model";
import type { Scene } from "./scene-renderer";
const documents = import.meta.glob<Scene>(
  "../../../../../internal/onair/families/documents/*.json",
  { eager: true, import: "default" },
);
const controlFiles = import.meta.glob<StyleControl[]>(
  "../../../../../internal/onair/families/documents/*.controls.json",
  { eager: true, import: "default" },
);
const records = [
  [
    "nocturne",
    "Nocturne",
    "lateNight",
    "#060508",
    "#C8102E",
    "#EDE6D6",
    "#8FA3B8",
    "Playfair Display",
    "Special Elite",
    "Late night · lights off",
  ],
  [
    "meridian",
    "Meridian",
    "cinema",
    "#070d1d",
    "#D8B26E",
    "#F4EEDF",
    "#17254a",
    "Fraunces",
    "Inter",
    "A world of cinema",
  ],
  [
    "blorp",
    "BLORP!",
    "kids",
    "#6B2BD9",
    "#FF7A00",
    "#ffffff",
    "#7CE03A",
    "Bungee",
    "Fredoka",
    "Totally gooey TV",
  ],
  [
    "neon",
    "NEON 84",
    "retro",
    "#12002b",
    "#ff4fd8",
    "#ffffff",
    "#22e6ff",
    "Orbitron",
    "VT323",
    "Stay tuned",
  ],
  [
    "personal",
    "Your TV",
    "personal",
    "#100b23",
    "#9b85ff",
    "#ffffff",
    "#ff5ca8",
    "Space Grotesk",
    "Inter",
    "Made for you",
  ],
] as [string, string, string, string, string, string, string, string, string, string][];
export const templates: Template[] = [
  "clean",
  "kids",
  "late-night",
  "cinema",
  "retro",
  "personal",
  "grunge",
  "aqua",
  "flat",
].map((id) => {
  const row =
    records.find((r) => (r[2] === "lateNight" ? "late-night" : r[2]) === id) ?? records[1]!;
  const palette =
    id === "clean"
      ? ["#0b1320", "#78b9e8", "#f2f5fa", "#17254a"]
      : id === "grunge"
        ? ["#141021", "#edff38", "#ffffff", "#ed4c99"]
        : id === "aqua"
          ? ["#031d37", "#009edb", "#ffffff", "#8cf5ff"]
          : id === "flat"
            ? ["#102a35", "#f15440", "#ffffff", "#ffd66e"]
            : row.slice(3, 7);
  const words: Record<string, string> = {
    upNext: "UP NEXT",
    tonight: "TONIGHT",
    now: "NOW PLAYING",
    break:
      id === "cinema"
        ? "INTERMISSION"
        : id === "kids"
          ? "WE'LL BE RIGHT BACK!"
          : "Back in a moment",
    back: id === "kids" ? "AND NOW BACK TO" : "NOW RESUMING",
    offair: id === "kids" ? "BLORP! is napping" : "OFF AIR",
    tagline: row[9]!,
  };
  const scenes = Object.entries(documents)
    .filter(
      ([path]) =>
        path.endsWith(".json") &&
        !path.endsWith(".controls.json") &&
        path
          .split("/")
          .at(-1)!
          .startsWith(id + "-"),
    )
    .map(([path, source]) => {
      const role = path
        .split("/")
        .at(-1)!
        .slice(id.length + 1, -5) as Role;
      const doc = JSON.parse(
        JSON.stringify(source)
          .replaceAll("{type.mono}", "{type.body}")
          .replaceAll("{moon}", "#ded9c5")
          .replaceAll("{fog}", palette[3]!)
          .replace(/\{words\.([^{}]+)\}/g, (_m, k: string) => words[k] ?? ""),
      ) as Scene;
      doc.duration = 8;
      if (doc.poster >= 8) doc.poster = 4;
      return {
        id: id + "-" + role,
        role,
        name: role,
        enabled: true,
        weight: 1,
        document: doc,
        sounds: [],
      };
    });
  const controls =
    Object.entries(controlFiles).find(([path]) =>
      path.endsWith("/" + id + ".controls.json"),
    )?.[1] ?? [];
  const name = id === "late-night" ? "Late night" : id[0]!.toUpperCase() + id.slice(1);
  return {
    id,
    name,
    blurb: {
      clean: "Room to breathe. Space to discover.",
      kids: "Big colours for little adventures.",
      "late-night": "Stay a little longer after dark.",
      cinema: "Every evening deserves a premiere.",
      retro: "Neon dreams on a midnight dial.",
      personal: "Your own corner of the airwaves.",
      grunge: "Turn it up. Tear the edges.",
      aqua: "A bright new world behind glass.",
      flat: "Bold blocks. Perfect timing.",
    }[id]!,
    tags: [id],
    style: {
      id: "",
      name,
      template: id,
      brand: {
        palette: {
          background: palette[0]!,
          accent: palette[1]!,
          text: palette[2]!,
          glow: palette[3]!,
        },
        type: {
          display: ["clean", "flat"].includes(id)
            ? "Inter"
            : id === "grunge"
              ? "Archivo Black"
              : id === "aqua"
                ? "Space Grotesk"
                : row[7]!,
          body:
            id === "grunge"
              ? "Special Elite"
              : ["aqua", "flat", "clean"].includes(id)
                ? "Inter"
                : row[8]!,
        },
        energy: 0.5,
        textures: ["grain"],
        tagline: row[9]!,
        language: "en",
      },
      roles: Object.fromEntries(scenes.map((s) => [s.role, { duration: 8 }])),
      scenes,
      controls,
      channelIds: [],
    },
  };
});
export const referenceLooks: Look[] = records.map((r, i) => ({
  ...structuredClone(
    templates.find((t) => t.id === (r[2] === "lateNight" ? "late-night" : r[2]))!.style,
  ),
  id: r[0]!,
  name: r[1]!,
  channelIds:
    i === 0
      ? ["v-movie-night", "v-horror"]
      : i === 4
        ? ["v-personal"]
        : i === 2
          ? ["v-family"]
          : i === 3
            ? ["v-80s"]
            : ["v-movie-night"],
}));
