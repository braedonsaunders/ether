import { ConfirmRoot } from "@braedonsaunders/appkit-ui";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";
import { invalidateRegistry } from "../lib/extend";
import LiveTV from "../pages/livetv/LiveTV";
import { ScheduleTimeline } from "../pages/livetv/ScheduleTimeline";
import { calls as fakeCalls } from "./harness";
import { renderAppWith } from "./routes";

const hlsLoads = vi.hoisted(() => [] as string[]);
vi.mock("hls.js", () => {
  class FakeHls {
    static Events = { ERROR: "hlsError", LEVEL_SWITCHED: "hlsLevelSwitched" };
    static isSupported = () => true;
    on() {}
    attachMedia() {}
    loadSource(u: string) {
      hlsLoads.push(u);
    }
    destroy() {}
  }
  return { default: FakeHls };
});

const WAIT = { timeout: 4000 };

function renderLiveTV(path = "/livetv") {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <ConfirmRoot />
      <Routes>
        <Route path="/livetv/:tab?" element={<LiveTV />} />
      </Routes>
    </MemoryRouter>,
  );
}

type Call = { url: string; init?: RequestInit };

// The console asks for pages; this mirrors the server's page shape so the
// tests can keep declaring a flat channel array.
function channelPage(channels: any[], url: string) {
  const u = new URL(url, "http://local");
  const offset = Number(u.searchParams.get("offset") ?? 0);
  const limit = Number(u.searchParams.get("limit") ?? 200);
  const search = (u.searchParams.get("q") ?? "").trim().toLowerCase();
  const filter = u.searchParams.get("filter") ?? "all";
  const numeric = (c: any) => parseFloat(c.number) || 0;
  const sorted = [...channels].sort(
    (a, b) =>
      Number(a.kind === "virtual" ? 0 : 1) - Number(b.kind === "virtual" ? 0 : 1) ||
      String(a.sourceId ?? "").localeCompare(String(b.sourceId ?? "")) ||
      numeric(a) - numeric(b) ||
      String(a.number).localeCompare(String(b.number)),
  );
  const matches = sorted.filter((c) => {
    if (filter === "live" && !c.enabled) return false;
    if (filter === "favorites" && !c.favorite) return false;
    if (filter === "virtual" && c.kind !== "virtual") return false;
    if (filter === "iptv" && c.kind !== "iptv") return false;
    if (filter === "off" && c.enabled) return false;
    if (filter === "missing" && (!c.enabled || c.kind !== "iptv" || c.guideId)) return false;
    return !search || `${c.number} ${c.name} ${c.group ?? ""}`.toLowerCase().includes(search);
  });
  const groups: any[] = [];
  matches.forEach((c, i) => {
    const key = c.kind === "virtual" ? "virtual" : `src-${c.sourceId ?? ""}`;
    if (!groups.length || groups[groups.length - 1].key !== key)
      groups.push({
        key,
        sourceId: c.sourceId,
        kind: c.kind === "virtual" ? "virtual" : "iptv",
        count: 0,
        start: i,
      });
    groups[groups.length - 1].count++;
  });
  const counts = {
    all: channels.length,
    live: channels.filter((c) => c.enabled).length,
    favorites: channels.filter((c) => c.favorite).length,
    virtual: channels.filter((c) => c.kind === "virtual").length,
    iptv: channels.filter((c) => c.kind === "iptv").length,
    off: channels.filter((c) => !c.enabled).length,
    missing: channels.filter((c) => c.enabled && c.kind === "iptv" && !c.guideId).length,
  };
  const page = matches.slice(offset, offset + limit);
  return {
    channels: page,
    total: matches.length,
    counts,
    groups,
    coverage: Object.fromEntries(
      page.map((c) => [c.id, c.kind === "virtual" || Boolean(c.guideId)]),
    ),
    nextNumber: String(Math.max(0, ...channels.map((c) => Math.floor(numeric(c)))) + 1),
  };
}

function backend(
  channels: unknown[] = [],
  extra: (url: string, init?: RequestInit) => unknown = () => undefined,
) {
  const calls: Call[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      calls.push({ url, init });
      const method = init?.method ?? "GET";
      let data: unknown = await extra(url, init);
      if (data instanceof Response) return data;
      if (data !== undefined) return new Response(JSON.stringify(data), { status: 200 });
      data = [];
      if (/\/livetv\/channels\/[^/]+\/timeline\?/.test(url))
        data = {
          channelId: url.split("/channels/")[1]?.split("/")[0],
          timezone: "UTC",
          now: Date.now(),
          elements: [],
          dayparts: [],
        };
      else if (url.includes("/livetv/channels/page")) data = channelPage(channels, url);
      else if (url.endsWith("/livetv/channels/summary")) data = channels;
      else if (/\/livetv\/channels\/[^/?]+$/.test(url) && method === "GET")
        data = channels.find((c: any) => url.endsWith(`/livetv/channels/${c.id}`)) ?? {};
      else if (url.endsWith("/livetv/channels") && method === "GET") data = channels;
      else if (/\/livetv\/channels(\/|$)/.test(url)) data = JSON.parse(String(init?.body ?? "{}"));
      else if (url.endsWith("/users"))
        data = { users: [{ id: 12, title: "Sam", username: "sam" }] };
      else if (url.endsWith("/collections"))
        data = {
          collections: [
            { id: "42", ratingKey: "6000000000000042", name: "Family favourites", itemCount: 8 },
          ],
        };
      else if (url.includes("/catalog/search"))
        data = {
          items: [
            {
              key: "1000000000000007",
              type: "movie",
              title: "Adventure",
              runtimeMinutes: 108,
              contentRating: "PG",
            },
          ],
        };
      return new Response(JSON.stringify(data), { status: 200 });
    }),
  );
  return calls;
}
const sent = (calls: Call[], method: string, re: RegExp) =>
  calls.filter((c) => re.test(c.url) && (c.init?.method ?? "GET") === method);
const body = (c: Call | undefined) => JSON.parse(String(c?.init?.body ?? "null"));

async function newBlankChannel(name: string) {
  fireEvent.click(await screen.findByRole("button", { name: "Add" }));
  fireEvent.click(await screen.findByRole("menuitem", { name: /Virtual channel/ }));
  fireEvent.click(await screen.findByRole("button", { name: "Blank channel template" }));
  fireEvent.change(await screen.findByLabelText("Name"), { target: { value: name } });
}

const iptv = (id: string, extra: Record<string, unknown> = {}) => ({
  id,
  number: "100",
  name: "News 24",
  kind: "iptv",
  enabled: true,
  favorite: false,
  sourceId: "src1",
  guideId: "",
  streamUrl: "https://iptv.example/news.m3u8",
  epoch: "1970-01-01T00:00:00Z",
  seed: 1,
  shuffle: false,
  items: [],
  ...extra,
});

describe("Live TV studio", () => {
  it("finds channels in the lineup and narrows to favourites", async () => {
    backend(
      Array.from({ length: 25 }, (_, i) => ({
        id: String(i),
        number: String(i + 1),
        name: `Station ${i + 1}`,
        kind: "iptv",
        enabled: true,
        favorite: i === 24,
        group: i === 24 ? "Weather" : "General",
      })),
    );
    renderLiveTV();
    const lineup = await screen.findByRole("grid", { name: "Programme guide" });
    await within(lineup).findByRole("button", { name: "1 Station 1" }, WAIT);
    fireEvent.change(screen.getByRole("searchbox", { name: "Search channels" }), {
      target: { value: "Weather" },
    });
    await waitFor(
      () => expect(within(lineup).queryByRole("button", { name: "1 Station 1" })).toBeNull(),
      WAIT,
    );
    expect(within(lineup).getByRole("button", { name: "25 Station 25" })).toBeInTheDocument();
    fireEvent.change(screen.getByRole("searchbox", { name: "Search channels" }), {
      target: { value: "" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Show" }));
    fireEvent.click(screen.getByRole("menuitemradio", { name: /Favourites/ }));
    await waitFor(
      () => expect(within(lineup).queryByRole("button", { name: "1 Station 1" })).toBeNull(),
      WAIT,
    );
    expect(within(lineup).getByRole("button", { name: "25 Station 25" })).toBeInTheDocument();
  });

  it("builds a virtual channel from a catalogue search and named viewer", async () => {
    const calls = backend([], (url, init) =>
      url.endsWith("/livetv/channels") && init?.method === "POST"
        ? { ...JSON.parse(String(init.body)), id: "v1" }
        : undefined,
    );
    renderLiveTV();
    await newBlankChannel("Movie night");
    fireEvent.click(screen.getAllByRole("button", { name: "Add" }).at(-1)!);
    fireEvent.click(await screen.findByRole("button", { name: "Programme block" }));
    fireEvent.change(await screen.findByRole("textbox", { name: "Block name" }), {
      target: { value: "Movie night" },
    });

    fireEvent.change(screen.getByLabelText("Search titles"), { target: { value: "Adventure" } });
    fireEvent.click(await screen.findByRole("button", { name: /Adventure/ }, WAIT));
    expect(await screen.findByRole("button", { name: "Remove Adventure" })).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Apply" }));
    fireEvent.click(screen.getByRole("tab", { name: "Viewers" }));
    // Sam has no Live TV limits, so a new channel reaches them until switched off.
    const sam = await screen.findByRole("switch", { name: "Sam" });
    expect(sam).toBeChecked();
    fireEvent.click(sam);
    fireEvent.click(screen.getByRole("button", { name: /Create channel/ }));
    await waitFor(() =>
      expect(sent(calls, "PUT", /\/livetv\/channels\/v1\/viewers$/)).toHaveLength(1),
    );
    const created = body(sent(calls, "POST", /\/livetv\/channels$/)[0]);
    expect(created).toMatchObject({
      name: "Movie night",
      number: "1",
      blocks: [
        {
          sources: [
            {
              items: [
                { key: 1000000000000007, title: "Adventure", durationMs: 6480000, rating: "PG" },
              ],
            },
          ],
        },
      ],
    });
    expect(created).not.toHaveProperty("allowedUsers");
    expect(body(sent(calls, "PUT", /\/livetv\/channels\/v1\/viewers$/)[0])).toEqual({
      allow: [],
      deny: [12],
    });
  });

  it("shows and edits each viewer's allowed channels from the channel's Viewers tab", async () => {
    const calls = backend([iptv("c1")], (url) => {
      if (url.endsWith("/users"))
        return {
          users: [
            { id: 1, title: "Owner", username: "owner", isOwner: true },
            { id: 12, title: "Sam", username: "sam" },
            { id: 13, title: "Kim", username: "kim" },
            { id: 14, title: "Lee", username: "lee" },
          ],
        };
      if (url.endsWith("/livetv/policies"))
        return [
          { userId: 13, disabled: false, restricted: true, channels: ["c9"] },
          { userId: 14, disabled: true, restricted: false },
        ];
      return undefined;
    });
    renderLiveTV("/livetv?channel=c1");
    fireEvent.click(await screen.findByRole("tab", { name: "Viewers" }, WAIT));
    expect(await screen.findByRole("switch", { name: "Sam" }, WAIT)).toBeChecked();
    expect(screen.getByRole("switch", { name: "Kim" })).not.toBeChecked();
    expect(screen.getByRole("switch", { name: "Lee" })).toBeDisabled();
    expect(screen.getByText("Live TV is off")).toBeInTheDocument();
    expect(screen.queryByRole("switch", { name: "Owner" })).toBeNull();
    expect(screen.getByText("Only 1 viewer")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Allow everyone" }));
    expect(screen.getByRole("switch", { name: "Kim" })).toBeChecked();
    expect(screen.getByText("Everyone with Live TV access")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: /Save channel/ }));
    await waitFor(() =>
      expect(sent(calls, "PUT", /\/livetv\/channels\/c1\/viewers$/)).toHaveLength(1),
    );
    expect(body(sent(calls, "PUT", /\/livetv\/channels\/c1\/viewers$/)[0])).toEqual({
      allow: [13],
      deny: [],
    });
    expect(body(sent(calls, "PUT", /\/livetv\/channels\/c1$/)[0])).not.toHaveProperty(
      "allowedUsers",
    );
  });

  it("makes a channel private and gives it to chosen viewers", async () => {
    const calls = backend([iptv("c1")], (url) => {
      if (url.endsWith("/users"))
        return {
          users: [
            { id: 12, title: "Sam", username: "sam" },
            { id: 13, title: "Kim", username: "kim" },
          ],
        };
      if (url.endsWith("/livetv/policies"))
        return [{ userId: 13, disabled: false, restricted: false, granted: [] }];
      return undefined;
    });
    renderLiveTV("/livetv?channel=c1");
    fireEvent.click(await screen.findByRole("tab", { name: "Viewers" }, WAIT));
    expect(await screen.findByRole("switch", { name: "Sam" }, WAIT)).toBeChecked();
    fireEvent.click(screen.getByRole("switch", { name: "Private" }));
    // Private hides it from everyone not given it.
    expect(screen.getByRole("switch", { name: "Sam" })).not.toBeChecked();
    expect(screen.getByRole("switch", { name: "Kim" })).not.toBeChecked();
    fireEvent.click(screen.getByRole("switch", { name: "Kim" }));
    fireEvent.click(screen.getByRole("button", { name: /Save channel/ }));
    await waitFor(() =>
      expect(sent(calls, "PUT", /\/livetv\/channels\/c1\/viewers$/)).toHaveLength(1),
    );
    expect(body(sent(calls, "PUT", /\/livetv\/channels\/c1$/)[0]).private).toBe(true);
    expect(body(sent(calls, "PUT", /\/livetv\/channels\/c1\/viewers$/)[0])).toEqual({
      allow: [13],
      deny: [],
    });
  });

  it("programmes a named block from a collection chosen by name", async () => {
    const calls = backend();
    renderLiveTV();
    await waitFor(() => expect(calls.some((c) => c.url.endsWith("/collections"))).toBe(true));
    await newBlankChannel("Family TV");
    fireEvent.click(screen.getAllByRole("button", { name: "Add" }).at(-1)!);
    fireEvent.click(await screen.findByRole("button", { name: "Programme block" }));
    fireEvent.change(await screen.findByRole("textbox", { name: "Block name" }), {
      target: { value: "Family afternoons" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Source type" }));
    fireEvent.click(await screen.findByRole("option", { name: "Collection" }));
    fireEvent.click(screen.getByRole("button", { name: "Collection" }));
    fireEvent.click(await screen.findByRole("option", { name: "Family favourites" }));
    fireEvent.click(screen.getByRole("button", { name: "Apply" }));
    fireEvent.click(screen.getByRole("button", { name: /Create channel/ }));
    await waitFor(() => expect(sent(calls, "POST", /\/livetv\/channels$/)).toHaveLength(1));
    expect(
      body(sent(calls, "POST", /\/livetv\/channels$/)[0]).blocks[0].sources[0].collection,
    ).toBe(6000000000000042);
  });

  it("edits a schedule block in a drawer and previews its applied programming", async () => {
    const calls = backend();
    renderLiveTV("/livetv?channel=new");
    fireEvent.click(await screen.findByRole("button", { name: "80s Action 24/7 template" }));
    fireEvent.click(screen.getByRole("tab", { name: /Schedule/ }));
    fireEvent.click(screen.getByRole("gridcell", { name: "Monday at 00:00" }));
    const dialog = await screen.findByRole("dialog", { name: "Programme block" });
    fireEvent.change(within(dialog).getByRole("textbox", { name: "Block name" }), {
      target: { value: "Morning movies" },
    });
    // Add an explicit source: a grid block begins as an empty selected-title list.
    fireEvent.click(within(dialog).getByRole("button", { name: "Add source" }));
    fireEvent.change(within(dialog).getByLabelText("Search titles"), {
      target: { value: "Adventure" },
    });
    fireEvent.click(await within(dialog).findByRole("button", { name: /Adventure/ }, WAIT));
    fireEvent.click(within(dialog).getByRole("button", { name: "Apply" }));
    await waitFor(() => {
      const previews = sent(calls, "POST", /\/livetv\/preview/);
      expect(
        previews.some(
          (c) => body(c).blocks?.[0]?.sources?.[0]?.items?.[0]?.key === 1000000000000007,
        ),
      ).toBe(true);
    }, WAIT);
  });

  it("maps a channel without listings from the Guide tab's drawer", async () => {
    const calls = backend([iptv("c1")], (url) => {
      if (url.endsWith("/livetv/guide/coverage")) return [{ channelId: "c1", programmes: 0 }];
      if (url.includes("/livetv/guide/channels"))
        return [
          {
            sourceId: "epg1",
            sourceName: "Regional listings",
            guideId: "news24.regional",
            name: "News 24 HD",
            programmes: 96,
          },
        ];
      return undefined;
    });
    renderLiveTV("/livetv/guide-data");
    expect(await screen.findByText(/1 of 1 need a guide/, {}, WAIT)).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: /Map guide for News 24/ }));
    fireEvent.click(await screen.findByRole("button", { name: /News 24 HD/ }, WAIT));
    fireEvent.click(await screen.findByRole("button", { name: /Use this guide/ }, WAIT));
    await waitFor(() => expect(sent(calls, "PUT", /\/livetv\/channels\/c1$/)).toHaveLength(1));
    expect(body(sent(calls, "PUT", /\/livetv\/channels\/c1$/)[0])).toMatchObject({
      guideSourceId: "epg1",
      guideId: "news24.regional",
    });
  });

  it("proposes guide matches and applies only the accepted ones", async () => {
    const calls = backend(
      [iptv("c1"), iptv("c2", { number: "101", name: "Cartoons" })],
      (url, init) => {
        if (url.endsWith("/livetv/guide/coverage"))
          return [
            { channelId: "c1", programmes: 0 },
            { channelId: "c2", programmes: 0 },
          ];
        if (url.endsWith("/livetv/guide/match")) {
          const b = JSON.parse(String(init?.body ?? "{}"));
          return {
            matched: b.dryRun ? 2 : 1,
            unmatched: 0,
            changes: [
              {
                channelId: "c1",
                guideSourceId: "epg1",
                guideId: "n24",
                guideName: "News 24 HD",
                method: "name",
                reviewRegion: true,
              },
              {
                channelId: "c2",
                guideSourceId: "epg1",
                guideId: "toons",
                guideName: "Toons",
                method: "id",
              },
            ],
          };
        }
        return undefined;
      },
    );
    renderLiveTV("/livetv/guide-data");
    fireEvent.click(await screen.findByRole("button", { name: /Auto-match/ }, WAIT));
    const uncertain = await screen.findByLabelText("Accept match for News 24", {}, WAIT);
    expect(uncertain).not.toBeChecked();
    expect(screen.getByText("Check region")).toBeVisible();
    expect(screen.getByLabelText("Accept match for Cartoons")).toBeChecked();
    fireEvent.click(uncertain);
    fireEvent.click(await screen.findByLabelText("Accept match for Cartoons", {}, WAIT));
    fireEvent.click(await screen.findByRole("button", { name: /Apply 1/ }, WAIT));
    await waitFor(() => expect(sent(calls, "POST", /\/livetv\/guide\/match$/)).toHaveLength(2));
    const [dry, apply] = sent(calls, "POST", /\/livetv\/guide\/match$/);
    expect(body(dry)).toEqual({ dryRun: true });
    expect(body(apply)).toEqual({ channelIds: ["c1"] });
  });

  it("imports a guide-only XMLTV source", async () => {
    const calls = backend();
    renderLiveTV();
    fireEvent.click(await screen.findByRole("button", { name: "Add" }));
    fireEvent.click(await screen.findByRole("menuitem", { name: /Guide source/ }));
    fireEvent.change(await screen.findByLabelText("Source name"), {
      target: { value: "Regional listings" },
    });
    fireEvent.change(screen.getByLabelText("XMLTV guide"), {
      target: { value: "https://epg.example/regional.xml.gz" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Import source" }));
    await waitFor(() => expect(sent(calls, "POST", /\/livetv\/sources(\?|$)/)).toHaveLength(1));
    expect(body(sent(calls, "POST", /\/livetv\/sources(\?|$)/)[0])).toMatchObject({
      name: "Regional listings",
      guideUrl: "https://epg.example/regional.xml.gz",
      playlistUrl: "",
      playlist: "",
    });
  });

  it("orders a primary and backup stream source", async () => {
    const calls = backend([iptv("c1")]);
    renderLiveTV("/livetv?channel=c1");
    fireEvent.click(await screen.findByRole("button", { name: /Add backup/ }, WAIT));
    fireEvent.change(screen.getByLabelText("Source 2 URL"), {
      target: { value: "https://backup.example/news.m3u8" },
    });
    fireEvent.click(screen.getByRole("button", { name: /Save channel/ }));
    await waitFor(() => expect(sent(calls, "PUT", /\/livetv\/channels\/c1$/)).toHaveLength(1));
    const saved = body(sent(calls, "PUT", /\/livetv\/channels\/c1$/)[0]);
    expect(saved.streamUrl).toBe("https://iptv.example/news.m3u8");
    expect(saved.streamSources).toMatchObject([
      { url: "https://iptv.example/news.m3u8", enabled: true, priority: 0 },
      { url: "https://backup.example/news.m3u8", enabled: true, priority: 1 },
    ]);
  });

  it("rebinds an open channel drawer to another channel and names that channel on delete", async () => {
    const channels: Record<string, unknown>[] = [iptv("news", { number: "2", name: "News 24" })];
    const calls = backend(channels, (url, init) => {
      const method = init?.method ?? "GET";
      if (url.endsWith("/livetv/channels") && method === "POST") {
        const saved = JSON.parse(String(init?.body ?? "{}")) as Record<string, unknown>;
        saved.id = "created";
        channels.push(saved);
        return saved;
      }
      return undefined;
    });
    renderLiveTV();
    await newBlankChannel("Movie night");
    fireEvent.click(screen.getAllByRole("button", { name: "Add" }).at(-1)!);
    fireEvent.click(await screen.findByRole("button", { name: "Programme block" }));
    fireEvent.change(await screen.findByRole("textbox", { name: "Block name" }), {
      target: { value: "Movie night" },
    });

    fireEvent.change(screen.getByLabelText("Search titles"), { target: { value: "Adventure" } });
    fireEvent.click(await screen.findByRole("button", { name: /Adventure/ }, WAIT));
    fireEvent.click(screen.getByRole("button", { name: "Apply" }));
    fireEvent.click(screen.getByRole("button", { name: /Create channel/ }));
    await waitFor(() => expect(sent(calls, "POST", /\/livetv\/channels$/)).toHaveLength(1));
    expect(await screen.findByLabelText("Name")).toHaveValue("Movie night");
    const lineup = screen.getByRole("grid", { name: "Programme guide" });
    fireEvent.click(within(lineup).getByRole("button", { name: "2 News 24" }));
    expect(await screen.findByLabelText("Name")).toHaveValue("News 24");
    fireEvent.click(screen.getByRole("tab", { name: "Details" }));
    fireEvent.click(screen.getByRole("button", { name: "Delete News 24" }));
    const heading = await screen.findByRole("heading", { name: "Delete News 24?" });
    fireEvent.click(
      within(heading.closest('[role="dialog"]') as HTMLElement).getByRole("button", {
        name: "Delete News 24",
      }),
    );
    await waitFor(() => expect(sent(calls, "DELETE", /\/livetv\/channels\/news$/)).toHaveLength(1));
    expect(sent(calls, "DELETE", /\/livetv\/channels\/created$/)).toHaveLength(0);
  });
});

describe("Live TV preview player", () => {
  it("tunes the saved channel in a mini player and stops the session on close", async () => {
    const user = userEvent.setup();
    vi.spyOn(HTMLMediaElement.prototype, "play").mockResolvedValue(undefined);
    const calls = backend([iptv("c1")], (url, init) => {
      if (url.endsWith("/livetv/channels/c1/play") && init?.method === "POST")
        return { id: "s1", playlist: "sessions/s1/index.m3u8", activeSourceName: "Samsung" };
      return undefined;
    });
    renderLiveTV("/livetv?channel=c1");
    await user.click(await screen.findByRole("button", { name: "Watch" }, WAIT));
    const player = await screen.findByRole("dialog", { name: "Watching News 24" });
    await waitFor(() => expect(hlsLoads).toContain("/ether/api/livetv/sessions/s1/index.m3u8"));
    expect(await within(player).findByText("via Samsung")).toBeInTheDocument();
    await user.click(within(player).getByRole("button", { name: "Stop watching" }));
    await waitFor(() => expect(sent(calls, "DELETE", /\/livetv\/sessions\/s1$/)).toHaveLength(1));
    expect(screen.queryByRole("dialog", { name: "Watching News 24" })).toBeNull();
  });
});

describe("Live TV source sign-in", () => {
  it("imports an Xtream Codes source from a server and login", async () => {
    const calls = backend();
    renderLiveTV();
    fireEvent.click(await screen.findByRole("button", { name: "Add" }));
    fireEvent.click(await screen.findByRole("menuitem", { name: /IPTV source/ }));
    fireEvent.change(await screen.findByLabelText("Source name"), {
      target: { value: "My provider" },
    });
    fireEvent.click(screen.getByRole("radio", { name: /Xtream Codes login/ }));
    fireEvent.change(screen.getByLabelText("Server"), {
      target: { value: "http://panel.example:8080" },
    });
    fireEvent.change(screen.getByLabelText("Username"), { target: { value: "me" } });
    fireEvent.change(screen.getByLabelText("Password"), { target: { value: "pw" } });
    fireEvent.click(screen.getByRole("button", { name: "Import source" }));
    await waitFor(() => expect(sent(calls, "POST", /\/livetv\/sources(\?|$)/)).toHaveLength(1));
    expect(body(sent(calls, "POST", /\/livetv\/sources(\?|$)/)[0])).toMatchObject({
      name: "My provider",
      playlistUrl: "",
      auth: {
        type: "xtream",
        server: "http://panel.example:8080",
        username: "me",
        password: "pw",
        output: "ts",
      },
    });
  });

  it("keeps a stored password by sending the placeholder back", async () => {
    const calls = backend([], (url, init) => {
      if (url.endsWith("/livetv/sources") && (init?.method ?? "GET") === "GET")
        return [
          {
            id: "p1",
            name: "Protected",
            playlistUrl: "https://iptv.example/list.m3u",
            updatedAt: "2026-10-19T00:00:00Z",
            hasPlaylist: true,
            auth: { type: "basic", username: "owner", password: "••••", userAgent: "VLC/3.0" },
          },
        ];
      return undefined;
    });
    renderLiveTV("/livetv?source=p1");
    expect(await screen.findByText(/Saved. Leave as is to keep it/, {}, WAIT)).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Save and re-import" }));
    await waitFor(() => expect(sent(calls, "PUT", /\/livetv\/sources\/p1(\?|$)/)).toHaveLength(1));
    expect(body(sent(calls, "PUT", /\/livetv\/sources\/p1(\?|$)/)[0]).auth).toEqual({
      type: "basic",
      username: "owner",
      password: "••••",
      userAgent: "VLC/3.0",
    });
  });

  it("edits an Xtream lineup's channel groups and sends them once on save", async () => {
    const calls = backend([], (url, init) => {
      if (url.endsWith("/livetv/sources") && (init?.method ?? "GET") === "GET")
        return [
          {
            id: "x1",
            name: "Big panel",
            updatedAt: "2026-10-19T00:00:00Z",
            hasPlaylist: true,
            channelCount: 130,
            auth: {
              type: "xtream",
              server: "http://panel.example",
              username: "me",
              password: "••••",
            },
            account: {
              status: "Active",
              trial: true,
              expiresAt: new Date(Date.now() + 5 * 3600e3).toISOString(),
              maxConnections: 1,
              activeConnections: 0,
              checkedAt: "2026-10-19T00:00:00Z",
            },
            groups: [
              { name: "EVENTS", channels: 20 },
              { name: "UK | News", channels: 100 },
              { name: "Adult", channels: 10, adult: true, excluded: true },
            ],
            excludeGroups: ["Adult"],
          },
        ];
      return undefined;
    });
    renderLiveTV("/livetv?source=x1");
    expect(await screen.findByText("Allows 1 connection", {}, WAIT)).toBeInTheDocument();
    expect(screen.getAllByText("Trial").length).toBeGreaterThan(0);
    fireEvent.click(screen.getByRole("tab", { name: /Channel groups/ }));
    expect(screen.getByText("120 channels will be imported")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("checkbox", { name: /EVENTS/ }));
    expect(screen.getByText("100 channels will be imported")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("switch", { name: "Include new groups automatically" }));
    fireEvent.click(screen.getByRole("radio", { name: "My order" }));
    fireEvent.click(screen.getByRole("button", { name: "Save and re-import" }));
    await waitFor(() =>
      expect(sent(calls, "PUT", /\/livetv\/sources\/x1(?:\?|$)/)).toHaveLength(1),
    );
    expect(body(sent(calls, "PUT", /\/livetv\/sources\/x1(?:\?|$)/)[0])).toMatchObject({
      excludeGroups: ["Adult", "EVENTS"],
      newGroups: "exclude",
      hideAdult: false,
      groupOrder: ["UK | News"],
    });
  });

  it("lets a backup input use another source's login", async () => {
    const calls = backend(
      [
        iptv("c1", {
          streamSources: [
            {
              id: "imp",
              name: "Primary",
              url: "https://a.example/1.m3u8",
              enabled: true,
              priority: 0,
            },
            {
              id: "bk",
              name: "Backup",
              url: "https://b.example/1.m3u8",
              enabled: true,
              priority: 1,
            },
          ],
        }),
      ],
      (url, init) => {
        if (url.endsWith("/livetv/sources") && (init?.method ?? "GET") === "GET")
          return [
            {
              id: "b",
              name: "Provider B",
              updatedAt: "2026-10-19T00:00:00Z",
              auth: { type: "bearer", token: "••••" },
            },
          ];
        return undefined;
      },
    );
    renderLiveTV("/livetv?channel=c1");
    fireEvent.click(await screen.findByRole("button", { name: "Login for source 2" }, WAIT));
    fireEvent.click(screen.getByRole("menuitemradio", { name: /Provider B/ }));
    fireEvent.click(screen.getByRole("button", { name: /Save channel/ }));
    await waitFor(() => expect(sent(calls, "PUT", /\/livetv\/channels\/c1$/)).toHaveLength(1));
    const saved = body(sent(calls, "PUT", /\/livetv\/channels\/c1$/)[0]);
    expect(saved.streamSources[0].authSourceId).toBeUndefined();
    expect(saved.streamSources[1]).toMatchObject({ id: "bk", authSourceId: "b" });
  });
});

describe("Live TV Stremio plugins", () => {
  const kind = "plugin:addon.abc/channels";
  const addonDescriptor = {
    kind: "liveTVSource",
    id: kind,
    owner: "addon.abc",
    label: { resolved: "Sports TV" },
    hint: { source: "Live channels from a Stremio plugin." },
    meta: { editor: "addon", yieldsChannels: true, yieldsGuide: true },
  };

  it("shows a plugin source's plugin and Plugins link, never a link field", async () => {
    invalidateRegistry();
    backend([], (url, init) => {
      if (url.includes("/extend/registry")) return { version: 1, descriptors: [addonDescriptor] };
      if (url.endsWith("/livetv/sources") && (init?.method ?? "GET") === "GET")
        return [
          {
            id: "a1",
            name: "My sports",
            kind,
            playlistUrl: kind,
            hasPlaylist: true,
            updatedAt: "2026-10-19T00:00:00Z",
          },
        ];
      return undefined;
    });
    renderLiveTV("/livetv?source=a1");
    expect(await screen.findByText("Channels come from Sports TV.", {}, WAIT)).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Manage in Plugins" })).toHaveAttribute(
      "href",
      "/plugins/installed",
    );
    expect(screen.queryByLabelText("M3U playlist")).not.toBeInTheDocument();
    expect(screen.queryByText("Sign-in")).not.toBeInTheDocument();
  });

  it("adds a plugin through the shared drawer, then imports its channels", async () => {
    invalidateRegistry();
    let sources: unknown[] = [];
    const calls = backend([], (url, init) => {
      const method = init?.method ?? "GET";
      if (url.includes("/extend/registry")) return { version: 1, descriptors: [addonDescriptor] };
      if (url.endsWith("/plugins/addons/preview"))
        return {
          id: "addon.abc",
          name: "Sports TV",
          host: "tv.example",
          roles: [{ role: "livetv_channels", on: false }],
        };
      if (url.endsWith("/plugins/addons") && method === "POST")
        return {
          id: "addon.abc",
          name: "Sports TV",
          version: "1.0.0",
          enabled: true,
          stremioOnly: true,
          roles: [{ role: "livetv_channels", on: true }],
        };
      if (url.endsWith("/livetv/sources") && method === "POST") {
        const saved = { ...body({ url, init }), id: "a1", updatedAt: "2026-10-19T00:00:00Z" };
        sources = [saved];
        return saved;
      }
      if (url.endsWith("/livetv/sources") && method === "GET") return sources;
      return undefined;
    });
    renderLiveTV("/livetv");
    fireEvent.click(await screen.findByRole("button", { name: "Add" }));
    fireEvent.click(await screen.findByRole("menuitem", { name: /Stremio plugin/ }));
    fireEvent.change(await screen.findByPlaceholderText("https://… /manifest.json"), {
      target: { value: "https://tv.example/cfg/manifest.json" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Continue" }));
    fireEvent.click(await screen.findByRole("button", { name: "Add plugin" }, WAIT));
    await waitFor(() => expect(sent(calls, "POST", /\/livetv\/sources$/)).toHaveLength(1), WAIT);
    expect(body(sent(calls, "POST", /\/livetv\/sources$/)[0])).toMatchObject({
      name: "Sports TV",
      kind,
      playlistUrl: kind,
    });
    expect(body(sent(calls, "POST", /\/plugins\/addons$/)[0])).toEqual({
      url: "https://tv.example/cfg/manifest.json",
      roles: ["livetv_channels"],
    });
    expect(await screen.findByText("Channels come from Sports TV.", {}, WAIT)).toBeInTheDocument();
  });
});

describe("Live TV access on the viewer profile", () => {
  it(
    "only switches Live TV on or off for the viewer; channels are managed on each channel",
    { timeout: 30000 },
    async () => {
      let policy = {
        userId: 2,
        disabled: false,
        restricted: true,
        channels: ["c1"],
        favorites: [],
        maxRating: "",
      };
      const { fake } = renderAppWith("/viewers/2/limits", (method, path, body) => {
        if (path === "/livetv/channels")
          return [
            { id: "c1", number: "1", name: "Movie night", kind: "virtual", enabled: true },
            { id: "c2", number: "2", name: "Cartoons", kind: "virtual", enabled: true },
          ];
        if (path === "/livetv/policies/2") {
          if (method === "PUT") policy = body as typeof policy;
          return policy;
        }
        return undefined;
      });
      // First visit to the lazily loaded viewer profile route: allow a cold chunk load.
      fireEvent.click(
        await screen.findByLabelText("Disable Live TV for this viewer", {}, { timeout: 15000 }),
      );
      // Thousands of channels never list here; access lives on each channel.
      expect(screen.queryByLabelText("Allow Movie night")).not.toBeInTheDocument();
      expect(screen.queryByText("Allowed channels")).not.toBeInTheDocument();
      fireEvent.click(await screen.findByRole("button", { name: "Save changes" }));
      await waitFor(() =>
        expect(fakeCalls(fake, "PUT", "/livetv/policies/2").at(-1)?.body).toMatchObject({
          disabled: true,
          restricted: true,
          channels: ["c1"],
        }),
      );
    },
  );
});

it("bulk selects matching channels beyond the loaded page without fetching full rows", async () => {
  const channels = Array.from({ length: 450 }, (_, i) =>
    iptv(`paged-${i}`, {
      number: String(i + 1),
      name: `Paged ${i + 1}`,
      enabled: i !== 449,
    }),
  );
  const calls = backend(channels);
  renderLiveTV();
  await screen.findByRole("button", { name: "1 Paged 1" }, WAIT);
  fireEvent.click(screen.getByRole("checkbox", { name: "Select all matches" }));
  fireEvent.click(await screen.findByRole("button", { name: /^Favourite$/ }));
  await waitFor(() => expect(sent(calls, "PATCH", /\/livetv\/channels\/bulk$/)).toHaveLength(1));
  const edit = body(sent(calls, "PATCH", /\/livetv\/channels\/bulk$/)[0]);
  expect(edit.ids).toHaveLength(449);
  expect(edit.ids).toContain("paged-448");
  expect(edit.ids).not.toContain("paged-449");
  expect(edit.favorite).toBe(true);
  expect(sent(calls, "GET", /\/livetv\/channels$/)).toHaveLength(0);
  expect(
    calls
      .filter((c) => c.url.includes("/livetv/channels/page"))
      .every((c) => new URL(c.url, "http://local").searchParams.get("offset") === "0"),
  ).toBe(true);
});

it("keeps placeholder rows aligned while a deep page is loading", async () => {
  const channels = Array.from({ length: 450 }, (_, i) =>
    iptv(`sparse-${i}`, { number: String(i + 1), name: `Sparse ${i + 1}` }),
  );
  let release: (() => void) | undefined;
  backend(channels, (url) => {
    if (
      url.includes("/livetv/channels/page") &&
      new URL(url, "http://local").searchParams.get("offset") === "200"
    )
      return new Promise((resolve) => {
        release = () => resolve(channelPage(channels, url));
      });
    return undefined;
  });
  renderLiveTV();
  await screen.findByRole("button", { name: "1 Sparse 1" }, WAIT);
  const grid = screen.getByRole("grid", { name: "Programme guide" });
  fireEvent.scroll(grid, { target: { scrollTop: 21000 } });
  await waitFor(() =>
    expect(grid.querySelectorAll('[role="row"][aria-busy="true"]').length).toBeGreaterThan(0),
  );
  await waitFor(() => expect(release).toBeDefined());
  release!();
  await within(grid).findByRole("button", { name: "351 Sparse 351" }, WAIT);
  await waitFor(() =>
    expect(grid.querySelectorAll('[role="row"][aria-busy="true"]').length).toBe(0),
  );
});

it("uses saved source state when a different cluster node has no import progress", async () => {
  const source = {
    id: "src1",
    name: "Shared source",
    hasPlaylist: true,
    playlistUrl: "https://example.invalid/list.m3u",
    channelCount: 17,
  };
  let finish!: (v: unknown) => void;
  const importing = new Promise((resolve) => {
    finish = resolve;
  });
  const calls = backend([iptv("c1")], (url, init) => {
    if (url.endsWith("/livetv/sources")) return [source];
    if (url.includes("/livetv/sources/progress/")) return null;
    if (url.includes("/livetv/sources/src1?") && init?.method === "PUT") return importing;
    return undefined;
  });
  renderLiveTV("/livetv?source=src1");
  const save = await screen.findByRole("button", { name: "Save and re-import" }, WAIT);
  await waitFor(() => expect(save).toBeEnabled());
  fireEvent.click(save);
  await waitFor(() => expect(sent(calls, "PUT", /sources/)).toHaveLength(1));
  expect(
    await screen.findByText(
      "Import in progress. Detailed progress is unavailable on this server.",
      {},
      WAIT,
    ),
  ).toBeVisible();
  expect(await screen.findByText("17 channels in the last saved import", {}, WAIT)).toBeVisible();
  expect(sent(calls, "GET", /\/livetv\/sources$/).length).toBeGreaterThan(1);
  finish(source);
  await waitFor(() =>
    expect(
      screen.queryByText("Import in progress. Detailed progress is unavailable on this server."),
    ).toBeNull(),
  );
});

it("estimates AI cost before running and applies only reviewed proposals", async () => {
  const calls = backend(
    [iptv("c1"), iptv("c2", { name: "Cartoons", number: "102" })],
    (url, init) => {
      if (url.endsWith("/livetv/guide/coverage"))
        return [
          { channelId: "c1", programmes: 0 },
          { channelId: "c2", programmes: 0 },
        ];
      if (!url.endsWith("/livetv/guide/match")) return undefined;
      const b = JSON.parse(String(init?.body ?? "{}"));
      return {
        matched: 2,
        unmatched: 0,
        ai: {
          configured: true,
          estimatedCalls: 1,
          inputTokens: 1200,
          cached: 0,
          calls: b.ai === "propose" ? 1 : 0,
          deferred: 0,
        },
        changes:
          b.ai === "estimate"
            ? []
            : [
                {
                  channelId: "c1",
                  guideSourceId: "epg1",
                  guideId: "news",
                  guideName: "News 24",
                  method: "ai",
                  confidence: 0.98,
                  reason: "Same station and region",
                  proposalKey: "key-1",
                },
                {
                  channelId: "c2",
                  guideSourceId: "epg1",
                  guideId: "cartoons",
                  guideName: "Cartoons",
                  method: "ai",
                  confidence: 0.7,
                  reason: "Region uncertain",
                  proposalKey: "key-2",
                },
              ],
      };
    },
  );
  renderLiveTV("/livetv/guide-data");
  fireEvent.click(await screen.findByRole("button", { name: "Match with AI" }, WAIT));
  expect(await screen.findByText(/Estimated calls: 1/)).toBeVisible();
  expect(calls.filter((c) => c.url.endsWith("/guide/match"))).toHaveLength(1);
  fireEvent.click(screen.getByRole("button", { name: "Find suggestions" }));
  expect(await screen.findByRole("button", { name: "Accept match for News 24" })).toHaveAttribute(
    "aria-pressed",
    "true",
  );
  expect(screen.getByRole("button", { name: "Accept match for Cartoons" })).toHaveAttribute(
    "aria-pressed",
    "false",
  );
  expect(screen.getByText("70% confidence")).toBeVisible();
  fireEvent.click(screen.getByRole("button", { name: "Apply 1" }));
  await waitFor(() => expect(calls.filter((c) => c.url.endsWith("/guide/match"))).toHaveLength(3));
  const matchCalls = calls.filter((c) => c.url.endsWith("/guide/match"));
  expect(JSON.parse(String(matchCalls[2]?.init?.body))).toMatchObject({
    ai: "commit",
    selections: [{ channelId: "c1", guideSourceId: "epg1", guideId: "news", proposalKey: "key-1" }],
  });
});

describe("timeline snapshot boundary", () => {
  const range = { from: Date.UTC(2026, 10, 5), to: Date.UTC(2026, 10, 6) };
  it.each([
    null,
    {},
    [],
    {
      timezone: "UTC",
      dayparts: [],
      elements: [
        { id: "bad", title: "Bad", rule: "", kind: "pod", start: 1, end: 2, children: {} },
      ],
    },
  ])("renders a safe error for malformed snapshots %j", async (snapshot) => {
    backend([], (url) => (url.includes("/timeline?") ? snapshot : undefined));
    render(<ScheduleTimeline channelId="test" range={range} />);
    expect(await screen.findByText("Unable to load schedule")).toBeVisible();
  });
  it("retries a cold snapshot and renders it when ready", async () => {
    let attempts = 0;
    backend([], (url) => {
      if (!url.includes("/timeline?")) return undefined;
      if (attempts++ === 0)
        return new Response(JSON.stringify({ error: { code: "warming", message: "warming" } }), {
          status: 503,
          headers: { "Retry-After": "1" },
        });
      return { channelId: "test", timezone: "UTC", now: range.from, elements: [], dayparts: [] };
    });
    render(<ScheduleTimeline channelId="test" range={range} />);
    await waitFor(() => expect(attempts).toBe(2), { timeout: 3500 });
    expect(screen.queryByText("Unable to load schedule")).not.toBeInTheDocument();
  });
});
