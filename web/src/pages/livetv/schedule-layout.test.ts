import { describe, expect, it } from "vitest";
import { HOUR, labelDetail, layoutElements, tooltipContent, daypartBands } from "./schedule-layout";
import { scheduleFixture } from "../../mock/schedule-timeline";
describe("schedule layout", () => {
  const from = Date.UTC(2026, 10, 5),
    range = { from, to: from + 7 * 24 * HOUR };
  const data = scheduleFixture("retro", range.from, range.to);
  it("groups short spots at week scale and expands them when zoomed", () => {
    const week = layoutElements(data.elements, range, 800, 168),
      close = layoutElements(data.elements, range, 800, 1);
    expect(week.some((i) => i.el.kind === "pod")).toBe(true);
    expect(week.some((i) => i.el.kind === "spot")).toBe(false);
    expect(close.some((i) => i.el.kind === "spot")).toBe(true);
    expect(close.every((i) => i.width > 0 && i.left >= 0)).toBe(true);
  });
  it("recomputes label detail with container width", () => {
    expect(labelDetail(40)).toBe("compact");
    expect(labelDetail(100)).toBe("title");
    expect(labelDetail(200)).toBe("subtitle");
    const small = layoutElements(data.elements, range, 400, 6),
      wide = layoutElements(data.elements, range, 1400, 6);
    expect(wide[0]!.width).toBeGreaterThan(small[0]!.width);
    expect(wide[0]!.detail).not.toBe(small[0]!.detail);
  });
  it("contains complete timezone, episode, placement and pod details", () => {
    const episode = data.elements.find((e) => e.subtitle?.includes("S01"))!;
    const text = tooltipContent(episode, data.timezone, [], "en-US");
    expect(text).toContain("S01E03");
    expect(text).toContain("America/Toronto");
    expect(text).toContain(episode.rule);
    const pod = data.elements.find((e) => e.kind === "pod")!,
      spots = pod.children!;
    expect(tooltipContent(pod, data.timezone, spots)).toContain(spots[0]!.title);
  });
  it("uses channel weekdays and preserves overnight dayparts", () => {
    const bands = daypartBands(
      { ...data, dayparts: [{ id: "night", name: "Night", start: 1380, end: 120, days: [3] }] },
      { from: Date.UTC(2026, 10, 5, 3), to: Date.UTC(2026, 10, 5, 8) },
    );
    expect(bands).toHaveLength(1);
    expect(bands[0]!.start).toBe(Date.UTC(2026, 10, 5, 4));
    expect(bands[0]!.end).toBe(Date.UTC(2026, 10, 5, 7));
  });
});
