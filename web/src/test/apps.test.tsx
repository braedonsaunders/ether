import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes, useLocation } from "react-router-dom";
import { beforeEach, describe, expect, it, vi } from "vitest";
import Apps from "../pages/apps/Apps";

const apiFetch = vi.fn();
vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return {
    ...actual,
    apiFetch: (...args: unknown[]) => apiFetch(...args),
    apiBlob: () => Promise.reject(new Error("QR unavailable")),
  };
});
vi.mock("../app-context", () => ({
  useApp: () => ({
    dashboard: { sections: { remote: {} } },
    reloadBoot: vi.fn(),
  }),
}));

type AppRow = {
  descriptor: {
    id: string;
    name: string;
    certifiedApps?: { name: string; steps: string[] }[];
  };
  enabled: boolean;
  status: {
    addresses: string[];
    connectUrls?: string[];
    lan: string;
    wan: string;
    clients24h: number;
    clients24hKnown: boolean;
    liveStreams: number;
    liveStreamsKnown: boolean;
    targetVersion: string;
    conformance: string;
    error?: string;
  };
};

function row(id: string, name: string, enabled: boolean, extra?: Partial<AppRow>): AppRow {
  return {
    descriptor: { id, name, ...extra?.descriptor },
    enabled,
    status: {
      addresses: enabled ? ["192.168.1.2:8090"] : [],
      connectUrls: ["http://192.168.1.2:8090"],
      lan: "reachable",
      wan: "unknown",
      clients24h: 1,
      clients24hKnown: true,
      liveStreams: 0,
      liveStreamsKnown: true,
      targetVersion: "1",
      conformance: "passing",
      ...extra?.status,
    },
  };
}

function LocationProbe() {
  const location = useLocation();
  return <div data-testid="loc">{`${location.pathname}${location.search}`}</div>;
}

function renderApps(entry: string) {
  return render(
    <MemoryRouter initialEntries={[entry]}>
      <LocationProbe />
      <Routes>
        <Route path="/apps/:tab?" element={<Apps />} />
      </Routes>
    </MemoryRouter>,
  );
}

describe("Apps", () => {
  beforeEach(() => {
    apiFetch.mockReset();
  });

  it("shows live status and toggles a registered protocol", async () => {
    let enabled = true;
    apiFetch.mockImplementation(async (path: string, options?: RequestInit) => {
      if (path === "/frontends" && !options?.method) {
        return [
          row("silo", "Silo", enabled, {
            descriptor: {
              id: "silo",
              name: "Silo",
              certifiedApps: [
                { name: "Certified player", steps: ["openApp", "addServer", "unknown"] },
              ],
            },
          }),
        ];
      }
      if (path === "/frontends/silo/enabled") {
        enabled = false;
        return { ok: true };
      }
      if (path === "/frontends/silo/settings") return { port: 8090 };
      throw new Error(`Unexpected request: ${path}`);
    });
    renderApps("/apps");
    expect(await screen.findByRole("switch", { name: "Silo enabled" })).toBeChecked();
    expect(screen.getByText("On")).toBeInTheDocument();
    expect(screen.queryByText("192.168.1.2:8090")).not.toBeInTheDocument();
    expect(screen.queryByText(/LAN:/)).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Connect a device" })).not.toBeInTheDocument();
    expect(screen.queryByRole("spinbutton", { name: "Port" })).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("tab", { name: "Silo" }));
    expect(await screen.findByText("192.168.1.2:8090")).toBeInTheDocument();
    expect(screen.getByText(/LAN:/)).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Connect a device" }));
    expect(screen.getByText("Certified player")).toBeInTheDocument();
    expect(screen.getByText("Open the app on your device.")).toBeInTheDocument();
    expect(screen.queryByText("unknown")).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("tab", { name: "Overview" }));
    fireEvent.click(screen.getByRole("switch", { name: "Silo enabled" }));
    await waitFor(() =>
      expect(screen.getByRole("switch", { name: "Silo enabled" })).not.toBeChecked(),
    );
    expect(apiFetch).toHaveBeenCalledWith("/frontends/silo/enabled", {
      method: "PUT",
      body: JSON.stringify({ enabled: false }),
    });
    expect(screen.queryByRole("tab", { name: "Silo" })).not.toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "Settings" })).not.toBeInTheDocument();
  });

  it("reveals a config tab when an app is enabled and keeps forms off the overview", async () => {
    let enabled = false;
    apiFetch.mockImplementation(async (path: string, options?: RequestInit) => {
      if (path === "/frontends" && !options?.method) return [row("jellyfin", "Jellyfin", enabled)];
      if (path === "/frontends/jellyfin/enabled") {
        enabled = true;
        return { ok: true };
      }
      if (path === "/frontends/jellyfin/settings") return { serverName: "House", port: 8096 };
      throw new Error(`Unexpected request: ${path}`);
    });
    renderApps("/apps");
    expect(await screen.findByRole("switch", { name: "Jellyfin enabled" })).not.toBeChecked();
    expect(screen.queryByRole("tab", { name: "Jellyfin" })).not.toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "Settings" })).not.toBeInTheDocument();
    expect(screen.queryByRole("spinbutton", { name: "Port" })).not.toBeInTheDocument();
    expect(screen.queryByRole("textbox", { name: "Server name" })).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("switch", { name: "Jellyfin enabled" }));
    expect(await screen.findByRole("tab", { name: "Jellyfin" })).toBeInTheDocument();
    expect(screen.queryByRole("spinbutton", { name: "Port" })).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("tab", { name: "Jellyfin" }));
    expect(await screen.findByRole("spinbutton", { name: "Port" })).toBeInTheDocument();
  });

  it("sends a disabled app's deep link back to the overview", async () => {
    apiFetch.mockImplementation(async (path: string) => {
      if (path === "/frontends") return [row("jellyfin", "Jellyfin", false)];
      throw new Error(`Unexpected request: ${path}`);
    });
    renderApps("/apps/jellyfin");
    await waitFor(() => expect(screen.getByTestId("loc")).toHaveTextContent("/apps/overview"));
    expect(screen.queryByRole("tab", { name: "Jellyfin" })).not.toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "Settings" })).not.toBeInTheDocument();
    expect(screen.queryByRole("spinbutton", { name: "Port" })).not.toBeInTheDocument();
  });

  it("sends a disabled Plex route back to the overview", async () => {
    apiFetch.mockImplementation(async (path: string) => {
      if (path === "/frontends") return [row("plex", "Plex", false)];
      throw new Error(`Unexpected request: ${path}`);
    });
    const { unmount } = renderApps("/apps/plex");
    await waitFor(() => expect(screen.getByTestId("loc")).toHaveTextContent("/apps/overview"));
    expect(screen.queryByRole("tab", { name: "Plex" })).not.toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "Settings" })).not.toBeInTheDocument();
    expect(screen.getByText("Off")).toBeInTheDocument();
    unmount();
    renderApps("/apps/secure");
    await waitFor(() => expect(screen.getByTestId("loc")).toHaveTextContent("/apps/overview"));
    expect(screen.queryByLabelText("Secure connections")).not.toBeInTheDocument();
  });

  it("keeps the previous switch state when enabling fails", async () => {
    apiFetch.mockImplementation(async (path: string, options?: RequestInit) => {
      if (path === "/frontends" && !options?.method) return [row("silo", "Silo", true)];
      if (path === "/frontends/silo/enabled") throw new Error("enable failed");
      throw new Error(`Unexpected request: ${path}`);
    });
    renderApps("/apps");
    const toggle = await screen.findByRole("switch", { name: "Silo enabled" });
    expect(toggle).toBeChecked();
    fireEvent.click(toggle);
    expect(await screen.findByRole("alert")).toHaveTextContent("enable failed");
    expect(screen.getByRole("switch", { name: "Silo enabled" })).toBeChecked();
    expect(screen.getByRole("tab", { name: "Silo" })).toBeInTheDocument();
  });
});
