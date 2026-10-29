import type { PagedTableLabels } from "@braedonsaunders/appkit-ui";
import { t, tx } from ".";

const strong = (n: number) => <strong className="font-semibold text-fg">{n}</strong>;

/** Translated PagedTable chrome; pass page-specific search copy as overrides. */
export function pagedTableLabels(overrides?: Partial<PagedTableLabels>): Partial<PagedTableLabels> {
  return {
    searchPlaceholder: t("Search…"),
    searchLabel: t("Search"),
    showing: (from, to, total) =>
      tx("Showing {from}–{to} of {total}", {
        from: strong(from),
        to: strong(to),
        total: strong(total),
      }),
    prev: t("Prev"),
    next: t("Next"),
    pageOf: (page, pages) => t("Page {page} of {pages}", { page, pages }),
    noResults: (query) => t("No matches for “{query}”", { query }),
    sortBy: (column) => t("Sort by {column}", { column }),
    ...overrides,
  };
}

/** Translated SearchSelect chrome, spread onto each `<SearchSelect>`. */
export function searchSelectLabels() {
  return {
    placeholder: t("Select…"),
    searchPlaceholder: t("Search…"),
    noneLabel: t("None"),
    noMatchesLabel: t("No matches"),
    searchingLabel: t("Searching…"),
    selectLabel: t("Select"),
    closeLabel: t("Close"),
  };
}
