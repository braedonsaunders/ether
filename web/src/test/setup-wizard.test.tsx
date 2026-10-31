import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { systemMessageKey, type MessageCatalog } from "@braedonsaunders/appkit-i18n";
import { MemoryRouter } from "react-router-dom";
import { afterEach, describe, expect, it, vi } from "vitest";
import { loadLocale } from "../i18n";
import fr from "../i18n/locales/fr.json";
import { createFakeServer } from "../mock/server";
import Setup, {
  SETUP_STEP_KEY,
  canAdvance,
  mergeFrontendSelection,
  savedDraft,
} from "../pages/Setup";
import type { DiskInfo, Indexer, UsenetProvider } from "../lib/types";
import { api } from "../lib/api";
import { calls, renderApp } from "./harness";

const french = (source: string) => {
  const entry = (fr as MessageCatalog)[systemMessageKey(source)];
  return typeof entry === "string" ? entry : source;
};

afterEach(async () => {
  localStorage.removeItem("ether.locale");
  await loadLocale("en");
});

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
    accountReady: false,
    providers: [provider],
    indexers: [indexer],
    providerTests: {},
    indexerTests: {},
    skipped: {},
  };
  it("blocks the account step until sign-in or a local owner is ready", () => {
    expect(canAdvance("account", base)).toBe(false);
    expect(canAdvance("account", { ...base, accountReady: true })).toBe(true);
  });
  it("blocks providers until every enabled provider passes its test", () => {
    expect(canAdvance("sources", base)).toBe(false);
    expect(canAdvance("sources", { ...base, providerTests: { p1: { status: "fail" } } })).toBe(
      false,
    );
    expect(canAdvance("sources", { ...base, providerTests: { p1: { status: "ok" } } })).toBe(true);
    expect(canAdvance("sources", { ...base, providers: [{ ...provider, enabled: false }] })).toBe(
      false,
    );
    expect(
      canAdvance("sources", {
        ...base,
        providers: [{ ...provider, host: "" }],
        providerTests: { p1: { status: "ok" } },
      }),
    ).toBe(false);
  });
  it("blocks indexers until tested; the skip escape hatch always passes", () => {
    expect(canAdvance("indexers", base)).toBe(false);
    expect(canAdvance("indexers", { ...base, indexerTests: { i1: { status: "ok" } } })).toBe(true);
    expect(canAdvance("account", { ...base, skipped: { account: true } })).toBe(true);
    expect(canAdvance("sources", { ...base, skipped: { sources: true } })).toBe(true);
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
    expect(canAdvance("sources", none)).toBe(false);
    expect(canAdvance("sources", { ...none, debrid: [rd] })).toBe(false);
    expect(
      canAdvance("sources", { ...none, debrid: [rd], debridTests: { d1: { status: "ok" } } }),
    ).toBe(true);
    // With both kinds, an untested Usenet provider still blocks.
    expect(
      canAdvance("sources", { ...base, debrid: [rd], debridTests: { d1: { status: "ok" } } }),
    ).toBe(false);
    expect(canAdvance("sources", { ...none, debrid: [{ ...rd, enabled: false }] })).toBe(false);
  });
  it("requires a working TMDB key on the catalogue step", () => {
    expect(canAdvance("catalog", { ...base, tmdbReady: false })).toBe(false);
    expect(canAdvance("catalog", { ...base, tmdbReady: true })).toBe(true);
    expect(canAdvance("catalog", { ...base, tmdbReady: false, skipped: { catalog: true } })).toBe(
      false,
    );
  });
  it("app choice and cache never block", () => {
    expect(canAdvance("apps", base)).toBe(true);
    expect(canAdvance("cache", base)).toBe(true);
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
  sessionStorage.removeItem(SETUP_STEP_KEY);
  sessionStorage.removeItem("ether.setup.skipped");
  localStorage.removeItem("ether.locale");
  const fake = createFakeServer();
  fake.state.streamDefaults = {
    audioLanguages: [],
    subtitleLanguages: [],
    preferOriginal: false,
    subtitleMode: "off",
    subtitleStyle: "normal",
    loudness: false,
    nightMode: false,
    dialogueBoost: false,
  };
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

async function continueLanguage() {
  expect(await screen.findByRole("heading", { name: "Choose a language" })).toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "Save and continue" }));
}

async function selectPlex() {
  await continueLanguage();
  expect(
    await screen.findByRole("heading", { name: "Where will your household watch?" }),
  ).toBeInTheDocument();
  const plex = screen.getByRole("switch", { name: "Plex enabled" }) as HTMLInputElement;
  if (!plex.checked) fireEvent.click(plex);
  fireEvent.click(screen.getByRole("button", { name: "Save and continue" }));
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
    await selectPlex();
    await screen.findByText(/Signed in as alice/);
    fireEvent.click(screen.getByRole("button", { name: "Save and continue" }));
    for (const heading of ["Usenet providers", "Indexers and addons"]) {
      await screen.findByRole("heading", { name: heading });
      fireEvent.click(screen.getByLabelText(/Skip the connection test/));
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
    await selectPlex();
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
    await selectPlex();
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
    await selectPlex();
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
    await selectPlex();
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
      fireEvent.click(screen.getByLabelText(/Skip the connection test/));
      fireEvent.click(screen.getByRole("button", { name: "Save and continue" }));
    }
    await screen.findByRole("heading", { name: "Cache folder and disk budget" });
    fireEvent.change(screen.getByDisplayValue(fake.state.config.cache.dir), {
      target: { value: "/cache" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Save and continue" }));
    await screen.findByRole("button", { name: "Finish setup" });
    const finishNow = screen.getByRole("button", { name: "Finish now, add sources later" });
    fireEvent.click(finishNow);
    expect(
      await screen.findByRole("heading", { name: "Restart Ether to use your cache folder" }),
    ).toBeInTheDocument();
    expect(screen.getByText("/cache")).toBeInTheDocument();
    // The restart is one click away, not a manual Docker or service step.
    expect(screen.getByRole("button", { name: "Restart Ether" })).toBeInTheDocument();
    expect(fake.state.config.cache.dir).toBe("/cache");
  });

  it("saves a fresh Plex step and a provider without submitting blank future sources", async () => {
    const fake = renderWizard((f) => {
      f.state.config.usenet.providers = [];
      f.state.config.indexers = [];
    });
    await selectPlex();
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
    await selectPlex();
    await screen.findByText(/Signed in as alice/);
    fireEvent.click(screen.getByRole("button", { name: "Save and continue" }));
    await screen.findByRole("heading", { name: "Debrid accounts" });
    expect(screen.queryByText(/ships none/)).toBeNull();
    expect(
      screen.getByText("Add a debrid account, a Usenet provider, or both, and test each one."),
    ).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Add debrid account" }));
    expect(
      screen.queryByText("Add a debrid account, a Usenet provider, or both, and test each one."),
    ).toBeNull();
    expect(screen.queryByLabelText("API token")).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Debrid service" }));
    fireEvent.click(screen.getByRole("option", { name: "Real-Debrid" }));
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
    await selectPlex();
    await screen.findByText(/Signed in as alice/);
    fireEvent.click(screen.getByRole("button", { name: "Save and continue" }));
    await screen.findByRole("heading", { name: "Usenet providers" });
    fireEvent.click(screen.getByLabelText(/Skip the connection test/));
    expect(screen.getByRole("button", { name: "Save and continue" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Finish now, add sources later" })).toBeEnabled();
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
      await selectPlex();
      await screen.findByText(/Signed in as alice/);
      fireEvent.click(screen.getByRole("button", { name: "Save and continue" }));
      await screen.findByRole("heading", { name: "Usenet providers" });
      if (kind === "indexer") {
        fireEvent.click(screen.getByRole("button", { name: "Use a ready-made setup" }));
        fireEvent.click(screen.getByRole("button", { name: "Save and continue" }));
        await screen.findByRole("heading", { name: "Indexers and addons" });
      }
      fireEvent.click(screen.getByRole("button", { name: `Add ${kind}` }));
      fireEvent.change(screen.getAllByRole("textbox")[0]!, { target: { value: "Keep my draft" } });
      fireEvent.click(screen.getByLabelText(/Skip the connection test/));
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
    await selectPlex();
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
    expect(calls(fake, "PUT", "/config").at(-1)?.body).toMatchObject({
      plex: { token: "••••", claimed: true, ownerUsername: "alice" },
    });
    expect(screen.getByRole("heading", { name: "Usenet providers" })).toBeInTheDocument();
  });

  it.each(
    [false, true].flatMap((ownerConfigured) =>
      ["success", "claim failure", "refresh failure", "wrong owner"].map((outcome) => ({
        ownerConfigured,
        outcome,
      })),
    ),
  )(
    "reconnects a saved revoked Plex account: $outcome (local owner=$ownerConfigured)",
    async ({ outcome, ownerConfigured }) => {
      const fake = renderWizard((f) => {
        f.state.config.setupComplete = true;
        f.state.config.plex.claimed = true;
        f.state.config.plex.token = "revoked-token";
        f.state.boot.ownerConfigured = ownerConfigured;
      });
      await selectPlex();
      await screen.findByText(/Signed in as alice/);
      const before = structuredClone(fake.state.config);
      if (outcome === "refresh failure") {
        vi.spyOn(api, "getConfig").mockRejectedValueOnce(new Error("Config refresh unavailable"));
      }
      vi.useFakeTimers();
      vi.spyOn(window, "open").mockReturnValue(null);
      vi.spyOn(api, "pinStart").mockResolvedValue({
        id: "reconnect",
        code: "ABCD",
        authUrl: "https://example.test/auth",
      });
      const poll = vi.spyOn(api, "pinPoll").mockImplementation(async () => {
        if (outcome === "wrong owner") throw new Error("Sign in as the existing owner");
        fake.state.config.plex.token = "fresh-owner-token";
        return { done: true };
      });
      const claim = vi.spyOn(api, "claim").mockImplementation(async () => {
        if (outcome === "claim failure") throw new Error("Plex claim unavailable");
        fake.state.config.plex.token = "fresh-server-token";
        return { ok: true };
      });
      await act(async () => {
        fireEvent.click(screen.getByRole("button", { name: "Reconnect" }));
      });
      expect(screen.getByRole("button", { name: "Save and continue" })).toBeDisabled();
      expect(screen.queryByText(/Signed in as alice/)).not.toBeInTheDocument();
      await act(async () => {
        await vi.advanceTimersByTimeAsync(2000);
      });
      expect(screen.queryByText("Waiting for approval")).not.toBeInTheDocument();
      if (outcome === "wrong owner") {
        expect(screen.getByText("Sign in as the existing owner")).toBeInTheDocument();
        expect(claim).not.toHaveBeenCalled();
        expect(fake.state.config).toEqual(before);
        expect(screen.getByRole("button", { name: "Save and continue" })).toBeDisabled();
      } else if (outcome === "claim failure" || outcome === "refresh failure") {
        const failure =
          outcome === "claim failure" ? /Plex claim unavailable/ : /Config refresh unavailable/;
        expect(screen.getAllByText(failure).length).toBeGreaterThan(0);
        expect(screen.getByRole("button", { name: "Save and continue" })).toBeDisabled();
        claim.mockImplementation(async () => {
          fake.state.config.plex.token = "fresh-server-token";
          return { ok: true };
        });
        await act(async () => {
          fireEvent.click(screen.getByRole("button", { name: "Claim server" }));
        });
        expect(claim).toHaveBeenCalledTimes(2);
      }
      await act(async () => {
        await vi.advanceTimersByTimeAsync(4000);
      });
      expect(poll).toHaveBeenCalledOnce();
      expect(screen.getByRole("button", { name: "Reconnect" })).toBeEnabled();
      if (outcome !== "wrong owner") {
        expect(screen.getByRole("button", { name: "Save and continue" })).toBeEnabled();
        expect(fake.state.config).toEqual({
          ...before,
          plex: { ...before.plex, token: "fresh-server-token" },
        });
        await act(async () => {
          fireEvent.click(screen.getByRole("button", { name: "Save and continue" }));
        });
        expect(calls(fake, "PUT", "/config").at(-1)?.body).toMatchObject({
          ...before,
          plex: { ...before.plex, token: "fresh-server-token", claimed: true },
        });
      }
    },
  );

  it("keeps claim failures actionable and refreshes credentials after a retry", async () => {
    const fake = renderWizard((f) => {
      f.state.config.plex.claimed = false;
    });
    await selectPlex();
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

  it("creates a local owner when Plex is not selected", async () => {
    renderWizard((f) => {
      f.state.config.plex.token = "";
      f.state.config.plex.claimed = false;
    });
    const create = vi.spyOn(api, "localSetup").mockResolvedValue({ token: "local-session" });
    await continueLanguage();
    await screen.findByRole("heading", { name: "Where will your household watch?" });
    fireEvent.click(screen.getByRole("switch", { name: "Jellyfin enabled" }));
    fireEvent.click(screen.getByRole("button", { name: "Save and continue" }));
    await screen.findByRole("heading", { name: "Create your owner account" });
    fireEvent.change(screen.getByLabelText("Username"), { target: { value: "owner" } });
    fireEvent.change(screen.getByLabelText("Password"), { target: { value: "secret" } });
    fireEvent.change(screen.getByLabelText("Password again"), { target: { value: "secret" } });
    fireEvent.click(screen.getByRole("button", { name: "Create owner" }));
    await waitFor(() => expect(create).toHaveBeenCalledWith("owner", "secret"));
    expect(
      await screen.findByRole("heading", { name: "Where Ether streams from" }),
    ).toBeInTheDocument();
  });

  it("walks from a claimed Plex step to providers and gates on the provider test", async () => {
    const fake = renderWizard();
    await selectPlex();
    expect(await screen.findByText(/Signed in as alice/)).toBeInTheDocument();
    const next = screen.getByRole("button", { name: "Save and continue" });
    expect(next).toBeEnabled();
    fireEvent.click(next);
    expect(await screen.findByRole("heading", { name: "Usenet providers" })).toBeInTheDocument();
    expect(calls(fake, "PUT", "/config")).toHaveLength(3);
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
    await selectPlex();
    await screen.findByText(/Signed in as alice/);
    fireEvent.click(screen.getByRole("button", { name: "Save and continue" }));
    await screen.findByRole("heading", { name: "Usenet providers" });
    expect(screen.getByRole("button", { name: "Save and continue" })).toBeDisabled();
    fireEvent.click(screen.getByLabelText(/Skip the connection test/));
    expect(screen.getByRole("button", { name: "Save and continue" })).toBeEnabled();
  });

  it("an unfinished fresh install enters the wizard and has no Finish setup nav", async () => {
    sessionStorage.removeItem(SETUP_STEP_KEY);
    renderApp("/", { bootstrap: { setupComplete: false, ownerConfigured: false } });
    expect(await screen.findByRole("heading", { name: "Set up Ether" })).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Choose a language" })).toBeInTheDocument();
    expect(screen.queryAllByRole("link", { name: "Finish setup" })).toHaveLength(0);
    expect(screen.queryAllByRole("link", { name: "Overview" })).toHaveLength(0);
  });

  it("sends an existing owner to Overview while setup is still incomplete", async () => {
    renderApp("/", {
      bootstrap: { setupComplete: false, ownerConfigured: true, authenticated: true },
    });
    expect(await screen.findByRole("heading", { name: "Overview" })).toBeInTheDocument();
    expect(
      screen.queryByRole("heading", { name: "Where will your household watch?" }),
    ).not.toBeInTheDocument();
    expect(screen.queryAllByRole("link", { name: "Finish setup" })).toHaveLength(0);
  });

  it("keeps app settings when the choice is saved, and keeps the choice when saving fails", async () => {
    const fake = renderWizard((f) => {
      f.state.config.frontends = {
        plex: { enabled: true, settings: { serverName: "Keep me" } },
        jellyfin: { enabled: false, settings: { serverName: "House" } },
        dlna: { enabled: true, settings: { guest: true } },
      };
    });
    await continueLanguage();
    expect(await screen.findByRole("switch", { name: "Jellyfin enabled" })).not.toBeChecked();
    fireEvent.click(screen.getByRole("switch", { name: "Jellyfin enabled" }));
    fireEvent.click(screen.getByRole("switch", { name: "Plex enabled" }));
    const put = vi.spyOn(api, "putConfig").mockRejectedValueOnce(new Error("disk full"));
    fireEvent.click(screen.getByRole("button", { name: "Save and continue" }));
    expect(await screen.findByText(/disk full/)).toBeInTheDocument();
    expect(
      screen.getByRole("heading", { name: "Where will your household watch?" }),
    ).toBeInTheDocument();
    expect(screen.getByRole("switch", { name: "Jellyfin enabled" })).toBeChecked();
    expect(screen.getByRole("switch", { name: "Plex enabled" })).not.toBeChecked();
    put.mockRestore();
    fireEvent.click(screen.getByRole("button", { name: "Save and continue" }));
    await screen.findByRole("heading", { name: "Create your owner account" });
    const body = calls(fake, "PUT", "/config").at(-1)?.body as {
      frontends: Record<string, { enabled: boolean; settings?: Record<string, unknown> }>;
    };
    expect(body.frontends.plex).toEqual({ enabled: false, settings: { serverName: "Keep me" } });
    expect(body.frontends.jellyfin).toEqual({ enabled: true, settings: { serverName: "House" } });
    expect(body.frontends.dlna).toEqual({ enabled: true, settings: { guest: true } });
    fireEvent.click(screen.getByRole("button", { name: "Back" }));
    expect(screen.getByRole("switch", { name: "Jellyfin enabled" })).toBeChecked();
    expect(screen.getByRole("switch", { name: "Plex enabled" })).not.toBeChecked();
  });

  it("resumes the same wizard step after the owner account is created", async () => {
    renderWizard((f) => {
      sessionStorage.setItem(SETUP_STEP_KEY, "2");
      f.state.config.plex.token = "";
      f.state.config.plex.claimed = false;
      f.state.config.frontends = { jellyfin: { enabled: true, settings: { serverName: "House" } } };
      f.state.boot.ownerConfigured = true;
    });
    expect(
      await screen.findByRole("heading", { name: "Create your owner account" }),
    ).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Set up Ether" })).toBeInTheDocument();
    expect(screen.queryAllByRole("link", { name: "Finish setup" })).toHaveLength(0);
  });

  it("saves one language as the console, the catalogue, and household defaults", async () => {
    const fake = renderWizard();
    await screen.findByRole("heading", { name: "Choose a language" });
    fireEvent.click(screen.getByLabelText("Language"));
    fireEvent.click(await screen.findByRole("option", { name: "Français" }));
    await waitFor(() =>
      expect(screen.getByLabelText(french("Language"))).toHaveTextContent("Français"),
    );
    fireEvent.click(screen.getByRole("button", { name: french("Save and continue") }));
    await screen.findByRole("heading", { name: french("Where will your household watch?") });
    const config = calls(fake, "PUT", "/config").at(-1)?.body as {
      catalog: { language: string; region: string };
    };
    expect(config.catalog.language).toBe("fr-FR");
    expect(config.catalog.region).toBe("US");
    expect(calls(fake, "PUT", "/stream-preferences/0").at(-1)?.body).toMatchObject({
      audioLanguages: ["fr"],
      subtitleLanguages: ["fr"],
      preferOriginal: false,
      subtitleMode: "off",
    });
    expect(localStorage.getItem("ether.locale")).toBe("fr");
  });

  it("keeps a separate audio language and the browser region under more options", async () => {
    const fake = renderWizard();
    await screen.findByRole("heading", { name: "Choose a language" });
    fireEvent.click(screen.getByRole("button", { name: "More options" }));
    fireEvent.click(screen.getByLabelText("Audio language"));
    fireEvent.click(await screen.findByRole("option", { name: "Japanese" }));
    fireEvent.click(screen.getByRole("button", { name: "Save and continue" }));
    await screen.findByRole("heading", { name: "Where will your household watch?" });
    const config = calls(fake, "PUT", "/config").at(-1)?.body as {
      catalog: { language: string; region: string };
    };
    expect(config.catalog.language).toBe("en-US");
    expect(config.catalog.region).toBe("US");
    expect(calls(fake, "PUT", "/stream-preferences/0").at(-1)?.body).toMatchObject({
      audioLanguages: ["ja"],
      subtitleLanguages: ["en"],
      preferOriginal: false,
      subtitleMode: "off",
    });
  });

  it("does not rewrite a finished server until the language choice changes", async () => {
    const fake = renderWizard((f) => {
      f.state.config.setupComplete = true;
      f.state.config.catalog.language = "de-DE";
      f.state.config.catalog.region = "DE";
      const defaults = f.state.streamDefaults;
      if (!defaults) throw new Error("household defaults are not modelled");
      defaults.audioLanguages = ["de"];
      defaults.subtitleLanguages = ["de"];
      defaults.subtitleMode = "foreign-only";
    });
    await screen.findByRole("heading", { name: "Choose a language" });
    fireEvent.click(screen.getByRole("button", { name: "Save and continue" }));
    await screen.findByRole("heading", { name: "Where will your household watch?" });
    expect(calls(fake, "PUT", "/stream-preferences/0")).toHaveLength(0);
    const config = calls(fake, "PUT", "/config").at(-1)?.body as {
      catalog: { language: string; region: string };
    };
    expect(config.catalog.language).toBe("de-DE");
    expect(config.catalog.region).toBe("DE");
    expect(fake.state.streamDefaults?.subtitleMode).toBe("foreign-only");
  });
});

describe("mergeFrontendSelection", () => {
  it("changes enabled flags and keeps settings and unknown front ends", () => {
    const cfg = createFakeServer().state.config;
    const next = mergeFrontendSelection(
      {
        ...cfg,
        frontends: {
          plex: { enabled: true, settings: { serverName: "Keep me" } },
          m3u: { enabled: true, settings: { path: "/tv" } },
        },
      },
      ["plex", "jellyfin"],
      { plex: false, jellyfin: true },
    );
    expect(next.frontends).toEqual({
      plex: { enabled: false, settings: { serverName: "Keep me" } },
      jellyfin: { enabled: true },
      m3u: { enabled: true, settings: { path: "/tv" } },
    });
  });
});
