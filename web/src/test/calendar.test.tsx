import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import {
  addDays,
  addMonths,
  localToday,
  periodRange,
  relativeDay,
  shiftAnchor,
  startOfWeek,
  stateOn,
  type CalendarEntry,
} from "../pages/calendar/model";
import { renderAppWith } from "./routes";

const WAIT = { timeout: 4000 };

afterEach(() => vi.unstubAllGlobals());

const calendarCalls = (calls: { method: string; path: string }[]) =>
  calls.filter((c) => c.method === "GET" && c.path.startsWith("/calendar/"));

describe("calendar dates", () => {
  it("lays a month out as six whole weeks from the locale's first weekday", () => {
    expect(periodRange("month", "2026-11-18", 0)).toEqual({ from: "2026-11-01", to: "2026-12-12" });
    expect(periodRange("month", "2026-11-18", 1)).toEqual({ from: "2026-10-26", to: "2026-12-06" });
    expect(periodRange("week", "2026-11-18", 1)).toEqual({ from: "2026-11-16", to: "2026-11-22" });
    expect(periodRange("agenda", "2026-11-18")).toEqual({ from: "2026-11-18", to: "2026-12-22" });
    expect(startOfWeek("2026-11-01", 1)).toBe("2026-10-26");
  });

  it("steps periods and clamps month ends", () => {
    expect(addMonths("2026-01-31", 1)).toBe("2026-02-28");
    expect(shiftAnchor("month", "2026-11-18", 1)).toBe("2026-12-01");
    expect(shiftAnchor("week", "2026-11-18", -1)).toBe("2026-11-11");
    expect(shiftAnchor("agenda", "2026-11-18", 1)).toBe("2026-12-23");
    expect(addDays("2026-12-31", 1)).toBe("2027-01-01");
  });

  it("names nearby days", () => {
    expect(relativeDay("2026-11-10", "2026-11-10")).toBe("Today");
    expect(relativeDay("2026-11-11", "2026-11-10")).toBe("Tomorrow");
    expect(relativeDay("2026-11-09", "2026-11-10")).toBe("Yesterday");
    expect(relativeDay("2026-11-13", "2026-11-10")).toBe("in 3 days");
  });

  it("keeps entry states right when the page outlives midnight", () => {
    const e = { kind: "episode", date: "2026-11-10", state: "today" } as CalendarEntry;
    expect(stateOn(e, "2026-11-09")).toBe("upcoming");
    expect(stateOn(e, "2026-11-10")).toBe("today");
    expect(stateOn(e, "2026-11-11")).toBe("available");
    expect(stateOn({ ...e, kind: "premiere" }, "2026-11-11")).toBe("unavailable");
    expect(stateOn({ ...e, state: "unavailable", date: "2026-11-01" }, "2026-11-11")).toBe(
      "unavailable",
    );
  });
});

describe("calendar page", () => {
  it("is in the Streaming section and shows the household's month", async () => {
    const { fake } = renderAppWith("/calendar/month", () => undefined);
    const nav = await screen.findByRole("link", { name: "Calendar" }, WAIT);
    expect(nav.getAttribute("href")).toBe("/calendar");
    const grid = await screen.findByRole("grid", { name: "Release calendar" }, WAIT);
    const today = localToday();
    const cell = grid.querySelector(`[data-day="${today}"]`)!;
    expect(cell.getAttribute("aria-current")).toBe("date");
    await waitFor(
      () =>
        expect(
          within(grid).getAllByRole("button", { name: /Little Engines/ }).length,
        ).toBeGreaterThan(0),
      WAIT,
    );
    expect(await screen.findByRole("complementary", { name: "Up next" }, WAIT)).toBeTruthy();
    const call = calendarCalls(fake.state.calls)[0]!;
    expect(call.path).toMatch(/movies=foryou/);
    expect(call.path).toMatch(/tz=/);
  });

  it("filters by viewer, kind and movie policy through the URL", async () => {
    const user = userEvent.setup();
    const { fake } = renderAppWith("/calendar/agenda", () => undefined);
    await screen.findAllByRole("button", { name: /Wild Atlas/ }, WAIT);

    await user.click(screen.getByRole("button", { name: /Show/ }));
    await user.click(await screen.findByRole("menuitemradio", { name: /TV only/ }));
    await waitFor(
      () =>
        expect(calendarCalls(fake.state.calls).some((c) => /types=show/.test(c.path))).toBe(true),
      WAIT,
    );
    await waitFor(
      () => expect(screen.queryAllByRole("button", { name: /Wild Atlas/ })).toHaveLength(0),
      WAIT,
    );
    // Movie policy only applies when movies are shown.
    expect(screen.queryByRole("button", { name: /Which movies/ })).toBeNull();

    await user.click(screen.getByRole("button", { name: /Whose calendar/ }));
    await user.click(await screen.findByRole("menuitemradio", { name: /The Kids/ }));
    await waitFor(
      () => expect(calendarCalls(fake.state.calls).some((c) => /viewer=3/.test(c.path))).toBe(true),
      WAIT,
    );
    await waitFor(
      () => expect(screen.queryAllByRole("button", { name: /Night Desk/ })).toHaveLength(0),
      WAIT,
    );
  });

  it("opens an entry's title detail and closes back to the calendar", async () => {
    const user = userEvent.setup();
    const { fake } = renderAppWith("/calendar/agenda", (method, path) =>
      method === "GET" && path.startsWith("/catalog/title/")
        ? { key: path.split("/").pop(), type: "show", title: "Little Engines", summary: "Trains." }
        : undefined,
    );
    const [entry] = await screen.findAllByRole("button", { name: /^Little Engines, S02E/ }, WAIT);
    await user.click(entry!);
    const dialog = await screen.findByRole("dialog", {}, WAIT);
    expect(await within(dialog).findByText("Trains.", {}, WAIT)).toBeTruthy();
    expect(fake.state.calls.some((c) => c.path === "/catalog/title/2000000000000102")).toBe(true);
  });

  it("navigates with keyboard shortcuts", async () => {
    renderAppWith("/calendar/month", () => undefined);
    await screen.findByRole("grid", { name: "Release calendar" }, WAIT);
    const title = () => screen.getByRole("heading", { level: 2, name: /\d{4}|–/ }).textContent;
    const before = title();
    fireEvent.keyDown(window, { key: "j" });
    await waitFor(() => expect(title()).not.toBe(before), WAIT);
    fireEvent.keyDown(window, { key: "t" });
    await waitFor(() => expect(title()).toBe(before), WAIT);
    fireEvent.keyDown(window, { key: "a" });
    await waitFor(
      () => expect(screen.queryByRole("grid", { name: "Release calendar" })).toBeNull(),
      WAIT,
    );
    expect(screen.getByRole("radio", { name: "Agenda" }).getAttribute("aria-checked")).toBe("true");
  });

  it("moves between days with the arrow keys", async () => {
    renderAppWith("/calendar/month", () => undefined);
    const grid = await screen.findByRole("grid", { name: "Release calendar" }, WAIT);
    const today = localToday();
    const cell = grid.querySelector<HTMLElement>(`[data-day="${today}"]`)!;
    expect(cell.tabIndex).toBe(0);
    cell.focus();
    fireEvent.keyDown(cell, { key: "ArrowLeft" });
    await waitFor(
      () => expect(document.activeElement?.getAttribute("data-day")).toBe(addDays(today, -1)),
      WAIT,
    );
  });

  it("explains how the calendar fills itself when nothing is followed", async () => {
    renderAppWith("/calendar/month", (method, path) =>
      method === "GET" && path.startsWith("/calendar/")
        ? {
            from: "",
            to: "",
            today: localToday(),
            viewers: [],
            entries: [],
            shows: 0,
            generatedAt: "",
          }
        : undefined,
    );
    expect(await screen.findByText("Nothing on the calendar yet", {}, WAIT)).toBeTruthy();
    expect(screen.getByRole("link", { name: /Look Ahead collection/ }).getAttribute("href")).toBe(
      "/collections/new",
    );
  });

  it("tells the owner while followed shows are still being fetched", async () => {
    renderAppWith("/calendar/agenda", (method, path) =>
      method === "GET" && path.startsWith("/calendar/")
        ? {
            from: "",
            to: "",
            today: localToday(),
            viewers: [],
            entries: [],
            shows: 3,
            warming: true,
            generatedAt: "",
          }
        : undefined,
    );
    expect(await screen.findByText(/fetching upcoming episodes/, {}, WAIT)).toBeTruthy();
  });
});
