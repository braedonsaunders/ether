import { ConfirmRoot, Toaster } from "@braedonsaunders/appkit-ui";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { ProwlarrImport } from "../components/ProwlarrImport";

describe("Prowlarr import limits", () => {
  it("previews and imports without requiring plan quotas or imposing arbitrary caps", async () => {
    const fetch = vi.fn(async (_url: unknown, init?: RequestInit) => {
      const importing = JSON.parse(init?.body as string).import === true;
      return new Response(
        JSON.stringify({
          indexers: [{ id: "test", name: "My indexer", dailyLimit: 0, dailyGrabs: 0 }],
          imported: importing,
          skippedUnknownProtocol: 1,
          warnings: [
            "skipped 1 Prowlarr indexer(s) with an unsupported protocol (only usenet/torrent can be imported)",
          ],
        }),
        { headers: { "Content-Type": "application/json" } },
      );
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
    await waitFor(() => expect(fetch).toHaveBeenCalledTimes(2));
    // Import keeps surfacing the skipped/warning diagnostics.
    expect(screen.getByText(/Skipped 1 indexer with an unsupported protocol/)).toBeInTheDocument();
    expect(screen.queryByText(/Removed \d+ previously imported/)).not.toBeInTheDocument();
    expect(
      fetch.mock.calls.map((call) =>
        JSON.parse((call as unknown as [string, RequestInit])[1].body as string),
      ),
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
});

describe("Prowlarr explicit sync", () => {
  function syncBackend() {
    const fetch = vi.fn(async (_url: unknown, init?: RequestInit) => {
      const body = JSON.parse(init?.body as string) as { import?: boolean; sync?: boolean };
      const removals = [{ id: "prowlarr-old", name: "Old indexer" }];
      const keptEdited = [{ id: "prowlarr-edited", name: "Edited indexer" }];
      return new Response(
        JSON.stringify(
          body.sync
            ? {
                indexers: [{ id: "test", name: "My indexer", dailyLimit: 0, dailyGrabs: 0 }],
                imported: body.import === true,
                removals,
                removed: body.import ? 1 : 0,
                keptEdited,
                approvedRemovals: ["prowlarr-old"],
                plan: "plan-1",
                warnings: [],
              }
            : {
                indexers: [{ id: "test", name: "My indexer", dailyLimit: 0, dailyGrabs: 0 }],
                imported: body.import === true,
              },
        ),
        { headers: { "Content-Type": "application/json" } },
      );
    });
    vi.stubGlobal("fetch", fetch);
    return fetch;
  }

  function renderSync() {
    render(
      <>
        <ProwlarrImport />
        <ConfirmRoot />
        <Toaster />
      </>,
    );
    fireEvent.change(screen.getByLabelText("Prowlarr URL"), {
      target: { value: "http://prowlarr.test:9696" },
    });
    fireEvent.change(screen.getByLabelText("Prowlarr API key"), { target: { value: "test-key" } });
  }

  function bodies(fetch: ReturnType<typeof syncBackend>) {
    return fetch.mock.calls.map((call) =>
      JSON.parse((call as unknown as [string, RequestInit])[1].body as string),
    );
  }

  it("previews removals, asks for confirmation, then applies the sync bound to the preview", async () => {
    const fetch = syncBackend();
    renderSync();
    fireEvent.click(screen.getByRole("button", { name: "Sync from Prowlarr" }));
    expect(
      await screen.findByText(/Would remove 1 previously imported indexer/),
    ).toBeInTheDocument();
    expect(screen.getByText("Kept locally edited indexers")).toBeInTheDocument();
    const dialog = await screen.findByRole("dialog");
    expect(
      within(dialog).getByText(/This will remove 1 previously imported indexer/),
    ).toBeInTheDocument();
    expect(within(dialog).getByText(/Old indexer/)).toBeInTheDocument();
    expect(bodies(fetch)).toHaveLength(1);
    expect(bodies(fetch)[0]).toMatchObject({ import: false, sync: true });

    fireEvent.click(within(dialog).getByRole("button", { name: "Sync" }));
    await waitFor(() => expect(fetch).toHaveBeenCalledTimes(2));
    // The apply is bound to the preview: it echoes approvedRemovals + plan.
    expect(bodies(fetch)[1]).toMatchObject({
      import: true,
      sync: true,
      approvedRemovals: ["prowlarr-old"],
      plan: "plan-1",
    });
    expect(await screen.findByText(/Removed 1 previously imported indexer/)).toBeInTheDocument();
  });

  it("shows 'source changed' and refetches the preview when the apply returns 409", async () => {
    const base = syncBackend();
    const fetch = vi.fn(async (url: unknown, init?: RequestInit) => {
      const body = JSON.parse(init?.body as string) as { import?: boolean; sync?: boolean };
      if (body.sync && body.import) {
        return new Response("prowlarr sync plan is stale; review again", { status: 409 });
      }
      return base(url, init);
    });
    vi.stubGlobal("fetch", fetch);
    renderSync();
    fireEvent.click(screen.getByRole("button", { name: "Sync from Prowlarr" }));
    const dialog = await screen.findByRole("dialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "Sync" }));
    // Apply (409) is followed by a fresh sync preview, not another apply.
    await waitFor(() => expect(fetch).toHaveBeenCalledTimes(3));
    const sent = fetch.mock.calls.map((call) =>
      JSON.parse((call as unknown as [string, RequestInit])[1].body as string),
    );
    expect(sent[1]).toMatchObject({ import: true, sync: true, plan: "plan-1" });
    expect(sent[2]).toMatchObject({ import: false, sync: true });
    expect(await screen.findByText("Source changed, review again")).toBeInTheDocument();
    expect(
      await screen.findByText(/Would remove 1 previously imported indexer/),
    ).toBeInTheDocument();
  });

  it("applies nothing when the sync confirmation is cancelled", async () => {
    const fetch = syncBackend();
    renderSync();
    fireEvent.click(screen.getByRole("button", { name: "Sync from Prowlarr" }));
    const dialog = await screen.findByRole("dialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(fetch).toHaveBeenCalledTimes(1);
    expect(bodies(fetch)[0]).toMatchObject({ import: false, sync: true });
    expect(screen.queryByText(/Removed 1 previously imported indexer/)).not.toBeInTheDocument();
  });
});
