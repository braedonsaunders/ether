import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { describe, expect, it, vi } from "vitest";
import { createFakeServer } from "../mock/server";
import Setup, { canAdvance, savedDraft } from "../pages/Setup";
import type { DiskInfo, Indexer, UsenetProvider } from "../lib/types";
import { api } from "../lib/api";
import { calls, renderApp } from "./harness";

const provider: UsenetProvider = {
  id: "p1",
  name: "Main",
  host: "news.example.com",
  port: 563,
  tls: true,
  username: "u",
  password: "pw",
  connections: 20,
  priority: 0,
  backfill: false,
  enabled: true,
};
const indexer: Indexer = {
  id: "i1",
  name: "Idx",
  url: "https://idx.example.com",
  apiKey: "k",
  dailyLimit: 0,
  dailyGrabs: 0,
  priority: 0,
  enabled: true,
  movieCategories: null,
  tvCategories: null,
};

describe("canAdvance gating", () => {
  const base = {
    plexDone: false,
    providers: [provider],
    indexers: [indexer],
    providerTests: {},
    indexerTests: {},
    skipped: {},
  };
  it("blocks the Plex step until sign-in completes", () => {
    expect(canAdvance(0, base)).toBe(false);
    expect(canAdvance(0, { ...base, plexDone: true })).toBe(true);
  });
  it("blocks providers until every enabled provider passes its test", () => {
    expect(canAdvance(1, base)).toBe(false);
    expect(canAdvance(1, { ...base, providerTests: { p1: { status: "fail" } } })).toBe(false);
    expect(canAdvance(1, { ...base, providerTests: { p1: { status: "ok" } } })).toBe(true);
    expect(canAdvance(1, { ...base, providers: [{ ...provider, enabled: false }] })).toBe(false);
    expect(
      canAdvance(1, {
        ...base,
        providers: [{ ...provider, host: "" }],
        providerTests: { p1: { status: "ok" } },
      }),
    ).toBe(false);
  });
  it("blocks indexers until tested; the skip escape hatch always passes", () => {
    expect(canAdvance(2, base)).toBe(false);
    expect(canAdvance(2, { ...base, indexerTests: { i1: { status: "ok" } } })).toBe(true);
    expect(canAdvance(0, { ...base, skipped: { 0: true } })).toBe(true);
    expect(canAdvance(1, { ...base, skipped: { 1: true } })).toBe(true);
  });
  it("accepts debrid instead of Usenet, but every enabled source must be tested", () => {
    const rd = {
      id: "d1",
      name: "",
      kind: "realdebrid" as const,
      apiKey: "tok",
      priority: 0,
      enabled: true,
    };
    const none = { ...base, providers: [], debrid: [] };
    expect(canAdvance(1, none)).toBe(false);
    expect(canAdvance(1, { ...none, debrid: [rd] })).toBe(false);
    expect(canAdvance(1, { ...none, debrid: [rd], debridTests: { d1: { status: "ok" } } })).toBe(
      true,
    );
    // With both kinds, an untested Usenet provider still blocks.
    expect(canAdvance(1, { ...base, debrid: [rd], debridTests: { d1: { status: "ok" } } })).toBe(
      false,
    );
    expect(canAdvance(1, { ...none, debrid: [{ ...rd, enabled: false }] })).toBe(false);
  });
  it("requires a working TMDB key on the last step, or the explicit skip", () => {
    expect(canAdvance(4, { ...base, tmdbReady: false })).toBe(false);
    expect(canAdvance(4, { ...base, tmdbReady: true })).toBe(true);
    expect(canAdvance(4, { ...base, tmdbReady: false, skipped: { 4: true } })).toBe(true);
  });
  it("cache and optional steps never block", () => {
    expect(canAdvance(3, base)).toBe(true);
    expect(canAdvance(4, base)).toBe(true);
  });
});

describe("savedDraft", () => {
  it("drops rows that were added but never filled in, keeping started ones", () => {
    const cfg = createFakeServer().state.config;
    const blankProvider = { ...provider, id: "p2", name: "", host: "", username: "", password: "" };
    const started = {
      ...provider,
      id: "p3",
      name: "",
      host: "news2.example.com",
      username: "",
      password: "",
    };
    const blankIndexer = { ...indexer, id: "i2", name: "", url: "", apiKey: "" };
    const draft = savedDraft({
      ...cfg,
      usenet: { providers: [blankProvider, provider, started] },
      indexers: [indexer, blankIndexer],
      debrid: cfg.debrid && {
        ...cfg.debrid,
        providers: [
          { id: "d1", name: "", kind: "realdebrid", apiKey: " ", priority: 0, enabled: true },
        ],
      },
    });
    expect(draft.usenet.providers?.map((p) => p.id)).toEqual(["p1", "p3"]);
    expect(draft.indexers?.map((ix) => ix.id)).toEqual(["i1"]);
    if (cfg.debrid) expect(draft.debrid?.providers).toEqual([]);
  });
});

function renderWizard(configure?: (fake: ReturnType<typeof createFakeServer>) => void) {
  const fake = createFakeServer();
  fake.state.config.setupComplete = false;
  fake.state.config.plex.ownerUsername = "alice";
  // A fresh install has no debrid accounts (the shared fixture has two).
  if (fake.state.config.debrid) fake.state.config.debrid.providers = [];
  configure?.(fake);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      if (init?.method === "PUT" && String(input).endsWith("/config")) {
        const config = JSON.parse(String(init.body));
        const fields = [
          ...(config.usenet.providers ?? []).flatMap((p: UsenetProvider, i: number) =>
            p.host.trim() ? [] : [{ field: `usenet.providers[${i}].host`, message: "is required" }],
          ),
          ...(config.indexers ?? []).flatMap((ix: Indexer, i: number) =>
            ix.url.trim() ? [] : [{ field: `indexers[${i}].url`, message: "is required" }],
          ),
        ];
        if (fields.length)
          return new Response(
            JSON.stringify({
              error: { code: "validation", message: "configuration is invalid", fields },
            }),
            { status: 400, headers: { "Content-Type": "application/json" } },
          );
      }
      return fake.fetch(input, init);
    }),
  );
  render(
    <MemoryRouter>
      <Setup />
    </MemoryRouter>,
  );
  return fake;
}

describe("setup wizard", () => {
  it.each([false, true])("ignores an older disk response (failure=%s)", async (failOld) => {
    const pending: { resolve: (d: DiskInfo) => void; reject: (e: Error) => void }[] = [];
    vi.spyOn(api, "disk").mockImplementation(
      () =>
        new Promise((resolve, reject) => {
          pending.push({ resolve, reject });
        }),
    );
    const fake = renderWizard();
    await screen.findByText(/Signed in as alice/);
    fireEvent.click(screen.getByRole("button", { name: "Save and continue" }));
    for (const heading of ["Usenet providers", "Indexers and addons"]) {
      await screen.findByRole("heading", { name: heading });
      fireEvent.click(screen.getByLabelText(/Skip test for this step/));
      fireEvent.click(screen.getByRole("button", { name: "Save and continue" }));
    }
    await screen.findByRole("heading", { name: "Cache folder and disk budget" });
    await waitFor(() => expect(pending).toHaveLength(1));
    fireEvent.change(screen.getByDisplayValue(fake.state.config.cache.dir), {
      target: { value: "/new-cache" },
    });
    await waitFor(() => expect(pending).toHaveLength(2));
    await act(async () => {
      pending[1]!.resolve({
        path: "/new-cache",
        probedPath: "/new-cache",
        usedBytes: 0,
        cacheBudgetBytes: 0,
        freeBytes: 20 * 1024 ** 3,
        totalBytes: 40 * 1024 ** 3,
      });
    });
    const current = screen.getByText(/free of.*on this disk/).textContent;
    await act(async () => {
      if (failOld) pending[0]!.reject(new Error("old disk unavailable"));
      else
        pending[0]!.resolve({
          path: "/old-cache",
          probedPath: "/old-cache",
          usedBytes: 0,
          cacheBudgetBytes: 0,
          freeBytes: 2 * 1024 ** 3,
          totalBytes: 4 * 1024 ** 3,
        });
    });
    expect(screen.getByText(/free of.*on this disk/).textContent).toBe(current);
  });

  it("does not open sign-in or start polling after an abandoned PIN request", async () => {
    renderWizard((f) => {
      f.state.config.plex.token = "";
      f.state.config.plex.claimed = false;
      f.state.config.plex.ownerUsername = "";
    });
    await screen.findByRole("button", { name: "Sign in to plex.tv" });
    vi.useFakeTimers();
    const open = vi.spyOn(window, "open").mockReturnValue(null);
    let finish!: (v: { id: string; code: string; authUrl: string }) => void;
    vi.spyOn(api, "pinStart").mockImplementation(
      () =>
        new Promise((resolve) => {
          finish = resolve;
        }),
    );
    const poll = vi.spyOn(api, "pinPoll");
    fireEvent.click(screen.getByRole("button", { name: "Sign in to plex.tv" }));
    cleanup();
    await act(async () => {
      finish({ id: "late-pin", code: "ABCD", authUrl: "https://example.test/auth" });
      await vi.advanceTimersByTimeAsync(4000);
    });
    expect(open).not.toHaveBeenCalled();
    expect(poll).not.toHaveBeenCalled();
  });

  it("does not claim after a pending PIN poll completes following unmount", async () => {
    renderWizard((f) => {
      f.state.config.plex.token = "";
      f.state.config.plex.claimed = false;
      f.state.config.plex.ownerUsername = "";
    });
    await screen.findByRole("button", { name: "Sign in to plex.tv" });
    vi.useFakeTimers();
    vi.spyOn(window, "open").mockReturnValue(null);
    vi.spyOn(api, "pinStart").mockResolvedValue({
      id: "late-pin",
      code: "ABCD",
      authUrl: "https://example.test/auth",
    });
    let finish!: (v: { done: boolean }) => void;
    const poll = vi.spyOn(api, "pinPoll").mockImplementation(
      () =>
        new Promise((resolve) => {
          finish = resolve;
        }),
    );
    const claim = vi.spyOn(api, "claim").mockResolvedValue({ ok: true });
    fireEvent.click(screen.getByRole("button", { name: "Sign in to plex.tv" }));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(4000);
    });
    expect(poll).toHaveBeenCalledOnce();
    cleanup();
    await act(async () => {
      finish({ done: true });
    });
    expect(claim).not.toHaveBeenCalled();
  });

  it("does not refresh configuration after an abandoned claim completes", async () => {
    renderWizard((f) => {
      f.state.config.plex.claimed = false;
    });
    const button = await screen.findByRole("button", { name: "Claim server" });
    let finish!: (v: { ok: boolean }) => void;
    vi.spyOn(api, "claim").mockImplementation(
      () =>
        new Promise((resolve) => {
          finish = resolve;
        }),
    );
    const config = vi.spyOn(api, "getConfig");
    fireEvent.click(button);
    cleanup();
    await act(async () => {
      finish({ ok: true });
    });
    expect(config).not.toHaveBeenCalled();
  });

  it("keeps the required cache restart visible after finishing setup", async () => {
    const fake = renderWizard();
    vi.spyOn(api, "setupComplete").mockResolvedValue({
      setupComplete: true,
      requiredComplete: true,
      steps: [],
      cacheRestartRequired: true,
    });
    await screen.findByText(/Signed in as alice/);
    fireEvent.click(screen.getByRole("button", { name: "Save and continue" }));
    for (const heading of ["Usenet providers", "Indexers and addons"]) {
      await screen.findByRole("heading", { name: heading });
      fireEvent.click(screen.getByLabelText(/Skip test for this step/));
      fireEvent.click(screen.getByRole("button", { name: "Save and continue" }));
    }
    await screen.findByRole("heading", { name: "Cache folder and disk budget" });
    fireEvent.change(screen.getByDisplayValue(fake.state.config.cache.dir), {
      target: { value: "/cache" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Save and continue" }));
    await screen.findByRole("button", { name: "Finish setup" });
    fireEvent.click(screen.getByLabelText(/Skip test for this step/));
    fireEvent.click(screen.getByRole("button", { name: "Finish setup" }));
    expect(
      await screen.findByRole("heading", { name: "Restart Ether to use your cache folder" }),
    ).toBeInTheDocument();
    expect(screen.getByText("/cache")).toBeInTheDocument();
    expect(screen.getByText(/Docker container/)).toBeInTheDocument();
    expect(fake.state.config.cache.dir).toBe("/cache");
  });

  it("saves a fresh Plex step and a provider without submitting blank future sources", async () => {
    const fake = renderWizard((f) => {
      f.state.config.usenet.providers = [];
      f.state.config.indexers = [];
    });
    await screen.findByText(/Signed in as alice/);
    fireEvent.click(screen.getByRole("button", { name: "Save and continue" }));
    await screen.findByRole("heading", { name: "Usenet providers" });
    expect(calls(fake, "PUT", "/config")[0]?.body).toMatchObject({
      usenet: { providers: [] },
      indexers: [],
    });
    fireEvent.click(screen.getByRole("button", { name: "Add provider" }));
    fireEvent.change(screen.getByPlaceholderText("news.example.com"), {
      target: { value: provider.host },
    });
    fireEvent.click(screen.getByRole("button", { name: /^Test$/ }));
    await waitFor(() =>
      expect(screen.getByRole("button", { name: "Save and continue" })).toBeEnabled(),
    );
    fireEvent.click(screen.getByRole("button", { name: "Save and continue" }));
    await screen.findByRole("heading", { name: "Indexers and addons" });
    expect(fake.state.config.usenet.providers?.[0]?.host).toBe(provider.host);
    expect(fake.state.config.indexers).toEqual([]);
  });

  it("sets up a debrid-only server with a Stremio addon, without Usenet", async () => {
    const fake = renderWizard((f) => {
      f.state.config.usenet.providers = [];
      f.state.config.indexers = [];
    });
    await screen.findByText(/Signed in as alice/);
    fireEvent.click(screen.getByRole("button", { name: "Save and continue" }));
    await screen.findByRole("heading", { name: "Debrid accounts" });
    fireEvent.click(screen.getByRole("button", { name: "Add debrid account" }));
    fireEvent.change(screen.getByLabelText("API token"), { target: { value: "rd-token" } });
    fireEvent.click(screen.getByRole("button", { name: /^Test$/ }));
    await waitFor(() =>
      expect(screen.getByRole("button", { name: "Save and continue" })).toBeEnabled(),
    );
    fireEvent.click(screen.getByRole("button", { name: "Save and continue" }));
    await screen.findByRole("heading", { name: "Indexers and addons" });
    expect(fake.state.config.debrid?.providers).toMatchObject([
      { kind: "realdebrid", enabled: true },
    ]);
    expect(fake.state.config.usenet.providers).toEqual([]);

    // A debrid-only server starts its first indexer as a Stremio addon.
    fireEvent.click(screen.getByRole("button", { name: "Add indexer" }));
    expect(screen.queryByLabelText("API key")).not.toBeInTheDocument();
    fireEvent.change(screen.getByPlaceholderText("https://addon.example/manifest.json"), {
      target: { value: " stremio://addon.example/manifest.json " },
    });
    fireEvent.click(screen.getByRole("button", { name: /^Test$/ }));
    await waitFor(() => expect(calls(fake, "POST", "/test/indexer")).toHaveLength(1));
    expect(calls(fake, "POST", "/test/indexer")[0]?.body).toMatchObject({
      kind: "stremio",
      protocol: "torrent",
      url: "https://addon.example/manifest.json",
    });
    await waitFor(() =>
      expect(screen.getByRole("button", { name: "Save and continue" })).toBeEnabled(),
    );
    fireEvent.click(screen.getByRole("button", { name: "Save and continue" }));
    await screen.findByRole("heading", { name: "Cache folder and disk budget" });
    expect(fake.state.config.indexers).toMatchObject([
      { kind: "stremio", protocol: "torrent", url: "https://addon.example/manifest.json" },
    ]);
  });

  it("can explicitly skip unused sources without creating invalid rows", async () => {
    const fake = renderWizard((f) => {
      f.state.config.usenet.providers = [];
      f.state.config.indexers = [];
    });
    await screen.findByText(/Signed in as alice/);
    fireEvent.click(screen.getByRole("button", { name: "Save and continue" }));
    for (const heading of ["Usenet providers", "Indexers and addons"]) {
      await screen.findByRole("heading", { name: heading });
      fireEvent.click(screen.getByLabelText(/Skip test for this step/));
      fireEvent.click(screen.getByRole("button", { name: "Save and continue" }));
    }
    await screen.findByRole("heading", { name: "Cache folder and disk budget" });
    expect(fake.state.config.usenet.providers).toEqual([]);
    expect(fake.state.config.indexers).toEqual([]);
  });

  it.each(["provider", "indexer"] as const)(
    "keeps an incomplete %s visible when skipping its test",
    async (kind) => {
      renderWizard((f) => {
        f.state.config.usenet.providers = [];
        f.state.config.indexers = [];
      });
      await screen.findByText(/Signed in as alice/);
      fireEvent.click(screen.getByRole("button", { name: "Save and continue" }));
      await screen.findByRole("heading", { name: "Usenet providers" });
      if (kind === "indexer") {
        fireEvent.click(screen.getByLabelText(/Skip test for this step/));
        fireEvent.click(screen.getByRole("button", { name: "Save and continue" }));
        await screen.findByRole("heading", { name: "Indexers and addons" });
      }
      fireEvent.click(screen.getByRole("button", { name: `Add ${kind}` }));
      fireEvent.change(screen.getAllByRole("textbox")[0]!, { target: { value: "Keep my draft" } });
      fireEvent.click(screen.getByLabelText(/Skip test for this step/));
      fireEvent.click(screen.getByRole("button", { name: "Save and continue" }));
      expect(await screen.findByText(/is required/)).toBeInTheDocument();
      expect(screen.getAllByRole("textbox")[0]!).toHaveValue("Keep my draft");
    },
  );

  it("clears the pending PIN and preserves newly claimed credentials on the next save", async () => {
    const fake = renderWizard((f) => {
      f.state.config.plex.token = "";
      f.state.config.plex.claimed = false;
      f.state.config.plex.ownerUsername = "";
    });
    await screen.findByRole("button", { name: "Sign in to plex.tv" });
    vi.useFakeTimers();
    vi.spyOn(window, "open").mockReturnValue(null);
    vi.spyOn(api, "pinStart").mockResolvedValue({
      id: "pin",
      code: "ABCD",
      authUrl: "https://example.test/auth",
    });
    const poll = vi.spyOn(api, "pinPoll").mockImplementation(async () => {
      fake.state.config.plex.token = "••••";
      fake.state.config.plex.ownerUsername = "alice";
      return { done: true };
    });
    let finishClaim!: () => void;
    vi.spyOn(api, "claim").mockImplementation(
      () =>
        new Promise((resolve) => {
          finishClaim = () => {
            fake.state.config.plex.claimed = true;
            resolve({ ok: true });
          };
        }),
    );
    fireEvent.click(screen.getByRole("button", { name: "Sign in to plex.tv" }));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(2000);
    });
    expect(screen.queryByText(/waiting for approval/)).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: /Save and continue|Saving/ })).toBeDisabled();
    await act(async () => {
      finishClaim();
    });
    expect(screen.getByRole("button", { name: "Save and continue" })).toBeEnabled();
    expect(screen.queryByRole("button", { name: "Claim server" })).not.toBeInTheDocument();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(4000);
    });
    expect(poll).toHaveBeenCalledOnce();
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Save and continue" }));
    });
    expect(calls(fake, "PUT", "/config")[0]?.body).toMatchObject({
      plex: { token: "••••", claimed: true, ownerUsername: "alice" },
    });
    expect(screen.getByRole("heading", { name: "Usenet providers" })).toBeInTheDocument();
  });

  it("keeps claim failures actionable and refreshes credentials after a retry", async () => {
    const fake = renderWizard((f) => {
      f.state.config.plex.claimed = false;
    });
    const claim = vi
      .spyOn(api, "claim")
      .mockRejectedValueOnce(new Error("Plex unavailable"))
      .mockImplementationOnce(async () => {
        fake.state.config.plex.claimed = true;
        return { ok: true };
      });
    const retry = await screen.findByRole("button", { name: "Claim server" });
    fireEvent.click(retry);
    expect((await screen.findAllByText(/Plex unavailable/)).length).toBeGreaterThan(0);
    expect(screen.getByRole("button", { name: "Save and continue" })).toBeDisabled();
    fireEvent.click(screen.getByRole("button", { name: "Claim server" }));
    await waitFor(() =>
      expect(screen.queryByRole("button", { name: "Claim server" })).not.toBeInTheDocument(),
    );
    expect(claim).toHaveBeenCalledTimes(2);
    expect(screen.getByRole("button", { name: "Save and continue" })).toBeEnabled();
  });

  it("walks from a claimed Plex step to providers and gates on the provider test", async () => {
    const fake = renderWizard();
    expect(await screen.findByText(/Signed in as alice/)).toBeInTheDocument();
    const next = screen.getByRole("button", { name: "Save and continue" });
    expect(next).toBeEnabled();
    fireEvent.click(next);
    expect(await screen.findByRole("heading", { name: "Usenet providers" })).toBeInTheDocument();
    expect(calls(fake, "PUT", "/config")).toHaveLength(1);
    expect(screen.getAllByRole("button", { name: "Save and continue" })[0]).toBeDisabled();
    fireEvent.click(screen.getAllByRole("button", { name: /^Test$/ })[0]!);
    await waitFor(() => expect(screen.getAllByText(/OK · \d+ ms/).length).toBeGreaterThan(0));
    // Both seeded providers must pass before continuing.
    expect(screen.getByRole("button", { name: "Save and continue" })).toBeDisabled();
    fireEvent.click(screen.getAllByRole("button", { name: /^Test$/ })[1]!);
    await waitFor(() =>
      expect(screen.getByRole("button", { name: "Save and continue" })).toBeEnabled(),
    );
  });

  it("the skip-test checkbox unblocks an untested step", async () => {
    renderWizard();
    await screen.findByText(/Signed in as alice/);
    fireEvent.click(screen.getByRole("button", { name: "Save and continue" }));
    await screen.findByRole("heading", { name: "Usenet providers" });
    expect(screen.getByRole("button", { name: "Save and continue" })).toBeDisabled();
    fireEvent.click(screen.getByLabelText(/Skip test for this step/));
    expect(screen.getByRole("button", { name: "Save and continue" })).toBeEnabled();
  });

  it("an unfinished setup sends the owner to the wizard instead of Home", async () => {
    renderApp("/", { bootstrap: { setupComplete: false } });
    expect(await screen.findByRole("heading", { name: "Set up Ether" })).toBeInTheDocument();
    expect(screen.getAllByText("Finish setup").length).toBeGreaterThan(0);
  });
});
