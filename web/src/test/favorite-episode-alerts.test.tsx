import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { expect, it, vi } from "vitest";
import { FavoriteEpisodeAlerts } from "../pages/me/FavoriteEpisodeAlerts";

it("loads the saved opt-in and changes it only after a successful save", async () => {
  const load = vi.fn(async () => ({ enabled: false }));
  const save = vi.fn(async () => undefined);
  render(<FavoriteEpisodeAlerts load={load} save={save} />);
  const toggle = await screen.findByRole("switch", { name: "New episodes from favourite shows" });
  await waitFor(() => expect(toggle).toBeEnabled());
  expect(toggle).not.toBeChecked();
  fireEvent.click(toggle);
  await waitFor(() => expect(toggle).toBeChecked());
  expect(save).toHaveBeenCalledWith(true);
});

it("keeps the saved setting and reports a failed change", async () => {
  const load = vi.fn(async () => ({ enabled: false }));
  const save = vi.fn(async () => {
    throw new Error("offline");
  });
  render(<FavoriteEpisodeAlerts load={load} save={save} />);
  const toggle = await screen.findByRole("switch", { name: "New episodes from favourite shows" });
  await waitFor(() => expect(toggle).toBeEnabled());
  fireEvent.click(toggle);
  await screen.findByRole("alert");
  expect(toggle).not.toBeChecked();
});

it("shows an owner the saved preference without allowing them to opt the viewer in", async () => {
  const load = vi.fn(async () => ({ enabled: false }));
  render(<FavoriteEpisodeAlerts load={load} />);
  const toggle = await screen.findByRole("switch", { name: "New episodes from favourite shows" });
  await waitFor(() => expect(load).toHaveBeenCalled());
  expect(toggle).toBeDisabled();
  expect(toggle).not.toBeChecked();
  expect(
    screen.getByText("The viewer can change this setting in their profile."),
  ).toBeInTheDocument();
});
