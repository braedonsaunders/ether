import { SubtabNav } from "../../../components/layout";
import { Button, EmptyState, toast } from "@braedonsaunders/appkit-ui";
import {
  ArrowRight,
  Bookmark,
  LayoutGrid,
  List,
  RefreshCw,
  Search,
  LoaderCircle,
} from "lucide-react";
import { useEffect, useLayoutEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { Link } from "react-router-dom";
import { useApp } from "../../../app-context";
import { Drawer } from "../../../components/Drawer";
import { TitleSearch } from "../../../components/pickers";
import { ErrorPanel, PosterSkeletons } from "../../../components/feedback";
import { ToolbarPicker } from "../../../components/ToolbarPicker";
import { localized, t, tn, tx } from "../../../i18n";
import { api, apiFetch } from "../../../lib/api";
import { useAsync, useDebouncedValue } from "../../../lib/hooks";
import type { CollectionSpec } from "../../../lib/contract";
import { TitleInspector } from "./TitleInspector";
import { CatalogueResults } from "./CatalogueResults";
import "./catalog.css";
import { useCatalogAISearch } from "./useCatalogAISearch";
import { CatalogFilterBar, compileQuery, readQuery } from "./CatalogFilterBar";

const PAGE_SIZE = 48;
/** The most titles a collection browse shows (the preview's own cap). */
const COLLECTION_BROWSE_MAX = 200;
const SORTS = localized(() => [
  { value: "popularity", label: t("Most popular") },
  { value: "released", label: t("Recently released") },
  { value: "rating", label: t("Highest rated") },
  { value: "added", label: t("Recently added") },
  { value: "title", label: t("Title A–Z") },
  { value: "year", label: t("Year") },
  { value: "duration", label: t("Runtime") },
]);

export function CatalogBrowser({
  params,
  setParams,
  refreshHealth,
  ruleFilter,
  collection,
  heading,
  headerActions,
  libraryScope,
}: {
  heading?: string;
  headerActions?: ReactNode;
  ruleFilter?: { ruleVisibility: string; rules: string; ruleFallback: string };
  /** Pin the browser to one library: its id, media type, plus its saved filter ANDed in. */
  libraryScope?: { id?: number; type: string; filter?: Record<string, unknown> };
  /** Browse one collection's titles, in its own order, instead of the catalogue. */
  collection?: CollectionSpec;
  params: URLSearchParams;
  setParams: (p: URLSearchParams, options?: { replace?: boolean }) => void;
  refreshHealth: () => Promise<void>;
}) {
  const { dashboard } = useApp();
  useEffect(() => {
    // Remove data written by the retired catalogue view feature on upgrade.
    try {
      localStorage.removeItem("ether.catalog.views");
    } catch {
      /* Storage may be unavailable. */
    }
  }, []);
  const query = params.get("q") ?? "";
  const debouncedQuery = useDebouncedValue(query, 300);
  const type = libraryScope
    ? libraryScope.type
    : ["movie", "show"].includes(params.get("type") ?? "")
      ? params.get("type")!
      : "";
  const genre = params.get("genre") ?? "";
  const decade = params.get("decade") ?? "";
  const waiting = params.get("waiting") === "1";
  const sort = SORTS.some((s) => s.value === params.get("sort"))
    ? params.get("sort")!
    : "popularity";
  const secondarySort = params.get("secondarySort") || "";
  const descending = (value: string | null, field: string) =>
    value === "0" ? false : value === "1" ? true : field !== "title" && field !== "duration";
  const desc = descending(params.get("desc"), sort);
  const secondaryDesc = descending(params.get("secondaryDesc"), secondarySort);
  const rawPage = Number(params.get("page"));
  const collectionPageNumber =
    Number.isSafeInteger(rawPage) && rawPage > 0 ? Math.min(rawPage, 1000000) : 1;
  const cursorParams = new URLSearchParams(params);
  ["page", "title", "view"].forEach((key) => cursorParams.delete(key));
  cursorParams.sort();
  const cursorIdentity = `${cursorParams}|${JSON.stringify(ruleFilter)}|${JSON.stringify(collection)}|${JSON.stringify(libraryScope)}`;
  const [cursorPage, setCursorPage] = useState({ identity: "", after: "", page: 1 });
  const activeCursor = cursorPage.identity === cursorIdentity ? cursorPage : { after: "", page: 1 };
  const page = collection ? collectionPageNumber : activeCursor.page;
  const list = params.get("view") === "list";
  const selected = params.get("title");
  const advancedRaw = params.get("filters") || "";
  const advanced = useDebouncedValue(advancedRaw, 250);
  const viewer = params.get("viewer") || "";
  const compiledFilter = advanced ? compileQuery(readQuery(advanced), viewer) : {};
  // libraryId scopes the request on the server; only send the user's filters.
  const filterParam = Object.keys(compiledFilter).length
    ? JSON.stringify(compiledFilter)
    : undefined;
  const [lookupOpen, setLookupOpen] = useState(false);
  // Router transitions can update the URL before the previous controls commit.
  // Merge rapid interactions into the latest requested state, not a stale render.
  const latestParams = useRef(params);
  useLayoutEffect(() => {
    latestParams.current = params;
  }, [params]);
  const update = (key: string, value: string, replace = false) => {
    const next = new URLSearchParams(latestParams.current);
    if (value) next.set(key, value);
    else next.delete(key);
    if (!["page", "view", "title"].includes(key)) next.delete("page");
    if (key === "type") {
      next.delete("genre");
      next.delete("decade");
    }
    latestParams.current = next;
    setParams(next, { replace });
  };
  const specKey = collection ? JSON.stringify(collection) : "";
  const members = useAsync(
    (signal) =>
      collection
        ? api.previewCollection(collection, signal, COLLECTION_BROWSE_MAX)
        : Promise.resolve(null),
    [specKey],
  );
  const libraryId = libraryScope?.id;
  const browseRequestKey = JSON.stringify([
    libraryId,
    debouncedQuery,
    type,
    genre,
    decade,
    waiting,
    sort,
    page,
    activeCursor.after,
    ruleFilter,
    specKey,
    advanced,
    viewer,
    secondarySort,
    desc,
    secondaryDesc,
  ]);
  const browsed = useAsync(
    (signal) =>
      collection
        ? Promise.resolve(null)
        : api
            .browseCatalog(
              {
                ...ruleFilter,
                libraryId,
                q: debouncedQuery,
                type,
                genre,
                decade,
                waiting: waiting ? "1" : "",
                sort,
                noTotal: "1",
                filter: filterParam,
                secondarySort: secondarySort || undefined,
                secondaryDesc: secondarySort ? (secondaryDesc ? "1" : "0") : undefined,
                desc: desc ? "1" : "0",
                cursor: "1",
                after: activeCursor.after || undefined,
                limit: PAGE_SIZE,
              },
              signal,
            )
            .then((result) => ({ ...result, requestKey: browseRequestKey })),
    [
      libraryId,
      debouncedQuery,
      type,
      genre,
      decade,
      waiting,
      sort,
      page,
      activeCursor.after,
      ruleFilter,
      specKey,
      advanced,
      viewer,
      params.get("secondarySort"),
      desc,
      secondaryDesc,
    ],
  );
  const countKey = JSON.stringify([
    libraryId,
    debouncedQuery,
    type,
    genre,
    decade,
    waiting,
    advanced,
    viewer,
    ruleFilter,
    specKey,
  ]);
  const count = useAsync(
    async (signal) => {
      if (collection) return null;
      const p = new URLSearchParams({
        q: debouncedQuery,
        type,
        genre,
        decade,
        waiting: waiting ? "1" : "",
        countOnly: "1",
      });
      if (libraryId) p.set("libraryId", String(libraryId));
      if (filterParam) p.set("filter", filterParam);
      if (ruleFilter) Object.entries(ruleFilter).forEach(([key, value]) => p.set(key, value));
      for (let attempt = 0; attempt < 24; attempt++) {
        const result = await apiFetch<{ total: number; pending: boolean; totalExact: boolean }>(
          `/catalog/browse?${p}`,
          { signal },
        );
        if (!result.pending) return { ...result, key: countKey };
        await new Promise<void>((resolve, reject) => {
          const stop = () => {
            clearTimeout(timer);
            reject(new DOMException("Aborted", "AbortError"));
          };
          const timer = setTimeout(() => {
            signal?.removeEventListener("abort", stop);
            resolve();
          }, 500);
          signal?.addEventListener("abort", stop, { once: true });
        });
      }
      return null;
    },
    [
      libraryId,
      debouncedQuery,
      type,
      genre,
      decade,
      waiting,
      advanced,
      viewer,
      ruleFilter,
      specKey,
    ],
  );
  // A collection is at most a few hundred titles: its filters run here.
  const collectionPage = useMemo(() => {
    if (!members.data) return undefined;
    const q = debouncedQuery.trim().toLowerCase();
    const from = Number(decade);
    const all = members.data.items.filter(
      (it) =>
        (!q || it.title.toLowerCase().includes(q)) &&
        (!type || it.type === type) &&
        (!genre || (it.genres ?? []).some((g) => g.toLowerCase() === genre.toLowerCase())) &&
        (!decade || ((it.year ?? 0) >= from && (it.year ?? 0) < from + 10)),
    );
    return { total: all.length, items: all.slice((page - 1) * PAGE_SIZE, page * PAGE_SIZE) };
  }, [members.data, debouncedQuery, type, genre, decade, page]);
  const regularResults = collection
    ? { ...members, data: collectionPage }
    : { ...browsed, data: browsed.data ?? undefined };
  const regularBusy =
    regularResults.loading || query !== debouncedQuery || advancedRaw !== advanced;
  const ai = useCatalogAISearch(cursorIdentity, query, type);
  const aiResult = !collection ? ai.result : null;
  const aiActive = !!aiResult;
  useEffect(() => {
    if (
      !collection &&
      ai.enabled &&
      !aiResult &&
      query.trim() &&
      !regularBusy &&
      !regularResults.error &&
      page === 1 &&
      browsed.data?.requestKey === browseRequestKey &&
      browsed.data.items.length === 0
    )
      void ai.start();
  }, [
    collection,
    ai.enabled,
    aiResult,
    query,
    regularBusy,
    regularResults.error,
    page,
    browsed.data,
    browseRequestKey,
    ai.start,
  ]);
  const results = aiResult
    ? {
        data: aiResult.preview,
        loading: aiResult.busy,
        error: aiResult.error
          ? new Error(
              t(
                "AI search is unavailable or could not interpret this request. Try again or use filters.",
              ),
            )
          : null,
        reload: ai.start,
      }
    : regularResults;
  const busy = aiResult ? aiResult.busy : regularBusy;
  const currentCount = count.data?.key === countKey ? count.data : null;
  const total = aiResult
    ? (aiResult.preview?.total ?? 0)
    : (currentCount?.total ??
      (results.data?.total ?? 0) + (collection ? 0 : (page - 1) * PAGE_SIZE));
  const totalExact =
    aiActive || collection || currentCount?.totalExact || browsed.data?.totalExact !== false;
  const items = results.data?.items ?? [];
  const viewers = useAsync(() => api.viewers(), []);
  const [chosen, setChosen] = useState<number[] | null>(null);
  const targets =
    chosen ??
    viewers.data
      ?.filter((v) => v.isOwner)
      .map((v) => v.id)
      .slice(0, 1) ??
    [];
  const [visibleKeys, setVisibleKeys] = useState<string[]>([]);
  const keys = Array.from(new Set([...visibleKeys, ...(selected ? [selected] : [])]));
  const membership = useAsync(
    async (signal) => {
      const pairs = await Promise.all(
        targets.map(
          async (id) =>
            [
              id,
              await apiFetch<Record<string, boolean>>(`/discovery/users/${id}/membership`, {
                method: "POST",
                body: JSON.stringify({ keys }),
                signal,
              }),
            ] as const,
        ),
      );
      return Object.fromEntries(pairs);
    },
    [targets.join(","), keys.join(",")],
  );
  const [writing, setWriting] = useState(false);
  const allSaved = (key: string) =>
    targets.length > 0 && targets.every((id) => membership.data?.[id]?.[key]);
  const toggleWatchlist = async (key: string) => {
    const add = !allSaved(key);
    setWriting(true);
    try {
      const responses = await Promise.allSettled(
        targets.map((id) =>
          apiFetch<{ traktFailed: boolean }>(`/discovery/users/${id}/catalog/${key}`, {
            method: add ? "PUT" : "DELETE",
          }),
        ),
      );
      if (responses.some((r) => r.status === "rejected"))
        toast.error(t("Some watchlists could not be updated. Try again."));
      if (responses.some((r) => r.status === "fulfilled" && r.value.traktFailed))
        toast.error(
          t("Changed in Ether, but Trakt could not be updated. Update Trakt separately."),
        );
      await membership.reload();
    } finally {
      setWriting(false);
    }
  };
  const watchlistPicker = () => (
    <details className="catalog-watchlist-picker">
      <summary>{t("Watchlist viewers")}</summary>
      <div className="catalog-watchlist-menu">
        {(viewers.data ?? [])
          .filter((v) => v.accessStatus !== "inactive")
          .map((v) => (
            <label key={v.id}>
              <input
                type="checkbox"
                checked={targets.includes(v.id)}
                disabled={!targets.includes(v.id) && targets.length >= 10}
                onChange={(e) =>
                  setChosen(
                    e.target.checked ? [...targets, v.id] : targets.filter((id) => id !== v.id),
                  )
                }
              />{" "}
              {v.title || v.username}
            </label>
          ))}
        <p>{t("Plex watchlist changes stay in Ether. Plex sync may restore removed titles.")}</p>
      </div>
    </details>
  );
  const watchButton = (key: string) => (
    <div className="catalog-watchlist-action">
      <Button
        size="sm"
        variant="outline"
        disabled={!targets.length || membership.loading || !!membership.error || writing}
        aria-pressed={allSaved(key)}
        onClick={() => void toggleWatchlist(key)}
      >
        <Bookmark size={14} fill={allSaved(key) ? "currentColor" : "none"} />
        {allSaved(key) ? t("Remove from Watchlist") : t("Add to Watchlist")}
      </Button>
      {targets.length > 1 && !membership.loading && !membership.error ? (
        <span>
          {t("Watchlist")} {targets.filter((id) => membership.data?.[id]?.[key]).length}/
          {targets.length}
        </span>
      ) : null}
    </div>
  );
  const queryParams = new URLSearchParams(params);
  ["page", "view", "title"].forEach((key) => queryParams.delete(key));
  queryParams.sort();
  const resultQueryKey = `${queryParams}|${specKey}|${JSON.stringify(ruleFilter)}`;
  const addBulkWatchlist = async (titleKeys: string[]) => {
    setWriting(true);
    let failed = false;
    let traktFailed = false;
    try {
      // Bound write concurrency to the selected viewer count (at most ten).
      for (const key of titleKeys) {
        const responses = await Promise.allSettled(
          targets.map((id) =>
            apiFetch<{ traktFailed: boolean }>(`/discovery/users/${id}/catalog/${key}`, {
              method: "PUT",
            }),
          ),
        );
        failed ||= responses.some((response) => response.status === "rejected");
        traktFailed ||= responses.some(
          (response) => response.status === "fulfilled" && response.value.traktFailed,
        );
      }
      await membership.reload();
      if (traktFailed)
        toast.error(
          t("Changed in Ether, but Trakt could not be updated. Update Trakt separately."),
        );
      if (failed) throw new Error("Watchlist update failed");
    } finally {
      setWriting(false);
    }
  };
  const hasFilters = Boolean(query || genre || decade || waiting || advancedRaw);
  const catalogStep = dashboard?.setupSteps?.find((s) => s.id === "catalog");
  const tmdbMissing = catalogStep
    ? !catalogStep.complete
    : !busy && !results.error && !items.length && !hasFilters && page <= 1;
  const reset = () => {
    const next = new URLSearchParams(latestParams.current);
    ["q", "genre", "decade", "waiting", "page", "filters", "viewer"].forEach((k) => next.delete(k));
    latestParams.current = next;
    setParams(next);
  };
  return (
    <div className="catalog-scroll app-scroll">
      <CatalogFilterBar
        leading={heading ? <h1 className="catalog-page-title">{heading}</h1> : undefined}
        actions={headerActions}
        params={params}
        viewers={viewers.data || []}
        collection={!!collection}
        aiEnabled={!collection && ai.enabled}
        aiBusy={aiResult?.busy}
        onAISearch={() => void ai.start()}
        onChange={(next) => {
          latestParams.current = next;
          setParams(next);
        }}
      />
      <div className="catalog-toolbar">
        {libraryScope ? (
          <span />
        ) : (
          <SubtabNav
            ariaLabel={t("Catalog media type")}
            active={type || "all"}
            onSelect={(v) => update("type", v === "all" ? "" : v)}
            tabs={[
              { key: "all", label: t("All titles") },
              { key: "movie", label: t("Movies") },
              { key: "show", label: t("TV shows") },
            ]}
          />
        )}
        <div className="catalog-tools">
          {watchlistPicker()}
          <Button size="sm" variant="ghost" onClick={() => setLookupOpen(true)}>
            <Search size={14} />
            {t("Find on TMDB")}
          </Button>
          <Button
            size="sm"
            variant="ghost"
            onClick={() => {
              void results.reload();
              void refreshHealth();
            }}
            disabled={busy}
            aria-label={t("Reload catalog")}
          >
            <RefreshCw size={14} className={busy ? "animate-spin" : ""} />
          </Button>
          <div className="catalog-view-toggle" role="group" aria-label={t("Catalog layout")}>
            <Button
              size="icon"
              variant={!list ? "secondary" : "ghost"}
              aria-label={t("Poster view")}
              aria-pressed={!list}
              onClick={() => update("view", "")}
            >
              <LayoutGrid size={15} />
            </Button>
            <Button
              size="icon"
              variant={list ? "secondary" : "ghost"}
              aria-label={t("List view")}
              aria-pressed={list}
              onClick={() => update("view", "list")}
            >
              <List size={15} />
            </Button>
          </div>
        </div>
      </div>
      <div className="catalog-content">
        <section className="catalog-results" aria-label={t("Catalog titles")} aria-busy={busy}>
          {viewers.error || membership.error ? (
            <p role="alert">{t("Watchlist status could not be loaded.")}</p>
          ) : null}
          <div className="catalog-results-head">
            <div>
              <h3>
                {collection
                  ? collection.name || t("Untitled collection")
                  : waiting
                    ? t("Waiting for a release")
                    : query
                      ? t("Results for “{query}”", { query })
                      : type === "movie"
                        ? t("Movies")
                        : type === "show"
                          ? t("TV shows")
                          : t("Movies & TV shows")}
              </h3>
              <p role="status" className={aiResult?.busy ? "catalog-search-working" : undefined}>
                {aiResult?.busy ? (
                  <LoaderCircle size={20} className="animate-spin" aria-hidden />
                ) : null}
                {busy
                  ? aiActive
                    ? t("Searching with AI…")
                    : t("Loading titles…")
                  : results.error
                    ? t("Catalog could not be loaded")
                    : !totalExact
                      ? t("{count}+ titles · keep exploring", { count: total })
                      : hasFilters
                        ? tn(total, "{count} matching title", "{count} matching titles")
                        : tn(total, "{count} title", "{count} titles")}
              </p>
            </div>
            <div className="catalog-tools">
              {aiResult?.busy ? (
                <Button variant="ghost" onClick={ai.stop}>
                  {t("Stop")}
                </Button>
              ) : null}
              {collection || aiActive ? null : (
                <ToolbarPicker
                  ariaLabel={t("Sort titles")}
                  value={sort}
                  options={SORTS}
                  onChange={(v) => update("sort", v)}
                />
              )}
              {!collection && !aiActive ? (
                <>
                  <ToolbarPicker
                    ariaLabel={t("Secondary sort")}
                    value={params.get("secondarySort") || ""}
                    onChange={(v) => update("secondarySort", v)}
                    options={[{ value: "", label: t("Then by…") }, ...SORTS]}
                  />
                  <Button
                    size="sm"
                    variant="ghost"
                    aria-label={t("Reverse sort direction")}
                    onClick={() => update("desc", desc ? "0" : "1")}
                  >
                    <ArrowRight size={14} className={desc ? "rotate-90" : "-rotate-90"} />
                  </Button>
                  {secondarySort ? (
                    <Button
                      size="sm"
                      variant="ghost"
                      aria-label={`${t("Secondary sort")} · ${t("Reverse sort direction")}`}
                      onClick={() => update("secondaryDesc", secondaryDesc ? "0" : "1")}
                    >
                      <ArrowRight
                        size={14}
                        className={secondaryDesc ? "rotate-90" : "-rotate-90"}
                      />
                    </Button>
                  ) : null}
                </>
              ) : null}
            </div>
          </div>
          {tmdbMissing && !collection ? (
            <p className="mb-3 text-sm text-fg-muted">
              {tx("No TMDB key is configured. {link} to populate the catalog.", {
                link: (
                  <Link to="/services/tmdb" className="font-medium text-primary underline">
                    {t("Add a TMDB key")}
                  </Link>
                ),
              })}
            </p>
          ) : null}
          {results.error ? (
            <ErrorPanel
              error={results.error}
              onRetry={() => {
                if (!collection && page > 1)
                  setCursorPage({ identity: cursorIdentity, after: "", page: 1 });
                else void results.reload();
              }}
            />
          ) : busy && (aiActive ? !items.length : page === 1) ? (
            <PosterSkeletons count={12} className="catalog-grid" />
          ) : !items.length && !busy ? (
            <EmptyState
              icon={<Search size={30} />}
              title={
                hasFilters
                  ? t("No matching titles")
                  : page > 1
                    ? t("No titles on this page")
                    : t("Your catalog is getting started")
              }
              description={
                hasFilters
                  ? t("Try another title or broaden your filters.")
                  : page > 1
                    ? t("The catalog may have changed since your last visit.")
                    : t(
                        "The library is empty until the catalogue syncs. That needs a TMDB key. Playback also needs a debrid account or a Usenet provider.",
                      )
              }
              action={
                hasFilters ? (
                  <Button variant="outline" onClick={reset}>
                    {t("Clear filters")}
                  </Button>
                ) : page > 1 ? (
                  <Button
                    variant="outline"
                    onClick={() =>
                      collection
                        ? update("page", "")
                        : setCursorPage({ identity: cursorIdentity, after: "", page: 1 })
                    }
                  >
                    {t("Back to first page")}
                  </Button>
                ) : undefined
              }
            />
          ) : (
            <CatalogueResults
              items={items}
              list={list}
              queryKey={
                aiActive ? `${resultQueryKey}|ai|${JSON.stringify(aiResult.spec)}` : resultQueryKey
              }
              page={aiActive ? 1 : page}
              hasMore={
                aiActive
                  ? false
                  : collection
                    ? page * PAGE_SIZE < total
                    : (browsed.data?.hasMore ?? page * PAGE_SIZE < total)
              }
              loading={aiActive ? false : busy}
              onLoadMore={() => {
                if (collection) update("page", String(page + 1));
                else if (browsed.data?.nextCursor)
                  setCursorPage({
                    identity: cursorIdentity,
                    after: browsed.data.nextCursor,
                    page: page + 1,
                  });
              }}
              onInspect={(key) => update("title", key)}
              renderAction={watchButton}
              onVisibleKeys={setVisibleKeys}
              onBulkWatchlist={targets.length ? addBulkWatchlist : undefined}
            />
          )}
        </section>
      </div>
      <Drawer
        open={lookupOpen}
        onClose={() => setLookupOpen(false)}
        title={t("Find a title on TMDB")}
        size="md"
      >
        <p className="mb-5 text-sm text-fg-muted">
          {t(
            "Look beyond your browsable library. Open a movie or show to see why it isn’t listed or add it to your library.",
          )}
        </p>
        <TitleSearch
          action="open"
          includeUnbrowsable
          placeholder={t("Search movies and shows…")}
          onPick={(title) => {
            setLookupOpen(false);
            update("title", title.key);
          }}
        />
      </Drawer>
      {selected ? (
        <TitleInspector
          key={selected}
          titleKey={selected}
          onClose={() => update("title", "")}
          onLibraryChange={() => {
            void browsed.reload();
            void count.reload();
          }}
          watchlistAction={
            <>
              <div className="w-full">{watchlistPicker()}</div>
              {watchButton(selected)}
            </>
          }
        />
      ) : null}
    </div>
  );
}
