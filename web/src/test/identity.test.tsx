import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { calls, renderApp } from "./harness";

const WAIT = { timeout: 4000 };

async function identityCard() {
  return screen.findByRole("region", { name: "User agent" }, WAIT);
}

describe("Sources › Indexers client identity", () => {
  it("shows the global policy defaults and the inherit count", async () => {
    renderApp("/sources/indexers");
    const card = await identityCard();
    expect(within(card).getByLabelText("TV search")).toHaveValue("ether");
    expect(within(card).getByLabelText("Movie search")).toHaveValue("ether");
    expect(within(card).getByLabelText("TV grab")).toHaveValue("same");
    expect(within(card).getByLabelText("Movie grab")).toHaveValue("same");
    expect(within(card).getAllByRole("option", { name: "Same as search" })).toHaveLength(2);
    expect(within(card).getByText("3 indexers inherit this policy")).toBeInTheDocument();
    expect(
      within(card).getByText(/Indexer overrides take precedence over global values/),
    ).toBeInTheDocument();
    const tvSearch = within(card).getByLabelText("TV search");
    expect(within(tvSearch).getByRole("option", { name: "Ether (Ether/dev)" })).toBeInTheDocument();
  });

  it("saves the standard Servarr stack in one policy request", async () => {
    const { fake } = renderApp("/sources/indexers");
    const card = await identityCard();
    fireEvent.click(within(card).getByRole("button", { name: "Standard Servarr stack" }));
    await waitFor(() => expect(calls(fake, "PUT", "/config/clientIdentity")).toHaveLength(1));
    expect(calls(fake, "PUT", "/config/clientIdentity")[0]!.body).toMatchObject({
      searchTv: "sonarr",
      searchMovies: "radarr",
      grabTv: "same",
      grabMovies: "same",
    });
    await waitFor(() => expect(within(card).getByLabelText("TV search")).toHaveValue("sonarr"));
  });

  it("inherits in the drawer and previews an override", async () => {
    const { fake } = renderApp("/sources/indexers");
    fireEvent.click(await screen.findByRole("button", { name: "Edit Indexer One" }, WAIT));
    const drawer = await screen.findByRole("dialog", { name: "Indexer One" });
    const search = within(drawer).getByLabelText("Search identity");
    const grab = within(drawer).getByLabelText("Grab identity");
    expect(search).toHaveValue("");
    expect(
      within(search).getByRole("option", { name: "Inherit (TV Ether, movies Ether)" }),
    ).toBeInTheDocument();
    expect(
      within(search).getByRole("option", {
        name: "Prowlarr (Prowlarr/2.7.0.5723 (alpine 3.24.2))",
      }),
    ).toBeInTheDocument();
    expect(grab).toHaveValue("");
    expect(
      within(grab).getByRole("option", { name: "Inherit (TV Ether, movies Ether)" }),
    ).toBeInTheDocument();
    expect(
      within(drawer).getByText(
        "Effective: TV search Ether, movies search Ether; TV grab Ether, movies grab Ether.",
      ),
    ).toBeInTheDocument();

    fireEvent.change(search, { target: { value: "prowlarr" } });
    expect(
      within(drawer).getByText(
        "Effective: TV search Prowlarr, movies search Prowlarr; TV grab Prowlarr, movies grab Prowlarr.",
      ),
    ).toBeInTheDocument();

    fireEvent.change(grab, { target: { value: "custom" } });
    expect(within(drawer).getByLabelText("User agent")).toBeInTheDocument();
    expect(within(drawer).getByRole("button", { name: "Add header" })).toBeInTheDocument();

    fireEvent.click(within(drawer).getByRole("button", { name: "Save indexer" }));
    await waitFor(() => expect(calls(fake, "PUT", "/config/indexers")).toHaveLength(1));
    const saved = calls(fake, "PUT", "/config/indexers")[0]!.body as Record<string, unknown>[];
    expect(saved[0]).toMatchObject({ searchIdentity: "prowlarr", grabIdentity: "custom" });
  });

  it("keeps a stored custom pair while no selector uses it", async () => {
    const { fake } = renderApp("/sources/indexers", {}, (fake) => {
      fake.state.config.clientIdentity = {
        searchTv: "sonarr",
        searchMovies: "radarr",
        grabTv: "same",
        grabMovies: "same",
        userAgent: "Stored/1.0",
        headers: { "X-Key": "kept" },
      };
    });
    const card = await identityCard();
    fireEvent.change(within(card).getByLabelText("TV search"), { target: { value: "ether" } });
    fireEvent.click(within(card).getByRole("button", { name: "Save client identity" }));
    await waitFor(() => expect(calls(fake, "PUT", "/config/clientIdentity")).toHaveLength(1));
    const body = calls(fake, "PUT", "/config/clientIdentity")[0]!.body as Record<string, unknown>;
    expect(body.userAgent).toBe("Stored/1.0");
    expect(body.headers).toEqual({ "X-Key": "kept" });
  });

  it("hides identity overrides on addons that never use them", async () => {
    renderApp("/sources/indexers");
    fireEvent.click(await screen.findByRole("button", { name: "Edit Stream Addon" }, WAIT));
    const drawer = await screen.findByRole("dialog", { name: "Stream Addon" });
    expect(within(drawer).queryByLabelText("Search identity")).toBeNull();
  });

  it("keeps a stored indexer custom pair when an override is not custom", async () => {
    const { fake } = renderApp("/sources/indexers", {}, (fake) => {
      const ix = fake.state.config.indexers![0]!;
      ix.userAgent = "Indexer/1.0";
      ix.headers = { "X-Key": "kept" };
    });
    fireEvent.click(await screen.findByRole("button", { name: "Edit Indexer One" }, WAIT));
    const drawer = await screen.findByRole("dialog", { name: "Indexer One" });
    fireEvent.change(within(drawer).getByLabelText("Search identity"), {
      target: { value: "prowlarr" },
    });
    fireEvent.click(within(drawer).getByRole("button", { name: "Save indexer" }));
    await waitFor(() => expect(calls(fake, "PUT", "/config/indexers")).toHaveLength(1));
    const saved = calls(fake, "PUT", "/config/indexers")[0]!.body as Record<string, unknown>[];
    expect(saved[0]).toMatchObject({
      searchIdentity: "prowlarr",
      userAgent: "Indexer/1.0",
      headers: { "X-Key": "kept" },
    });
  });
});
