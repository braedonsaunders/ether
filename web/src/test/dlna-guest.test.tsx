import { fireEvent, render, screen } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { expect, it, vi } from "vitest";
import Apps from "../pages/apps/Apps";

vi.mock("@braedonsaunders/appkit-ui", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@braedonsaunders/appkit-ui")>()),
  confirmDialog: vi.fn(async () => true),
}));
const apiFetch = vi.fn();
vi.mock("../lib/api", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../lib/api")>()),
  apiFetch: (...args: unknown[]) => apiFetch(...args),
  api: {
    viewers: async () => [
      { id: 1, title: "Owner", username: "owner", isOwner: true },
      { id: 2, title: "Inactive", username: "inactive", isOwner: false, accessStatus: "inactive" },
      { id: 42, title: "Living-room guest", username: "", isOwner: false, accessStatus: "active" },
    ],
  },
}));

it("creates a restricted guest and stages it as the selected DLNA viewer", async () => {
  apiFetch.mockImplementation(async (path: string, init?: RequestInit) => {
    if (path === "/frontends")
      return [
        {
          descriptor: { id: "dlna", name: "DLNA" },
          enabled: true,
          status: {
            addresses: [],
            lan: "unknown",
            wan: "unknown",
            clients24h: 0,
            liveStreams: 0,
            targetVersion: "",
            conformance: "unknown",
          },
        },
      ];
    if (path === "/frontends/dlna/settings") return {};
    if (path === "/frontends/dlna/guest" && init?.method === "POST") return { id: 42 };
    throw new Error(`Unexpected request: ${path}`);
  });
  render(
    <MemoryRouter initialEntries={["/apps/dlna"]}>
      <Routes>
        <Route path="/apps/:tab?" element={<Apps />} />
      </Routes>
    </MemoryRouter>,
  );
  const select = await screen.findByLabelText("Browse as");
  expect(screen.queryByRole("option", { name: "Inactive" })).not.toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "Create a living-room guest" }));
  expect(await screen.findByRole("region", { name: "Unsaved changes" })).toBeInTheDocument();
  expect(select).toHaveValue("42");
});
