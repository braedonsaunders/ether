import { fireEvent, render, screen, within } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import {
  GuideGrid,
  guideDayCount,
  guideDayOffset,
  guideHourTarget,
} from "../pages/livetv/GuideGrid";
import { ProgrammeInspector } from "../pages/livetv/ProgrammeInspector";
import type { Channel, Programme } from "../pages/livetv/model";

const HOUR = 60 * 60_000;
// Local times, so the day maths runs in whatever zone the suite uses.
const at = (day: number, hour: number, minute = 0) =>
  new Date(2026, 10, day, hour, minute).getTime();

const channel: Channel = {
  id: "ch1",
  number: "1.1",
  name: "News Channel",
  kind: "iptv",
  enabled: true,
  favorite: false,
  group: "News",
  streamUrl: "http://example.com/stream.m3u8",
  epoch: "2026-01-01T00:00:00Z",
  seed: 0,
  shuffle: false,
  items: [],
};

describe("guide day maths", () => {
  it("offers today through the last day with listings, at most a week", () => {
    const now = at(7, 10);
    expect(guideDayCount(now)).toBe(7);
    expect(guideDayCount(now, now - HOUR)).toBe(1);
    expect(guideDayCount(now, at(8, 16))).toBe(2);
    // Listings ending exactly at midnight do not open an empty day.
    expect(guideDayCount(now, at(9, 0))).toBe(2);
    expect(guideDayCount(now, at(30, 0))).toBe(7);
    expect(guideDayOffset(at(9, 23, 59), now)).toBe(2);
  });

  it("jumps to tonight's hour, or tomorrow's once it has aired", () => {
    expect(guideHourTarget(at(7, 12), 0, 20)).toBe(at(7, 20));
    expect(guideHourTarget(at(7, 20, 30), 0, 20)).toBe(at(7, 20));
    expect(guideHourTarget(at(7, 21, 30), 0, 20)).toBe(at(8, 20));
    expect(guideHourTarget(at(7, 21, 30), 0, 23)).toBe(at(7, 23));
    expect(guideHourTarget(at(7, 12), 2, 23)).toBe(at(9, 23));
    // A day before today (scrolled back) still jumps forward.
    expect(guideHourTarget(at(7, 12), -1, 20)).toBe(at(7, 20));
  });
});

describe("GuideGrid day picker", () => {
  const renderGrid = (props: Partial<Parameters<typeof GuideGrid>[0]> = {}) => {
    const now = at(7, 12);
    const from = new Date(at(7, 11));
    const onShift = vi.fn();
    render(
      <GuideGrid
        channels={[channel]}
        guide={new Map()}
        from={from}
        until={new Date(from.getTime() + 24 * HOUR)}
        now={now}
        selected={null}
        onSelect={vi.fn()}
        onShift={onShift}
        onNow={vi.fn()}
        loading={false}
        head={<div />}
        {...props}
      />,
    );
    const open = () => fireEvent.click(screen.getByRole("button", { name: "Day" }));
    return { onShift, open };
  };

  it("moves the window to the whole of the picked day", () => {
    const { onShift, open } = renderGrid();
    expect(screen.getByRole("button", { name: "Day" })).toHaveTextContent("Today");
    open();
    fireEvent.click(screen.getByRole("menuitemradio", { name: "Tomorrow" }));
    // 11:00 today to midnight tomorrow.
    expect(onShift).toHaveBeenCalledWith(13);
  });

  it("scrolls to prime time without moving a window that already holds it", () => {
    const { onShift, open } = renderGrid();
    open();
    fireEvent.click(screen.getByRole("menuitemradio", { name: /^Prime time/ }));
    expect(onShift).not.toHaveBeenCalled();
  });

  it("only offers days and jumps the guide has listings for", () => {
    const { open } = renderGrid({ guideUntil: at(7, 22) });
    open();
    const menu = screen.getByRole("menu", { name: "Day" });
    expect(
      within(menu)
        .getAllByRole("menuitemradio")
        .map((b) => b.textContent),
    ).toEqual(["Today", expect.stringMatching(/^Prime time/)]);
  });
});

describe("ProgrammeInspector", () => {
  const programme: Programme = {
    id: "p1",
    channelId: "ch1",
    start: new Date(at(7, 12)).toISOString(),
    end: new Date(at(7, 13)).toISOString(),
    item: {
      key: 1,
      durationMs: HOUR,
      title: "Evening News",
      summary: "Top stories of the day",
      season: 3,
      episode: 4,
    },
  };

  it("opens as a drawer with the channel's actions", () => {
    const onPlay = vi.fn();
    render(
      <ProgrammeInspector
        open
        inspected={{ channel, programme }}
        now={at(7, 12, 30)}
        onClose={vi.fn()}
        onPlay={onPlay}
      />,
    );
    const drawer = screen.getByRole("dialog", { name: "Evening News" });
    expect(within(drawer).getByText("Top stories of the day")).toBeInTheDocument();
    expect(within(drawer).getByText("S3 E4")).toBeInTheDocument();
    expect(within(drawer).getByText("On now")).toBeInTheDocument();
    fireEvent.click(within(drawer).getByRole("button", { name: /Watch/ }));
    expect(onPlay).toHaveBeenCalledWith(channel);
  });

  it("copies the stream link over plain HTTP, where the Clipboard API is missing", () => {
    const clipboard = Object.getOwnPropertyDescriptor(navigator, "clipboard");
    Object.defineProperty(navigator, "clipboard", { value: undefined, configurable: true });
    const exec = vi.fn().mockReturnValue(true);
    document.execCommand = exec;
    try {
      render(
        <ProgrammeInspector
          open
          inspected={{ channel, programme }}
          now={at(7, 12, 30)}
          onClose={vi.fn()}
        />,
      );
      fireEvent.click(screen.getByRole("button", { name: /Copy link/ }));
      expect(exec).toHaveBeenCalledWith("copy");
    } finally {
      delete (document as { execCommand?: unknown }).execCommand;
      if (clipboard) Object.defineProperty(navigator, "clipboard", clipboard);
      else delete (navigator as { clipboard?: unknown }).clipboard;
    }
  });
});
