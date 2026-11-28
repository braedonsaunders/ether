import { describe, expect, it } from "vitest";
import { deviceCappedHeight } from "../pages/releases/History";
import type { HistoryRecord } from "../lib/releases";

const rec = (p: Partial<HistoryRecord>): HistoryRecord =>
  ({
    id: 1,
    at: "",
    titleKey: "k",
    titleName: "T",
    candidates: 0,
    profile: "4K",
    ...p,
  }) as HistoryRecord;

describe("deviceCappedHeight", () => {
  it("reports a device limit below the profile's ceiling", () => {
    expect(
      deviceCappedHeight(
        rec({ profileMaxHeight: 2160, clientMaxHeight: 1080, effectiveMaxHeight: 1080 }),
      ),
    ).toBe(1080);
    // "Any" profile ceiling (0) held down by the device.
    expect(
      deviceCappedHeight(
        rec({ profileMaxHeight: 0, clientMaxHeight: 1080, effectiveMaxHeight: 1080 }),
      ),
    ).toBe(1080);
  });

  it("stays quiet when the profile's own ceiling applied", () => {
    expect(
      deviceCappedHeight(
        rec({ profileMaxHeight: 2160, clientMaxHeight: 2160, effectiveMaxHeight: 2160 }),
      ),
    ).toBe(0);
    expect(
      deviceCappedHeight(
        rec({ profileMaxHeight: 1080, clientMaxHeight: 2160, effectiveMaxHeight: 1080 }),
      ),
    ).toBe(0);
    expect(deviceCappedHeight(rec({ profileMaxHeight: 2160 }))).toBe(0);
    expect(deviceCappedHeight(rec({}))).toBe(0);
    // Legacy record: the configured ceiling is unknown, so no claim.
    expect(deviceCappedHeight(rec({ clientMaxHeight: 1080, effectiveMaxHeight: 1080 }))).toBe(0);
  });
});
