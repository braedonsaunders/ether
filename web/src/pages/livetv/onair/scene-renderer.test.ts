import { test, expect, vi } from "vitest";
import { referenceLooks } from "./fixtures";
import { MOMENTS } from "./model";
import { evaluate, ease, random, renderScene, type Scene } from "./scene-renderer";
function context() {
  const calls: unknown[] = [];
  const canvas = { width: 320, height: 180 };
  const gradient = { addColorStop: (...args: unknown[]) => calls.push(["stop", ...args]) };
  const ctx = new Proxy(
    { canvas },
    {
      get(target, key) {
        if (key in target) return target[key as keyof typeof target];
        if (key === "measureText") return (s: string) => ({ width: s.length * 20 });
        if (key === "getTransform")
          return () => ({
            a: 1,
            b: 0,
            c: 0,
            d: 1,
            e: 0,
            f: 0,
            inverse: () => ({ a: 1, b: 0, c: 0, d: 1, e: 0, f: 0 }),
          });
        if (key === "getImageData" || key === "createImageData")
          return (...args: number[]) => ({
            data: new Uint8ClampedArray(
              (args.length === 4 ? args[2]! * args[3]! : args[0]! * args[1]!) * 4,
            ),
          });
        if (key === "createLinearGradient" || key === "createRadialGradient") return () => gradient;
        if (key === "createPattern") return () => "pattern";
        return (...args: unknown[]) => {
          calls.push([key, ...args]);
        };
      },
      set(target, key, value) {
        calls.push([key, value]);
        Reflect.set(target, key, value);
        return true;
      },
    },
  ) as unknown as CanvasRenderingContext2D;
  return { ctx, calls };
}
test("same look, bindings and time produce identical drawing commands", async () => {
  const create = document.createElement.bind(document);
  vi.spyOn(document, "createElement").mockImplementation(((name: string) =>
    name === "canvas"
      ? { getContext: () => context().ctx }
      : create(name)) as typeof document.createElement);
  vi.stubGlobal(
    "Path2D",
    class {
      commands: unknown[] = [];
      constructor(path?: string) {
        if (path) this.commands.push(path);
      }
      rect(...args: number[]) {
        this.commands.push(["rect", ...args]);
      }
      moveTo(...args: number[]) {
        this.commands.push(["moveTo", ...args]);
      }
      lineTo(...args: number[]) {
        this.commands.push(["lineTo", ...args]);
      }
      closePath() {
        this.commands.push(["closePath"]);
      }
    },
  );
  const { renderDocument, styleBindings } = await import("./scene-renderer");
  const renderStyleScene = (
    ctx: CanvasRenderingContext2D,
    look: import("./model").Look,
    moment: string,
    b: import("./model").Bindings,
    t: number,
  ) => {
    const sc = look.scenes.find((s) => s.role === moment);
    if (sc) renderDocument(ctx, sc.document, styleBindings(look, b), t);
  };
  const bindings = { next: { title: "Arrival" }, viewer: { name: "Alex" } };
  renderStyleScene(context().ctx, referenceLooks[0]!, "ident", bindings, 3);
  for (const look of referenceLooks)
    for (const moment of MOMENTS) {
      const a = context(),
        b = context();
      renderStyleScene(a.ctx, look, moment, bindings, 3);
      renderStyleScene(b.ctx, look, moment, bindings, 3);
      expect(JSON.stringify(a.calls)).toEqual(JSON.stringify(b.calls));
    }
  const documents = import.meta.glob<Scene>(
    "../../../../../internal/onair/families/documents/*.json",
    { eager: true, import: "default" },
  );
  expect(Object.values(documents).filter((d) => d.v === 1).length).toBeGreaterThanOrEqual(53);
  for (const doc of Object.values(documents).filter((d) => d.v === 1)) {
    const before = JSON.stringify(doc),
      bindings = {
        tokens: { seed: 7, energy: 2 },
        slots: { "channel.name": "Station", "next.title": "A very long title for wrapping" },
      };
    const a = context(),
      b = context();
    renderScene(a.ctx, doc, bindings, doc.poster);
    renderScene(b.ctx, doc, bindings, doc.poster);
    expect(a.calls.length).toBeGreaterThan(5);
    expect(JSON.stringify(a.calls)).toEqual(JSON.stringify(b.calls));
    expect(JSON.stringify(doc)).toBe(before);
  }
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

test("scene evaluation preserves Go boundary, slot, premultiplied-colour and energy semantics", () => {
  const b = { tokens: { energy: 2, nested: { value: "resolved" } }, slots: { title: "Hello" } };
  expect(evaluate("{title} {nested.value}", b, 0)).toBe("Hello resolved");
  expect(
    evaluate(
      {
        keys: [
          [1, 10],
          [2, 20, "linear"],
        ],
      },
      b,
      0,
    ),
  ).toBe(10);
  expect(
    evaluate(
      {
        keys: [
          [1, 10],
          [2, 20, "linear"],
        ],
      },
      b,
      1.5,
    ),
  ).toBe(15);
  expect(
    evaluate(
      {
        keys: [
          [1, 10],
          [2, 20, "linear"],
        ],
      },
      b,
      3,
    ),
  ).toBe(20);
  expect(
    evaluate(
      {
        keys: [
          [0, "#ff000000"],
          [2, "#0000ffff"],
        ],
      },
      b,
      1,
    ),
  ).toBe("#0000ff80");
  expect(ease("back", 1, 2)).toBe(1);
  expect(ease("spring", 0, 2)).toBe(0);
  expect(ease("back", 0.7, 2)).toBeGreaterThan(ease("back", 0.7, 0));
  expect(random(7, 0)).not.toBe(random(8, 0));
  expect(random(7, 0)).toBe(random(7, 4294967296));
});

test("a tile paints immediately while its worker starts, and releases its worker", async () => {
  const sent: unknown[] = [];
  const terminate = vi.fn();
  vi.stubGlobal(
    "Worker",
    class {
      postMessage(value: unknown) {
        sent.push(value);
      }
      terminate = terminate;
    },
  );
  vi.stubGlobal("OffscreenCanvas", class {});
  vi.stubGlobal(
    "Path2D",
    class {
      rect() {}
      moveTo() {}
      lineTo() {}
      closePath() {}
    },
  );
  const { renderDocument, styleBindings, releaseRenderer } = await import("./scene-renderer");
  const renderStyleScene = (
    ctx: CanvasRenderingContext2D,
    look: import("./model").Look,
    _moment: string,
    b: import("./model").Bindings,
    t: number,
  ) => renderDocument(ctx, look.scenes[0]!.document, styleBindings(look, b), t);
  const look = { ...referenceLooks[1]!, family: "clean" };
  const a = context();
  renderStyleScene(a.ctx, look, "ident", {}, 5.5);
  expect(sent).toHaveLength(1);
  expect(a.calls.length).toBeGreaterThan(5);
  const first = a.calls.length;
  renderStyleScene(a.ctx, look, "ident", {}, 5.5);
  expect(a.calls).toHaveLength(first);
  releaseRenderer(a.ctx);
  expect(terminate).toHaveBeenCalledOnce();
  vi.unstubAllGlobals();
});
