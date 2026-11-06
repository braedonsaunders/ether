import { describe, it, expect } from "vitest";
import {
  groupLayers,
  ungroup,
  moveLayer,
  duplicateLayers,
  findLayer,
  selectionPath,
  selectionTarget,
  updateSharedText,
  sharedTextLayers,
} from "./scene-edit";
import type { Scene } from "../scene-renderer";
const scene: Scene = {
  v: 1,
  moment: "ident",
  duration: 8,
  poster: 3,
  layers: [
    { id: "a", type: "text" },
    { id: "b", type: "path" },
    { id: "c", type: "rect" },
  ],
};
describe("document operations", () => {
  it("groups and ungroups without changing order or input", () => {
    const grouped = groupLayers(scene, ["a", "b"]);
    expect(grouped.layers[0]?.layers?.map((l) => l.id)).toEqual(["a", "b"]);
    expect(ungroup(grouped, grouped.layers[0]!.id).layers.map((l) => l.id)).toEqual([
      "a",
      "b",
      "c",
    ]);
    expect(scene.layers).toHaveLength(3);
  });
  it("rejects cyclic moves and allocates every duplicate id", () => {
    const g = groupLayers(scene, ["a", "b"]);
    expect(moveLayer(g, g.layers[0]!.id, "a", 0)).toBe(g);
    const d = duplicateLayers(g, [g.layers[0]!.id]);
    const ids: string[] = [];
    const walk = (ls: Scene["layers"]) =>
      ls.forEach((l) => {
        ids.push(l.id);
        if (l.layers) walk(l.layers);
      });
    walk(d.layers);
    expect(new Set(ids).size).toBe(ids.length);
    expect(findLayer(d, "a")).toBeDefined();
  });
});

it("keeps scene envelopes outside the drill scope and edits text stacks atomically", () => {
  const doc: Scene = {
    ...scene,
    layers: [
      {
        id: "envelope",
        type: "group",
        layers: [
          { id: "bg", type: "rect", w: 1920, h: 1080 },
          {
            id: "title",
            type: "group",
            layers: [
              { id: "front", type: "text", text: "{channel.name}" },
              { id: "back", type: "text", text: "{channel.name}" },
            ],
          },
        ],
      },
    ],
  };
  expect(selectionPath(doc, "front").map((l) => l.id)).toEqual(["title", "front"]);
  expect(selectionTarget(doc, "front")).toBe("title");
  expect(selectionTarget(doc, "front", "title")).toBe("front");
  expect(selectionTarget(doc, "front", undefined, true)).toBe("front");
  const edited = updateSharedText(doc, "title", {
    text: "My channel",
    font: "orbitron",
    size: 120,
    track: 4,
  });
  const texts = sharedTextLayers(findLayer(edited, "title")!);
  expect(texts.map((l) => l.id)).toEqual(["front", "back"]);
  expect(
    texts.every(
      (l) => l.text === "My channel" && l.font === "orbitron" && l.size === 120 && l.track === 4,
    ),
  ).toBe(true);
  expect(findLayer(doc, "front")?.text).toBe("{channel.name}");
});
