import { StrictMode } from "react";
import { act, fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import Login from "../pages/Login";
import { api, getToken, setToken } from "../lib/api";

const boot = {
  ok: true,
  version: "dev",
  setupComplete: true,
  authenticated: false,
  authRequired: true,
  serverName: "Ether",
};

describe("Plex sign-in lifecycle", () => {
  it("polls and completes under StrictMode", async () => {
    vi.useFakeTimers();
    vi.spyOn(window, "open").mockReturnValue(null);
    vi.spyOn(api, "signInStart").mockResolvedValue({
      handle: "h1",
      code: "ABCD",
      authUrl: "https://example.test/auth",
    });
    const poll = vi
      .spyOn(api, "signInPoll")
      .mockResolvedValue({ done: true, token: "session-token" });
    const ready = vi.fn();
    render(
      <StrictMode>
        <Login boot={boot} onReady={ready} />
      </StrictMode>,
    );
    fireEvent.click(screen.getByRole("button", { name: "Sign in with Plex" }));
    await vi.advanceTimersByTimeAsync(2000);
    expect(poll).toHaveBeenCalledOnce();
    expect(ready).toHaveBeenCalledOnce();
  });

  it("does not poll or install credentials after unmount", async () => {
    vi.useFakeTimers();
    const close = vi.fn();
    vi.spyOn(window, "open").mockReturnValue({
      location: { href: "" },
      close,
    } as unknown as Window);
    vi.spyOn(api, "signInStart").mockResolvedValue({
      handle: "h2",
      code: "EFGH",
      authUrl: "https://example.test/auth",
    });
    const poll = vi.spyOn(api, "signInPoll").mockResolvedValue({ done: true, token: "late-token" });
    const ready = vi.fn();
    const view = render(<Login boot={boot} onReady={ready} />);
    fireEvent.click(screen.getByRole("button", { name: "Sign in with Plex" }));
    await vi.advanceTimersByTimeAsync(1);
    view.unmount();
    await vi.advanceTimersByTimeAsync(2000);
    expect(poll).not.toHaveBeenCalled();
    expect(ready).not.toHaveBeenCalled();
    expect(getToken()).toBe("");
    expect(close).toHaveBeenCalled();
  });
});

describe("Token fallback lifecycle", () => {
  it("does not clear a newer login when an abandoned token check fails", async () => {
    let reject!: (err: Error) => void;
    vi.spyOn(api, "createSession").mockImplementation(
      () =>
        new Promise((_, fail) => {
          reject = fail;
        }),
    );
    const ready = vi.fn();
    const view = render(
      <StrictMode>
        <Login boot={boot} onReady={ready} />
      </StrictMode>,
    );
    fireEvent.click(screen.getByText("Use a Plex token instead"));
    fireEvent.change(screen.getByLabelText("Owner token (X-Plex-Token)"), {
      target: { value: "old-token" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Unlock" }));
    view.unmount();
    setToken("new-login");
    await act(async () => {
      reject(new Error("old check failed"));
    });
    expect(getToken()).toBe("new-login");
    expect(ready).not.toHaveBeenCalled();
  });

  it("does not install a session that arrives after the form is gone", async () => {
    let resolve!: (v: { done: boolean; token: string }) => void;
    vi.spyOn(api, "createSession").mockImplementation(
      () =>
        new Promise((done) => {
          resolve = done;
        }),
    );
    const ready = vi.fn();
    const view = render(<Login boot={boot} onReady={ready} />);
    fireEvent.click(screen.getByText("Use a Plex token instead"));
    fireEvent.change(screen.getByLabelText("Owner token (X-Plex-Token)"), {
      target: { value: "old-token" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Unlock" }));
    view.unmount();
    setToken("new-login");
    await act(async () => {
      resolve({ done: true, token: "ether-admin-stale" });
    });
    expect(getToken()).toBe("new-login");
    expect(ready).not.toHaveBeenCalled();
  });

  it("stores the console session, never the pasted Plex token", async () => {
    setToken("");
    const exchange = vi
      .spyOn(api, "createSession")
      .mockResolvedValue({ done: true, token: "ether-admin-abc" });
    const ready = vi.fn();
    render(<Login boot={boot} onReady={ready} />);
    fireEvent.click(screen.getByText("Use a Plex token instead"));
    fireEvent.change(screen.getByLabelText("Owner token (X-Plex-Token)"), {
      target: { value: " plex-account-token " },
    });
    fireEvent.click(screen.getByRole("button", { name: "Unlock" }));
    await vi.waitFor(() => expect(ready).toHaveBeenCalledOnce());
    expect(exchange).toHaveBeenCalledWith("plex-account-token");
    expect(getToken()).toBe("ether-admin-abc");
    expect(JSON.stringify({ ...localStorage })).not.toContain("plex-account-token");
    setToken("");
  });
});
