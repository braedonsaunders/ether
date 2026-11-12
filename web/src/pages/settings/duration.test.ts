import { expect, it } from "vitest";
import { parseDuration, formatDuration } from "./duration";
it.each([
  ["720h", "30", "d", "720h"],
  ["6h0m0s", "6", "h", "6h"],
  ["90m", "90", "m", "90m"],
  ["", "", "h", ""],
] as const)("round trips %s", (raw, amount, unit, canonical) => {
  expect(parseDuration(raw)).toEqual({ amount, unit });
  expect(formatDuration(amount, unit)).toBe(canonical);
  expect(parseDuration(formatDuration(amount, unit))).toEqual(parseDuration(raw));
});
