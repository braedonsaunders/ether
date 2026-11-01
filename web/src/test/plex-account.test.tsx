import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { api, ApiError } from "../lib/api";
import { claimFailureMessage, plexAccountClaimed } from "../pages/apps/plex-account";
import { calls, renderApp } from "./harness";

const WAIT = { timeout: 8000 };

describe("plex account status", () => {
  it("is not claimed when the flag is set without a server token", () => {
    expect(plexAccountClaimed({ claimed: true, token: "" })).toBe(false);
    expect(plexAccountClaimed({ claimed: true })).toBe(false);
    expect(plexAccountClaimed({ claimed: true, token: "••••" })).toBe(true);
    expect(plexAccountClaimed({ claimed: false, token: "••••" })).toBe(false);
    expect(plexAccountClaimed(null)).toBe(false);
  });

  it("shows a clear message when plex.tv claim exchange fails", () => {
    expect(
      claimFailureMessage(new ApiError(502, "plex_claim_failed", "plextv: claim exchange failed")),
    ).toBe("Plex couldn't link this server. Try again in a minute.");
  });
});

describe("plex claim actions", () => {
  it("signs in and claims from the account page", async () => {
    const open = vi.spyOn(window, "open").mockReturnValue(null);
    const { fake } = renderApp("/apps/plex", {}, (server) => {
      server.state.config.plex.claimed = true;
      server.state.config.plex.token = "";
      server.state.config.plex.ownerUsername = "";
    });
    fireEvent.click(await screen.findByRole("button", { name: "Sign in & claim" }, WAIT));
    expect(await screen.findByText("WXYZ", {}, WAIT)).toBeInTheDocument();
    await waitFor(() => expect(calls(fake, "POST", "/plex/claim")).toHaveLength(1), WAIT);
    expect(calls(fake, "POST", "/plex/pin")).toHaveLength(1);
    expect(
      (await screen.findAllByText("Claimed by the Plex account owner.", {}, WAIT)).length,
    ).toBeGreaterThan(0);
    expect(screen.queryByRole("switch", { name: "Claimed" })).toBeNull();
    expect(screen.queryByLabelText("Owner token")).toBeNull();
    open.mockRestore();
  });

  it("shows the translated claim failure message when plex.tv rejects the exchange", async () => {
    const open = vi.spyOn(window, "open").mockReturnValue(null);
    const claim = vi
      .spyOn(api, "claim")
      .mockRejectedValueOnce(
        new ApiError(502, "plex_claim_failed", "Plex couldn't link this server. Try again in a minute."),
      );
    renderApp("/apps/plex", {}, (server) => {
      server.state.config.plex.claimed = false;
      server.state.config.plex.token = "";
      server.state.config.plex.ownerUsername = "";
    });
    fireEvent.click(await screen.findByRole("button", { name: "Sign in & claim" }, WAIT));
    expect(
      await screen.findByText("Plex couldn't link this server. Try again in a minute.", {}, WAIT),
    ).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Claim server" })).toBeInTheDocument();
    claim.mockRestore();
    open.mockRestore();
  });

  it("does not sign the server out when confirmation is cancelled", async () => {
    const { fake } = renderApp("/apps/plex");
    fireEvent.click(await screen.findByRole("button", { name: "Sign out of Plex" }, WAIT));
    const dialog = await screen.findByRole("dialog", {}, WAIT);
    fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
    expect(calls(fake, "PUT", "/config/plex")).toHaveLength(0);
    expect(screen.getAllByText("Claimed by the Plex account owner.").length).toBeGreaterThan(0);
  });

  it("signs the server out after confirmation", async () => {
    const { fake } = renderApp("/apps/plex");
    fireEvent.click(await screen.findByRole("button", { name: "Sign out of Plex" }, WAIT));
    const dialog = await screen.findByRole("dialog", {}, WAIT);
    fireEvent.click(within(dialog).getByRole("button", { name: "Sign out of Plex" }));
    await waitFor(() => expect(calls(fake, "PUT", "/config/plex")).toHaveLength(1), WAIT);
    expect(calls(fake, "PUT", "/config/plex")[0]!.body).toMatchObject({ claimed: false });
    expect(
      await screen.findByRole("button", { name: "Sign in & claim" }, WAIT),
    ).toBeInTheDocument();
    expect(screen.queryByLabelText("Owner token")).toBeNull();
    expect(screen.queryByRole("switch", { name: "Claimed" })).toBeNull();
  });

  it("keeps an unsaved network setting while switching Plex sections", async () => {
    renderApp("/apps/plex/remote");
    const port = await screen.findByRole("spinbutton", { name: "Port" }, WAIT);
    fireEvent.change(port, { target: { value: "32450" } });
    fireEvent.click(screen.getByRole("tab", { name: "Account" }));
    expect(
      await screen.findByRole("button", { name: "Sign out of Plex" }, WAIT),
    ).toBeInTheDocument();
    expect(screen.queryByRole("spinbutton", { name: "Port" })).toBeNull();
    fireEvent.click(screen.getByRole("tab", { name: "Remote access" }));
    expect(await screen.findByRole("spinbutton", { name: "Port" }, WAIT)).toHaveValue(32450);
    expect(screen.getByRole("region", { name: "Unsaved changes" })).toBeVisible();
  });
});
