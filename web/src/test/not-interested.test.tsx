import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { expect, it, vi } from "vitest";
import { NotInterested } from "../pages/me/NotInterested";

it("restores a visible entry only after the server confirms it", async () => {
  const load = vi
    .fn()
    .mockResolvedValue([{ handle: "opaque-a", unavailable: false, title: "A Film", year: 2020 }]);
  const restore = vi.fn().mockResolvedValue(undefined);
  render(<NotInterested load={load} restore={restore} />);
  const button = await screen.findByRole("button", { name: "Show A Film in recommendations" });
  expect(screen.getByText("A Film (2020)")).toBeInTheDocument();
  fireEvent.click(button);
  await waitFor(() => expect(restore).toHaveBeenCalledWith("opaque-a"));
  expect(await screen.findByText("No hidden recommendations.")).toBeInTheDocument();
});

it("never displays metadata supplied for an unavailable entry and keeps it when restore fails", async () => {
  const load = vi
    .fn()
    .mockResolvedValue([{ handle: "opaque-b", unavailable: true, title: "Private", year: 2024 }]);
  const restore = vi.fn().mockRejectedValue(new Error("offline"));
  render(<NotInterested load={load} restore={restore} />);
  const button = await screen.findByRole("button", {
    name: "Show Unavailable title in recommendations",
  });
  expect(screen.queryByText(/Private|2024/)).not.toBeInTheDocument();
  fireEvent.click(button);
  await screen.findByRole("alert");
  expect(screen.getByText("Unavailable title")).toBeInTheDocument();
  expect(restore).toHaveBeenCalledWith("opaque-b");
});
