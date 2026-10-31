import {
  GlobalSearch,
  type GlobalSearchGroup,
  type GlobalSearchResult,
} from "@braedonsaunders/appkit-ui";
import { useCallback } from "react";
import { useNavigate } from "react-router-dom";
import { api } from "../lib/api";
import { localized, t, tn, msg, searchTerms } from "../i18n";
import { destinations } from "../lib/sitemap";
import { loadViewers, viewerName } from "./pickers";

const ACTIONS = localized(() => [
  {
    title: t("Record a problem"),
    href: "/maintenance/logs?record=1",
    keywords: searchTerms(msg("support zip diagnostic capture")),
  },
  {
    title: t("Add an indexer"),
    href: "/sources/yours/indexers",
    keywords: searchTerms(msg("newznab prowlarr")),
  },
  {
    title: t("Add a Usenet provider"),
    href: "/sources/yours/usenet",
    keywords: searchTerms(msg("nntp")),
  },
  {
    title: t("Add a debrid provider"),
    href: "/sources/yours/debrid",
    keywords: searchTerms(
      msg("real-debrid torbox alldebrid premiumize debrid-link debridlink offcloud torrent"),
    ),
  },
  { title: t("New collection"), href: "/collections/new", keywords: searchTerms(msg("create")) },
  {
    title: t("New notification channel"),
    href: "/notifications/channels",
    keywords: searchTerms(msg("alert")),
  },
  {
    title: t("Back up now"),
    href: "/maintenance/backups",
    keywords: searchTerms(msg("backup snapshot")),
  },
]);

function matches(q: string, ...fields: (string | undefined)[]): boolean {
  const hay = fields.filter(Boolean).join(" ").toLowerCase();
  return q
    .toLowerCase()
    .split(/\s+/)
    .filter(Boolean)
    .every((w) => hay.includes(w));
}

/** ⌘K: jump to any page, section, viewer or collection, or start a common task. */
export function CommandSearch() {
  const navigate = useNavigate();
  const search = useCallback(
    async (q: string, signal: AbortSignal): Promise<GlobalSearchResult> => {
      const groups: GlobalSearchGroup[] = [];
      const places = destinations()
        .filter((d) => matches(q, d.title, d.page, d.keywords))
        .slice(0, 8)
        .map((d) => ({
          id: d.href,
          type: "page",
          title: d.title,
          subtitle: d.isPage ? undefined : d.page,
          href: d.href,
        }));
      if (places.length) groups.push({ id: "places", label: t("Go to"), hits: places });
      const actions = ACTIONS.filter((a) => matches(q, a.title, a.keywords)).map((a) => ({
        id: a.title,
        type: "action",
        title: a.title,
        href: a.href,
      }));
      if (actions.length) groups.push({ id: "actions", label: t("Actions"), hits: actions });
      const [viewers, collections] = await Promise.all([
        loadViewers().catch(() => []),
        api.collections().catch(() => []),
      ]);
      if (signal.aborted) return { groups: [], total: 0 };
      const people = viewers
        .filter((v) => matches(q, viewerName(v), v.username))
        .slice(0, 5)
        .map((v) => ({
          id: String(v.id),
          type: "viewer",
          title: viewerName(v),
          subtitle: v.isOwner ? t("Owner") : t("Viewer"),
          href: `/viewers/${v.id}`,
        }));
      if (people.length) groups.push({ id: "viewers", label: t("Viewers"), hits: people });
      const cols = (Array.isArray(collections) ? collections : [])
        .filter((c) => c.id && matches(q, c.name))
        .slice(0, 5)
        .map((c) => ({
          id: c.id!,
          type: "collection",
          title: c.name,
          subtitle: t("Collection"),
          href: `/collections/${encodeURIComponent(c.id!)}`,
        }));
      if (cols.length) groups.push({ id: "collections", label: t("Collections"), hits: cols });
      return { groups, total: groups.reduce((n, g) => n + g.hits.length, 0) };
    },
    [],
  );
  return (
    <GlobalSearch
      className="w-full max-w-md"
      search={search}
      minimumQueryLength={1}
      onNavigate={(hit) => navigate(hit.href)}
      labels={{
        placeholder: t("Search or jump to…"),
        ariaLabel: t("Search Ether"),
        clear: t("Clear search"),
        searching: t("Searching…"),
        noMatches: (query) => t("Nothing matches “{query}”", { query }),
        navigate: t("navigate"),
        open: t("open"),
        close: t("close"),
        resultCount: (count) => tn(count, "{count} result", "{count} results"),
      }}
    />
  );
}
