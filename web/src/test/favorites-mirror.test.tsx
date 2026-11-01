import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { expect, it, vi } from "vitest";
import { FavoritesMirror } from "../pages/me/FavoritesMirror";

it("saves the Plex playlist preference and explains that favourites are kept", async () => {
  const save = vi.fn(async () => ({}));
  render(<FavoritesMirror load={async () => ({ enabled: true })} save={save} />);
  const toggle = await screen.findByRole("switch", {
    name: "Show my favourites as a playlist in Plex.",
  });
  await waitFor(() => expect(toggle).toBeChecked());
  fireEvent.click(toggle);
  await waitFor(() => expect(toggle).not.toBeChecked());
  expect(save).toHaveBeenCalledWith(false);
  expect(screen.getByText("Your favourites are kept when this is off.")).toBeVisible();
});

it("lets an owner update the selected viewer's Plex playlist setting", async () => {
  const { renderApp } = await import("./harness");
  const { fake } = renderApp("/viewers/2/watchlist");
  const toggle = await screen.findByRole("switch", {
    name: "Show my favourites as a playlist in Plex.",
  });
  await waitFor(() => expect(toggle).toBeChecked());
  fireEvent.click(toggle);
  await waitFor(() => expect(toggle).not.toBeChecked());
  expect(
    fake.state.calls.some(
      (call) =>
        call.method === "POST" &&
        call.path === "/viewers/2/favorites-mirror" &&
        (call.body as { enabled: boolean }).enabled === false,
    ),
  ).toBe(true);
});
