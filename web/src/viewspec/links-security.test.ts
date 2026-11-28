import { describe, expect, it } from "vitest";
import { safeHref } from "./cells";

describe("plugin link boundaries", () => {
  it("rejects browser-normalized external paths and malformed HTTPS URLs", () => {
    for (const href of [
      "/\\evil.example",
      "/\t/evil.example",
      "https://",
      "https://user:pass@example.com",
    ]) {
      expect(safeHref(href)).toBeNull();
    }
    expect(safeHref("/catalog")).toBe("/catalog");
    expect(safeHref("https://example.org/page")).toBe("https://example.org/page");
  });
});

describe("plugin screen logo boundary", () => {
  it("only loads external http(s) logos, never console paths", async () => {
    const { safeLogoUrl } = await import("./widgets");
    for (const logo of [
      "/ether/api/admin/config",
      `${window.location.origin}/ether/api/admin/config`,
      "//evil.example/x.png",
      "javascript:alert(1)",
      "https://user:pass@example.com/x.png",
      "https://cdn.example.com/\nx.png",
      42,
    ]) {
      expect(safeLogoUrl(logo)).toBeNull();
    }
    expect(safeLogoUrl("https://cdn.example.com/logo.png")).toBe("https://cdn.example.com/logo.png");
    expect(safeLogoUrl("http://logos.example.net/a.png")).toBe("http://logos.example.net/a.png");
  });
});
