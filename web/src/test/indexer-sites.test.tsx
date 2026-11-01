import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import type { Indexer } from "../lib/types";
import { calls, renderApp } from "./harness";

const WAIT = { timeout: 4000 };

const builtinIndexer: Indexer = {
  id: "site-1",
  name: "Example Private",
  url: "https://example-private.invalid/",
  apiKey: "",
  dailyLimit: 50,
  dailyGrabs: 10,
  priority: 3,
  enabled: true,
  movieCategories: null,
  tvCategories: null,
  protocol: "torrent",
  kind: "cardigann",
  definition: "example-private",
  settings: { username: "alice" },
  secretSettings: { password: "••••" },
};

async function openAddSite() {
  fireEvent.click(await screen.findByRole("button", { name: /Add indexer/ }, WAIT));
  fireEvent.click(await screen.findByRole("menuitem", { name: "Site…" }));
  return screen.findByRole("dialog", { name: "Add a site" });
}

describe("Sources › Indexers sites", () => {
  it("adds a site from an installed definition, tests it and saves it", async () => {
    const { fake } = renderApp("/sources/indexers");
    const picker = await openAddSite();
    fireEvent.click(await within(picker).findByRole("button", { name: "Use Example Private" }));
    const drawer = await screen.findByRole("dialog", { name: "Example Private" });
    expect(within(drawer).getByText("Torrent site")).toBeInTheDocument();
    expect(within(drawer).getByText("Example Private (example-private)")).toBeInTheDocument();
    expect(within(drawer).getByText(/filter "sparkle" is not supported/)).toBeInTheDocument();
    expect(within(drawer).getByLabelText("Site address")).toHaveValue(
      "https://example-private.invalid/",
    );
    fireEvent.change(within(drawer).getByLabelText("Username"), { target: { value: "alice" } });
    fireEvent.change(within(drawer).getByLabelText("Password"), { target: { value: "s3cret" } });
    fireEvent.click(within(drawer).getByRole("button", { name: /Test/ }));
    await waitFor(() => expect(calls(fake, "POST", "/indexer-sites/test")).toHaveLength(1));
    expect(calls(fake, "POST", "/indexer-sites/test")[0]!.body).toMatchObject({
      kind: "cardigann",
      definition: "example-private",
      settings: { username: "alice" },
      secretSettings: { password: "s3cret" },
    });
    expect(await within(drawer).findByText(/37 results/)).toBeInTheDocument();
    expect(within(drawer).getByText("Example.Film.2024.1080p.WEB-DL")).toBeInTheDocument();

    fireEvent.click(within(drawer).getByRole("button", { name: "Save indexer" }));
    await waitFor(() => expect(calls(fake, "PUT", "/config/indexers")).toHaveLength(1));
    const body = calls(fake, "PUT", "/config/indexers")[0]!.body as Indexer[];
    expect(body).toHaveLength(5);
    expect(body[4]).toMatchObject({
      name: "Example Private",
      kind: "cardigann",
      definition: "example-private",
      protocol: "torrent",
      url: "https://example-private.invalid/",
      settings: { username: "alice" },
      secretSettings: { password: "s3cret" },
      priority: 4,
      enabled: true,
    });
    // Newznab indexers keep their redacted keys.
    expect(body.slice(0, 3).map((ix) => ix.apiKey)).toEqual(["••••", "••••", "••••"]);
    const list = screen.getByRole("region", { name: "Indexers" });
    expect(await within(list).findByText("Torrent site", {}, WAIT)).toBeVisible();
    expect(within(list).getByText(/Priority 5 · example-private.invalid/)).toBeVisible();
    // The definition now backs an indexer, so it leaves the unused list.
    const unused = screen.getByRole("list", { name: "Unused definitions" });
    await waitFor(() => expect(within(unused).queryByText("Example Private")).toBeNull());
    expect(within(unused).getByText("Example Tracker")).toBeInTheDocument();
    expect(
      screen.getByText("2 definitions installed, 1 used by your indexers above."),
    ).toBeInTheDocument();
  });

  it("generates the settings form from the definition", async () => {
    renderApp("/sources/indexers");
    const picker = await openAddSite();
    fireEvent.click(await within(picker).findByRole("button", { name: "Use Example Tracker" }));
    const drawer = await screen.findByRole("dialog", { name: "Example Tracker" });
    expect(
      within(drawer).getByRole("button", { name: "Sort requested from site" }),
    ).toHaveTextContent("Date added");
    expect(within(drawer).getByRole("switch", { name: "Search freeleech only" })).not.toBeChecked();
    expect(within(drawer).getByText("No account needed.")).toBeInTheDocument();
    expect(
      within(drawer).getByText(/Also: https:\/\/mirror.example-tracker.invalid\//),
    ).toBeVisible();
  });

  it("lists sites with their type and last test, and keeps stored secrets", async () => {
    const { fake } = renderApp("/sources/indexers", {}, (f) => {
      f.state.config.indexers = [...(f.state.config.indexers ?? []), builtinIndexer];
      f.sites.state.statuses = [
        {
          indexerId: "site-1",
          ok: false,
          message: "Example Private: sign-in failed",
          results: 0,
          testedAt: new Date().toISOString(),
        },
      ];
    });
    const list = await screen.findByRole("region", { name: "Indexers" }, WAIT);
    expect(await within(list).findByText("Test failed", {}, WAIT)).toHaveAttribute(
      "title",
      "Example Private: sign-in failed",
    );
    expect(within(list).getByText("Torrent site")).toBeInTheDocument();
    expect(
      within(list).getByText(
        /example-private.invalid · 66 of 50 searches used today · 10 torrent downloads a day/,
      ),
    ).toBeInTheDocument();
    // Shown once: as the indexer, not again among the definitions.
    expect(screen.getAllByText("Example Private")).toHaveLength(1);

    fireEvent.click(within(list).getByRole("button", { name: "Edit Example Private" }));
    const drawer = await screen.findByRole("dialog", { name: "Example Private" });
    expect(within(drawer).getByLabelText("Password")).toHaveValue("••••");
    fireEvent.click(within(drawer).getByRole("button", { name: /Test/ }));
    await waitFor(() => expect(calls(fake, "POST", "/indexer-sites/test")).toHaveLength(1));
    expect(calls(fake, "POST", "/indexer-sites/test")[0]!.body).toMatchObject({
      id: "site-1",
      secretSettings: { password: "••••" },
    });
    fireEvent.click(within(drawer).getByRole("button", { name: "More options" }));
    fireEvent.change(within(drawer).getByLabelText("Searches per day"), {
      target: { value: "80" },
    });
    fireEvent.click(within(drawer).getByRole("button", { name: "Save indexer" }));
    await waitFor(() => expect(calls(fake, "PUT", "/config/indexers")).toHaveLength(1));
    const body = calls(fake, "PUT", "/config/indexers")[0]!.body as Indexer[];
    expect(body.find((ix) => ix.id === "site-1")).toMatchObject({
      id: "site-1",
      dailyLimit: 80,
      secretSettings: { password: "••••" },
    });
  });

  it("shows a failed test's error", async () => {
    renderApp("/sources/indexers", {}, (f) => {
      f.state.config.indexers = [
        ...(f.state.config.indexers ?? []),
        { ...builtinIndexer, secretSettings: { password: "wrong" } },
      ];
    });
    fireEvent.click(await screen.findByRole("button", { name: "Edit Example Private" }, WAIT));
    const drawer = await screen.findByRole("dialog", { name: "Example Private" });
    fireEvent.click(within(drawer).getByRole("button", { name: /Test/ }));
    expect(await within(drawer).findByText(/Wrong username or password/)).toBeInTheDocument();
  });

  it("flags a site whose definition was removed", async () => {
    renderApp("/sources/indexers", {}, (f) => {
      f.state.config.indexers = [{ ...builtinIndexer, definition: "gone" }];
    });
    const list = await screen.findByRole("region", { name: "Indexers" }, WAIT);
    expect(await within(list).findByText("Definition missing", {}, WAIT)).toBeInTheDocument();
    fireEvent.click(within(list).getByRole("button", { name: "Edit Example Private" }));
    const drawer = await screen.findByRole("dialog", { name: "Example Private" });
    expect(within(drawer).getByText(/“gone” is not installed/)).toBeInTheDocument();
    expect(within(drawer).getByRole("button", { name: /Test/ })).toBeDisabled();
  });

  it("installs a site from the definitions source when picked", async () => {
    const { fake } = renderApp("/sources/indexers", {}, (f) => {
      f.state.config.indexerSites = { definitionsUrl: "https://defs.invalid/definitions/" };
    });
    const picker = await openAddSite();
    const source = within(picker).getByRole("region", { name: "From your definitions source" });
    expect(await within(source).findByText("sample-usenet")).toBeInTheDocument();
    fireEvent.change(within(picker).getByLabelText("Search sites"), { target: { value: "anime" } });
    expect(within(source).queryByText("sample-usenet")).toBeNull();
    fireEvent.click(within(source).getByRole("button", { name: "Install sample-anime" }));
    await waitFor(() =>
      expect(calls(fake, "POST", "/indexer-sites/catalog/install")).toHaveLength(1),
    );
    expect(calls(fake, "POST", "/indexer-sites/catalog/install")[0]!.body).toEqual({
      id: "sample-anime",
    });
    const drawer = await screen.findByRole("dialog", { name: "Sample Anime" });
    expect(within(drawer).getByLabelText("Cookie")).toBeInTheDocument();
  });

  it("explains that browsing needs a definitions source", async () => {
    renderApp("/sources/indexers");
    const picker = await openAddSite();
    expect(await within(picker).findByText(/No definitions source is set/)).toBeInTheDocument();
  });

  it("marks sites that are already indexers in the picker", async () => {
    renderApp("/sources/indexers", {}, (f) => {
      f.state.config.indexers = [...(f.state.config.indexers ?? []), builtinIndexer];
    });
    await screen.findByText("Torrent site", {}, WAIT);
    const picker = await openAddSite();
    const installed = within(picker).getByRole("region", { name: "Installed sites" });
    const rows = within(installed).getAllByRole("listitem");
    expect(within(rows[1]!).getByText("Added")).toBeInTheDocument();
    expect(within(rows[0]!).queryByText("Added")).toBeNull();
  });

  it("uploads a definition from the picker and continues to its settings", async () => {
    const { fake } = renderApp("/sources/indexers");
    const picker = await openAddSite();
    fireEvent.click(within(picker).getByRole("button", { name: /Upload definition/ }));
    const upload = await screen.findByRole("dialog", { name: "Upload definition" });
    fireEvent.change(within(upload).getByLabelText("Definition YAML"), {
      target: { value: "id: my-site\nname: My Site\nlinks:\n  - https://my-site.invalid/\n" },
    });
    fireEvent.click(within(upload).getByRole("button", { name: /Install definition/ }));
    await waitFor(() => expect(calls(fake, "POST", "/indexer-sites/definitions")).toHaveLength(1));
    const drawer = await screen.findByRole("dialog", { name: "My Site" }, WAIT);
    expect(within(drawer).getByLabelText("Site address")).toHaveValue("https://my-site.invalid/");
    expect(within(drawer).queryByText(/is not installed/)).toBeNull();
    expect(within(drawer).getByRole("button", { name: /Test/ })).toBeEnabled();
  });

  it("still adds Newznab and Torznab URLs from the same menu", async () => {
    renderApp("/sources/indexers");
    fireEvent.click(await screen.findByRole("button", { name: /Add indexer/ }, WAIT));
    fireEvent.click(await screen.findByRole("menuitem", { name: "Newznab or Torznab URL…" }));
    const drawer = await screen.findByRole("dialog", { name: "New indexer" });
    expect(within(drawer).getByRole("radiogroup", { name: "Protocol" })).toBeInTheDocument();
    expect(within(drawer).getByLabelText("API key")).toBeInTheDocument();
  });

  it("does not present Prowlarr as a requirement", async () => {
    renderApp("/sources/indexers");
    const list = await screen.findByRole("region", { name: "Indexers" }, WAIT);
    expect(within(list).queryByText(/Prowlarr/)).toBeNull();
    expect(screen.queryByText(/ships none/)).toBeNull();
    expect(screen.queryByText(/ships no/)).toBeNull();
    expect(
      await screen.findByText("Add a definitions source to search tracker sites"),
    ).toBeInTheDocument();
  });
});

describe("Sources › Indexers definitions", () => {
  it("edits the definitions source and FlareSolverr in place", async () => {
    const { fake } = renderApp("/sources/indexers");
    const flare = await screen.findByLabelText("FlareSolverr", {}, WAIT);
    expect(screen.queryByRole("button", { name: /Save changes/ })).toBeNull();
    fireEvent.change(flare, { target: { value: "http://localhost:8191" } });
    fireEvent.change(screen.getByLabelText("Definitions source"), {
      target: { value: "https://defs.invalid/definitions/" },
    });
    fireEvent.click(await screen.findByRole("button", { name: /Save changes/ }));
    await waitFor(() => expect(calls(fake, "PUT", "/config/indexerSites")).toHaveLength(1));
    expect(calls(fake, "PUT", "/config/indexerSites")[0]!.body).toEqual({
      definitionsUrl: "https://defs.invalid/definitions/",
      flareSolverrUrl: "http://localhost:8191",
    });
  });

  it("lists unused definitions with warnings and updates them from the source", async () => {
    const { fake } = renderApp("/sources/indexers");
    const unused = await screen.findByRole("list", { name: "Unused definitions" }, WAIT);
    expect(within(unused).getByText("Example Tracker")).toBeInTheDocument();
    expect(within(unused).getByText(/filter "sparkle" is not supported/)).toBeInTheDocument();
    expect(screen.getByText("2 definitions installed.")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: /Update definitions/ }));
    await waitFor(() => expect(calls(fake, "POST", "/indexer-sites/update")).toHaveLength(1));
    expect(await screen.findByText(/1 updated · 0 unchanged/)).toBeInTheDocument();
  });

  it("uploads a pasted definition and reports invalid ones", async () => {
    const { fake } = renderApp("/sources/indexers");
    fireEvent.click(await screen.findByRole("button", { name: /Upload definition/ }, WAIT));
    const drawer = await screen.findByRole("dialog", { name: "Upload definition" });
    const install = within(drawer).getByRole("button", { name: /Install definition/ });
    expect(install).toBeDisabled();
    fireEvent.change(within(drawer).getByLabelText("Definition YAML"), {
      target: { value: "name: Missing id\n" },
    });
    fireEvent.click(install);
    expect(await within(drawer).findByText(/id is required/)).toBeInTheDocument();
    fireEvent.change(within(drawer).getByLabelText("Definition YAML"), {
      target: { value: "id: my-site\nname: My Site\nlinks:\n  - https://my-site.invalid/\n" },
    });
    fireEvent.click(install);
    await waitFor(() => expect(calls(fake, "POST", "/indexer-sites/definitions")).toHaveLength(2));
    expect(calls(fake, "POST", "/indexer-sites/definitions")[1]!.body).toMatchObject({
      yaml: expect.stringContaining("id: my-site"),
    });
    const unused = await screen.findByRole("list", { name: "Unused definitions" });
    expect(await within(unused).findByText("My Site", {}, WAIT)).toBeInTheDocument();
    // Uploading from the page installs only; no indexer drawer opens.
    expect(screen.queryByRole("dialog", { name: "My Site" })).toBeNull();
  });

  it("adds an unused definition as an indexer", async () => {
    renderApp("/sources/indexers");
    fireEvent.click(
      await screen.findByRole("button", { name: "Add Example Tracker as an indexer" }, WAIT),
    );
    const drawer = await screen.findByRole("dialog", { name: "Example Tracker" });
    expect(within(drawer).getByRole("button", { name: "Save indexer" })).toBeInTheDocument();
    expect(within(drawer).getByLabelText("Site address")).toHaveValue(
      "https://example-tracker.invalid/",
    );
  });

  it("removes an unused definition and keeps ones in use off the list", async () => {
    const { fake } = renderApp("/sources/indexers", {}, (f) => {
      f.state.config.indexers = [...(f.state.config.indexers ?? []), builtinIndexer];
    });
    const installed = await screen.findByRole("list", { name: "Unused definitions" }, WAIT);
    expect(within(installed).queryByRole("button", { name: "Remove Example Private" })).toBeNull();
    fireEvent.click(within(installed).getByRole("button", { name: "Remove Example Tracker" }));
    fireEvent.click(await screen.findByRole("button", { name: "Remove" }));
    await waitFor(() =>
      expect(calls(fake, "DELETE", "/indexer-sites/definitions/example-tracker")).toHaveLength(1),
    );
    await waitFor(() =>
      expect(screen.queryByRole("list", { name: "Unused definitions" })).toBeNull(),
    );
    expect(
      screen.getByText("1 definition installed, all used by your indexers above."),
    ).toBeInTheDocument();
  });
});
