import { fireEvent, render, screen } from "@testing-library/react";
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
