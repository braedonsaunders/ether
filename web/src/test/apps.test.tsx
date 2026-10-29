import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { describe, expect, it, vi } from "vitest";
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

describe("Apps", () => {
  it("shows live status and toggles a registered protocol", async () => {
    let enabled = true;
    apiFetch.mockImplementation(async (path: string, options?: RequestInit) => {
      if (path === "/frontends" && !options?.method)
        return [
          {
            descriptor: {
              id: "silo",
              name: "Silo",
              certifiedApps: [
                { name: "Certified player", steps: ["openApp", "addServer", "unknown"] },
              ],
            },
            enabled,
            status: {
              addresses: ["192.168.1.2:8090"],
              connectUrls: ["http://192.168.1.2:8090"],
              lan: "reachable",
              wan: "unknown",
              clients24h: 2,
              clients24hKnown: true,
              liveStreams: 1,
              liveStreamsKnown: true,
              targetVersion: "1",
              conformance: "passing",
            },
          },
        ];
      if (path === "/frontends/silo/enabled") {
        enabled = false;
        return { ok: true };
      }
      throw new Error(`Unexpected request: ${path}`);
    });
    render(
      <MemoryRouter initialEntries={["/apps"]}>
        <Routes>
          <Route path="/apps/:tab?" element={<Apps />} />
        </Routes>
      </MemoryRouter>,
    );
    expect(await screen.findByText("192.168.1.2:8090")).toBeInTheDocument();
    expect(screen.getByRole("tab", { name: "Silo" })).toBeInTheDocument();
    expect(screen.queryByRole("tab", { name: "Stremio" })).not.toBeInTheDocument();
    expect(screen.queryByRole("tab", { name: "Files" })).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Connect a device" }));
    expect(screen.getByText("Certified player")).toBeInTheDocument();
    expect(screen.getByText("Open the app on your device.")).toBeInTheDocument();
    expect(screen.queryByText("unknown")).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("switch", { name: "Silo enabled" }));
    await waitFor(() =>
      expect(screen.getByRole("switch", { name: "Silo enabled" })).not.toBeChecked(),
    );
    expect(apiFetch).toHaveBeenCalledWith(
      "/frontends/silo/enabled",
      expect.objectContaining({ method: "PUT", body: JSON.stringify({ enabled: false }) }),
    );
  });
});
