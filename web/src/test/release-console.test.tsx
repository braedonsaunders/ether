import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { calls, renderApp } from "./harness";

const WAIT = { timeout: 4000 };

async function choose(label: string, option: string | RegExp) {
  fireEvent.click(screen.getByRole("button", { name: label }));
  fireEvent.click(await screen.findByRole("option", { name: option }));
}

/** Open the release search for a catalogue title through the title picker. */
async function pickTitle(title: string) {
  fireEvent.change(await screen.findByLabelText("Search titles", {}, WAIT), {
    target: { value: title },
  });
  const results = await screen.findByRole("list", { name: "Search results" }, WAIT);
  fireEvent.click(within(results).getAllByRole("button", { name: new RegExp(title) })[0]!);
}

describe("Release search", () => {
  it("ranks a movie's releases as playback would and pins one with a note", async () => {
    const { fake } = renderApp("/releases/pinned");
    const movie = fake.catalog.filter((t) => t.type === "movie")[3]!;
    await pickTitle(movie.title);
    expect(
      await screen.findByRole(
        "region",
        { name: `Releases for ${movie.title} (${movie.year})` },
        WAIT,
      ),
    ).toBeInTheDocument();
    expect(await screen.findByText("above profile max height", {}, WAIT)).toBeInTheDocument();
    expect(screen.getByText("cam not allowed")).toBeInTheDocument();
    expect(screen.getByText("Plays first")).toBeInTheDocument();
    expect(calls(fake, "GET", `/releases/search?key=${movie.key}`)).toHaveLength(1);

    const menus = screen.getAllByRole("button", { name: /^Actions for / });
    fireEvent.click(menus[1]!);
    fireEvent.click(await screen.findByRole("menuitem", { name: "Pin to this title" }));
    const drawer = await screen.findByRole("dialog", { name: "Pin release" });
    fireEvent.change(within(drawer).getByLabelText("Note"), { target: { value: "best mix" } });
    fireEvent.click(within(drawer).getByRole("button", { name: "Pin release" }));
    await waitFor(() => expect(calls(fake, "POST", "/releases/pins")).toHaveLength(1));
    expect(calls(fake, "POST", "/releases/pins")[0]!.body).toMatchObject({
      key: movie.key,
      releaseId: `${movie.key}-r2`,
      note: "best mix",
    });
    expect(await screen.findByText("Pinned release", {}, WAIT)).toBeInTheDocument();
    // The note shows on the search result and in the pinned list below,
    // which refreshes as soon as the pin is saved.
    await waitFor(() => expect(screen.getAllByText("best mix")).toHaveLength(2));
  });

  it("blocks a release with a reason and searches again without the cache", async () => {
    const { fake } = renderApp("/releases/pinned");
    const movie = fake.catalog.filter((t) => t.type === "movie")[4]!;
    await pickTitle(movie.title);
    await screen.findByText("Plays first", {}, WAIT);
    fireEvent.click(screen.getAllByRole("button", { name: /^Actions for / })[0]!);
    fireEvent.click(await screen.findByRole("menuitem", { name: "Block…" }));
    fireEvent.change(await screen.findByLabelText("Reason"), { target: { value: "dubbed" } });
    fireEvent.click(screen.getByRole("button", { name: "Block" }));
    await waitFor(() => expect(calls(fake, "POST", "/releases/blocklist")).toHaveLength(1));
    expect(calls(fake, "POST", "/releases/blocklist")[0]!.body).toMatchObject({
      releaseId: `${movie.key}-r1`,
      key: movie.key,
      reason: "dubbed",
    });
    expect(await screen.findByText("marked bad", {}, WAIT)).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: /Search again/ }));
    await waitFor(() => expect(calls(fake, "POST", "/releases/forget")).toHaveLength(1));
    expect(calls(fake, "POST", "/releases/forget")[0]!.body).toEqual({ key: movie.key });
    await waitFor(() =>
      expect(calls(fake, "GET", "/releases/search").some((c) => c.path.includes("fresh=1"))).toBe(
        true,
      ),
    );
  });

  it("ranks with another profile and reports a failed health check", async () => {
    const { fake } = renderApp("/releases/pinned");
    const movie = fake.catalog.filter((t) => t.type === "movie")[5]!;
    await pickTitle(movie.title);
    await screen.findByText("Plays first", {}, WAIT);
    await choose("Quality profile", "No profile limits");
    await waitFor(() =>
      expect(
        calls(fake, "GET", "/releases/search").some((c) => c.path.includes("profile=none")),
      ).toBe(true),
    );
    await waitFor(() => expect(screen.queryByText("above profile max height")).toBeNull());
    const menus = screen.getAllByRole("button", { name: /^Actions for .*YTS/ });
    fireEvent.click(menus[0]!);
    fireEvent.click(await screen.findByRole("menuitem", { name: "Check health" }));
    await waitFor(() => expect(calls(fake, "POST", "/releases/health")).toHaveLength(1));
    // A definitive failure is blocklisted, so the release now reads as blocked.
    expect(await screen.findByText("marked bad", {}, WAIT)).toBeInTheDocument();
    expect(screen.getAllByText("Blocked").length).toBeGreaterThan(0);
  });

  it("finds an episode through its show and season", async () => {
    const { fake } = renderApp("/releases/pinned");
    const show = fake.catalog.find((t) => t.type === "show")!;
    await pickTitle(show.title);
    await waitFor(() => expect(screen.getByRole("button", { name: "Season" })).toBeEnabled(), WAIT);
    await choose("Season", "Season 1");
    await waitFor(
      () => expect(screen.getByRole("button", { name: "Episode" })).toBeEnabled(),
      WAIT,
    );
    await choose("Episode", /^E02 /);
    await waitFor(() =>
      expect(calls(fake, "GET", `/releases/search?key=${show.key}.s1.e2`)).toHaveLength(1),
    );
    expect(
      await screen.findByRole(
        "region",
        { name: new RegExp(`Releases for ${show.title} S01E02`) },
        WAIT,
      ),
    ).toBeInTheDocument();
  });
});

describe("Pinned releases", () => {
  it("lists pins with their last status and unpins one", async () => {
    const { fake } = renderApp("/releases/pinned");
    const movie = fake.catalog.filter((t) => t.type === "movie")[0]!;
    const name = `${movie.title} (${movie.year})`;
    expect(await screen.findByRole("link", { name }, WAIT)).toHaveAttribute(
      "href",
      `/releases/pinned?key=${movie.key}&title=${encodeURIComponent(name)}`,
    );
    expect(screen.getByText("Playing")).toBeInTheDocument();
    expect(screen.getByText("The only encode with the original mix")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: `Unpin ${name}` }));
    fireEvent.click(await screen.findByRole("button", { name: "Unpin" }));
    await waitFor(() =>
      expect(calls(fake, "DELETE", `/releases/pins/${movie.key}`)).toHaveLength(1),
    );
    expect(await screen.findByText("No pinned releases", {}, WAIT)).toBeInTheDocument();
  });
});

describe("Blocklist", () => {
  it("filters by source and unblocks a release", async () => {
    const { fake } = renderApp("/releases/blocklist");
    expect(await screen.findByText("health: missing articles", {}, WAIT)).toBeInTheDocument();
    expect(screen.getByText("Wrong audio language")).toBeInTheDocument();
    await choose("Source", "Blocked by you");
    await waitFor(() =>
      expect(calls(fake, "GET", "/releases/blocklist?source=owner")).toHaveLength(1),
    );
    await waitFor(() => expect(screen.queryByText("health: missing articles")).toBeNull());
    const unblock = screen.getByRole("button", { name: /^Unblock / });
    fireEvent.click(unblock);
    await waitFor(() => expect(calls(fake, "DELETE", "/releases/blocklist/")).toHaveLength(1));
    expect(await screen.findByText("Nothing matches", {}, WAIT)).toBeInTheDocument();
  });
});

describe("Release history", () => {
  it("lists decisions newest first and explains one in a drawer", async () => {
    const { fake } = renderApp("/releases/history");
    const movies = fake.catalog.filter((t) => t.type === "movie");
    const first = `${movies[0]!.title} (${movies[0]!.year})`;
    fireEvent.click(await screen.findByText(first, {}, WAIT));
    const drawer = await screen.findByRole("dialog", { name: first });
    expect(
      await within(drawer).findByText("above profile max height", {}, WAIT),
    ).toBeInTheDocument();
    expect(within(drawer).getAllByText("Picked").length).toBeGreaterThan(0);
    expect(within(drawer).getAllByText("Auto").length).toBeGreaterThan(0);
    expect(within(drawer).getAllByText("Pinned").length).toBeGreaterThan(0);
    expect(calls(fake, "GET", "/releases/history/1")).toHaveLength(1);
    expect(screen.getAllByText("Failover").length).toBeGreaterThan(0);
    const searchLink = within(drawer).getByRole("link", { name: "Search releases for this title" });
    expect(searchLink).toHaveAttribute(
      "href",
      `/releases/pinned?key=${movies[0]!.key}&title=${encodeURIComponent(first)}`,
    );
    fireEvent.click(searchLink);
    expect(
      await screen.findByRole(
        "region",
        { name: new RegExp(`Releases for ${movies[0]!.title}`) },
        WAIT,
      ),
    ).toBeInTheDocument();
  });

  it("does not keep a /releases/search page", async () => {
    const { fake } = renderApp("/releases/search?key=1000000000000550&title=Fight%20Club%20(1999)");
    expect(await screen.findByRole("tab", { name: "Profiles" }, WAIT)).toBeInTheDocument();
    expect(calls(fake, "GET", "/releases/search?key=1000000000000550")).toHaveLength(0);
  });
});
