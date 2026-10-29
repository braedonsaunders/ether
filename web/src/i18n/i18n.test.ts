import type { MessageCatalog } from "@braedonsaunders/appkit-i18n";
import { afterEach, describe, expect, it, vi } from "vitest";
import {
  getLocale,
  loadLocale,
  localized,
  preferredLocale,
  rememberLocale,
  startLocale,
  t,
  tn,
  type CatalogLoader,
} from ".";

const offline: CatalogLoader = () => Promise.reject(new Error("chunk failed to load"));

afterEach(async () => {
  localStorage.clear();
  await loadLocale("en");
});

describe("locale preference", () => {
  it("prefers the saved choice over the browser", () => {
    rememberLocale("ja");
    expect(preferredLocale()).toBe("ja");
    localStorage.setItem("ether.locale", "klingon");
    expect(preferredLocale()).not.toBe("klingon");
  });
});

describe("translation", () => {
  it("returns the English source and fills placeholders", () => {
    expect(t("Saved {name}", { name: "Movies" })).toBe("Saved Movies");
    expect(t("Keep {unknown}")).toBe("Keep {unknown}");
    expect(tn(1, "{count} title", "{count} titles")).toBe("1 title");
    expect(tn(1200, "{count} title", "{count} titles")).toBe("1,200 titles");
  });

  it("switches catalogues and plural rules", async () => {
    await loadLocale("de");
    expect(getLocale()).toBe("de");
    expect(document.documentElement.lang).toBe("de");
    expect(t("Display language")).not.toBe("Display language");
    expect(tn(1200, "{count} title", "{count} titles")).toContain("1.200");
  });

  it("rebuilds localized tables after a switch", async () => {
    const table = localized(() => ({ label: t("Display language") }));
    const english = table.label;
    await loadLocale("fr");
    expect(table.label).not.toBe(english);
    expect(Object.keys(table)).toEqual(["label"]);
  });
});

describe("loading", () => {
  it("applies the last requested language when loads finish out of order", async () => {
    let finishGerman!: (catalog: MessageCatalog) => void;
    const loader: CatalogLoader = (locale) =>
      locale === "de" ? new Promise((resolve) => (finishGerman = resolve)) : Promise.resolve({});
    const german = loadLocale("de", loader);
    const french = loadLocale("fr", loader);
    expect(await french).toBe(true);
    finishGerman({});
    expect(await german).toBe(false);
    expect(getLocale()).toBe("fr");
    expect(document.documentElement.lang).toBe("fr");
  });

  it("keeps the current language when a switch fails", async () => {
    await loadLocale("fr");
    const french = t("Display language");
    await expect(loadLocale("de", offline)).rejects.toThrow("chunk failed to load");
    expect(getLocale()).toBe("fr");
    expect(t("Display language")).toBe(french);
  });

  it("starts in English when the saved language cannot load, keeping the preference", async () => {
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
    rememberLocale("de");
    expect(await startLocale(offline)).toBe("en");
    expect(t("Display language")).toBe("Display language");
    expect(localStorage.getItem("ether.locale")).toBe("de");
    expect(warn).toHaveBeenCalled();
    warn.mockRestore();
  });

  it("starts in the saved language when it loads", async () => {
    rememberLocale("ja");
    expect(await startLocale()).toBe("ja");
    expect(t("Display language")).not.toBe("Display language");
  });
});
