import { render, screen, fireEvent, waitFor, within } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { describe, it, expect, vi } from "vitest";
import { setToken } from "../lib/api";
import Activity from "../pages/activity/Activity";
import Notifications from "../pages/notifications/Notifications";
import { HardwareStatus } from "../pages/playback/Hardware";
import { SourceUsage } from "../pages/sources/Usage";

const WAIT = { timeout: 4000 };
const emptySummary = {
  plays: 0,
  watchedMs: 0,
  bytes: 0,
  completed: 0,
  daily: [],
  hourly: [],
  platforms: [],
  titles: [],
  users: [],
};
const schedule = {
  enabled: false,
  weekday: 1,
  hour: 9,
  userId: 0,
  nextDue: 0,
  lastAttempt: 0,
  lastStatus: "",
};

function backend() {
  const calls: { url: string; init?: RequestInit }[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      calls.push({ url, init });
      let v: unknown = { ok: true };
      if (url.includes("/stats/summary")) v = emptySummary;
      else if (url.includes("/stats/history")) v = [];
      else if (url.endsWith("/users"))
        v = {
          users: [
            { id: 7, title: "Sam", username: "sam-account", isOwner: true },
            { id: 12, title: "Alex", username: "alex-account", isOwner: false },
          ],
        };
      else if (url.endsWith("/status/sessions"))
        v = {
          value: [
            {
              ID: "live",
              TitleName: "Example film",
              Username: "Sam",
              Decision: "directplay",
              SpeedBps: 125000,
              Client: { deviceName: "Phone" },
            },
            {
              ID: "other",
              TitleName: "Other film",
              Username: "Alex",
              Client: { deviceName: "TV", protocol: "jellyfin" },
            },
          ],
        };
      else if (url.endsWith("/stats/infrastructure"))
        v = {
          snapshots: {
            providers: {
              at: 1,
              data: [
                {
                  id: "provider-key",
                  name: "My news provider",
                  host: "news.example",
                  state: "ok",
                  avgLatency: 0,
                },
              ],
            },
            indexers: {
              at: 1,
              data: [
                {
                  indexer: "My indexer",
                  hitsUsed: 2,
                  hitsRemaining: 98,
                  grabsUsed: 1,
                  grabsRemaining: 49,
                },
              ],
            },
          },
          providerMonth: [],
          indexerMonth: [],
          costs: [],
          notes: [],
        };
      else if (url.includes("/discovery/settings"))
        v = {
          enabled: false,
          plex: true,
          trakt: true,
          notify: false,
          prewarm: false,
          checkHours: 6,
          naturalSearch: false,
          recaps: false,
        };
      else if (url.endsWith("/notifications/"))
        v = { channels: [], events: ["playback.error", "newsletter.weekly"], deliveries: [] };
      else if (url.endsWith("/status/hwaccel"))
        v = { value: { hwaccel: { name: "none", encoders: [] } } };
      else if (url.endsWith("/newsletter/settings") && !init?.method) v = schedule;
      return new Response(JSON.stringify(v), { status: 200 });
    }),
  );
  return calls;
}

function renderAt(path: string) {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <Routes>
        <Route path="/activity/:tab?" element={<Activity />} />
        <Route path="/notifications/:tab?" element={<Notifications />} />
        <Route path="/usage" element={<SourceUsage />} />
        <Route path="/hardware" element={<HardwareStatus />} />
      </Routes>
    </MemoryRouter>,
  );
}

/** Open an AppKit select by its accessible name and pick an option. */
async function choose(label: string, option: string) {
  fireEvent.click(screen.getByRole("button", { name: label }));
  fireEvent.click(await screen.findByRole("option", { name: option }));
}

const body = (c: { init?: RequestInit }) => JSON.parse(String(c.init?.body));

describe("Activity", () => {
  it("shows actual live streams and sends stop action", async () => {
    const calls = backend();
    renderAt("/activity");
    expect(await screen.findByText("Example film", {}, WAIT)).toBeInTheDocument();
    expect(screen.getByText("Sam · Phone")).toBeInTheDocument();
    fireEvent.change(screen.getByLabelText("Protocol"), { target: { value: "jellyfin" } });
    expect(screen.queryByText("Example film")).toBeNull();
    expect(screen.getByText("Other film")).toBeInTheDocument();
    fireEvent.change(screen.getByLabelText("Protocol"), { target: { value: "plex" } });
    expect(screen.queryByText("Other film")).toBeNull();
    expect(within(screen.getByText("Example film").closest("li")!).getByText("plex")).toBeVisible();
    fireEvent.click(screen.getByRole("button", { name: "Stop stream" }));
    await waitFor(() =>
      expect(
        calls.some((c) => c.url.endsWith("/stats/sessions/live/stop") && c.init?.method === "POST"),
      ).toBe(true),
    );
    fireEvent.click(screen.getByRole("tab", { name: "History" }));
    expect(
      await screen.findByText(
        "History starts when Ether observes playback. There are no recorded plays in this period.",
        {},
        WAIT,
      ),
    ).toBeInTheDocument();
  });

  it("filters by a named viewer while sending the existing account ID", async () => {
    const calls = backend();
    renderAt("/activity/history");
    await screen.findByText(/History starts when Ether observes playback/, {}, WAIT);
    fireEvent.click(screen.getByRole("button", { name: "Viewer" }));
    expect(await screen.findByRole("option", { name: "Sam (owner)" })).toBeInTheDocument();
    fireEvent.click(screen.getByRole("option", { name: "Alex" }));
    await waitFor(() =>
      expect(calls.some((c) => c.url.includes("/stats/summary?days=30&user=12"))).toBe(true),
    );
    await waitFor(() =>
      expect(calls.some((c) => c.url.includes("/stats/history?days=30&user=12"))).toBe(true),
    );
  });

  it("shows viewing totals and breakdowns on the stats tab", async () => {
    backend();
    renderAt("/activity/stats");
    expect(await screen.findByText("Hours watched", {}, WAIT)).toBeInTheDocument();
    expect(screen.getByRole("radiogroup", { name: "Viewing breakdown" })).toBeInTheDocument();
    fireEvent.click(screen.getByRole("radio", { name: "Titles" }));
    expect(screen.getByText("Popular titles")).toBeInTheDocument();
  });
});

describe("Notifications", () => {
  it("saves the weekly schedule in place through the save bar", async () => {
    const calls = backend();
    renderAt("/notifications/report");
    const toggle = await screen.findByLabelText("Email the weekly report", {}, WAIT);
    expect(screen.queryByRole("button", { name: "Configure weekly report" })).toBeNull();
    expect(screen.queryByRole("region", { name: "Unsaved changes" })).toBeNull();
    fireEvent.click(toggle);
    await choose("Report viewer", "Alex");
    await choose("Day", "Friday");
    const bar = screen.getByRole("region", { name: "Unsaved changes" });
    fireEvent.click(within(bar).getByRole("button", { name: "Save changes" }));
    await waitFor(() =>
      expect(
        calls.some(
          (c) =>
            c.url.endsWith("/newsletter/settings") &&
            c.init?.method === "PUT" &&
            body(c).enabled === true &&
            body(c).userId === 12 &&
            body(c).weekday === 5 &&
            body(c).hour === 9,
        ),
      ).toBe(true),
    );
  });

  it("previews the report for the chosen viewer inline", async () => {
    setToken("ether-admin-report");
    const calls = backend();
    renderAt("/notifications/report");
    await screen.findByLabelText("Email the weekly report", {}, WAIT);
    fireEvent.click(screen.getByLabelText("Email the weekly report"));
    await choose("Report viewer", "Alex");
    fireEvent.click(screen.getByRole("button", { name: "Preview weekly report" }));
    await waitFor(() =>
      expect(calls.some((c) => c.url.endsWith("/stats/newsletter?user=12"))).toBe(true),
    );
    const preview = calls.find((c) => c.url.endsWith("/stats/newsletter?user=12"))!;
    const sent = new Headers(preview.init?.headers);
    expect(sent.get("X-Ether-Admin-Token")).toBe("ether-admin-report");
    expect(sent.get("X-Plex-Token")).toBeNull();
    setToken("");
    expect(await screen.findByTitle("Weekly newsletter preview")).toBeInTheDocument();
    expect(screen.getByLabelText("Email the weekly report")).toBeChecked();
    expect(screen.getByRole("button", { name: "Report viewer" })).toHaveTextContent("Alex");
    // A preview never saves the schedule.
    expect(calls.some((c) => c.init?.method === "PUT")).toBe(false);
  });

  it("creates a channel with real event rules in a drawer", async () => {
    const calls = backend();
    renderAt("/notifications");
    fireEvent.click(await screen.findByRole("button", { name: "New channel" }, WAIT));
    const drawer = await screen.findByRole("dialog", { name: "New notification channel" });
    fireEvent.change(within(drawer).getByLabelText("Endpoint URL"), {
      target: { value: "https://notify.example/hook" },
    });
    fireEvent.click(within(drawer).getByRole("button", { name: "Clear" }));
    expect(within(drawer).getByLabelText("Weekly newsletter")).toBeDisabled();
    fireEvent.click(within(drawer).getByLabelText("Playback failed"));
    fireEvent.click(within(drawer).getByRole("button", { name: "Save channel" }));
    await waitFor(() =>
      expect(
        calls.some(
          (c) =>
            c.url.includes("/notifications/") &&
            c.init?.method === "PUT" &&
            body(c).endpoint === "https://notify.example/hook" &&
            body(c).events.includes("playback.error"),
        ),
      ).toBe(true),
    );
  });

  it("shows the delivery history tab", async () => {
    backend();
    renderAt("/notifications/deliveries");
    expect(await screen.findByText("No queued notifications", {}, WAIT)).toBeInTheDocument();
  });
});

describe("Source usage", () => {
  it("saves provider and indexer prices in place without asking for IDs", async () => {
    const calls = backend();
    renderAt("/usage");
    const provider = await screen.findByLabelText("Monthly price for My news provider", {}, WAIT);
    expect(screen.queryByLabelText("Service ID")).not.toBeInTheDocument();
    expect(screen.queryByRole("dialog")).toBeNull();
    fireEvent.change(provider, { target: { value: "8.50" } });
    fireEvent.change(screen.getByLabelText("Monthly price for My indexer"), {
      target: { value: "4" },
    });
    fireEvent.change(screen.getByLabelText("Currency for My indexer"), {
      target: { value: "eur" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Save prices" }));
    await waitFor(() =>
      expect(
        calls.some(
          (c) =>
            c.url.endsWith("/stats/costs") &&
            c.init?.method === "PUT" &&
            body(c).kind === "provider" &&
            body(c).id === "provider-key" &&
            body(c).monthly === 8.5,
        ),
      ).toBe(true),
    );
    expect(
      calls.some(
        (c) =>
          c.url.endsWith("/stats/costs") &&
          body(c).kind === "indexer" &&
          body(c).id === "My indexer" &&
          body(c).monthly === 4 &&
          body(c).currency === "EUR",
      ),
    ).toBe(true);
  });
});

describe("Hardware status", () => {
  it("shows a tone-map failure on Activity and the transcoder card", async () => {
    const notice = "This FFmpeg cannot tone-map HDR video to SDR";
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL) => {
        const url = String(input);
        const value = url.endsWith("/status/sessions")
          ? []
          : url.endsWith("/users")
            ? undefined
            : undefined;
        if (url.endsWith("/status/hwaccel")) {
          return new Response(
            JSON.stringify({
              value: { hwaccel: { name: "none", encoders: [] }, playbackError: notice },
            }),
            { status: 200 },
          );
        }
        if (url.endsWith("/status/sessions")) {
          return new Response(JSON.stringify({ value: [] }), { status: 200 });
        }
        if (url.endsWith("/users"))
          return new Response(JSON.stringify({ users: [] }), { status: 200 });
        return new Response(JSON.stringify(value ?? { ok: true }), { status: 200 });
      }),
    );
    const activity = renderAt("/activity");
    expect(await screen.findByRole("alert", {}, WAIT)).toHaveTextContent("cannot tone-map");
    activity.unmount();
    renderAt("/hardware");
    expect(await screen.findByRole("alert", {}, WAIT)).toHaveTextContent("cannot tone-map");
  });

  it("reports software-only transcoding", async () => {
    backend();
    renderAt("/hardware");
    expect(await screen.findByText("Software only", {}, WAIT)).toBeInTheDocument();
    expect(screen.getByText("No hardware encoders detected")).toBeInTheDocument();
  });
});
