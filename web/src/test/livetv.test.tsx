import { ConfirmRoot } from "@braedonsaunders/appkit-ui";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";
import LiveTV from "../pages/livetv/LiveTV";
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
      let data: unknown = extra(url, init);
      if (data !== undefined) return new Response(JSON.stringify(data), { status: 200 });
      data = [];
      if (url.endsWith("/livetv/channels") && (init?.method ?? "GET") === "GET") data = channels;
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
    const lineup = await screen.findByRole("complementary", { name: "Lineup" });
    await within(lineup).findByRole("button", { name: "1 Station 1" }, WAIT);
    fireEvent.change(screen.getByRole("searchbox", { name: "Search channels" }), {
      target: { value: "Weather" },
    });
    expect(within(lineup).getByRole("button", { name: "25 Station 25" })).toBeInTheDocument();
    expect(within(lineup).queryByRole("button", { name: "1 Station 1" })).toBeNull();
    fireEvent.change(screen.getByRole("searchbox", { name: "Search channels" }), {
      target: { value: "" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Show" }));
    fireEvent.click(screen.getByRole("menuitemradio", { name: /Favourites/ }));
    expect(within(lineup).getByRole("button", { name: "25 Station 25" })).toBeInTheDocument();
    expect(within(lineup).queryByRole("button", { name: "1 Station 1" })).toBeNull();
  });

  it("builds a virtual channel from a catalogue search and named viewer", async () => {
    const calls = backend();
    renderLiveTV();
    await newBlankChannel("Movie night");
    fireEvent.change(screen.getByLabelText("Search titles"), { target: { value: "Adventure" } });
    fireEvent.click(await screen.findByRole("button", { name: /Adventure/ }, WAIT));
    expect(await screen.findByRole("button", { name: "Remove Adventure" })).toBeInTheDocument();
    fireEvent.click(screen.getByRole("tab", { name: "Viewers" }));
    fireEvent.click(await screen.findByRole("switch", { name: "Sam" }));
    fireEvent.click(screen.getByRole("button", { name: /Create channel/ }));
    await waitFor(() => expect(sent(calls, "POST", /\/livetv\/channels$/)).toHaveLength(1));
    expect(body(sent(calls, "POST", /\/livetv\/channels$/)[0])).toMatchObject({
      name: "Movie night",
      number: "1",
      allowedUsers: [12],
      items: [{ key: 1000000000000007, title: "Adventure", durationMs: 6480000, rating: "PG" }],
    });
  });

  it("programmes a channel from a collection chosen by name", async () => {
    const calls = backend();
    renderLiveTV();
    await waitFor(() => expect(calls.some((c) => c.url.endsWith("/collections"))).toBe(true));
    await newBlankChannel("Family TV");
    fireEvent.click(screen.getByRole("radio", { name: /^Collection/ }));
    fireEvent.click(screen.getByRole("button", { name: "Collection" }));
    fireEvent.click(await screen.findByRole("option", { name: "Family favourites · 8 titles" }));
    fireEvent.click(screen.getByRole("button", { name: /Create channel/ }));
    await waitFor(() => expect(sent(calls, "POST", /\/livetv\/channels$/)).toHaveLength(1));
    expect(body(sent(calls, "POST", /\/livetv\/channels$/)[0]).collection).toBe(6000000000000042);
  });

  it("explains an empty time block and previews the schedule live once it's valid", async () => {
    const calls = backend();
    renderLiveTV("/livetv?channel=new");
    fireEvent.click(await screen.findByRole("button", { name: "80s Action 24/7 template" }));
    fireEvent.click(screen.getByRole("tab", { name: /Schedule/ }));
    fireEvent.click(screen.getByRole("gridcell", { name: "Monday at 00:00" }));
    expect(
      await screen.findByText(/Add at least one movie to each time block/),
    ).toBeInTheDocument();
    const block = screen.getByRole("group", { name: "Time block 1 programming" });
    fireEvent.change(within(block).getByLabelText("Search titles"), {
      target: { value: "Adventure" },
    });
    fireEvent.click(await within(block).findByRole("button", { name: /Adventure/ }, WAIT));
    await within(block).findByRole("button", { name: "Remove Adventure" });
    await waitFor(() => {
      const previews = sent(calls, "POST", /\/livetv\/preview/);
      expect(previews.length).toBeGreaterThan(0);
      expect(body(previews.at(-1))).toMatchObject({
        filter: { yearFrom: 1980, yearTo: 1989, genres: ["Action"] },
        blocks: [{ start: 0, end: 60, items: [{ key: 1000000000000007 }] }],
      });
    }, WAIT);
    // No preview was requested while the block was empty.
    expect(
      sent(calls, "POST", /\/livetv\/preview/).every((c) =>
        (body(c).blocks ?? []).every((b: { items: unknown[] }) => b.items.length > 0),
      ),
    ).toBe(true);
  });

  it("maps a channel without listings to a guide channel from Guide data", async () => {
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
    fireEvent.click(screen.getByRole("button", { name: "Map guide for News 24" }));
    fireEvent.click(await screen.findByRole("button", { name: /News 24 HD/ }, WAIT));
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
    const proposals = await screen.findByRole("region", { name: "Suggested matches" }, WAIT);
    fireEvent.click(within(proposals).getByLabelText("Accept match for Cartoons"));
    fireEvent.click(within(proposals).getByRole("button", { name: /Apply 1/ }));
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
    await waitFor(() => expect(sent(calls, "POST", /\/livetv\/sources$/)).toHaveLength(1));
    expect(body(sent(calls, "POST", /\/livetv\/sources$/)[0])).toMatchObject({
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

  it("rebinds an open channel drawer from the lineup and names that channel on delete", async () => {
    const channels: Record<string, unknown>[] = [iptv("news", { number: "2", name: "News 24" })];
    const calls = backend(channels, (url, init) => {
      const method = init?.method ?? "GET";
      if (url.endsWith("/livetv/channels") && method === "GET") return channels;
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
    fireEvent.change(screen.getByLabelText("Search titles"), { target: { value: "Adventure" } });
    fireEvent.click(await screen.findByRole("button", { name: /Adventure/ }, WAIT));
    fireEvent.click(screen.getByRole("button", { name: /Create channel/ }));
    await waitFor(() => expect(sent(calls, "POST", /\/livetv\/channels$/)).toHaveLength(1));
    expect(await screen.findByLabelText("Name")).toHaveValue("Movie night");
    const lineup = screen.getByRole("complementary", { name: "Lineup" });
    expect(lineup.className).toContain("lg:z-[60]");
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
    await waitFor(() => expect(sent(calls, "POST", /\/livetv\/sources$/)).toHaveLength(1));
    expect(body(sent(calls, "POST", /\/livetv\/sources$/)[0])).toMatchObject({
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
    await waitFor(() => expect(sent(calls, "PUT", /\/livetv\/sources\/p1$/)).toHaveLength(1));
    expect(body(sent(calls, "PUT", /\/livetv\/sources\/p1$/)[0]).auth).toEqual({
      type: "basic",
      username: "owner",
      password: "••••",
      userAgent: "VLC/3.0",
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

describe("Live TV access on the viewer profile", () => {
  it("edits one viewer's Live TV policy in place on their Access tab", async () => {
    let policy = {
      userId: 2,
      disabled: false,
      channels: [] as string[],
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
    fireEvent.click(await screen.findByLabelText("Disable Live TV for this viewer", {}, WAIT));
    fireEvent.click(await screen.findByRole("button", { name: "Save changes" }));
    await waitFor(() =>
      expect(
        fakeCalls(fake, "PUT", "/livetv/policies/2").some(
          (c) => (c.body as { disabled: boolean }).disabled,
        ),
      ).toBe(true),
    );
    await waitFor(() => expect(screen.queryByRole("button", { name: "Save changes" })).toBeNull());
    fireEvent.click(screen.getByLabelText("Disable Live TV for this viewer"));
    fireEvent.click(screen.getByLabelText("Allow Cartoons"));
    fireEvent.click(await screen.findByRole("button", { name: "Save changes" }));
    await waitFor(() =>
      expect(fakeCalls(fake, "PUT", "/livetv/policies/2").at(-1)?.body).toMatchObject({
        disabled: false,
        channels: ["c2"],
      }),
    );
  });
});
