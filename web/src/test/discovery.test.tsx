import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { calls } from "./harness";
import { renderAppWith } from "./routes";

const WAIT = { timeout: 4000 };

/** `/discovery/settings` and one viewer's watchlist, layered over the fake backend. */
function discoveryRoutes(opts: { enabled?: boolean; items?: unknown[] } = {}) {
  let settings = {
    enabled: opts.enabled ?? false,
    plex: true,
    trakt: true,
    notify: false,
    prewarm: false,
    checkHours: 6,
    naturalSearch: false,
    recaps: false,
    subtitleTranslate: false,
    speechToText: false,
    overviewTranslate: false,
  };
  let items = opts.items ?? [];
  return (method: string, path: string, body: unknown) => {
    if (path === "/discovery/settings") {
      if (method === "PUT") settings = body as typeof settings;
      return settings;
    }
    const m = /^\/discovery\/users\/\d+\/(.*)$/.exec(path);
    if (!m) return undefined;
    const rest = m[1]!;
    if (method === "DELETE" && rest.startsWith("items/")) {
      items = items.filter(
        (i) => String((i as { title: { key: unknown } }).title.key) !== rest.slice(6),
      );
      return {};
    }
    return items;
  };
}
const puts = (fake: Parameters<typeof calls>[0], path: string) =>
  calls(fake, "PUT", path)
    .filter((c) => c.path === path)
    .map((c) => c.body);

/** Extra endpoints for whole-app renders of the Metadata page. */
function metadataRoutes() {
  let ratings = {
    enabled: false,
    provider: "omdb",
    apiKey: "",
    critic: "tomatoes",
    audience: "imdb",
    cacheHours: 168,
  };
  let overlays = {
    enabled: false,
    status: false,
    quality: false,
    ratings: false,
    personalize: false,
    localize: false,
  };
  return (method: string, path: string, body: unknown) => {
    if (path === "/discovery/ratings/settings") {
      if (method === "PUT") ratings = { ...(body as typeof ratings), apiKey: "[configured]" };
      return ratings;
    }
    if (path.startsWith("/discovery/ratings/refresh/"))
      return [{ source: "imdb", value: 8.1, scale: 10 }];
    if (path === "/discovery/overlays/settings") {
      if (method === "PUT") overlays = body as typeof overlays;
      return overlays;
    }
    if (path.startsWith("/discovery/overlays/variants/"))
      return {
        posters: [{ path: "/a.jpg", language: "en" }, { path: "/b.jpg" }],
        backdrops: [{ path: "/bd.jpg" }],
      };
    if (path.startsWith("/discovery/overlays/artwork/"))
      return method === "PUT" ? body : { posters: [], backdrops: [] };
    if (path.startsWith("/discovery/overlays/preview/"))
      return new Response("<svg/>", { headers: { "Content-Type": "image/svg+xml" } });
    return undefined;
  };
}

describe("Watchlist alerts (Notifications)", () => {
  it("starts monitoring and notifications off and saves opt-in settings in place", async () => {
    const { fake } = renderAppWith("/notifications", discoveryRoutes());
    const enabled = await screen.findByLabelText("Watchlist monitoring", {}, WAIT);
    expect(enabled).not.toBeChecked();
    expect(screen.getByLabelText("Tell me too")).not.toBeChecked();
    expect(screen.queryByLabelText("Warm up new arrivals")).toBeNull();
    expect(screen.queryByRole("button", { name: /Edit monitoring/ })).toBeNull();
    expect(screen.queryByRole("region", { name: "Unsaved changes" })).toBeNull();
    fireEvent.click(enabled);
    fireEvent.click(await screen.findByRole("button", { name: "Save changes" }));
    await waitFor(() =>
      expect(puts(fake, "/discovery/settings")).toContainEqual(
        expect.objectContaining({
          enabled: true,
          notify: false,
          prewarm: false,
          naturalSearch: false,
        }),
      ),
    );
    await waitFor(() =>
      expect(screen.queryByRole("region", { name: "Unsaved changes" })).toBeNull(),
    );
  });
});

describe("Warm up new arrivals (Cache › Preloading)", () => {
  it("keeps the prefetch toggle with the cache, off until monitoring is on", async () => {
    renderAppWith("/cache/preloading", discoveryRoutes());
    const warm = await screen.findByLabelText("Warm up new arrivals", {}, WAIT);
    expect(warm).toBeDisabled();
    expect(warm).not.toBeChecked();
    expect(screen.queryByLabelText("Watchlist monitoring")).toBeNull();
  });
});

describe("A viewer's watchlist (viewer profile › Watchlist)", () => {
  it("lists watched-for titles and syncs, checks and removes them", async () => {
    const { fake } = renderAppWith(
      "/viewers/2/watchlist",
      discoveryRoutes({
        enabled: true,
        items: [
          {
            title: { key: 1000000000000007, title: "Adventure", released: "2026-11-01T00:00:00Z" },
            sources: ["plex"],
          },
        ],
      }),
    );
    expect(await screen.findByText("Adventure", { selector: "p" }, WAIT)).toBeInTheDocument();
    expect(screen.queryByText(/Add a reminder/)).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: /Sync viewer/ }));
    await waitFor(() => expect(calls(fake, "POST", "/discovery/users/2/sync")).toHaveLength(1));
    fireEvent.click(await screen.findByRole("button", { name: /Check availability/ }));
    await waitFor(() => expect(calls(fake, "POST", "/discovery/users/2/check")).toHaveLength(1));
    fireEvent.click(await screen.findByRole("button", { name: "Remove Adventure" }));
    await waitFor(() =>
      expect(screen.queryByRole("list", { name: "Watched-for titles" })).toBeNull(),
    );
    expect(calls(fake, "DELETE", "/discovery/users/2/items/1000000000000007")).toHaveLength(1);
  });
});

describe("Language model features (Integrations)", () => {
  it("persists LLM feature toggles through the discovery settings API", async () => {
    const { fake } = renderAppWith("/services/llm", discoveryRoutes());
    const search = await screen.findByLabelText("Natural-language search", {}, WAIT);
    expect(search).not.toBeChecked();
    expect(screen.getByText(/A viewer runs that search/)).toBeInTheDocument();
    expect(screen.getAllByText("Uses your language model — connected ✓").length).toBeGreaterThan(0);
    expect(screen.getByRole("link", { name: "Needs speech-to-text → Set up" })).toHaveAttribute(
      "href",
      "/services/llm#speech-to-text",
    );
    expect(screen.queryByText(/Nothing here runs on a schedule/)).toBeNull();
    fireEvent.click(search);
    fireEvent.click(screen.getByLabelText("Translate subtitles"));
    fireEvent.click(await screen.findByRole("button", { name: "Save changes" }));
    await waitFor(() =>
      expect(puts(fake, "/discovery/settings")).toContainEqual(
        expect.objectContaining({
          naturalSearch: true,
          recaps: false,
          subtitleTranslate: true,
          speechToText: false,
          overviewTranslate: false,
          enabled: false,
        }),
      ),
    );
  });
});

describe("Metadata › Ratings", () => {
  it("edits rating sources in place and checks a title's badges", async () => {
    const { fake } = renderAppWith("/metadata/ratings", metadataRoutes());
    const toggle = await screen.findByLabelText("External ratings", {}, WAIT);
    expect(screen.queryByRole("button", { name: /Edit rating sources/ })).toBeNull();
    expect(screen.queryByLabelText("Viewer")).toBeNull();
    fireEvent.click(toggle);
    fireEvent.change(screen.getByLabelText("Provider"), { target: { value: "mdblist" } });
    expect(screen.queryByLabelText("API key")).not.toBeInTheDocument();
    expect(
      screen
        .getAllByRole("link", { name: "Integrations" })
        .map((link) => link.getAttribute("href")),
    ).toContain("/services/ratings");
    fireEvent.click(await screen.findByRole("button", { name: "Save changes" }));
    await waitFor(() =>
      expect(calls(fake, "PUT", "/discovery/ratings/settings").at(-1)?.body).toMatchObject({
        enabled: true,
        provider: "mdblist",
        apiKey: "",
      }),
    );
    expect(await screen.findByRole("link", { name: "OMDb and MDBList connected" })).toHaveAttribute(
      "href",
      "/services/ratings",
    );

    const name = fake.catalog[0]!.title;
    const badges = screen.getByRole("region", { name: "Title badges" });
    fireEvent.change(within(badges).getByLabelText("Search titles"), {
      target: { value: name },
    });
    fireEvent.click(
      (await within(badges).findAllByRole("button", { name: new RegExp(name) }, WAIT))[0]!,
    );
    fireEvent.click(within(badges).getByRole("button", { name: /Refresh ratings/ }));
    expect(await within(badges).findByText("imdb: 8.1/10")).toBeInTheDocument();
    expect(calls(fake, "POST", `/discovery/ratings/refresh/${fake.catalog[0]!.key}`)).toHaveLength(
      1,
    );
  });
});

describe("Metadata › Artwork", () => {
  it("saves poster settings in place without a viewer picker", async () => {
    const { fake } = renderAppWith("/metadata/artwork", metadataRoutes());
    const posters = await screen.findByLabelText("Ether posters", {}, WAIT);
    expect(screen.queryByRole("button", { name: /Edit artwork preferences/ })).toBeNull();
    expect(screen.queryByLabelText("Viewer")).toBeNull();
    expect(screen.getByLabelText("Quality badge")).toBeDisabled();
    fireEvent.click(posters);
    fireEvent.click(screen.getByLabelText("Quality badge"));
    fireEvent.click(await screen.findByRole("button", { name: "Save changes" }));
    await waitFor(() =>
      expect(calls(fake, "PUT", "/discovery/overlays/settings").at(-1)?.body).toMatchObject({
        enabled: true,
        quality: true,
        status: false,
      }),
    );
  });

  it("pins one title's poster in a drawer", async () => {
    const { fake } = renderAppWith("/metadata/artwork", metadataRoutes());
    const section = await screen.findByRole("region", { name: "Title artwork" }, WAIT);
    const title = fake.catalog[0]!;
    fireEvent.change(within(section).getByLabelText("Search titles"), {
      target: { value: title.title },
    });
    fireEvent.click(
      (await within(section).findAllByRole("button", { name: new RegExp(title.title) }, WAIT))[0]!,
    );
    const drawer = await screen.findByRole("dialog", { name: title.title });
    fireEvent.click(await within(drawer).findByRole("button", { name: "Pin poster (en)" }));
    fireEvent.change(within(drawer).getByLabelText("Backdrop"), { target: { value: "/bd.jpg" } });
    fireEvent.click(within(drawer).getByRole("button", { name: "Save title artwork" }));
    await waitFor(() =>
      expect(
        calls(fake, "PUT", `/discovery/overlays/artwork/${title.key}`).at(-1)?.body,
      ).toMatchObject({ pinnedPoster: "/a.jpg", pinnedBackdrop: "/bd.jpg" }),
    );
    await waitFor(() => expect(screen.queryByRole("dialog", { name: title.title })).toBeNull());
  });
});
