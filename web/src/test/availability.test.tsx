import { fireEvent, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { applyReleaseMode, releaseMode } from "../lib/release-mode";
import { destinations } from "../lib/sitemap";
import { calls, renderApp, settleUi } from "./harness";

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

describe("New release modes", () => {
  it("maps the shipped default to When they can play", () => {
    expect(
      releaseMode({
        monitor: true,
        verify: false,
        preferVerified: true,
        newlyAvailable: true,
        indexerIds: [],
        debrid: true,
        pollMinutes: 15,
        maxPages: 5,
        feedRequests: 400,
        checkRequests: 100,
        grabs: 20,
        checks: 20,
        checkBytes: 1,
        reservePercent: 80,
      }),
    ).toBe("playable");
    expect(
      releaseMode({
        disableMovieEvidenceGate: true,
        monitor: false,
        verify: false,
        preferVerified: true,
        newlyAvailable: true,
        indexerIds: [],
        debrid: true,
        pollMinutes: 15,
        maxPages: 5,
        feedRequests: 400,
        checkRequests: 100,
        grabs: 20,
        checks: 20,
        checkBytes: 1,
        reservePercent: 80,
      }),
    ).toBe("immediate");
    const digital = applyReleaseMode(
      {
        monitor: true,
        verify: false,
        preferVerified: true,
        newlyAvailable: true,
        indexerIds: [],
        debrid: true,
        pollMinutes: 15,
        maxPages: 5,
        feedRequests: 400,
        checkRequests: 100,
        grabs: 20,
        checks: 20,
        checkBytes: 1,
        reservePercent: 80,
        effort: "gentle",
      },
      "digital",
    );
    expect(digital).toMatchObject({
      mode: "digital",
      monitor: false,
      digitalDateEvidence: true,
      disableMovieEvidenceGate: false,
      effort: "normal",
    });
  });

  it("lives in Catalog settings, not Sources", async () => {
    renderApp("/catalog/settings");
    await settleUi(() => {
      expect(screen.getByRole("radio", { name: /When they can play/ })).toBeVisible();
    });
    expect(screen.getByText("Recommended")).toBeVisible();
    expect(
      screen.getByText(
        "A new movie appears once Ether finds a release you can play with your quality settings. Ether checks for you automatically.",
      ),
    ).toBeVisible();
    expect(screen.getByRole("link", { name: /waiting for a release/ })).toHaveAttribute(
      "href",
      "/catalog?waiting=1&type=movie",
    );
    expect(
      screen.queryByRole("switch", { name: "Look for playable releases" }),
    ).not.toBeInTheDocument();
    expect(screen.queryByText("Gentle")).not.toBeInTheDocument();
    expect(screen.queryByText("Indexer requests today")).not.toBeInTheDocument();
    expect(destinations().some((d) => d.href === "/sources/availability")).toBe(false);
    expect(destinations().some((d) => d.href === "/catalog/settings")).toBe(true);
  });

  it("saves On digital release without a second movie table", async () => {
    const { fake } = renderApp("/catalog/settings");
    await settleUi(() => {
      expect(screen.getByRole("radio", { name: /On digital release/ })).toBeVisible();
    });
    expect(screen.queryByText("Ready Movie")).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("radio", { name: /On digital release/ }));
    await settleUi(() => {
      expect(calls(fake, "PUT", "/config/availability")).toHaveLength(1);
    });
    expect(calls(fake, "PUT", "/config/availability")[0]!.body).toMatchObject({
      mode: "digital",
      monitor: false,
      digitalDateEvidence: true,
      disableMovieEvidenceGate: false,
    });
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  });

  it("keeps a daily ceiling behind Advanced", async () => {
    const { fake } = renderApp("/catalog/settings");
    await settleUi(() => {
      expect(screen.getByRole("button", { name: "More options: Advanced" })).toBeVisible();
    });
    fireEvent.click(screen.getByRole("button", { name: "More options: Advanced" }));
    const field = screen.getByLabelText("Daily request ceiling");
    fireEvent.change(field, { target: { value: "48" } });
    fireEvent.blur(field);
    await settleUi(() => {
      expect(calls(fake, "PUT", "/config/availability")).toHaveLength(1);
    });
    expect(calls(fake, "PUT", "/config/availability")[0]!.body).toMatchObject({
      effort: "custom",
      feedRequests: 48,
    });
  });
});
