import { Badge, Button, EmptyState, Input, SubtabNav } from "@braedonsaunders/appkit-ui";
import {
  ArrowLeft,
  ArrowRight,
  Film,
  LayoutGrid,
  List,
  RefreshCw,
  Search,
  Settings2,
  SlidersHorizontal,
  X,
} from "lucide-react";
import { useLayoutEffect, useRef, useState } from "react";
import { useNavigate, useParams, useSearchParams } from "react-router-dom";
import { useApp } from "../../app-context";
import { Drawer } from "../../components/Drawer";
import { TitleSearch } from "../../components/pickers";
import { Artwork } from "../../components/Artwork";
import { ErrorPanel, PosterSkeletons } from "../../components/feedback";
import { LevelBadge } from "../../components/status";
import { ToolbarPicker } from "../../components/ToolbarPicker";
import { Workspace } from "../../components/workspace";
import { localized, t, tn } from "../../i18n";
import { api } from "../../lib/api";
import { formatRating } from "../../lib/contentRatings";
import { fixed, formatNumber } from "../../lib/format";
import { useAsync, useDebouncedValue } from "../../lib/hooks";
import { CatalogSettings } from "./CatalogSettings";
import { TitleInspector } from "./TitleInspector";
import "./catalog.css";

const PAGE_SIZE = 48;
const SORTS = localized(() => [
  { value: "popularity", label: t("Most popular") },
  { value: "released", label: t("Recently released") },
  { value: "rating", label: t("Highest rated") },
  { value: "added", label: t("Recently added") },
  { value: "title", label: t("Title A–Z") },
]);

export default function Catalog() {
  const { tab } = useParams();
  const navigate = useNavigate();
  const [params, setParams] = useSearchParams();
  const settings = tab === "settings";
  const visibility = tab === "visibility";
  const onSettings = settings || visibility;
  const { health, refreshDashboard } = useApp();
  const status = health.find((h) => h.id === "catalog");
  const catalogPath = `/catalog${params.size ? `?${params}` : ""}`;
  return (
    <Workspace
      title={t("Catalog")}
      actions={
        <>
          {status ? (
            <span className="hidden xl:inline-flex">
              <LevelBadge
                level={status.level}
                label={status.level === "ok" ? t("Catalog healthy") : status.summary}
              />
            </span>
          ) : null}
          {onSettings ? (
            <Button size="sm" variant="outline" onClick={() => navigate(catalogPath)}>
              <ArrowLeft size={14} />
              {t("Back to catalog")}
            </Button>
          ) : (
            <Button size="sm" variant="outline" onClick={() => navigate("/catalog/settings")}>
              <Settings2 size={14} />
              {t("Settings")}
            </Button>
          )}
        </>
      }
    >
      {visibility || settings ? (
        <CatalogSettings />
      ) : (
        <CatalogBrowser params={params} setParams={setParams} refreshHealth={refreshDashboard} />
      )}
    </Workspace>
  );
}

function CatalogBrowser({
  params,
  setParams,
  refreshHealth,
}: {
  params: URLSearchParams;
  setParams: (p: URLSearchParams, options?: { replace?: boolean }) => void;
  refreshHealth: () => Promise<void>;
}) {
  const query = params.get("q") ?? "";
  const debouncedQuery = useDebouncedValue(query, 300);
  const type = ["movie", "show"].includes(params.get("type") ?? "") ? params.get("type")! : "";
  const genre = params.get("genre") ?? "";
  const decade = params.get("decade") ?? "";
  const sort = SORTS.some((s) => s.value === params.get("sort"))
    ? params.get("sort")!
    : "popularity";
  const rawPage = Number(params.get("page"));
  const page = Number.isSafeInteger(rawPage) && rawPage > 0 ? Math.min(rawPage, 1000000) : 1;
  const list = params.get("view") === "list";
  const selected = params.get("title");
  const [filtersOpen, setFiltersOpen] = useState(false);
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
  const results = useAsync(
    (signal) =>
      api.browseCatalog(
        {
          q: debouncedQuery,
          type,
          genre,
          decade,
          sort,
          offset: (page - 1) * PAGE_SIZE,
          limit: PAGE_SIZE,
        },
        signal,
      ),
    [debouncedQuery, type, genre, decade, sort, page],
  );
  const facets = useAsync(async () => {
    const [genres, decades] = await Promise.all([
      api.facets(type, "genre"),
      api.facets(type, "decade"),
    ]);
    return { genres, decades };
  }, [type]);
  const busy = results.loading || query !== debouncedQuery;
  const total = results.data?.total ?? 0;
  const items = results.data?.items ?? [];
  const hasFilters = Boolean(query || genre || decade);
  const reset = () => {
    const next = new URLSearchParams(latestParams.current);
    ["q", "genre", "decade", "page"].forEach((k) => next.delete(k));
    latestParams.current = next;
    setParams(next);
  };
  return (
    <div className="catalog-scroll app-scroll">
      <div className="catalog-toolbar">
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
        <div className="catalog-tools">
          <Button size="sm" variant="ghost" onClick={() => setLookupOpen(true)}>
            <Search size={14} />
            {t("Find on TMDB")}
          </Button>
          <Button
            size="sm"
            variant="ghost"
            onClick={() => {
              void results.reload();
              void facets.reload();
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
        <aside
          className={`catalog-filters ${filtersOpen ? "is-open" : ""}`}
          aria-label={t("Catalog filters")}
        >
          <div className="catalog-filter-heading">
            <SlidersHorizontal size={14} />
            <h3>{t("Refine catalog")}</h3>
            {hasFilters ? <button onClick={reset}>{t("Reset")}</button> : null}
          </div>
          <label className="catalog-field-label" htmlFor="catalog-search">
            {t("Find a title")}
          </label>
          <div className="relative">
            <Search size={15} className="absolute left-3 top-3 text-fg-subtle" />
            <Input
              id="catalog-search"
              placeholder={t("Search catalog…")}
              value={query}
              onChange={(e) => update("q", e.target.value, true)}
              className="pl-9"
            />
          </div>
          <div className="catalog-filter-field">
            <span className="catalog-field-label">{t("Genre")}</span>
            <ToolbarPicker
              className="w-full justify-between"
              ariaLabel={t("Filter by genre")}
              value={genre}
              onChange={(v) => update("genre", v)}
              options={[
                { value: "", label: t("All genres") },
                ...(genre && !facets.data?.genres.some((v) => v.key === genre)
                  ? [{ value: genre, label: genre }]
                  : []),
                ...(facets.data?.genres ?? []).map((v) => ({ value: v.key, label: v.title })),
              ]}
            />
          </div>
          <div className="catalog-filter-field">
            <span className="catalog-field-label">{t("Decade")}</span>
            <ToolbarPicker
              className="w-full justify-between"
              ariaLabel={t("Filter by decade")}
              value={decade}
              onChange={(v) => update("decade", v)}
              options={[
                { value: "", label: t("All decades") },
                ...(decade && !facets.data?.decades.some((v) => v.key === decade)
                  ? [{ value: decade, label: t("{decade}s", { decade }) }]
                  : []),
                ...(facets.data?.decades ?? []).map((v) => ({ value: v.key, label: v.title })),
              ]}
            />
          </div>
          <ErrorPanel error={facets.error} onRetry={() => void facets.reload()} />
          <div className="catalog-policy">
            <Film size={18} />
            <h4>{t("Ready for discovery")}</h4>
            <p>
              {t(
                "Only released titles appear here. Popularity and vote thresholds are in Catalog settings.",
              )}
            </p>
          </div>
        </aside>
        <section className="catalog-results" aria-label={t("Catalog titles")} aria-busy={busy}>
          <div className="catalog-results-head">
            <div>
              <h3>
                {query
                  ? t("Results for “{query}”", { query })
                  : type === "movie"
                    ? t("Movies")
                    : type === "show"
                      ? t("TV shows")
                      : t("Movies & TV shows")}
              </h3>
              <p role="status">
                {busy
                  ? t("Loading titles…")
                  : results.error
                    ? t("Catalog could not be loaded")
                    : hasFilters
                      ? tn(total, "{count} matching title", "{count} matching titles")
                      : tn(total, "{count} title", "{count} titles")}
              </p>
            </div>
            <div className="catalog-tools">
              <Button
                size="sm"
                variant="outline"
                className="catalog-mobile-filter"
                aria-expanded={filtersOpen}
                onClick={() => setFiltersOpen(!filtersOpen)}
              >
                <SlidersHorizontal size={14} />
                {t("Filters")}
              </Button>
              <ToolbarPicker
                ariaLabel={t("Sort titles")}
                value={sort}
                options={SORTS}
                onChange={(v) => update("sort", v)}
              />
            </div>
          </div>
          {hasFilters ? (
            <div className="catalog-active-filters">
              {[
                ["q", query],
                ["genre", genre],
                ["decade", decade ? t("{decade}s", { decade }) : ""],
              ]
                .filter(([, v]) => v)
                .map(([key, label]) => (
                  <button
                    key={key}
                    onClick={() => update(key!, "")}
                    aria-label={t("Remove {filter} filter", { filter: label ?? "" })}
                  >
                    {label}
                    <X size={12} />
                  </button>
                ))}
            </div>
          ) : null}
          {results.error ? (
            <ErrorPanel error={results.error} onRetry={() => void results.reload()} />
          ) : busy ? (
            <PosterSkeletons count={12} className="catalog-grid" />
          ) : !items.length ? (
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
                  <Button variant="outline" onClick={() => update("page", "")}>
                    {t("Back to first page")}
                  </Button>
                ) : undefined
              }
            />
          ) : (
            <>
              <div className={list ? "catalog-list" : "catalog-grid"}>
                {items.map((title) => (
                  <button
                    key={title.key}
                    className={list ? "catalog-list-item" : "catalog-poster"}
                    onClick={() => update("title", title.key)}
                    aria-label={t("Inspect {title}", { title: title.title })}
                  >
                    <div className="catalog-poster-image">
                      <Artwork
                        src={title.poster}
                        alt=""
                        title={title.title}
                        className="aspect-[2/3] w-full rounded-md"
                      />
                      {!list ? (
                        <span className="catalog-inspect">
                          {t("Inspect title")} <ArrowRight size={13} />
                        </span>
                      ) : null}
                    </div>
                    <div className="catalog-title-copy">
                      <h4>{title.title}</h4>
                      <p>
                        {title.year || t("Year unknown")}
                        <span>·</span>
                        {title.type === "show" ? t("TV show") : t("Movie")}
                      </p>
                      {list ? (
                        <p className="catalog-list-genres">
                          {title.genres?.join(" · ") || t("Genres unavailable")}
                        </p>
                      ) : null}
                    </div>
                    {title.rating ? (
                      <span className="catalog-rating">
                        <span aria-hidden>★</span> {fixed(title.rating, 1)}
                      </span>
                    ) : null}
                    {list ? (
                      <>
                        <Badge variant="secondary">
                          {title.contentRating ? formatRating(title.contentRating) : t("Unrated")}
                        </Badge>
                        <ArrowRight size={15} className="text-fg-subtle" />
                      </>
                    ) : null}
                  </button>
                ))}
              </div>
              <nav className="catalog-pagination" aria-label={t("Catalog pages")}>
                <span>
                  {tn(total, "{from}–{to} of {count} title", "{from}–{to} of {count} titles", {
                    from: formatNumber((page - 1) * PAGE_SIZE + 1),
                    to: formatNumber(Math.min(page * PAGE_SIZE, total)),
                  })}
                </span>
                <div className="catalog-tools">
                  <Button
                    variant="outline"
                    size="sm"
                    disabled={page <= 1}
                    onClick={() => update("page", String(page - 1))}
                  >
                    <ArrowLeft size={14} />
                    {t("Previous")}
                  </Button>
                  <span>
                    {t("Page {page} of {pages}", {
                      page: formatNumber(page),
                      pages: formatNumber(Math.ceil(total / PAGE_SIZE)),
                    })}
                  </span>
                  <Button
                    variant="outline"
                    size="sm"
                    disabled={page * PAGE_SIZE >= total}
                    onClick={() => update("page", String(page + 1))}
                  >
                    {t("Next")}
                    <ArrowRight size={14} />
                  </Button>
                </div>
              </nav>
            </>
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
            "Look beyond your browsable library. Search for a released movie or show to inspect its metadata.",
          )}
        </p>
        <TitleSearch
          placeholder={t("Search movies and shows…")}
          onPick={(title) => {
            setLookupOpen(false);
            update("title", title.key);
          }}
        />
      </Drawer>
      {selected ? (
        <TitleInspector key={selected} titleKey={selected} onClose={() => update("title", "")} />
      ) : null}
    </div>
  );
}
