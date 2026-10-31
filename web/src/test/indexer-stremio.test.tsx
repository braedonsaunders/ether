import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import type { Indexer } from "../lib/types";
import { addonUrlHasDebridKey, normalizeAddonUrl } from "../pages/sources/Indexers";
import { calls, renderApp } from "./harness";

const WAIT = { timeout: 4000 };

async function openAddAddon() {
  fireEvent.click(await screen.findByRole("button", { name: /Add indexer/ }, WAIT));
  fireEvent.click(await screen.findByRole("menuitem", { name: "Stremio addon URL…" }));
  return screen.findByRole("dialog", { name: "New Stremio addon" });
}

describe("Sources › Indexers Stremio addons", () => {
  it("adds an addon from its stremio:// install link as a torrent source", async () => {
    const { fake } = renderApp("/sources/yours/indexers");
    const drawer = await openAddAddon();
    expect(within(drawer).getByText("Stremio addon · Torrent")).toBeInTheDocument();
    expect(within(drawer).queryByLabelText("API key")).toBeNull();
    expect(within(drawer).queryByText("Movie categories")).toBeNull();
    fireEvent.change(within(drawer).getByLabelText("Name"), { target: { value: "My Addon" } });
    fireEvent.change(within(drawer).getByLabelText("Addon URL"), {
      target: { value: "  stremio://addon.example/opts=1/manifest.json " },
    });
    expect(within(drawer).queryByText(/includes a debrid API key/)).toBeNull();

    fireEvent.click(within(drawer).getByRole("button", { name: /Test/ }));
    await waitFor(() => expect(calls(fake, "POST", "/test/indexer")).toHaveLength(1));
    expect(calls(fake, "POST", "/test/indexer")[0]!.body).toMatchObject({
      kind: "stremio",
      url: "https://addon.example/opts=1/manifest.json",
    });

    fireEvent.click(within(drawer).getByRole("button", { name: "Save indexer" }));
    await waitFor(() => expect(calls(fake, "PUT", "/config/indexers")).toHaveLength(1));
    const body = calls(fake, "PUT", "/config/indexers")[0]!.body as Indexer[];
    expect(body.at(-1)).toMatchObject({
      name: "My Addon",
      kind: "stremio",
      protocol: "torrent",
      url: "https://addon.example/opts=1/manifest.json",
      enabled: true,
    });
    const list = screen.getByRole("region", { name: "Indexers" });
    await waitFor(() => expect(within(list).getAllByText("Stremio addon")).toHaveLength(2));
  });

  it("warns when the link embeds a debrid key", async () => {
    renderApp("/sources/yours/indexers");
    const drawer = await openAddAddon();
    fireEvent.change(within(drawer).getByLabelText("Addon URL"), {
      target: { value: "https://addon.example/sort=size|realdebrid=ABC123/manifest.json" },
    });
    expect(await within(drawer).findByText(/includes a debrid API key/)).toBeVisible();
  });

  it("detects keys in plain and base64 addon configurations", () => {
    const config = btoa(JSON.stringify({ debridService: "torbox", debridApiKey: "k-123" }));
    expect(addonUrlHasDebridKey(`https://addon.example/${config}/manifest.json`)).toBe(true);
    const urlSafe = config.replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
    expect(addonUrlHasDebridKey(`https://addon.example/${urlSafe}/manifest.json`)).toBe(true);
    expect(
      addonUrlHasDebridKey("https://addon.example/providers=a,b%7CTorBox=xyz/manifest.json"),
    ).toBe(true);
    const keyless = btoa(JSON.stringify({ maxResults: 20, debridService: "" }));
    expect(addonUrlHasDebridKey(`https://addon.example/${keyless}/manifest.json`)).toBe(false);
    expect(addonUrlHasDebridKey("https://addon.example/qualityfilter=cam/manifest.json")).toBe(
      false,
    );
    expect(normalizeAddonUrl(" stremio://addon.example/manifest.json")).toBe(
      "https://addon.example/manifest.json",
    );
  });
});
