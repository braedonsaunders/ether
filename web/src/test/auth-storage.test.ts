import { afterEach, describe, expect, it, vi } from "vitest";
import { apiBlob, apiFetch, getToken, SESSION_HEADER, sessionXhrSetup, setToken } from "../lib/api";

afterEach(() => {
  vi.restoreAllMocks();
  setToken("");
});

describe("owner credentials when browser storage is unavailable", () => {
  it("uses the supplied credential in memory and clears it on sign out", async () => {
    vi.spyOn(Storage.prototype, "getItem").mockImplementation(() => {
      throw new DOMException("blocked", "SecurityError");
    });
    vi.spyOn(Storage.prototype, "setItem").mockImplementation(() => {
      throw new DOMException("blocked", "SecurityError");
    });
    vi.spyOn(Storage.prototype, "removeItem").mockImplementation(() => {
      throw new DOMException("blocked", "SecurityError");
    });
    const fetcher = vi.fn<typeof fetch>().mockResolvedValue(new Response("{}", { status: 200 }));
    vi.stubGlobal("fetch", fetcher);
    setToken("session-only-token");
    expect(getToken()).toBe("session-only-token");
    await apiFetch("/config");
    const sent = new Headers(fetcher.mock.calls[0]?.[1]?.headers);
    expect(sent.get(SESSION_HEADER)).toBe("session-only-token");
    expect(sent.get("X-Plex-Token")).toBeNull();
    setToken("");
    expect(getToken()).toBe("");
  });

  it("uses the new token if writes fail but reads still return an old one", () => {
    setToken("old-token");
    vi.spyOn(Storage.prototype, "setItem").mockImplementation(() => {
      throw new DOMException("full", "QuotaExceededError");
    });
    setToken("new-token");
    expect(getToken()).toBe("new-token");
    vi.spyOn(Storage.prototype, "removeItem").mockImplementation(() => {
      throw new DOMException("blocked", "SecurityError");
    });
    setToken("");
    expect(getToken()).toBe("");
  });
});

describe("console session", () => {
  it("is stored under its own key and drops a stored plex.tv token", async () => {
    localStorage.setItem("ether-admin-token", "plex-account-token");
    vi.resetModules();
    const fresh = await import("../lib/api");
    expect(localStorage.getItem("ether-admin-token")).toBeNull();
    expect(fresh.getToken()).toBe("");
    fresh.setToken("ether-admin-1");
    expect(localStorage.getItem("ether-admin-session")).toBe("ether-admin-1");
    fresh.setToken("");
    expect(localStorage.getItem("ether-admin-session")).toBeNull();
  });

  it("goes only to this console's API", async () => {
    const fetcher = vi
      .fn<typeof fetch>()
      .mockImplementation(async () => new Response(new Blob(["x"]), { status: 200 }));
    vi.stubGlobal("fetch", fetcher);
    setToken("ether-admin-2");
    await apiBlob("/ether/api/catalog/artwork/1");
    await apiBlob(window.location.origin + "/ether/api/backups/download/a");
    await apiBlob("https://image.tmdb.org/t/p/w500/poster.jpg");
    await apiBlob("/library/metadata/1/thumb");
    const sent = fetcher.mock.calls.map((c) => new Headers(c[1]?.headers).get(SESSION_HEADER));
    expect(sent).toEqual(["ether-admin-2", "ether-admin-2", null, null]);
  });

  it("is attached to Live TV preview playlist and segment requests, not to channel hosts", () => {
    setToken("ether-admin-3");
    const sent: Record<string, string | undefined> = {};
    for (const url of [
      "/ether/api/livetv/preview/abc/index.m3u8",
      window.location.origin + "/ether/api/livetv/preview/abc/seg-1.ts",
      "https://cdn.example/live/seg-1.ts",
      "//cdn.example/live/index.m3u8",
    ]) {
      const xhr = { setRequestHeader: vi.fn() } as unknown as XMLHttpRequest;
      sessionXhrSetup(xhr, url);
      const call = vi.mocked(xhr.setRequestHeader).mock.calls[0];
      sent[url] = call ? `${call[0]}: ${call[1]}` : undefined;
    }
    expect(Object.values(sent)).toEqual([
      `${SESSION_HEADER}: ether-admin-3`,
      `${SESSION_HEADER}: ether-admin-3`,
      undefined,
      undefined,
    ]);
  });
});
