import { describe, expect, it } from "vitest";
import {
  checkCatalog,
  extract,
  localeCodes,
  sourceCatalogIsCurrent,
  staleMemos,
} from "../../scripts/i18n-lib.mjs";

/**
 * Every shipped language must cover every message, with matching
 * placeholders and the plural categories its grammar needs. When this fails
 * after adding copy, run `pnpm i18n sync`, then `pnpm i18n todo <locale>`
 * and `pnpm i18n merge` for each language.
 */
describe("translation catalogues", () => {
  const { messages } = extract();

  it("finds the console's messages", () => {
    expect(messages.size).toBeGreaterThan(50);
  });

  it("keeps the English reference catalogue current", () => {
    expect(sourceCatalogIsCurrent(messages)).toBe(true);
  });

  for (const locale of localeCodes()) {
    it(`${locale} is complete and valid`, () => {
      const { missing, stale, invalid } = checkCatalog(locale, messages);
      expect({ missing: missing.slice(0, 20), count: missing.length }).toEqual({
        missing: [],
        count: 0,
      });
      expect(stale).toEqual([]);
      expect(invalid).toEqual([]);
    });
  }
});

/**
 * A language switch re-renders the console in place (drafts survive), so a
 * memo that builds copy must list `locale` in its dependencies or it keeps
 * the previous language. Add `const { locale } = useLocale()` and the
 * dependency, or mark a memo that holds no copy with
 * `// i18n: memo holds no copy`.
 */
describe("memoised copy", () => {
  it("recomputes after a language switch", () => {
    expect(staleMemos()).toEqual([]);
  });
});
