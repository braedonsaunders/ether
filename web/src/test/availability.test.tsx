import { fireEvent, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { calls, renderApp, settleUi } from "./harness";
import { destinations } from "../lib/sitemap";

const matchMedia = window.matchMedia;

beforeEach(() => {
  window.matchMedia = (query: string) => ({
    matches: query.includes("min-width") || query.includes("prefers-reduced-motion"),
    media: query,
    onchange: null,
    addEventListener: () => {},
    removeEventListener: () => {},
    addListener: () => {},
    removeListener: () => {},
    dispatchEvent: () => false,
  });
});
afterEach(() => {
  window.matchMedia = matchMedia;
});

describe("Sources new releases", () => {
  it("lists in-window movies and does not start checks just by viewing", async () => {
    const { fake } = renderApp("/sources/availability");
    await settleUi(() => {
      expect(
        screen.getByText("New movies appear as soon as a release you can play is found."),
      ).toBeVisible();
      expect(screen.getByText("Ready Movie")).toBeVisible();
    });
    expect(
      destinations().some((d) => d.href === "/sources/availability" && d.title === "New releases"),
    ).toBe(true);
    expect(screen.queryByRole("button", { name: "Sync feeds" })).not.toBeInTheDocument();
    expect(screen.getByText("Release checks are off.")).toBeVisible();
    fireEvent.click(screen.getByRole("switch", { name: "Look for playable releases" }));
    await settleUi(() => {
      expect(screen.getByText("Watching new uploads on 4 indexers")).toBeVisible();
    });
    expect(calls(fake, "POST", "/availability")).toHaveLength(0);
    expect(calls(fake, "POST", "/availability/sync")).toHaveLength(0);
  });
  it("filters by status", async () => {
    renderApp("/sources/availability");
    await settleUi(() => {
      expect(screen.getByText("Ready Movie")).toBeVisible();
    });
    fireEvent.click(screen.getByRole("button", { name: "Status" }));
    fireEvent.click(screen.getByRole("option", { name: /In theatres only/ }));
    await settleUi(() => {
      expect(screen.getByText("Theatre Movie")).toBeVisible();
      expect(screen.queryByText("Ready Movie")).not.toBeInTheDocument();
    });
  });
  it("saves a gentler preset and a custom budget", async () => {
    const { fake } = renderApp("/sources/availability");
    await settleUi(() => {
      expect(screen.getByRole("button", { name: "Settings" })).toBeVisible();
    });
    fireEvent.click(screen.getByRole("button", { name: "Settings" }));
    const d = screen.getByRole("dialog", { name: "New release settings" });
    fireEvent.click(within(d).getByRole("radio", { name: "Gentle" }));
    fireEvent.click(within(d).getByText("More options"));
    fireEvent.change(within(d).getByLabelText("Feed API requests"), { target: { value: "48" } });
    fireEvent.click(within(d).getByRole("button", { name: "Save settings" }));
    await settleUi(() => {
      expect(calls(fake, "PUT", "/config/availability")).toHaveLength(1);
      expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    });
    expect(calls(fake, "PUT", "/config/availability")[0]!.body).toMatchObject({
      effort: "custom",
      feedRequests: 48,
      reservePercent: 80,
    });
  });
  it("preserves the movie window when disabling the gate", async () => {
    const { fake } = renderApp("/sources/availability");
    await settleUi(() => {
      expect(screen.getByRole("button", { name: "Settings" })).toBeVisible();
    });
    fireEvent.click(screen.getByRole("button", { name: "Settings" }));
    const d = screen.getByRole("dialog", { name: "New release settings" });
    fireEvent.click(within(d).getByText("More options"));
    expect(within(d).getByLabelText("Movie evidence window")).toHaveValue(60);
    fireEvent.change(within(d).getByLabelText("Movie evidence window"), {
      target: { value: "120" },
    });
    fireEvent.click(within(d).getByRole("switch", { name: "Require movie release evidence" }));
    fireEvent.click(within(d).getByRole("button", { name: "Save settings" }));
    await settleUi(() => {
      expect(calls(fake, "PUT", "/config/availability")).toHaveLength(1);
    });
    expect(calls(fake, "PUT", "/config/availability")[0]!.body).toMatchObject({
      disableMovieEvidenceGate: true,
      movieEvidenceDays: 120,
    });
  });
  it("discards unsaved source selection", async () => {
    const { fake } = renderApp("/sources/availability");
    await settleUi(() => {
      expect(screen.getByRole("button", { name: "Settings" })).toBeVisible();
    });
    fireEvent.click(screen.getByRole("button", { name: "Settings" }));
    const d = screen.getByRole("dialog", { name: "New release settings" });
    fireEvent.click(within(d).getByText("More options"));
    fireEvent.click(within(d).getByRole("switch", { name: "Use all supported feeds" }));
    fireEvent.click(within(d).getByRole("button", { name: "Cancel" }));
    expect(calls(fake, "PUT", "/config/availability")).toHaveLength(0);
    await settleUi(() => {
      expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    });
    fireEvent.click(screen.getByRole("button", { name: "Settings" }));
    const again = screen.getByRole("dialog", { name: "New release settings" });
    fireEvent.click(within(again).getByRole("button", { name: "More options" }));
    expect(within(again).getByRole("switch", { name: "Use all supported feeds" })).toBeChecked();
  });
});
