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

describe("PlexWatchSync holds", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
  });

  it("shows held sends separately from retryable delivery and resolves via /resolve", async () => {
    const calls: string[] = [];
    mockFetch((path, init) => {
      calls.push(`${init?.method ?? "GET"} ${path}`);
      if (path === "/api/viewers/7/plex-watch" && (!init?.method || init.method === "GET")) {
        return {
          connected: true,
          plexId: 77,
          username: "viewer7",
          enabled: true,
          eligibility: "allowed",
          revoked: false,
          pending: 0,
          dead: 1,
          held: 2,
          heldLegacy: 0,
          inFlight: false,
          skipped: 0,
          restricted: false,
          accountMatch: true,
        };
      }
      if (path === "/api/viewers/7/plex-watch/retry" && init?.method === "POST") {
        return {
          connected: true,
          plexId: 77,
          username: "viewer7",
          enabled: true,
          eligibility: "allowed",
          revoked: false,
          pending: 1,
          dead: 0,
          held: 2,
          heldLegacy: 0,
          inFlight: false,
          skipped: 0,
          restricted: false,
          accountMatch: true,
        };
      }
      if (path === "/api/viewers/7/plex-watch/resolve" && init?.method === "POST") {
        return {
          connected: true,
          plexId: 77,
          username: "viewer7",
          enabled: true,
          eligibility: "allowed",
          revoked: false,
          pending: 1,
          dead: 0,
          held: 0,
          heldLegacy: 0,
          inFlight: false,
          skipped: 0,
          restricted: false,
          accountMatch: true,
        };
      }
      throw new Error(`unexpected ${init?.method ?? "GET"} ${path}`);
    });

    render(<PlexWatchSync viewerId={7} plexId={77} />);
    await waitFor(() => expect(screen.getByText("Held sends: 2")).toBeInTheDocument());
    expect(
      screen.getByText("Unconfirmed sends are held until checked against Plex."),
    ).toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: "Retry failed items" }));
    await waitFor(() => expect(calls).toContain("POST /api/viewers/7/plex-watch/retry"));
    expect(screen.getByText("Held sends: 2")).toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: "Resolve" }));
    await waitFor(() => expect(calls).toContain("POST /api/viewers/7/plex-watch/resolve"));
    await waitFor(() => expect(screen.queryByText("Held sends: 2")).not.toBeInTheDocument());
  });

  it("explains legacy upgrade holds without offering a blind retry", async () => {
    mockFetch((path) => {
      if (path === "/api/viewers/7/plex-watch") {
        return {
          connected: true,
          plexId: 77,
          username: "viewer7",
          enabled: true,
          eligibility: "allowed",
          revoked: false,
          pending: 0,
          dead: 0,
          held: 1,
          heldLegacy: 1,
          inFlight: false,
          skipped: 0,
          restricted: false,
          accountMatch: true,
        };
      }
      throw new Error(`unexpected ${path}`);
    });

    render(<PlexWatchSync viewerId={7} plexId={77} />);
    await waitFor(() => expect(screen.getByText("Held sends: 1")).toBeInTheDocument());
    expect(
      screen.getByText("Legacy undelivered changes are held until checked against Plex."),
    ).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Retry failed items" })).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Resolve" })).toBeInTheDocument();
  });
});
