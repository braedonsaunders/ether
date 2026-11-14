import {
  GlobalSearch,
  toast,
  type GlobalSearchGroup,
  type GlobalSearchResult,
} from "@braedonsaunders/appkit-ui";
import { useCallback, useRef } from "react";
import { useNavigate } from "react-router-dom";
import { api, apiFetch, errorMessage } from "../lib/api";
import { t, tn } from "../i18n";
import { BUILTIN, textOf, useRegistry, type Descriptor } from "../lib/extend";
import {
  pluginPageRoute,
  searchTextOf,
  type CommandMeta,
  type PageMeta,
} from "../lib/console-descriptors";
import { destinationsFrom } from "../lib/sitemap";
import { settingsIndex } from "../lib/settings-index";
import { settingHref } from "./SettingFocus";
import { loadViewers, viewerName } from "./pickers";

function matches(q: string, ...fields: (string | undefined)[]): boolean {
  const hay = fields.filter(Boolean).join(" ").toLowerCase();
  return q
    .toLowerCase()
    .split(/\s+/)
    .filter(Boolean)
    .every((w) => hay.includes(w));
}

const ACTION_PREFIX = "action:";

interface PluginActionOutcome {
  toast?: string | { text: string; kind?: "success" | "error" | "info" };
  open?: { page?: string; route?: string; params?: Record<string, string> };
}

/**
 * ⌘K: jump to any page, section, viewer or collection, or start a common
 * task. Destinations and actions come from the descriptor registry (WS1d):
 * page descriptors (built-in and plugin) fill "Go to", command descriptors
 * with a route or plugin action fill "Actions". Agent suggestions are not ⌘K
 * actions.
 */
export function CommandSearch() {
  const navigate = useNavigate();
  const { descriptors } = useRegistry("page", "navItem", "command");
  const live = useRef<Descriptor[]>([]);
  live.current = descriptors;
  const search = useCallback(
    async (q: string, signal: AbortSignal): Promise<GlobalSearchResult> => {
      const all = live.current;
      const pages = all.filter((d) => d.kind === "page") as Descriptor<PageMeta>[];
      const commands = all.filter((d) => d.kind === "command") as Descriptor<CommandMeta>[];
      const groups: GlobalSearchGroup[] = [];
      const places = destinationsFrom(pages)
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
      const seen = new Set<string>();
      const settings = settingsIndex()
        .filter((s) => matches(q, s.title, s.where, s.keywords))
        .filter((s) => {
          const key = `${s.href}|${s.title}`;
          if (seen.has(key)) return false;
          seen.add(key);
          return true;
        })
        .slice(0, 8)
        .map((s) => ({
          id: `${s.href}|${s.title}`,
          type: "setting",
          title: s.title,
          subtitle: s.where,
          href: settingHref(s.href, s.key, s.more),
        }));
      if (settings.length) groups.push({ id: "settings", label: t("Settings"), hits: settings });
      const actions = commands
        .filter(
          (c) =>
            (c.meta?.route || c.meta?.action) &&
            !c.meta.agentSuggestion &&
            matches(q, textOf(c.label), searchTextOf(c.meta.keywords)),
        )
        .map((c) => ({
          id: c.id,
          type: "action",
          title: textOf(c.label),
          href:
            c.meta?.route ||
            `${ACTION_PREFIX}${encodeURIComponent(c.owner)}/${encodeURIComponent(c.meta!.action!)}`,
        }));
      if (actions.length) groups.push({ id: "actions", label: t("Actions"), hits: actions });
      const [viewers, collections, liveTV] = await Promise.all([
        loadViewers().catch(() => []),
        api.collections().catch(() => []),
        q.trim()
          ? apiFetch<{
              channels: { id: string; name: string; number: string }[];
              programmes: { id: string; channelId: string; title: string; episodeTitle?: string }[];
            }>(`/livetv/search?q=${encodeURIComponent(q)}`, { signal }).catch(() => null)
          : null,
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
      if (liveTV?.channels.length)
        groups.push({
          id: "live-channels",
          label: t("Channels"),
          hits: liveTV.channels.map((c) => ({
            id: `live-channel:${c.id}`,
            type: "channel",
            title: c.name,
            subtitle: c.number,
            href: `/livetv?channel=${encodeURIComponent(c.id)}`,
          })),
        });
      if (liveTV?.programmes.length)
        groups.push({
          id: "live-programmes",
          label: t("Guide"),
          hits: liveTV.programmes.map((p) => ({
            id: `live-programme:${p.id}`,
            type: "programme",
            title: p.title,
            subtitle: p.episodeTitle,
            href: `/livetv?channel=${encodeURIComponent(p.channelId)}`,
          })),
        });
      return { groups, total: groups.reduce((n, g) => n + g.hits.length, 0) };
    },
    [],
  );
  const runHit = useCallback(
    (href: string) => {
      if (!href.startsWith(ACTION_PREFIX)) {
        navigate(href);
        return;
      }
      const [rawOwner = "", rawAction = ""] = href.slice(ACTION_PREFIX.length).split("/", 2);
      const owner = decodeURIComponent(rawOwner);
      const action = decodeURIComponent(rawAction);
      if (!owner || owner === BUILTIN || !action) return;
      void apiFetch<PluginActionOutcome>(
        `/plugins/${encodeURIComponent(owner)}/actions/${encodeURIComponent(action)}`,
        { method: "POST", body: JSON.stringify({ params: {} }) },
      )
        .then((res) => {
          if (res?.toast) {
            const spec =
              typeof res.toast === "string"
                ? { text: res.toast, kind: "success" as const }
                : res.toast;
            if (spec.kind === "error") toast.error(spec.text);
            else if (spec.kind === "info") toast.info(spec.text);
            else toast.success(spec.text);
          }
          if (res?.open?.page) {
            const q = new URLSearchParams(res.open.params ?? {});
            const suffix = q.toString() ? `?${q}` : "";
            navigate(`${pluginPageRoute(owner, res.open.page)}${suffix}`);
          } else if (
            res?.open?.route &&
            res.open.route.startsWith("/") &&
            !res.open.route.startsWith("//")
          ) {
            navigate(res.open.route);
          }
        })
        .catch((e) => toast.error(errorMessage(e)));
    },
    [navigate],
  );
  return (
    <GlobalSearch
      className="w-full max-w-md"
      search={search}
      minimumQueryLength={1}
      onNavigate={(hit) => runHit(hit.href)}
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
