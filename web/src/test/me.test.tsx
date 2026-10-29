import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import Me from "../pages/me/Me";
afterEach(() => sessionStorage.clear());
it("uses a separate portal credential to approve a device", async () => {
  const fetchMock = vi.fn(async (url: string, init?: RequestInit) => {
    if (url.endsWith("/login"))
      return new Response(JSON.stringify({ token: "viewer-grant" }), { status: 200 });
    if (url.endsWith("/quickconnect"))
      return new Response(JSON.stringify({ ok: true }), { status: 200 });
    expect((init?.headers as Record<string, string>).Authorization).toBe("Bearer viewer-grant");
    return new Response(JSON.stringify({ id: 1, username: "alice" }), { status: 200 });
  });
  vi.stubGlobal("fetch", fetchMock);
  render(<Me />);
  fireEvent.change(screen.getByLabelText("Username"), { target: { value: "alice" } });
  fireEvent.change(screen.getByLabelText("Password"), { target: { value: "secret" } });
  fireEvent.click(screen.getByRole("button", { name: "Sign in" }));
  fireEvent.change(await screen.findByLabelText("Enter the code shown on your TV"), {
    target: { value: "ABC123" },
  });
  fireEvent.click(screen.getByRole("button", { name: "Approve device" }));
  await waitFor(() => expect(screen.getByRole("status")).toHaveTextContent("Device approved."));
  const [, options] = fetchMock.mock.calls.find(([url]) => url.endsWith("/quickconnect"))!;
  expect((options?.headers as Record<string, string>)["X-Ether-Request"]).toBe("1");
  expect((options?.headers as Record<string, string>).Authorization).toBe("Bearer viewer-grant");
  expect(localStorage.getItem("ether.viewerPortal")).toBeNull();
});

it("removes an invite from the address and redeems it only on acceptance", async () => {
  window.history.replaceState(null, "", "/ether/me#invite=secret-invite");
  const fetchMock = vi.fn(async (url: string, init?: RequestInit) => {
    if (url.endsWith("/invite/redeem")) {
      expect(JSON.parse(String(init?.body))).toEqual({ secret: "secret-invite" });
      return new Response(JSON.stringify({ token: "viewer-grant" }));
    }
    if (url.endsWith("/identities")) return new Response(JSON.stringify({ identities: [] }));
    if (url.endsWith("/devices")) return new Response(JSON.stringify({ devices: [] }));
    return new Response(JSON.stringify({ id: 1, username: "alice" }));
  });
  vi.stubGlobal("fetch", fetchMock);
  render(<Me />);
  expect(window.location.hash).toBe("");
  expect(fetchMock).not.toHaveBeenCalled();
  fireEvent.click(screen.getByRole("button", { name: "Accept invite" }));
  expect(
    await screen.findByText("Invite accepted. Set a password to sign in again."),
  ).toBeInTheDocument();
  expect(sessionStorage.getItem("ether.viewerPortal")).toBe("viewer-grant");
  window.history.replaceState(null, "", "/");
});

it("uses the portal grant to update the viewer's Plex favourites playlist", async () => {
  sessionStorage.setItem("ether.viewerPortal", "viewer-grant");
  const fetchMock = vi.fn(async (url: string, init?: RequestInit) => {
    if (url.endsWith("/favorites-mirror")) {
      expect((init?.headers as Record<string, string>).Authorization).toBe("Bearer viewer-grant");
      return new Response(JSON.stringify({ enabled: init?.method === "POST" ? false : true }));
    }
    if (url.endsWith("/identities")) return new Response(JSON.stringify({ identities: [] }));
    if (url.endsWith("/devices")) return new Response(JSON.stringify({ devices: [] }));
    return new Response(JSON.stringify({ id: 1, username: "alice" }));
  });
  vi.stubGlobal("fetch", fetchMock);
  render(<Me />);
  const toggle = await screen.findByRole("switch", {
    name: "Show my favourites as a playlist in Plex.",
  });
  await waitFor(() => expect(toggle).toBeChecked());
  fireEvent.click(toggle);
  await waitFor(() => expect(toggle).not.toBeChecked());
  const [, options] = fetchMock.mock.calls.find(
    ([url, init]) => url.endsWith("/favorites-mirror") && init?.method === "POST",
  )!;
  expect(JSON.parse(String(options?.body))).toEqual({ enabled: false });
});

it("uses the viewer grant to opt into favourite episode alerts", async () => {
  sessionStorage.setItem("ether.viewerPortal", "viewer-grant");
  const fetchMock = vi.fn(async (url: string, init?: RequestInit) => {
    if (url.endsWith("/favorite-episode-alert")) {
      expect((init?.headers as Record<string, string>).Authorization).toBe("Bearer viewer-grant");
      return new Response(JSON.stringify({ enabled: init?.method === "POST" }));
    }
    if (url.endsWith("/favorites-mirror")) return new Response(JSON.stringify({ enabled: false }));
    if (url.endsWith("/identities")) return new Response(JSON.stringify({ identities: [] }));
    if (url.endsWith("/devices")) return new Response(JSON.stringify({ devices: [] }));
    return new Response(JSON.stringify({ id: 1, username: "alice" }));
  });
  vi.stubGlobal("fetch", fetchMock);
  render(<Me />);
  const toggle = await screen.findByRole("switch", { name: "New episodes from favourite shows" });
  await waitFor(() => expect(toggle).toBeEnabled());
  expect(toggle).not.toBeChecked();
  fireEvent.click(toggle);
  await waitFor(() => expect(toggle).toBeChecked());
  const [, options] = fetchMock.mock.calls.find(
    ([url, init]) => url.endsWith("/favorite-episode-alert") && init?.method === "POST",
  )!;
  expect(JSON.parse(String(options?.body))).toEqual({ enabled: true });
  expect((options?.headers as Record<string, string>)["X-Ether-Request"]).toBe("1");
});
