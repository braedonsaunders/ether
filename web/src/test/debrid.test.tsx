import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { destinations } from "../lib/sitemap";
import { engineSummary, latencySummary } from "../pages/sources/Debrid";
import { debridFix, sourceMonogram } from "../pages/sources/cards";
import { debridMark } from "../pages/sources/marks";
import type { DebridConfig, Indexer } from "../lib/types";
import { calls, renderApp } from "./harness";

const WAIT = { timeout: 4000 };

async function choose(label: string, option: string) {
  fireEvent.click(screen.getByRole("button", { name: label }));
  fireEvent.click(await screen.findByRole("option", { name: option }));
}

describe("Sources › Debrid", () => {
  it("is a Sources tab that search can find", async () => {
    expect(destinations().find((d) => d.href === "/sources/yours/debrid")).toMatchObject({
      title: "Debrid",
      page: "Sources",
    });
    renderApp("/sources/yours/debrid");
    expect(await screen.findByRole("heading", { name: "Sources" }, WAIT)).toBeInTheDocument();
    expect(screen.getByRole("tab", { name: "Your sources" })).toHaveAttribute(
      "aria-selected",
      "true",
    );
    expect(await screen.findByRole("region", { name: "Debrid providers" }, WAIT)).toBeInTheDocument();
  });

  it.each(["/sources/usenet", "/sources/indexers", "/sources/debrid", "/sources/usage"])(
    "does not treat %s as its own page",
    async (path) => {
      renderApp(path);
      expect(await screen.findByRole("region", { name: "Debrid providers" }, WAIT)).toBeInTheDocument();
      expect(screen.queryByRole("region", { name: "Usenet providers" })).toBeNull();
      expect(screen.queryByRole("region", { name: "Indexers" })).toBeNull();
      expect(screen.queryByText("Plays this month")).toBeNull();
    },
  );

  it("lists providers with their kind and live status", async () => {
    const { fake } = renderApp("/sources/yours/debrid");
    expect(await screen.findByRole("button", { name: "Edit Real-Debrid" }, WAIT)).toBeVisible();
    const list = screen.getByRole("region", { name: "Debrid providers" });
    expect(within(list).getByRole("button", { name: "Edit TorBox backup" })).toBeInTheDocument();
    expect(within(list).getByText("TorBox")).toBeInTheDocument();
    expect(await within(list).findByText("Ready", {}, WAIT)).toBeInTheDocument();
    expect(within(list).getByText(/^Paused/)).toBeInTheDocument();
    expect(within(list).getByText(/rate limited by the service/)).toBeInTheDocument();
    expect(within(list).getByText(/17 cached · 6 not cached/)).toBeInTheDocument();
    expect(within(list).getByText(/5 reused/)).toBeInTheDocument();
    // Averaged timings only for providers that have been used.
    expect(
      within(list).getByText("API 0.8 s · link 140 ms · first byte 92 ms · 48 MB/s"),
    ).toBeInTheDocument();
    expect(within(list).getAllByText(/first byte/)).toHaveLength(1);
    expect(within(list).getByLabelText("Streaming engine")).toHaveTextContent(
      "1 open file · 50 cache hits (10 memory, 40 disk) vs 120 fetched (60.0 MiB) · 2 hedged requests, 1 won · 3 links refreshed",
    );
    expect(calls(fake, "GET", "/status/debrid").length).toBeGreaterThan(0);
    // A torrent source is already configured, so the gap notice stays hidden.
    expect(screen.queryByText(/Ether ships no/)).toBeNull();
    expect(screen.queryByText(/no torrent source is set up yet/)).toBeNull();
  });

  it("points at indexers only when debrid is connected and no torrent source exists", async () => {
    renderApp("/sources/yours/debrid", {}, (fake) => {
      fake.state.config.indexers = (fake.state.config.indexers ?? []).map((ix) =>
        ix.protocol === "torrent" ? { ...ix, enabled: false } : ix,
      );
    });
    expect(await screen.findByText(/no torrent source is set up yet/, {}, WAIT)).toBeVisible();
    expect(screen.getByRole("link", { name: "Add one" })).toHaveAttribute(
      "href",
      "/sources/yours/indexers",
    );
    expect(screen.queryByText(/Ether ships no/)).toBeNull();
  });

  it("adds a provider from a drawer, tests it, and keeps stored keys", async () => {
    const { fake } = renderApp("/sources/yours/debrid");
    fireEvent.click(await screen.findByRole("button", { name: "Add a debrid service" }, WAIT));
    const drawer = await screen.findByRole("dialog", { name: "New debrid provider" });
    const test = within(drawer).getByRole("button", { name: /Test/ });
    expect(test).toBeDisabled();
    await choose("Service", "AllDebrid");
    fireEvent.change(within(drawer).getByLabelText("Name"), { target: { value: "AD" } });
    fireEvent.change(within(drawer).getByLabelText("API key"), { target: { value: "k-123" } });
    fireEvent.click(test);
    await waitFor(() => expect(calls(fake, "POST", "/test/debrid")).toHaveLength(1));
    expect(calls(fake, "POST", "/test/debrid")[0]!.body).toMatchObject({
      kind: "alldebrid",
      apiKey: "k-123",
      enabled: true,
    });
    expect(await within(drawer).findByText(/OK/)).toBeInTheDocument();

    fireEvent.click(within(drawer).getByRole("button", { name: "Save provider" }));
    await waitFor(() => expect(calls(fake, "PUT", "/config/debrid")).toHaveLength(1));
    const body = calls(fake, "PUT", "/config/debrid")[0]!.body as DebridConfig;
    expect(body).toMatchObject({ cachedOnly: true, removeUncached: true });
    expect(body.providers).toMatchObject([
      { id: "rd-1", apiKey: "••••", priority: 0 },
      { id: "tb-1", apiKey: "••••", priority: 1 },
      { name: "AD", kind: "alldebrid", apiKey: "k-123", priority: 2, enabled: true },
    ]);
    expect(await screen.findByRole("button", { name: "Edit AD" }, WAIT)).toBeInTheDocument();
  });

  it("tests a saved provider with its redacted key", async () => {
    const { fake } = renderApp("/sources/yours/debrid");
    fireEvent.click(await screen.findByRole("button", { name: "Edit Real-Debrid" }, WAIT));
    const drawer = await screen.findByRole("dialog", { name: "Real-Debrid" });
    fireEvent.click(within(drawer).getByRole("button", { name: /Test/ }));
    await waitFor(() => expect(calls(fake, "POST", "/test/debrid")).toHaveLength(1));
    expect(calls(fake, "POST", "/test/debrid")[0]!.body).toMatchObject({
      id: "rd-1",
      apiKey: "••••",
    });
  });

  it("edits cached-only handling in place and keeps the providers", async () => {
    const { fake } = renderApp("/sources/yours/debrid");
    const cached = await screen.findByRole("switch", { name: "Cached torrents only" }, WAIT);
    expect(cached).toBeChecked();
    expect(screen.queryByRole("button", { name: /Save changes/ })).not.toBeInTheDocument();
    fireEvent.click(cached);
    fireEvent.click(await screen.findByRole("button", { name: /Save changes/ }));
    await waitFor(() => expect(calls(fake, "PUT", "/config/debrid")).toHaveLength(1));
    const body = calls(fake, "PUT", "/config/debrid")[0]!.body as DebridConfig;
    expect(body).toMatchObject({ cachedOnly: false, removeUncached: true });
    expect(body.providers?.map((p) => [p.id, p.apiKey])).toEqual([
      ["rd-1", "••••"],
      ["tb-1", "••••"],
    ]);
  });
});

describe("Sources › Indexers protocol", () => {
  it("badges torrent indexers and saves the protocol from the drawer", async () => {
    const { fake } = renderApp("/sources/yours/indexers");
    fireEvent.click(await screen.findByRole("button", { name: "Edit Indexer Two" }, WAIT));
    const drawer = await screen.findByRole("dialog", { name: "Indexer Two" });
    const protocol = within(drawer).getByRole("radiogroup", { name: "Protocol" });
    expect(within(protocol).getByRole("radio", { name: "Usenet (Newznab)" })).toBeChecked();
    fireEvent.click(within(protocol).getByRole("radio", { name: "Torrent (Torznab)" }));
    fireEvent.click(within(drawer).getByRole("button", { name: "Save indexer" }));
    await waitFor(() => expect(calls(fake, "PUT", "/config/indexers")).toHaveLength(1));
    const body = calls(fake, "PUT", "/config/indexers")[0]!.body as Indexer[];
    expect(body.map((ix) => [ix.id, ix.protocol ?? ""])).toEqual([
      ["i1", ""],
      ["i2", "torrent"],
      ["i3", "torrent"],
      ["i4", "torrent"],
    ]);
    const list = screen.getByRole("region", { name: "Indexers" });
    await waitFor(() => expect(within(list).getAllByText("Torznab")).toHaveLength(2));
  });

  it("points torrent indexers at the Debrid tab when no debrid account is set up", async () => {
    renderApp("/sources/yours/indexers", {}, (fake) => {
      fake.state.config.debrid = { providers: [], cachedOnly: true, removeUncached: true };
    });
    expect(
      await screen.findByRole("link", { name: "Add a debrid provider" }, WAIT),
    ).toHaveAttribute("href", "/sources/yours/debrid");
  });

  it("does not nag when a debrid account is ready", async () => {
    renderApp("/sources/yours/indexers");
    expect(await screen.findByRole("button", { name: "Edit Torrent Tracker" }, WAIT)).toBeVisible();
    expect(screen.queryByRole("link", { name: "Add a debrid provider" })).toBeNull();
  });

  it("shows each Prowlarr result's protocol", async () => {
    renderApp("/sources/yours/indexers");
    await screen.findByRole("button", { name: "Edit Indexer One" }, WAIT);
    fireEvent.click(screen.getByRole("button", { name: /Import from Prowlarr/ }));
    const drawer = await screen.findByRole("dialog", { name: "Import from Prowlarr" });
    fireEvent.change(within(drawer).getByLabelText("Prowlarr URL"), {
      target: { value: "http://localhost:9696" },
    });
    fireEvent.change(within(drawer).getByLabelText("Prowlarr API key"), {
      target: { value: "secret" },
    });
    fireEvent.click(within(drawer).getByRole("button", { name: "Preview indexers" }));
    expect(await within(drawer).findByText(/Prowlarr Torrents/, {}, WAIT)).toBeInTheDocument();
    expect(within(drawer).getByText("Torznab")).toBeInTheDocument();
    expect(within(drawer).getByText("Newznab")).toBeInTheDocument();
  });
});

describe("source cards", () => {
  it("uses a monogram unless Simple Icons publishes the mark", () => {
    expect(sourceMonogram("Real-Debrid")).toBe("RD");
    expect(sourceMonogram("TorBox")).toBe("T");
    expect(debridMark("realdebrid")).toBeNull();
    expect(debridMark("torbox")).toBeNull();
    expect(debridMark("debridlink")).not.toBeNull();
    expect(debridFix(true, "auth_failed")).toMatch(/API key was rejected/);
    expect(debridFix(true, "ok")).toBeNull();
    expect(debridFix(false, "ok")).toMatch(/off/);
  });
});

describe("debrid status summaries", () => {
  it("hides latency before any samples and tolerates a missing engine", () => {
    expect(latencySummary(undefined)).toBeNull();
    expect(latencySummary({ resolveMs: 0, ttfbMs: 0, samples: 0 })).toBeNull();
    expect(latencySummary({ resolveMs: 1500, ttfbMs: 40, throughputBps: 2.5e6, samples: 1 })).toBe(
      "API 1.5 s · first byte 40 ms · 2.5 MB/s",
    );
    expect(engineSummary(undefined)).toBeNull();
    expect(engineSummary({ openFiles: 0, errors: 2 })).toBe(
      "0 open files · 0 cache hits (0 memory, 0 disk) vs 0 fetched · 2 errors",
    );
  });
});
