import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { expect, it, vi } from "vitest";
import { AddViewerDrawer, Membership } from "../pages/viewers/ManageViewers";

const FRONTENDS = [
  {
    descriptor: { id: "jellyfin" },
    enabled: true,
    status: { connectUrls: ["http://192.168.1.2:8096"], addresses: ["192.168.1.2:8096"] },
  },
  {
    descriptor: { id: "plex" },
    enabled: true,
    status: { connectUrls: [], addresses: [] },
  },
  {
    descriptor: { id: "stremio" },
    enabled: false,
    status: { connectUrls: [], addresses: [] },
  },
];

/** Answer the frontends lookup, and delegate everything else to `create`. */
function stubFetch(create: (url: string, init?: RequestInit) => Promise<Response>) {
  const fetchMock = vi.fn(async (input: string, init?: RequestInit) => {
    if (String(input).includes("/frontends")) {
      return new Response(JSON.stringify(FRONTENDS), { status: 200 });
    }
    return create(String(input), init);
  });
  vi.stubGlobal("fetch", fetchMock);
  return fetchMock;
}

function drawer() {
  return screen.getByRole("dialog", { name: "Add viewer" });
}

it("creates an invite-only viewer and shows a shareable link", async () => {
  const fetchMock = stubFetch(
    async () =>
      new Response(JSON.stringify({ id: 3, url: "/ether/me#invite=secret" }), { status: 200 }),
  );
  const updated = vi.fn();
  render(<AddViewerDrawer open onClose={() => {}} onCreated={updated} />);
  fireEvent.change(screen.getByLabelText("Their name"), { target: { value: "Guest" } });
  fireEvent.click(screen.getByRole("button", { name: "Continue" }));
  fireEvent.click(await screen.findByRole("radio", { name: /Send an invite link/ }));
  expect(
    screen.getByText("They set their own password or link their Plex account at /ether/me."),
  ).toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "Create invite" }));
  expect(await screen.findByText(/\/ether\/me#invite=secret/)).toBeInTheDocument();
  expect(screen.getByText("How to connect")).toBeInTheDocument();
  expect(screen.getByRole("heading", { name: "Jellyfin" })).toBeInTheDocument();
  expect(screen.getByText(/Server address: http:\/\/192\.168\.1\.2:8096/)).toBeInTheDocument();
  expect(screen.queryByRole("heading", { name: "Stremio" })).not.toBeInTheDocument();
  expect(updated).toHaveBeenCalledOnce();
  const createCall = fetchMock.mock.calls.find(([url]) => String(url).includes("/users/invite"))!;
  const [url, init] = createCall as unknown as [string, RequestInit];
  expect(url).toBe("/ether/api/users/invite");
  expect(JSON.parse(String(init.body))).toEqual({ display: "Guest" });
  expect(within(drawer()).getByRole("button", { name: /Copy as message/ })).toBeInTheDocument();
});

it("explains Plex sharing without creating anything in Ether", async () => {
  const fetchMock = stubFetch(async () => new Response("{}", { status: 200 }));
  const updated = vi.fn();
  render(<AddViewerDrawer open onClose={() => {}} onCreated={updated} />);
  fireEvent.change(screen.getByLabelText("Their name"), { target: { value: "Pat" } });
  fireEvent.click(screen.getByRole("button", { name: "Continue" }));
  fireEvent.click(await screen.findByRole("radio", { name: /Share with their Plex account/ }));
  const link = screen.getByRole("link", { name: "Plex.tv" });
  expect(link).toHaveAttribute(
    "href",
    "https://app.plex.tv/desktop/#!/settings/manage-library-access",
  );
  expect(
    screen.getByText(/they appear here automatically within a few minutes/),
  ).toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "Continue" }));
  expect(await screen.findByText("How to connect")).toBeInTheDocument();
  expect(
    screen.getByText(
      "Share your server with their Plex account on plex.tv. They appear here automatically within a few minutes.",
    ),
  ).toBeInTheDocument();
  expect(updated).not.toHaveBeenCalled();
  expect(fetchMock.mock.calls.filter(([url]) => String(url).includes("/users/"))).toHaveLength(0);
});

it("copies a friendly message for a local viewer", async () => {
  stubFetch(async () => new Response(JSON.stringify({ id: 9 }), { status: 201 }));
  const writeText = vi.fn(async (_text: string) => {});
  Object.assign(navigator, { clipboard: { writeText } });
  render(<AddViewerDrawer open onClose={() => {}} onCreated={() => {}} />);
  fireEvent.change(screen.getByLabelText("Their name"), { target: { value: "Riley Fox" } });
  fireEvent.click(screen.getByRole("button", { name: "Continue" }));
  expect(screen.getByLabelText("Username")).toHaveValue("riley-fox");
  fireEvent.change(screen.getByLabelText("Password"), {
    target: { value: "a-very-long-password" },
  });
  fireEvent.click(screen.getByRole("button", { name: "Create viewer" }));
  expect(await screen.findByText("How to connect")).toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: /Copy as message/ }));
  await waitFor(() => expect(writeText).toHaveBeenCalledOnce());
  const message = String(writeText.mock.calls[0]![0]);
  expect(message).toContain("Hi Riley Fox, you can watch on my Ether server.");
  expect(message).toContain("Your username is riley-fox.");
  expect(message).toContain("Jellyfin:");
  expect(message).toContain("http://192.168.1.2:8096");
});

it("updates membership without exposing an owner action", async () => {
  const fetchMock = vi.fn(async () => new Response(JSON.stringify({ ok: true }), { status: 200 }));
  vi.stubGlobal("fetch", fetchMock);
  const updated = vi.fn();
  render(<Membership id={7} active onChange={updated} />);
  fireEvent.click(screen.getByRole("button", { name: "Deactivate viewer" }));
  await waitFor(() => expect(updated).toHaveBeenCalledOnce());
  const [url, init] = fetchMock.mock.calls[0] as unknown as [string, RequestInit];
  expect(url).toBe("/ether/api/users/7/membership");
  expect(JSON.parse(String(init.body))).toEqual({ active: false });
});
