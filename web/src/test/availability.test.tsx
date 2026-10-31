import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { calls, renderApp } from "./harness";
import { destinations } from "../lib/sitemap";

describe("Sources availability", () => {
  it("is discoverable and does not start checks just by viewing", async () => {
    const { fake } = renderApp("/sources/availability");
    expect(await screen.findByText("Watch release feeds")).toBeVisible();
    expect(destinations().some((d) => d.href === "/sources/availability")).toBe(true);
    expect(screen.getByRole("button", { name: "Sync feeds" })).toBeDisabled();
    fireEvent.click(screen.getByRole("tab", { name: "Recent source evidence" }));
    expect(screen.getByText("Evidence will appear here")).toBeVisible();
    expect(calls(fake, "POST", "/availability")).toHaveLength(0);
  });
  it("saves monitoring and budgets, then schedules sync", async () => {
    const { fake } = renderApp("/sources/availability");
    fireEvent.click(await screen.findByRole("button", { name: "Settings" }));
    const d = await screen.findByRole("dialog", { name: "Availability settings" });
    fireEvent.click(within(d).getByRole("switch", { name: "Monitor release feeds" }));
    fireEvent.click(within(d).getByRole("switch", { name: "Check discovered sources" }));
    fireEvent.click(within(d).getByText("More options"));
    fireEvent.change(within(d).getByLabelText("Feed API requests"), { target: { value: "48" } });
    fireEvent.click(within(d).getByRole("button", { name: "Save settings" }));
    await waitFor(() => expect(calls(fake, "PUT", "/config/availability")).toHaveLength(1));
    expect(calls(fake, "PUT", "/config/availability")[0]!.body).toMatchObject({
      monitor: true,
      verify: true,
      feedRequests: 48,
      reservePercent: 80,
    });
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    fireEvent.click(screen.getByRole("button", { name: "Sync feeds" }));
    await waitFor(() => expect(calls(fake, "POST", "/availability/sync")).toHaveLength(1));
  });
  it("preserves the movie window when disabling the gate", async () => {
    const { fake } = renderApp("/sources/availability");
    fireEvent.click(await screen.findByRole("button", { name: "Settings" }));
    const d = await screen.findByRole("dialog", { name: "Availability settings" });
    expect(within(d).getByLabelText("Movie evidence window")).toHaveValue(60);
    fireEvent.change(within(d).getByLabelText("Movie evidence window"), {
      target: { value: "120" },
    });
    fireEvent.click(within(d).getByRole("switch", { name: "Require movie release evidence" }));
    fireEvent.click(within(d).getByRole("button", { name: "Save settings" }));
    await waitFor(() => expect(calls(fake, "PUT", "/config/availability")).toHaveLength(1));
    expect(calls(fake, "PUT", "/config/availability")[0]!.body).toMatchObject({
      disableMovieEvidenceGate: true,
      movieEvidenceDays: 120,
    });
  });
  it("discards unsaved source selection", async () => {
    const { fake } = renderApp("/sources/availability");
    fireEvent.click(await screen.findByRole("button", { name: "Settings" }));
    const d = await screen.findByRole("dialog", { name: "Availability settings" });
    fireEvent.click(within(d).getByText("More options"));
    fireEvent.click(within(d).getByRole("switch", { name: "Use all supported feeds" }));
    fireEvent.click(within(d).getByRole("button", { name: "Cancel" }));
    expect(calls(fake, "PUT", "/config/availability")).toHaveLength(0);
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    fireEvent.click(screen.getByRole("button", { name: "Settings" }));
    const again = await screen.findByRole("dialog", { name: "Availability settings" });
    fireEvent.click(within(again).getByRole("button", { name: "More options" }));
    expect(within(again).getByRole("switch", { name: "Use all supported feeds" })).toBeChecked();
  });
});
