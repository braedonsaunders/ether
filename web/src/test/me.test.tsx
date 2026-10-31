import { fireEvent, render, screen } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import Me from "../pages/me/Me";

afterEach(() => sessionStorage.clear());

function portalFetch(extra?: (url: string, init?: RequestInit) => Response | undefined) {
  return vi.fn(async (url: string, init?: RequestInit) => {
    const hit = extra?.(url, init);
    if (hit) return hit;
    if (url.endsWith("/bootstrap"))
      return new Response(JSON.stringify({ signInMethods: ["local", "plex"], version: "1" }));
    if (url.endsWith("/login"))
      return new Response(JSON.stringify({ token: "viewer-grant" }), { status: 200 });
    if (url.endsWith("/quickconnect"))
      return new Response(JSON.stringify({ ok: true, device: "Living Room", app: "jellyfin" }));
    return new Response(JSON.stringify({ id: 1, username: "alice", title: "Alice" }));
  });
}

async function choosePassword() {
  fireEvent.click(await screen.findByRole("radio", { name: "Username & password" }));
}

it("shows a sign-in toggle and asks for a password only for that method", async () => {
  vi.stubGlobal("fetch", portalFetch());
  render(<Me />);
  expect(await screen.findByRole("radio", { name: "Sign in with Plex" })).toBeChecked();
  expect(screen.queryByLabelText("Username")).not.toBeInTheDocument();
  await choosePassword();
  expect(screen.getByLabelText("Username")).toBeInTheDocument();
  expect(screen.getByLabelText("Password")).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Sign in with Plex" })).not.toBeInTheDocument();
});

it("offers only the sign-in methods this server configured", async () => {
  vi.stubGlobal(
    "fetch",
    portalFetch(() => undefined),
  );
  const fetchMock = vi.fn(async (url: string) => {
    if (url.endsWith("/bootstrap"))
      return new Response(JSON.stringify({ signInMethods: ["plex"], version: "1" }));
    return new Response(JSON.stringify({}));
  });
  vi.stubGlobal("fetch", fetchMock);
  render(<Me />);
  expect(await screen.findByRole("button", { name: "Sign in with Plex" })).toBeInTheDocument();
  expect(screen.queryByRole("radio")).not.toBeInTheDocument();
  expect(screen.queryByLabelText("Username")).not.toBeInTheDocument();
});

it("uses a separate portal credential to approve a device", async () => {
  const fetchMock = portalFetch((url, init) => {
    if (url.endsWith("/quickconnect")) {
      expect((init?.headers as Record<string, string>).Authorization).toBe("Bearer viewer-grant");
      return new Response(JSON.stringify({ ok: true, device: "Living Room", app: "jellyfin" }));
    }
    return undefined;
  });
  vi.stubGlobal("fetch", fetchMock);
  render(<Me />);
  await choosePassword();
  fireEvent.change(screen.getByLabelText("Username"), { target: { value: "alice" } });
  fireEvent.change(screen.getByLabelText("Password"), { target: { value: "secret" } });
  fireEvent.click(screen.getByRole("button", { name: "Sign in" }));
  const code = await screen.findByLabelText("Enter the code shown on your TV");
  fireEvent.change(code, { target: { value: "abc-123" } });
  expect(await screen.findByRole("heading", { name: "Device approved" })).toBeInTheDocument();
  expect(screen.getByText("Jellyfin on Living Room")).toBeInTheDocument();
  expect(document.querySelector("svg.ether-approve-pop")).toHaveClass("h-40", "w-40");
  const [, options] = fetchMock.mock.calls.find(([url]) => String(url).endsWith("/quickconnect"))!;
  expect((options?.headers as Record<string, string>)["X-Ether-Request"]).toBe("1");
  expect((options?.headers as Record<string, string>).Authorization).toBe("Bearer viewer-grant");
  expect(JSON.parse(String(options?.body))).toEqual({ code: "ABC123" });
  expect(localStorage.getItem("ether.viewerPortal")).toBeNull();
  expect(screen.queryByText("Favorites in Plex")).not.toBeInTheDocument();
  expect(screen.queryByText("Not interested")).not.toBeInTheDocument();
  expect(screen.queryByText("Browser push")).not.toBeInTheDocument();
  expect(screen.queryByRole("heading", { name: "Devices" })).not.toBeInTheDocument();
});

it("explains an expired code and lets the viewer approve another device", async () => {
  sessionStorage.setItem("ether.viewerPortal", "viewer-grant");
  vi.stubGlobal(
    "fetch",
    portalFetch((url) => {
      if (url.endsWith("/quickconnect"))
        return new Response(
          JSON.stringify({ error: { code: "expired", message: "code expired" } }),
          {
            status: 400,
          },
        );
      return undefined;
    }),
  );
  render(<Me />);
  fireEvent.change(await screen.findByLabelText("Enter the code shown on your TV"), {
    target: { value: "ABCDEF" },
  });
  expect(
    await screen.findByText("That code has expired. Get a new one on your TV and try again."),
  ).toBeInTheDocument();
});

it("removes an invite from the address and redeems it only on acceptance", async () => {
  window.history.replaceState(null, "", "/ether/me#invite=secret-invite");
  const fetchMock = vi.fn(async (url: string, init?: RequestInit) => {
    if (url.endsWith("/bootstrap"))
      return new Response(JSON.stringify({ signInMethods: ["local"], version: "1" }));
    if (url.endsWith("/invite/redeem")) {
      expect(JSON.parse(String(init?.body))).toEqual({ secret: "secret-invite" });
      return new Response(JSON.stringify({ token: "viewer-grant" }));
    }
    return new Response(JSON.stringify({ id: 1, username: "alice", title: "Alice" }));
  });
  vi.stubGlobal("fetch", fetchMock);
  render(<Me />);
  expect(window.location.hash).toBe("");
  expect(fetchMock.mock.calls.some(([url]) => String(url).endsWith("/invite/redeem"))).toBe(false);
  fireEvent.click(await screen.findByRole("button", { name: "Accept invite" }));
  expect(
    await screen.findByText("Invite accepted. You can approve a TV now."),
  ).toBeInTheDocument();
  expect(sessionStorage.getItem("ether.viewerPortal")).toBe("viewer-grant");
  expect(await screen.findByLabelText("Enter the code shown on your TV")).toBeInTheDocument();
  window.history.replaceState(null, "", "/");
});
