import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { expect, it, vi } from "vitest";
import SignInMethods from "../pages/me/SignInMethods";

it("shows identities and revokes only the selected device", async () => {
  const fetchMock = vi.fn(async (url: string) => {
    if (url.endsWith("/identities"))
      return new Response(
        JSON.stringify({ identities: [{ id: 1, provider: "plex", display: "alice" }] }),
      );
    if (url.endsWith("/devices"))
      return new Response(
        JSON.stringify({
          devices: [{ hash: "abc", surface: "jellyfin", client: "TV", lastUsed: 1000 }],
        }),
      );
    return new Response(JSON.stringify({ ok: true }));
  });
  vi.stubGlobal("fetch", fetchMock);
  render(<SignInMethods base="/me" token="viewer" />);
  expect(await screen.findByText("alice")).toBeInTheDocument();
  expect(screen.getAllByText("Plex").length).toBeGreaterThan(0);
  fireEvent.click(await screen.findByRole("button", { name: "Revoke" }));
  await waitFor(() =>
    expect(fetchMock.mock.calls.some(([url]) => url.endsWith("/devices/revoke"))).toBe(true),
  );
});

it("links Plex using the portal credential", async () => {
  const fetchMock = vi.fn(async (url: string, init?: RequestInit) => {
    if (url.endsWith("/identities")) return new Response(JSON.stringify({ identities: [] }));
    if (url.endsWith("/devices")) return new Response(JSON.stringify({ devices: [] }));
    if (url.endsWith("/link/plex")) {
      expect((init?.headers as Record<string, string>).Authorization).toBe("Bearer viewer");
      expect((init?.headers as Record<string, string>)["X-Ether-Request"]).toBe("1");
      return new Response(JSON.stringify({ handle: "abc", authUrl: "https://plex.tv/auth" }));
    }
    if (url.endsWith("/link/plex/abc")) return new Response(JSON.stringify({ done: true }));
    throw new Error("unexpected request");
  });
  vi.stubGlobal("fetch", fetchMock);
  vi.spyOn(window, "open").mockReturnValue(null);
  render(<SignInMethods base="/me" token="viewer" />);
  fireEvent.click(screen.getByRole("button", { name: "Link Plex account" }));
  expect(await screen.findByRole("link", { name: "Open Plex sign-in" })).toHaveAttribute(
    "href",
    "https://plex.tv/auth",
  );
  expect(
    await screen.findByText("Plex account linked.", {}, { timeout: 4000 }),
  ).toBeInTheDocument();
});
