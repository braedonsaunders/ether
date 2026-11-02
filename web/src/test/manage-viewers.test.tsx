import { fireEvent, render, screen } from "@testing-library/react";
import { expect, it, vi } from "vitest";
import { AddViewerDrawer } from "../pages/viewers/ManageViewers";

it("creates an invite-only viewer and shows a shareable link", async () => {
  const fetchMock = vi.fn(
    async () =>
      new Response(JSON.stringify({ id: 3, url: "/ether/me#invite=secret" }), { status: 200 }),
  );
  vi.stubGlobal("fetch", fetchMock);
  const updated = vi.fn();
  render(<AddViewerDrawer open onClose={() => {}} onCreated={updated} />);
  fireEvent.click(screen.getByRole("radio", { name: /Invite link/ }));
  fireEvent.change(screen.getByLabelText("Display name"), { target: { value: "Guest" } });
  fireEvent.click(screen.getByRole("button", { name: "Create invite" }));
  expect(await screen.findByText(/\/ether\/me#invite=secret/)).toBeInTheDocument();
  expect(updated).toHaveBeenCalledOnce();
  const [url, init] = fetchMock.mock.calls[0] as unknown as [string, RequestInit];
  expect(url).toBe("/ether/api/users/invite");
  expect(JSON.parse(String(init.body))).toEqual({ display: "Guest" });
});
