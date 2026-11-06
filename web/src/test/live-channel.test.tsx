import { fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { channelLabel, normalizeSessions } from "../lib/health";
import { routeApiImagesThroughFetch } from "../lib/api";
import { LOGO_BACKGROUND_HEADER, SessionArtwork } from "../components/SessionArtwork";
import { ChannelLogo } from "../pages/livetv/ChannelLogo";
import { renderApp } from "./harness";

const WAIT = { timeout: 4000 };

/** The test setup's IntersectionObserver never fires; make observed images load. */
class ImmediatelyVisible implements IntersectionObserver {
  readonly root: Element | Document | null = null;
  readonly rootMargin = "";
  readonly thresholds: readonly number[] = [];
  constructor(private readonly callback: IntersectionObserverCallback) {}
  observe(target: Element) {
    this.callback([{ isIntersecting: true, target } as IntersectionObserverEntry], this);
  }
  unobserve() {}
  disconnect() {}
  takeRecords(): IntersectionObserverEntry[] {
    return [];
  }
}

describe("live channel identity", () => {
  it("uses the scored channel logo in the Live TV list and falls back when it fails", async () => {
    vi.stubGlobal("IntersectionObserver", ImmediatelyVisible);
    const { createObjectURL, revokeObjectURL } = URL;
    URL.createObjectURL = () => "blob:channel-logo";
    URL.revokeObjectURL = () => {};
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => new Response("logo", { headers: { [LOGO_BACKGROUND_HEADER]: "light" } })),
    );
    try {
      const view = render(
        <ChannelLogo
          name="Local News"
          logo="https://example.com/logo.png"
          channelId="a/b"
          size={34}
        />,
      );
      const logo = await view.findByAltText("");
      expect(fetch).toHaveBeenCalledWith(
        "/ether/api/livetv/channels/a%2Fb/logo",
        expect.anything(),
      );
      expect(logo.className).toContain("bg-white");
      fireEvent.error(logo);
      expect(view.getByText("LN")).toBeInTheDocument();
    } finally {
      URL.createObjectURL = createObjectURL;
      URL.revokeObjectURL = revokeObjectURL;
      vi.unstubAllGlobals();
    }
  });
  it("draws the tile the logo response asks for and keeps dark without a usable header", async () => {
    routeApiImagesThroughFetch(false);
    vi.stubGlobal("IntersectionObserver", ImmediatelyVisible);
    const [live] = normalizeSessions([{ liveChannel: { id: "news", name: "News" } }]);
    const { createObjectURL, revokeObjectURL } = URL;
    URL.createObjectURL = () => "blob:logo";
    URL.revokeObjectURL = () => {};
    // The server scores the logo; the console only reads its answer. Unknown
    // values and failed fetches must leave the dark default in place.
    const renderLogo = (answer: "light" | "dark" | "zinc" | "offline") => {
      vi.stubGlobal(
        "fetch",
        vi.fn(async () => {
          if (answer === "offline") throw new Error("offline");
          return new Response("logo", { headers: { [LOGO_BACKGROUND_HEADER]: answer } });
        }),
      );
      return render(<SessionArtwork s={live!} className="" />);
    };
    try {
      const light = renderLogo("light");
      expect((await light.findByAltText("News")).className).toContain("bg-white");
      light.unmount();

      const dark = renderLogo("dark");
      expect((await dark.findByAltText("News")).className).toContain("bg-zinc-900");
      dark.unmount();

      const unknown = renderLogo("zinc");
      expect((await unknown.findByAltText("News")).className).toContain("bg-zinc-900");
      unknown.unmount();

      const offline = renderLogo("offline");
      expect(await offline.findByText("N")).toBeInTheDocument();
      expect(offline.queryByAltText("News")).toBeNull();
      offline.unmount();
    } finally {
      URL.createObjectURL = createObjectURL;
      URL.revokeObjectURL = revokeObjectURL;
      routeApiImagesThroughFetch(false);
      vi.unstubAllGlobals();
    }
  });
  it("keeps the dark tile until the response arrives", async () => {
    routeApiImagesThroughFetch(true);
    vi.stubGlobal("IntersectionObserver", ImmediatelyVisible);
    const [live] = normalizeSessions([{ liveChannel: { id: "news", name: "News" } }]);
    const { createObjectURL, revokeObjectURL } = URL;
    URL.createObjectURL = () => "blob:logo";
    URL.revokeObjectURL = () => {};
    // tsconfig targets a lib without Promise.withResolvers, so hold the resolve.
    let answer!: (response: Response) => void;
    vi.stubGlobal(
      "fetch",
      vi.fn(
        () =>
          new Promise<Response>((resolve) => {
            answer = resolve;
          }),
      ),
    );
    try {
      render(<SessionArtwork s={live!} className="" />);
      expect(screen.queryByAltText("News")).toBeNull();
      answer(new Response("logo", { headers: { [LOGO_BACKGROUND_HEADER]: "light" } }));
      expect((await screen.findByAltText("News")).className).toContain("bg-white");
    } finally {
      URL.createObjectURL = createObjectURL;
      URL.revokeObjectURL = revokeObjectURL;
      routeApiImagesThroughFetch(false);
      vi.unstubAllGlobals();
    }
  });
  it("falls back to channel initials on a missing or broken logo and keeps VOD artwork", async () => {
    vi.stubGlobal("IntersectionObserver", ImmediatelyVisible);
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => new Response("logo")),
    );
    URL.createObjectURL = () => "blob:logo";
    URL.revokeObjectURL = () => {};
    const [live, vod] = normalizeSessions([
      { liveChannel: { id: "a/b", name: "Local News" }, TitleName: "Programme" },
      { TitleName: "Movie" },
    ]);
    const { rerender } = render(<SessionArtwork s={live!} poster="/programme.jpg" className="" />);
    const logo = await screen.findByAltText("Local News");
    expect(fetch).toHaveBeenCalledWith("/ether/api/livetv/channels/a%2Fb/logo", expect.anything());
    fireEvent.error(logo);
    expect(screen.getByText("LN")).toBeInTheDocument();
    rerender(<SessionArtwork s={{ ...live!, channelId: "" }} className="" />);
    expect(screen.getByText("LN")).toBeInTheDocument();
    rerender(<SessionArtwork s={vod!} poster="/movie.jpg" className="" />);
    expect(screen.getByAltText("Movie")).toHaveAttribute("src", "/movie.jpg");
  });
  it("normalizes partial and absent channel metadata without changing VOD", () => {
    const sessions = normalizeSessions([
      { liveChannel: { name: " News ", number: " 12.1 " }, TitleName: "Programme" },
      { liveChannel: { name: "News" } },
      { liveChannel: { number: "12" } },
      { liveChannel: {} },
      { TitleName: "Movie" },
    ]);
    expect(sessions.map(channelLabel)).toEqual(["12.1 · News", "News", "12", "", ""]);
    expect(sessions.map((s) => s.isLive)).toEqual([true, true, true, true, false]);
    expect(sessions[4]?.title).toBe("Movie");
  });

  it.each(["/", "/activity"])("shows the channel alongside the programme on %s", async (path) => {
    localStorage.removeItem("now-view");
    renderApp(path, {}, (fake) => {
      const original = fake.fetch;
      fake.fetch = async (input, init) => {
        const res = await original(input, init);
        const dashboard = String(input).endsWith("/dashboard");
        const sessions = String(input).endsWith("/status/sessions");
        if (!dashboard && !sessions) return res;
        const body = await res.json();
        const values = dashboard ? body.sections.sessions : body.value;
        values.splice(0, values.length, {
          ID: "channel-test",
          TitleName: "Evening News",
          Username: "Viewer",
          Client: { deviceName: "TV" },
          liveChannel: { id: "channel-test", name: "Local News", number: "12.1" },
        });
        return new Response(JSON.stringify(body), { status: 200 });
      };
    });
    expect((await screen.findAllByText("Evening News", {}, WAIT)).length).toBeGreaterThan(0);
    expect(screen.getByText("12.1 · Local News")).toBeInTheDocument();
    expect(screen.getByText("LN")).toBeInTheDocument();
    if (path === "/activity") {
      fireEvent.click(screen.getByRole("radio", { name: "List" }));
      expect(screen.getAllByText("Evening News").length).toBeGreaterThan(0);
      expect(screen.getByText("12.1 · Local News")).toBeInTheDocument();
      expect(screen.getByText("LN")).toBeInTheDocument();
    }
  });
});
