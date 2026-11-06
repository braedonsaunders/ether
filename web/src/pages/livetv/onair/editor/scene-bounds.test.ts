import { it, expect } from "vitest";
import { pathBounds } from "./scene-bounds";
it("includes exact quadratic cubic and elliptical arc extrema", () => {
  expect(pathBounds("M0 0 Q50 100 100 0")).toEqual({ x: 0, y: 0, w: 100, h: 50 });
  const arc = pathBounds("M0 100 A100 100 0 1 0 200 100 A100 100 0 1 0 0 100 Z");
  expect(arc.x).toBeCloseTo(0);
  expect(arc.y).toBeCloseTo(0);
  expect(arc.w).toBeCloseTo(200);
  expect(arc.h).toBeCloseTo(200);
  expect(pathBounds("M0 0 C0 100 100 100 100 0").h).toBeCloseTo(75);
});
