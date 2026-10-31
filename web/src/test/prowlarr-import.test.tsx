import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { ProwlarrImport } from "../components/ProwlarrImport";

function jsonResponse(body: unknown, status = 200) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

describe("Prowlarr import limits", () => {
  it("previews and imports without requiring plan quotas or imposing arbitrary caps", async () => {
    const fetch = vi.fn(async (url: unknown, init?: RequestInit) => {
      if (!init?.body) return jsonResponse({ config: {} });
      if (String(url).endsWith("/playback/prowlarr/tags")) {
        return jsonResponse({ tags: [] });
      }
      const importing = JSON.parse(init.body as string).import === true;
      return jsonResponse({
        indexers: [{ id: "test", name: "My indexer", dailyLimit: 0, dailyGrabs: 0 }],
        imported: importing,
        skippedUnknownProtocol: 1,
        warnings: [
          "skipped 1 Prowlarr indexer(s) with an unsupported protocol (only usenet/torrent can be imported)",
        ],
      });
    });
    vi.stubGlobal("fetch", fetch);
    render(<ProwlarrImport />);
    expect(screen.queryByLabelText(/Daily searches/)).not.toBeInTheDocument();
    expect(screen.queryByLabelText(/Daily downloads/)).not.toBeInTheDocument();
    fireEvent.change(screen.getByLabelText("Prowlarr URL"), {
      target: { value: "http://prowlarr.test:9696" },
    });
    fireEvent.change(screen.getByLabelText("Prowlarr API key"), { target: { value: "test-key" } });
    fireEvent.click(screen.getByRole("button", { name: "Preview indexers" }));
    await screen.findByText("My indexer · Automatic limits");
    // Preview surfaces skipped indexers and the warning behind them.
    expect(screen.getByText(/Skipped 1 indexer with an unsupported protocol/)).toBeInTheDocument();
    expect(screen.getByText("Worth knowing")).toBeInTheDocument();
    expect(
      screen.getByText(/skipped 1 Prowlarr indexer\(s\) with an unsupported protocol/),
    ).toBeInTheDocument();
    // Import is additive: the UI never reports removals.
    expect(screen.queryByText(/Removed \d+ previously imported/)).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Import indexers" }));
    await waitFor(() => expect(fetch).toHaveBeenCalledTimes(3));
    // Import keeps surfacing the skipped/warning diagnostics.
    expect(screen.getByText(/Skipped 1 indexer with an unsupported protocol/)).toBeInTheDocument();
    expect(screen.queryByText(/Removed \d+ previously imported/)).not.toBeInTheDocument();
    expect(
      fetch.mock.calls
        .map((call) => (call as unknown as [string, RequestInit])[1].body)
        .filter(Boolean)
        .map((body) => JSON.parse(body as string)),
    ).toEqual([
      {
        config: {
          url: "http://prowlarr.test:9696",
          apiKey: "test-key",
          dailyLimit: 0,
          dailyGrabs: 0,
          priority: 0,
        },
        import: false,
      },
      {
        config: {
          url: "http://prowlarr.test:9696",
          apiKey: "test-key",
          dailyLimit: 0,
          dailyGrabs: 0,
          priority: 0,
        },
        import: true,
      },
    ]);
  });

  it("sends chosen tags and shows the last indexers when Prowlarr is unreachable", async () => {
    const fetch = vi.fn(async (url: unknown, init?: RequestInit) => {
      const path = String(url);
      if (!init?.body) {
        return jsonResponse({
          config: { url: "http://prowlarr.test:9696", apiKey: "••••", tags: [] },
          status: "unreachable",
          kept: true,
        });
      }
      if (path.endsWith("/playback/prowlarr/tags")) {
        return jsonResponse({ tags: [{ id: 7, label: "ether" }] });
      }
      const parsed = JSON.parse(init.body as string) as {
        import?: boolean;
        config?: { tags?: number[] };
      };
      if (parsed.import) {
        return jsonResponse({
          indexers: [{ id: "kept", name: "Kept indexer", dailyLimit: 0, dailyGrabs: 0 }],
          imported: false,
          status: "unreachable",
          kept: true,
        });
      }
      return jsonResponse({
        indexers: [{ id: "kept", name: "Kept indexer", dailyLimit: 0, dailyGrabs: 0 }],
        imported: false,
        status: "ok",
      });
    });
    vi.stubGlobal("fetch", fetch);
    render(<ProwlarrImport />);
    expect(await screen.findByText("Prowlarr is unreachable")).toBeInTheDocument();
    expect(screen.getByText("Still using the last indexers.")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Indexer tags" }));
    fireEvent.click(await screen.findByRole("option", { name: "ether" }));
    fireEvent.click(screen.getByRole("button", { name: "Preview indexers" }));
    await screen.findByText("Kept indexer · Automatic limits");
    fireEvent.click(screen.getByRole("button", { name: "Import indexers" }));
    await waitFor(() =>
      expect(screen.getAllByText("Still using the last indexers.").length).toBeGreaterThan(0),
    );
    expect(screen.queryByText("Prowlarr indexers imported")).not.toBeInTheDocument();
    const imports = fetch.mock.calls
      .map((call) => (call as unknown as [string, RequestInit])[1].body)
      .filter(Boolean)
      .map(
        (body) => JSON.parse(body as string) as { import?: boolean; config?: { tags?: number[] } },
      )
      .filter((body) => body.import !== undefined);
    expect(imports.every((body) => body.config?.tags?.includes(7))).toBe(true);
  });
});
