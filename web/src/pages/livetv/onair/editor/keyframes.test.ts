import { describe, expect, it } from "vitest";
import { setKeyframe, moveKeyframe, removeKeyframe, setEase, valueAt } from "./keyframes";
import type { Layer } from "../scene-renderer";
describe("immutable keyframe operations", () => {
  it("preserves the static origin, sorts inserts, and evaluates renderer easing", () => {
    const l: Layer = { id: "a", type: "rect", x: 12 };
    const a = setKeyframe(l, "x", 2, 32);
    expect(l.x).toBe(12);
    expect(valueAt(a.x, 1)).toBe(22);
    const b = setEase(a, "x", 2, "in");
    expect(valueAt(b.x, 1)).toBe(14.5);
  });
  it("resolves retiming collisions and collapses the last remaining key", () => {
    let l: Layer = { id: "a", type: "rect", opacity: 1 };
    l = setKeyframe(l, "opacity", 1, 0);
    l = setKeyframe(l, "opacity", 2, 0.5);
    l = moveKeyframe(l, "opacity", 2, 1);
    expect(l.opacity).toEqual({
      keys: [
        [0, 1],
        [1, 0.5, undefined],
      ],
    });
    expect(removeKeyframe(l, "opacity", 1).opacity).toBe(1);
  });
});
