import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { describe, expect, it, vi } from "vitest";
import App from "../App";
import { api, getToken } from "../lib/api";
import { createFakeServer } from "../mock/server";

const remoteBoot = {
  ok: true,
  version: "dev",
  setupComplete: true,
  authenticated: false,
  authRequired: true,
  serverName: "Ether",
};

function renderApp() {
  return render(
    <MemoryRouter>
      <App />
    </MemoryRouter>,
  );
}

describe("sign-in", () => {
  it("requires local-owner sign-in before setup is complete", async () => {
    const fake = createFakeServer();
    vi.stubGlobal("fetch", vi.fn(fake.fetch));
    const pendingSetup = {
      ...remoteBoot,
      setupComplete: false,
      ownerConfigured: true,
      signInMethods: ["local" as const],
    };
    vi.spyOn(api, "bootstrap")
      .mockResolvedValueOnce(pendingSetup)
      .mockResolvedValue({ ...pendingSetup, authenticated: true });
    const localSignIn = vi
      .spyOn(api, "localSignIn")
      .mockResolvedValue({ token: "local-owner-token" });
    const config = vi.spyOn(api, "getConfig");
    renderApp();
    expect(await screen.findByLabelText("Username")).toBeInTheDocument();
    expect(screen.getByText("Sign in to Ether")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Sign in with Plex" })).not.toBeInTheDocument();
    expect(screen.queryByText("Use a Plex token instead")).not.toBeInTheDocument();
    expect(config).not.toHaveBeenCalled();
    fireEvent.change(screen.getByLabelText("Username"), { target: { value: "owner" } });
    const password = screen.getByLabelText("Password");
    expect(password).toHaveAttribute("type", "password");
    expect(password).toHaveAttribute("autocomplete", "current-password");
    fireEvent.click(screen.getByRole("button", { name: "Show password" }));
    expect(password).toHaveAttribute("type", "text");
    expect(screen.getByRole("button", { name: "Hide password" })).toBeInTheDocument();
    fireEvent.change(password, { target: { value: "test-password" } });
    fireEvent.click(screen.getByRole("button", { name: /^Sign in$/ }));
    await waitFor(() => expect(localSignIn).toHaveBeenCalledWith("owner", "test-password"));
    expect(await screen.findByRole("button", { name: /Sign out/ })).toBeInTheDocument();
    expect(screen.getByText("Library")).toBeInTheDocument();
    expect(screen.queryByText("In Plex")).not.toBeInTheDocument();
    expect(getToken()).toBe("local-owner-token");
  });

  it("keeps Plex sign-in available for a configured Plex owner", async () => {
    vi.spyOn(api, "bootstrap").mockResolvedValue({ ...remoteBoot, signInMethods: ["plex"] });
    renderApp();
    expect(await screen.findByRole("button", { name: "Sign in with Plex" })).toBeInTheDocument();
    expect(screen.queryByLabelText("Username")).not.toBeInTheDocument();
  });

  it("offers Sign in with Plex, with the token form tucked away", async () => {
    vi.spyOn(api, "bootstrap").mockResolvedValue(remoteBoot);
    renderApp();
    expect(await screen.findByRole("button", { name: "Sign in with Plex" })).toBeInTheDocument();
    expect(screen.getByText("Use a Plex token instead")).toBeInTheDocument();
  });

  it("shows only the logo, keeping the server name for screen readers", async () => {
    vi.spyOn(api, "bootstrap").mockResolvedValue(remoteBoot);
    renderApp();
    expect(await screen.findByRole("img", { name: "Ether" })).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Ether" })).toHaveClass("sr-only");
  });

  it("stores the console session once plex.tv approves and opens the app", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const fake = createFakeServer();
    vi.stubGlobal("fetch", vi.fn(fake.fetch));
    vi.spyOn(api, "bootstrap")
      .mockResolvedValueOnce(remoteBoot)
      .mockResolvedValue({ ...remoteBoot, authenticated: true });
    vi.spyOn(api, "signInStart").mockResolvedValue({
      handle: "h1",
      code: "ABCD",
      authUrl: "https://app.plex.tv/auth#?code=ABCD",
    });
    const poll = vi
      .spyOn(api, "signInPoll")
      .mockResolvedValueOnce({ done: false })
      .mockResolvedValueOnce({ done: true, token: "ether-admin-session-1" });
    vi.spyOn(window, "open").mockReturnValue(null);
    renderApp();
    fireEvent.click(await screen.findByRole("button", { name: "Sign in with Plex" }));
    // Popup blocked: the sign-in page is offered as a link.
    expect(
      (await screen.findByRole("link", { name: "Open the Plex sign-in page" })).getAttribute(
        "href",
      ),
    ).toContain("app.plex.tv/auth");
    await vi.advanceTimersByTimeAsync(4500);
    await waitFor(() =>
      expect(
        screen.queryByRole("button", { name: /Sign in with Plex|Waiting for Plex/ }),
      ).toBeNull(),
    );
    expect(poll).toHaveBeenCalledTimes(2);
    expect(poll).toHaveBeenCalledWith("h1");
    expect(getToken()).toBe("ether-admin-session-1");
    // Signed in remotely: the shell offers Sign out, which ends the session
    // on the server before leaving.
    const signOut = vi.spyOn(api, "signOut").mockResolvedValue({ ok: true });
    const reload = vi.fn();
    vi.stubGlobal("location", { ...window.location, reload });
    fireEvent.click(await screen.findByRole("button", { name: /Sign out/ }));
    await waitFor(() => expect(reload).toHaveBeenCalledOnce());
    expect(signOut).toHaveBeenCalledOnce();
    expect(getToken()).toBe("");
  });

  it("rejects a token that isn't the owner's", async () => {
    vi.spyOn(api, "bootstrap").mockResolvedValue(remoteBoot);
    const { ApiError } = await import("../lib/api");
    vi.spyOn(api, "createSession").mockRejectedValue(new ApiError(401, "unauthorized", "nope"));
    renderApp();
    fireEvent.click(await screen.findByText("Use a Plex token instead"));
    fireEvent.change(screen.getByLabelText("Owner token (X-Plex-Token)"), {
      target: { value: "wrong" },
    });
    fireEvent.click(screen.getByRole("button", { name: /Unlock/ }));
    expect(
      await screen.findByText("That token was not accepted as the owner token."),
    ).toBeInTheDocument();
    expect(getToken()).toBe("");
  });

  it("is skipped on the server machine itself", async () => {
    const fake = createFakeServer({
      bootstrap: { authenticated: true, authRequired: false },
    });
    vi.stubGlobal("fetch", vi.fn(fake.fetch));
    renderApp();
    expect(await screen.findByRole("heading", { name: "Overview" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Sign in with Plex" })).toBeNull();
    expect(screen.queryByRole("button", { name: /Sign out/ })).toBeNull();
  });
});
