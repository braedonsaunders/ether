import type { TimelineElement, TimelineResponse } from "../pages/livetv/schedule-layout";
const HOUR = 3600000;
/** Rich, deterministic owner-only fixture. No content is shipped by Ether. */
export function scheduleFixture(channelId: string, from: number, to: number): TimelineResponse {
  const elements: TimelineElement[] = [];
  const origin = Math.floor(from / HOUR) * HOUR;
  for (let start = origin; start < to; start += HOUR) {
    const n = Math.floor(start / HOUR),
      blockId = "prime";
    const entry = (
      kind: TimelineElement["kind"],
      offset: number,
      duration: number,
      title: string,
      extra: Partial<TimelineElement> = {},
    ) =>
      elements.push({
        id: `${channelId}-${start}-${offset}-${kind}`,
        kind,
        start: start + offset,
        end: start + offset + duration,
        title,
        blockId,
        sourceId: "retro",
        rule: "Prime time · Eighties favourites in order (strip, daily)",
        ...extra,
      });
    entry("programme", 0, 24 * 60000, n % 2 ? "Cheers" : "The Golden Girls", {
      subtitle: n % 2 ? "S01E03 · Coach’s Daughter" : "S02E04 · It’s a Miserable Life",
      mediaType: "episode",
      breakPoints: [{ atMs: 12 * 60000, source: "detected", confidence: 0.92 }],
    });
    entry("pod", 24 * 60000, 4 * 60000, "Eighties commercial break", {
      rule: "1980s pattern · Four minutes between programmes",
    });
    for (let i = 0; i < 8; i++)
      entry(
        "spot",
        (24 + i * 0.5) * 60000,
        30000,
        ["Arcade weekend", "Summer on tape", "Your local record store", "Saturday matinee"][i % 4]!,
        { rule: "Retro reel · No repeats within this pod" },
      );
    entry("ident", 28 * 60000, 20000, "NEON 84");
    entry("upnext", 28 * 60000 + 20000, 20000, "Up next · Family Ties");
    entry("stationId", 28 * 60000 + 40000, 20000, "Channel 84");
    entry("plugin", 29 * 60000, 60000, "Local weather", { rule: "Weather · Every hour at :29" });
    if (n % 6 === 0)
      entry("special", 30 * 60000, 30 * 60000, "Friday movie premiere", {
        pinned: true,
        subtitle: "A night at the movies",
        rule: "One-off special · Pinned slot overrides Prime time",
      });
    else if (n % 6 === 1) {
      entry("gap", 30 * 60000, 15 * 60000, "Schedule gap", { rule: "No source matched this slot" });
      entry("offair", 45 * 60000, 15 * 60000, "Off air", {
        rule: "Overnight · Resume at the next hour",
      });
    } else {
      entry("programme", 30 * 60000, 28 * 60000, "Family Ties", {
        subtitle: "S03E01 · The Gambler",
        mediaType: "episode",
      });
      entry("break", 58 * 60000, 60000, "We’ll be right back");
      entry("back", 59 * 60000, 60000, "Back to Family Ties");
    }
  }
  for (const pod of elements.filter((e) => e.kind === "pod")) {
    pod.children = elements.filter(
      (e) => e.kind === "spot" && e.start >= pod.start && e.end <= pod.end,
    );
  }
  return {
    channelId,
    timezone: "America/Toronto",
    now: Date.now(),
    dayparts: [{ id: "prime", name: "Prime time", start: 0, end: 1440, days: [] }],
    elements: elements
      .filter((e) => e.kind !== "spot")
      .filter((e) => e.end > from && e.start < to)
      .sort((a, b) => a.start - b.start || b.end - a.end),
  };
}
