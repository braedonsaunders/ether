import { ConfirmRoot } from "@braedonsaunders/appkit-ui";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { invalidateRegistry } from "../lib/extend";
import { createFakeServer } from "../mock/server";
import Plugins from "../pages/plugins/Plugins";

let fakeServer = createFakeServer({ latencyMs: 0 });
let customIntercept:
  ((url: URL, init?: RequestInit) => Promise<Response | null> | Response | null) | null = null;

function renderPlugins(initialEntry = "/plugins") {
  return render(
    <MemoryRouter initialEntries={[initialEntry]}>
      <ConfirmRoot />
      <Routes>
        <Route path="/plugins/:tab?/*" element={<Plugins />} />
      </Routes>
    </MemoryRouter>,
  );
}

describe("Server › Plugins (WS10a)", () => {
  const realFetch = window.fetch.bind(window);

  beforeEach(() => {
    invalidateRegistry();
    customIntercept = null;
    fakeServer = createFakeServer({ latencyMs: 0 });
    window.fetch = async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = new URL(
        input instanceof Request ? input.url : String(input),
        window.location.origin,
      );
      if (customIntercept) {
        const res = await customIntercept(url, init);
        if (res) return res;
      }
      if (url.origin === window.location.origin && url.pathname.startsWith("/ether/api/")) {
        return fakeServer.fetch(url.toString(), init);
      }
      return realFetch(input, init);
    };
  });

  afterEach(() => {
    window.fetch = realFetch;
    cleanup();
  });

  it("renders installed plugins with kind, health, version, update, Unverified, and shadow badges and toggles enable/disable", async () => {
    renderPlugins("/plugins");

    expect(await screen.findByText("Pluto TV")).toBeTruthy();
    expect(screen.getByText("Dub language")).toBeTruthy();
    expect(screen.getByText("Ether Wrapped")).toBeTruthy();

    // An installed Stremio add-on is listed with the plugins.
    expect(screen.getByText("Stream Addon")).toBeTruthy();

    expect(screen.getByText("Update v1.3.0")).toBeTruthy();
    expect(
      within(screen.getByRole("article", { name: "Dub language" })).getByText("Unverified"),
    ).toBeVisible();
    expect(screen.getByText("Trying out", { selector: "span *, span" })).toBeTruthy();

    const plutoSwitch = screen.getByRole("switch", { name: "Enable Pluto TV" }) as HTMLInputElement;
    expect(plutoSwitch.checked).toBe(true);

    fireEvent.click(plutoSwitch);
    await waitFor(() => {
      expect(plutoSwitch.checked).toBe(false);
    });

    fireEvent.click(plutoSwitch);
    await waitFor(() => {
      expect(plutoSwitch.checked).toBe(true);
    });
  });

  it("renders the Installed empty state explaining plugins in one sentence and offering the marketplace and Studio", async () => {
    customIntercept = (url, init) => {
      if (url.pathname === "/ether/api/plugins" && (!init?.method || init.method === "GET")) {
        return new Response(JSON.stringify({ plugins: [] }), {
          status: 200,
          headers: { "Content-Type": "application/json" },
        });
      }
      return null;
    };

    renderPlugins("/plugins");

    expect(await screen.findByText("No plugins installed")).toBeTruthy();
    expect(
      screen.getByText(
        "Plugins add sources, channels, subtitle providers and custom pages to Ether.",
      ),
    ).toBeTruthy();

    expect(screen.getByRole("button", { name: "Open the marketplace" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Add" })).toBeTruthy();
    expect(screen.getByRole("link", { name: /Build a plugin/i })).toBeTruthy();
  });

  it("opens the plugin drawer with Overview, what it adds, Settings and Activity, and keeps updates, undo and removal in the header", async () => {
    renderPlugins("/plugins");

    fireEvent.click(await screen.findByText("Pluto TV"));

    // Header and tabs: three tabs plus one named for what the plugin adds.
    const tabs = await screen.findByRole("tablist", { name: "Plugin sections" });
    expect(
      within(tabs)
        .getAllByRole("tab")
        .map((t) => t.textContent),
    ).toEqual(["Overview", "Channels", "Settings", "Activity"]);

    // Health tiles open the Activity tab, not the Overview.
    expect(screen.queryByText("Responding")).toBeNull();
    fireEvent.click(within(tabs).getByRole("tab", { name: "Activity" }));
    expect(await screen.findByText("Responding")).toBeTruthy();
    expect(screen.getByText("180 ms")).toBeTruthy();
    expect(screen.getByText("Slowest 720 ms")).toBeTruthy();

    // Update banner: new access is approved in the confirm step.
    expect(screen.getByText("Version 1.3.0 is available and asks for new access.")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Update" }));
    fireEvent.click(await screen.findByRole("button", { name: "Allow and update" }));
    await waitFor(() =>
      expect(screen.queryByText("Version 1.3.0 is available and asks for new access.")).toBeNull(),
    );

    // Setup: settings with validation and Test.
    fireEvent.click(within(tabs).getByRole("tab", { name: "Settings" }));
    expect(await screen.findByText("Region")).toBeTruthy();
    expect(
      screen.getByText("Saved. Leave as is to keep it, or type a new value to replace it."),
    ).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: /Test/i }));
    expect(await screen.findByText(/OK · \d+ ms/)).toBeTruthy();

    customIntercept = (url, init) => {
      if (
        url.pathname === "/ether/api/plugins/community.plutotv.ether/settings" &&
        init?.method === "PUT"
      ) {
        return fakeServer.fetch(url.toString(), {
          ...init,
          body: JSON.stringify({ settings: { region: "invalid", token: "Saved" } }),
        });
      }
      return null;
    };
    fireEvent.click(screen.getByRole("button", { name: /Save settings/i }));
    expect(
      (await screen.findAllByText(/This value is not supported\./)).length,
    ).toBeGreaterThanOrEqual(1);
    customIntercept = null;
    fireEvent.click(screen.getByRole("button", { name: /Save settings/i }));
    await waitFor(() => {
      expect(screen.queryAllByText(/This value is not supported\./)).toHaveLength(0);
    });

    // More options: auto-update and network access, out of the way.
    fireEvent.click(screen.getByRole("button", { name: "More options" }));
    const autoUpdate = screen.getByRole("switch", { name: "Auto-update" }) as HTMLInputElement;
    const before = autoUpdate.checked;
    fireEvent.click(autoUpdate);
    await waitFor(() => expect(autoUpdate.checked).toBe(!before));
    const network = screen.getByRole("switch", {
      name: "Allow: Connects to api.pluto.tv, *.pluto.tv",
    }) as HTMLInputElement;
    expect(network.checked).toBe(true);
    fireEvent.click(network);
    await waitFor(() => expect(network.checked).toBe(false));

    // The ⋯ menu offers roll back.
    fireEvent.click(screen.getByRole("button", { name: "More actions" }));
    expect(await screen.findByRole("menuitem", { name: /Roll back to v/ })).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "More actions" }));

    // Activity on Ether Wrapped: changes undo back to a point.
    fireEvent.click(screen.getByText("Ether Wrapped"));
    fireEvent.click(await screen.findByRole("tab", { name: "Activity" }));
    expect(await screen.findByText("Changes it made")).toBeTruthy();
    expect(screen.getAllByText(/Your year in viewing/).length).toBeGreaterThanOrEqual(1);
    fireEvent.click(screen.getAllByRole("button", { name: /Undo back to here/ })[0]!);
    fireEvent.click(await screen.findByRole("button", { name: "Undo" }));
    expect((await screen.findAllByText("Undone")).length).toBeGreaterThanOrEqual(1);

    // Removal lives in the ⋯ menu behind a confirm dialog.
    fireEvent.click(screen.getByRole("button", { name: "More actions" }));
    fireEvent.click(await screen.findByRole("menuitem", { name: /Remove/ }));
    expect(await screen.findByText("Remove Ether Wrapped?")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: /^Remove$/ }));
    await waitFor(() => expect(screen.queryByText("Changes it made")).toBeNull());
  });

  it("installs a plugin from a link after previewing its permission sheet and opens the installed drawer", async () => {
    renderPlugins("/plugins");
    await screen.findByText("Pluto TV");

    fireEvent.click(screen.getByRole("button", { name: "Add" }));
    fireEvent.click(await screen.findByRole("menuitem", { name: /From a link/i }));
    const urlInput = await screen.findByLabelText("Plugin address");
    fireEvent.change(urlInput, {
      target: { value: "https://plugins.example.test/iptv/manifest.json" },
    });

    fireEvent.click(screen.getByRole("button", { name: /Review permissions/i }));

    // PermissionSheet appears with Unverified warning and plain-language permissions
    const sheet = await screen.findByTestId("permission-sheet");
    expect(within(sheet).getByText("Not reviewed by Ether")).toBeTruthy();
    expect(within(sheet).getByText("Connects to plugins.example.test")).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: /^Install$/i }));

    // Installing closes InstallDrawer, adds Preview plugin to the list, and opens its drawer
    expect(await screen.findByRole("tablist", { name: "Plugin sections" })).toBeTruthy();
    expect(screen.getAllByText("Preview plugin").length).toBeGreaterThanOrEqual(1);
  });

  it("browses, filters, and installs from the Marketplace tab, and shows 'The catalogue is not available yet' when empty", async () => {
    renderPlugins("/plugins/marketplace");

    expect(await screen.findByText("SubDL Subtitles")).toBeTruthy();
    expect(screen.getByText("Local folders")).toBeTruthy();

    // Search filter
    const searchInput = screen.getByLabelText("Search catalogue");
    fireEvent.change(searchInput, { target: { value: "SubDL" } });
    expect(screen.getByText("SubDL Subtitles")).toBeTruthy();
    expect(screen.queryByText("Local folders")).toBeNull();

    // Open SubDL Subtitles card drawer and install
    fireEvent.click(screen.getByText("SubDL Subtitles"));
    const sheet = await screen.findByTestId("permission-sheet");
    expect(within(sheet).getByText("Connects to api.subdl.com")).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: /^Install$/i }));

    // Navigates to Installed tab and opens the new plugin's drawer
    expect(await screen.findByRole("tablist", { name: "Plugin sections" })).toBeTruthy();
  });

  it("pairs and revokes a remote sidecar", async () => {
    let revoked = false;
    customIntercept = async (url, init) => {
      const base = "/ether/api/plugins/community.plutotv.ether";
      if (url.pathname === base && (!init?.method || init.method === "GET")) {
        const response = await fakeServer.fetch(url.toString(), init);
        const detail = await response.json();
        detail.manifest.ether.runtime = { kind: "remote" };
        return Response.json(detail);
      }
      if (url.pathname === `${base}/pairings` && (!init?.method || init.method === "GET")) {
        return Response.json({
          pairings: revoked
            ? []
            : [{ id: "sidecar-1", created: 1700000000000, lastUsed: 0, expires: 1900000000000 }],
        });
      }
      if (url.pathname === `${base}/pairings/sidecar-1` && init?.method === "DELETE") {
        revoked = true;
        return Response.json({ status: "revoked" });
      }
      return null;
    };
    renderPlugins();
    fireEvent.click(await screen.findByText("Pluto TV"));
    fireEvent.click(await screen.findByRole("button", { name: "Connect a sidecar" }));
    expect(await screen.findByText(/PAIR-1234/)).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Revoke" }));
    expect(await screen.findByText("No sidecars connected.")).toBeTruthy();
  });

  it("shows 'The catalogue is not available yet' when GET /api/plugins/catalogue is empty or 404", async () => {
    customIntercept = (url) => {
      if (url.pathname.startsWith("/ether/api/plugins/catalogue")) {
        return new Response(JSON.stringify({ entries: [] }), {
          status: 200,
          headers: { "Content-Type": "application/json" },
        });
      }
      return null;
    };

    renderPlugins("/plugins/marketplace");

    expect(await screen.findByText("The catalogue is not available yet")).toBeTruthy();
  });
});
