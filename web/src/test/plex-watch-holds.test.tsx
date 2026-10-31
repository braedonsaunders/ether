import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { apiFetch } from "../lib/api";
import { PlexWatchSync } from "../pages/viewers/sections/PlexWatchSync";

vi.mock("../lib/api", async (original) => ({
  ...(await original<typeof import("../lib/api")>()),
  apiFetch: vi.fn(),
}));

// ETHE-25: display the persisted held-send result honestly; the existing
// retry operation must not suggest or display that it released the hold.
// This is the Status shape from an accepted /state request with a lost reply.
const held = {
  connected: true,
  plexId: 42,
  username: "viewer",
  enabled: true,
  eligibility: "allowed",
  revoked: false,
  pending: 0,
  dead: 1,
  held: 1,
  heldLegacy: 0,
  inFlight: false,
  skipped: 0,
  restricted: false,
  accountMatch: true,
};

beforeEach(() => vi.mocked(apiFetch).mockReset());

describe("uncertain Plex sends", () => {
  it("keeps the held count and reason after retrying other failed items", async () => {
    vi.mocked(apiFetch).mockResolvedValue(held);
    render(<PlexWatchSync viewerId={2} plexId={42} />);
    expect(await screen.findByText("Held sends: 1")).toBeVisible();
    expect(screen.getByText(/An earlier send may have changed Plex/)).toBeVisible();
    fireEvent.click(screen.getByRole("button", { name: "Retry failed items" }));
    await waitFor(() =>
      expect(apiFetch).toHaveBeenCalledWith("/viewers/2/plex-watch/retry", {
        method: "POST",
        body: undefined,
      }),
    );
    expect(screen.getByText("Held sends: 1")).toBeVisible();
    expect(
      screen.getByText(
        "Retry checks eligible failed items. It never releases an uncertain-send hold.",
      ),
    ).toBeVisible();
  });

  it("explains that legacy unknown targets hold the entire affected account aspect", async () => {
    vi.mocked(apiFetch).mockResolvedValue({ ...held, heldLegacy: 1 });
    render(<PlexWatchSync viewerId={2} plexId={42} />);
    expect(await screen.findByText("Held sends: 1")).toBeVisible();
    expect(screen.getByText(/Old undelivered changes have no recorded target/)).toBeVisible();
    expect(screen.queryByText(/Other titles and playback continue/)).not.toBeInTheDocument();
  });
});
