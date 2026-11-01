import { act, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { ConnectApps } from "../pages/me/ConnectApps";

afterEach(() => vi.restoreAllMocks());

it("mints a viewer-scoped secret once and can reveal its QR code", async () => {
  const fetchMock = vi.spyOn(globalThis, "fetch").mockImplementation(async (_url, init) => {
    const body =
      init?.method === "POST"
        ? {
            surface: "stremio",
            url: "https://ether.example/stremio/secret/manifest.json",
            qr: "data:image/png;base64,AA==",
          }
        : { apps: ["stremio"] };
    return new Response(JSON.stringify(body), { status: 200 });
  });
  render(<ConnectApps base="/me" token="portal" />);
  fireEvent.click(await screen.findByRole("button", { name: "Create connection" }));
  expect(
    await screen.findByText("https://ether.example/stremio/secret/manifest.json"),
  ).toBeInTheDocument();
  expect(screen.getByText("This secret is shown only once.")).toBeInTheDocument();
  expect(screen.getByRole("link", { name: "Open in Stremio" })).toHaveAttribute(
    "href",
    "stremio://ether.example/stremio/secret/manifest.json",
  );
  fireEvent.click(screen.getByRole("button", { name: "Show QR code" }));
  expect(screen.getByAltText("QR code for the server address")).toHaveAttribute(
    "src",
    "data:image/png;base64,AA==",
  );
  expect(fetchMock).toHaveBeenLastCalledWith(
    "/ether/api/me/apps/stremio",
    expect.objectContaining({
      method: "POST",
      headers: expect.objectContaining({ Authorization: "Bearer portal", "X-Ether-Request": "1" }),
    }),
  );
});

it("explains how to enable HTTPS when Stremio has no secure address", async () => {
  vi.spyOn(globalThis, "fetch").mockImplementation(async (_url, init) =>
    init?.method === "POST"
      ? new Response(JSON.stringify({ error: { code: "stremio_https_required" } }), { status: 503 })
      : new Response(JSON.stringify({ apps: ["stremio"] }), { status: 200 }),
  );
  render(<ConnectApps base="/me" token="portal" />);
  fireEvent.click(await screen.findByRole("button", { name: "Create connection" }));
  expect(await screen.findByRole("alert")).toHaveTextContent(
    "Stremio needs HTTPS. Add a certificate under Apps → Secure connections.",
  );
});

it("explains what to configure when no server address is available", async () => {
  vi.spyOn(globalThis, "fetch").mockImplementation(async (_url, init) =>
    init?.method === "POST"
      ? new Response(JSON.stringify({ error: { code: "address_unavailable" } }), { status: 503 })
      : new Response(JSON.stringify({ apps: ["jellyfin"] }), { status: 200 }),
  );
  render(<ConnectApps base="/me" token="portal" />);
  fireEvent.click(await screen.findByRole("button", { name: "Create connection" }));
  expect(await screen.findByRole("alert")).toHaveTextContent(
    "Server address unavailable. Set Custom server URLs under Apps → Plex → Remote access.",
  );
});

it("does not show a previous viewer's secret after changing profiles", async () => {
  vi.spyOn(globalThis, "fetch").mockImplementation(
    async (_url, init) =>
      new Response(
        JSON.stringify(
          init?.method === "POST"
            ? {
                surface: "stremio",
                url: "https://ether.example/stremio/old-secret/manifest.json",
                qr: "data:image/png;base64,AA==",
              }
            : { apps: ["stremio"] },
        ),
        { status: 200 },
      ),
  );
  const view = render(<ConnectApps base="/viewers/1" token="owner" />);
  fireEvent.click(await screen.findByRole("button", { name: "Create connection" }));
  expect(
    await screen.findByText("https://ether.example/stremio/old-secret/manifest.json"),
  ).toBeInTheDocument();
  view.rerender(<ConnectApps base="/viewers/2" token="owner" />);
  await screen.findByRole("button", { name: "Create connection" });
  expect(
    screen.queryByText("https://ether.example/stremio/old-secret/manifest.json"),
  ).not.toBeInTheDocument();
});

it("does not show a mint that finishes after changing profiles", async () => {
  let finish: ((response: Response) => void) | undefined;
  vi.spyOn(globalThis, "fetch").mockImplementation(async (_url, init) =>
    init?.method === "POST"
      ? new Promise<Response>((resolve) => {
          finish = resolve;
        })
      : new Response(JSON.stringify({ apps: ["stremio"] }), { status: 200 }),
  );
  const view = render(<ConnectApps base="/viewers/1" token="owner" />);
  fireEvent.click(await screen.findByRole("button", { name: "Create connection" }));
  view.rerender(<ConnectApps base="/viewers/2" token="owner" />);
  await screen.findByRole("button", { name: "Create connection" });
  await act(async () => {
    finish?.(
      new Response(
        JSON.stringify({
          surface: "stremio",
          url: "https://ether.example/old-secret",
          qr: "data:image/png;base64,AA==",
        }),
        { status: 200 },
      ),
    );
  });
  expect(screen.queryByText("https://ether.example/old-secret")).not.toBeInTheDocument();
});
