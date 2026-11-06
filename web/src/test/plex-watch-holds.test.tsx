import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { PlexWatchSync } from "../pages/viewers/sections/PlexWatchSync";

function mockFetch(handler: (path: string, init?: RequestInit) => unknown) {
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const raw = typeof input === "string" ? input : input.toString();
      const path = raw.replace(/^\/ether/, "");
      const body = handler(path, init);
      return new Response(JSON.stringify(body), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      });
    }),
  );
}

describe("Plex watch state actions", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
  });
  it("offers import and export without an expansion or eligibility prose", async () => {
    const calls: string[] = [];
    mockFetch((path, init) => {
      calls.push(`${init?.method ?? "GET"} ${path}`);
      return {
        connected: true,
        direction: 1,
        importInterval: 10,
        skipped: 0,
        held: 2,
        eligibility: "allowed",
      };
    });
    render(<PlexWatchSync viewerId={7} plexId={77} />);
    const importButton = await screen.findByRole("button", { name: "Import now" });
    await userEvent.hover(screen.getByRole("button", { name: "About Plex watch state" }));
    expect(await screen.findByRole("tooltip")).toHaveTextContent(
      "Uploading existing Ether state is a separate action.",
    );
    expect(screen.queryByText("Plex eligibility")).not.toBeInTheDocument();
    expect(document.querySelector("details")).toBeNull();
    await userEvent.click(importButton);
    await waitFor(() => expect(calls).toContain("POST /api/viewers/7/plex-watch/import"));
    await userEvent.click(screen.getByRole("button", { name: "Export current state" }));
    await waitFor(() => expect(calls).toContain("POST /api/viewers/7/plex-watch/export"));
  });
  it("offers Resolve and Retry only while sends are held or failed", async () => {
    const calls: string[] = [];
    let status = {
      connected: true,
      direction: 1,
      importInterval: 10,
      skipped: 0,
      held: 2,
      heldLegacy: 2,
      dead: 1,
    };
    mockFetch((path, init) => {
      calls.push(`${init?.method ?? "GET"} ${path}`);
      if (path.endsWith("/resolve")) status = { ...status, held: 0, heldLegacy: 0 };
      if (path.endsWith("/retry")) status = { ...status, dead: 0 };
      return status;
    });
    render(<PlexWatchSync viewerId={7} plexId={77} />);
    const resolve = await screen.findByRole("button", { name: "Resolve 2 held changes" });
    await userEvent.hover(resolve.parentElement!);
    expect(await screen.findByRole("tooltip")).toHaveTextContent("Older changes are held");
    await userEvent.click(resolve);
    await waitFor(() => expect(calls).toContain("POST /api/viewers/7/plex-watch/resolve"));
    await waitFor(() =>
      expect(screen.queryByRole("button", { name: /held change/ })).not.toBeInTheDocument(),
    );
    await userEvent.click(screen.getByRole("button", { name: "Retry failed items" }));
    await waitFor(() => expect(calls).toContain("POST /api/viewers/7/plex-watch/retry"));
    await waitFor(() =>
      expect(screen.queryByRole("button", { name: "Retry failed items" })).not.toBeInTheDocument(),
    );
  });
  it("requires connection before importing or exporting", async () => {
    mockFetch(() => ({ connected: false, direction: 0, importInterval: 10, skipped: 0 }));
    render(<PlexWatchSync viewerId={7} plexId={77} />);
    expect(await screen.findByRole("button", { name: "Import now" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Export current state" })).toBeDisabled();
    await userEvent.hover(screen.getByRole("button", { name: "Import now" }).parentElement!);
    expect(await screen.findByRole("tooltip")).toHaveTextContent("Connect the Plex account first");
    expect(
      screen.getByRole("button", { name: "Connect this viewer's Plex account" }),
    ).toBeEnabled();
  });
  it("shows sync as unavailable while keeping Disconnect", async () => {
    const calls: string[] = [];
    let status = {
      connected: true,
      username: "viewer",
      accountMatch: true,
      direction: 3,
      importInterval: 10,
      skipped: 0,
      held: 1,
      dead: 2,
      problem: "unsupported",
      eligibility: "unsupported",
    };
    mockFetch((path, init) => {
      calls.push(`${init?.method ?? "GET"} ${path}`);
      if (path.endsWith("/disconnect")) status = { ...status, connected: false, username: "" };
      return status;
    });
    render(<PlexWatchSync viewerId={7} plexId={77} />);
    expect(await screen.findByText("Unavailable")).toBeInTheDocument();
    expect(
      screen.getByText(/Sync watched marks with other Plex Media Servers you run alongside Ether/),
    ).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Manage Plex watch sync" })).toHaveAttribute(
      "href",
      "/ether/services/plexservers",
    );
    for (const name of [/Import now/, /Export current state/, /Retry/, /Resolve/, /Connect this/]) {
      expect(screen.queryByRole("button", { name })).not.toBeInTheDocument();
    }
    expect(screen.queryByRole("combobox")).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Disconnect" }));
    await waitFor(() => expect(calls).toContain("POST /api/viewers/7/plex-watch/disconnect"));
    await waitFor(() =>
      expect(screen.queryByRole("button", { name: "Disconnect" })).not.toBeInTheDocument(),
    );
  });
});
