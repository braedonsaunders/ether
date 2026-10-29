import {
  Avatar,
  Badge,
  Button,
  Input,
  MultiSelect,
  SearchSelect,
  cn,
} from "@braedonsaunders/appkit-ui";
import { Plus, Search, X } from "lucide-react";
import { useEffect, useId, useMemo, useRef, useState } from "react";
import { localized, t, tn } from "../i18n";
import { api, errorMessage, isAbort } from "../lib/api";
import type {
  FacetName,
  FacetValue,
  LookupKind,
  LookupResult,
  TitleCard,
  Viewer,
} from "../lib/contract";
import { useDebouncedValue } from "../lib/hooks";
import { Artwork } from "./Artwork";
import { titleSubtitle } from "./Poster";
import { useLocale } from "../i18n/react";

// ---- Facets ---------------------------------------------------------------------

const facetCache = new Map<string, Promise<FacetValue[]>>();

export function loadFacet(type: string, facet: FacetName): Promise<FacetValue[]> {
  const key = `${type}:${facet}`;
  let p = facetCache.get(key);
  if (!p) {
    p = api.facets(type, facet);
    facetCache.set(key, p);
    p.catch(() => facetCache.delete(key));
  }
  return p;
}

export function useFacet(type: string, facet: FacetName) {
  const [values, setValues] = useState<FacetValue[] | null>(null);
  const [error, setError] = useState<unknown>(null);
  useEffect(() => {
    let live = true;
    setValues(null);
    setError(null);
    loadFacet(type, facet)
      .then((v) => {
        if (live) setValues(v);
      })
      .catch((e) => {
        if (live) setError(e);
      });
    return () => {
      live = false;
    };
  }, [type, facet]);
  return { values, error };
}

/** Multi-value picker over a catalogue facet (genres, decades, ratings…). */
export function FacetPicker({
  type,
  facet,
  value,
  onChange,
  ariaLabel,
  placeholder,
}: {
  type: string;
  facet: FacetName;
  value: string[];
  onChange: (v: string[]) => void;
  ariaLabel: string;
  placeholder?: string;
}) {
  const { values, error } = useFacet(type, facet);
  const { locale } = useLocale();
  const options = useMemo(() => {
    const opts = (values ?? []).map((v) => ({
      value: v.key,
      label: v.title,
      description: v.count ? tn(v.count, "{count} title", "{count} titles") : undefined,
    }));
    // Keep values that aren't in the facet list (e.g. from a template) selectable.
    for (const v of value)
      if (!opts.some((o) => o.value === v))
        opts.push({ value: v, label: v, description: undefined });
    return opts;
  }, [values, value, locale]);
  if (error) {
    return (
      <TextListFallback
        value={value}
        onChange={onChange}
        ariaLabel={ariaLabel}
        hint={t("Couldn't load the list: {error}. Type values separated by commas.", {
          error: errorMessage(error),
        })}
      />
    );
  }
  return (
    <MultiSelect
      value={value}
      onChange={onChange}
      options={options}
      ariaLabel={ariaLabel}
      placeholder={values === null ? t("Loading…") : (placeholder ?? t("Choose…"))}
      searchPlaceholder={t("Search…")}
      noMatchesLabel={t("No matches")}
      clearLabel={t("Clear")}
      selectAllLabel={t("Select all")}
      clearable
      maxVisibleValues={4}
    />
  );
}

function TextListFallback({
  value,
  onChange,
  ariaLabel,
  hint,
}: {
  value: string[];
  onChange: (v: string[]) => void;
  ariaLabel: string;
  hint: string;
}) {
  const text = value.join(", ");
  return (
    <div className="space-y-1">
      <Input
        key={text}
        defaultValue={text}
        aria-label={ariaLabel}
        onBlur={(e) =>
          onChange(
            e.currentTarget.value
              .split(",")
              .map((s) => s.trim())
              .filter(Boolean),
          )
        }
      />
      <p className="text-xs text-fg-muted">{hint}</p>
    </div>
  );
}

/** Single facet value as a searchable select. */
export function FacetSelect({
  type,
  facet,
  value,
  onChange,
  ariaLabel,
}: {
  type: string;
  facet: FacetName;
  value: string;
  onChange: (v: string) => void;
  ariaLabel: string;
}) {
  const { values } = useFacet(type, facet);
  const options = (values ?? []).map((v) => ({ value: v.key, label: v.title }));
  if (value && !options.some((o) => o.value === value)) options.push({ value, label: value });
  return (
    <SearchSelect
      value={value}
      onChange={onChange}
      options={options}
      ariaLabel={ariaLabel}
      placeholder={values === null ? t("Loading…") : t("Choose…")}
      searchPlaceholder={t("Search…")}
      noMatchesLabel={t("No matches")}
      selectLabel={t("Select")}
      closeLabel={t("Close")}
      searchable
    />
  );
}

// ---- Lookup (people, keywords, companies…) ----------------------------------------

export interface Picked {
  id: number;
  name: string;
  image?: string;
}

const LOOKUP_PLACEHOLDER = localized<Record<LookupKind, string>>(() => ({
  person: t("Search people…"),
  keyword: t("Search keywords…"),
  company: t("Search studios…"),
  network: t("Search networks…"),
  collection: t("Search collections…"),
  watchProvider: t("Search services…"),
}));

/**
 * Async search-as-you-type picker backed by /catalog/lookup. Multi mode shows
 * the picks as removable chips; single mode replaces the pick.
 */
export function LookupPicker({
  kind,
  value,
  onChange,
  multiple = true,
  region,
  ariaLabel,
  placeholder,
}: {
  kind: LookupKind;
  value: Picked[];
  onChange: (v: Picked[]) => void;
  multiple?: boolean;
  region?: string;
  ariaLabel: string;
  placeholder?: string;
}) {
  const [q, setQ] = useState("");
  const [open, setOpen] = useState(false);
  const [results, setResults] = useState<LookupResult[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [active, setActive] = useState(0);
  const debounced = useDebouncedValue(q, 250);
  const listId = useId();
  const box = useRef<HTMLDivElement>(null);

  useEffect(() => {
    const needle = debounced.trim();
    if (!open || (needle.length < 2 && kind !== "watchProvider")) {
      setResults([]);
      return;
    }
    const ctl = new AbortController();
    setLoading(true);
    setError(null);
    api
      .lookup(kind, needle, region, ctl.signal)
      .then((r) => {
        setResults(r);
        setActive(0);
      })
      .catch((e) => {
        if (!isAbort(e)) setError(errorMessage(e));
      })
      .finally(() => {
        if (!ctl.signal.aborted) setLoading(false);
      });
    return () => ctl.abort();
  }, [debounced, kind, region, open]);

  useEffect(() => {
    if (!open) return;
    const onDoc = (e: MouseEvent) => {
      if (box.current && !box.current.contains(e.target as Node)) setOpen(false);
    };
    document.addEventListener("mousedown", onDoc);
    return () => document.removeEventListener("mousedown", onDoc);
  }, [open]);

  const choose = (r: LookupResult) => {
    const pick = { id: r.id, name: r.name, image: r.image };
    if (multiple) {
      if (!value.some((v) => v.id === r.id)) onChange([...value, pick]);
    } else onChange([pick]);
    setQ("");
    setResults([]);
    if (!multiple) setOpen(false);
  };

  return (
    <div ref={box} className="relative space-y-2">
      {value.length ? (
        <div className="flex flex-wrap gap-1.5">
          {value.map((v) => (
            <Badge
              key={v.id}
              variant="secondary"
              className="gap-1 py-1 pl-1 pr-1.5 text-xs font-medium text-fg"
            >
              {kind === "person" ? <Avatar src={v.image} name={v.name} size={18} /> : null}
              {v.name}
              <button
                type="button"
                aria-label={t("Remove {name}", { name: v.name })}
                onClick={() => onChange(value.filter((x) => x.id !== v.id))}
                className="ml-0.5 rounded-full p-0.5 text-fg-subtle hover:bg-surface-hover hover:text-fg"
              >
                <X size={12} />
              </button>
            </Badge>
          ))}
        </div>
      ) : null}
      {multiple || value.length === 0 || open ? (
        <div className="relative">
          <Search
            size={14}
            className="pointer-events-none absolute left-3 top-1/2 -translate-y-1/2 text-fg-subtle"
            aria-hidden
          />
          <Input
            role="combobox"
            aria-label={ariaLabel}
            aria-expanded={open && results.length > 0}
            aria-controls={listId}
            aria-autocomplete="list"
            className="pl-8"
            value={q}
            placeholder={placeholder ?? LOOKUP_PLACEHOLDER[kind]}
            onFocus={() => setOpen(true)}
            onChange={(e) => {
              setQ(e.currentTarget.value);
              setOpen(true);
            }}
            onKeyDown={(e) => {
              if (e.key === "ArrowDown") {
                e.preventDefault();
                setActive((a) => Math.min(results.length - 1, a + 1));
              } else if (e.key === "ArrowUp") {
                e.preventDefault();
                setActive((a) => Math.max(0, a - 1));
              } else if (e.key === "Enter" && results[active]) {
                e.preventDefault();
                choose(results[active]!);
              } else if (e.key === "Escape") setOpen(false);
            }}
          />
        </div>
      ) : (
        <Button type="button" variant="ghost" size="sm" onClick={() => setOpen(true)}>
          {t("Change")}
        </Button>
      )}
      {open && (results.length > 0 || loading || error || debounced.trim().length >= 2) ? (
        <ul
          id={listId}
          role="listbox"
          className="absolute z-30 mt-1 max-h-72 w-full overflow-auto rounded-lg border border-border bg-elevated p-1 shadow-lg"
        >
          {loading && results.length === 0 ? (
            <li className="px-3 py-2 text-sm text-fg-muted">{t("Searching…")}</li>
          ) : null}
          {error ? <li className="px-3 py-2 text-sm text-danger">{error}</li> : null}
          {!loading && !error && results.length === 0 ? (
            <li className="px-3 py-2 text-sm text-fg-muted">
              {t("No matches for “{query}”", { query: debounced })}
            </li>
          ) : null}
          {results.map((r, i) => {
            const picked = value.some((v) => v.id === r.id);
            return (
              <li key={r.id} role="option" aria-selected={i === active}>
                <button
                  type="button"
                  onMouseEnter={() => setActive(i)}
                  onClick={() => choose(r)}
                  className={cn(
                    "flex w-full items-center gap-2.5 rounded-md px-2 py-1.5 text-left text-sm",
                    i === active ? "bg-surface-hover" : "",
                  )}
                >
                  {r.image !== undefined || kind === "person" ? (
                    <Artwork src={r.image} alt="" title="" className="size-8 shrink-0 rounded" />
                  ) : null}
                  <span className="min-w-0 flex-1">
                    <span className="block truncate text-fg">{r.name}</span>
                    {r.subtitle ? (
                      <span className="block truncate text-xs text-fg-muted">{r.subtitle}</span>
                    ) : null}
                  </span>
                  {picked ? (
                    <Badge variant="success">{t("Added")}</Badge>
                  ) : (
                    <Plus size={14} className="text-fg-subtle" />
                  )}
                </button>
              </li>
            );
          })}
        </ul>
      ) : null}
    </div>
  );
}

// ---- Title search -------------------------------------------------------------------

/** Search the catalogue and add titles (hand-picked collections, overrides…). */
export function TitleSearch({
  onPick,
  type,
  excludeKeys,
  placeholder = t("Search the catalogue…"),
}: {
  onPick: (title: TitleCard) => void;
  type?: string;
  excludeKeys?: Set<string>;
  placeholder?: string;
}) {
  const [q, setQ] = useState("");
  const debounced = useDebouncedValue(q, 250);
  const [items, setItems] = useState<TitleCard[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  useEffect(() => {
    const needle = debounced.trim();
    if (needle.length < 2) {
      setItems([]);
      return;
    }
    const ctl = new AbortController();
    setLoading(true);
    setError(null);
    api
      .searchCatalog(needle, type, 12, ctl.signal)
      .then(setItems)
      .catch((e) => {
        if (!isAbort(e)) setError(errorMessage(e));
      })
      .finally(() => {
        if (!ctl.signal.aborted) setLoading(false);
      });
    return () => ctl.abort();
  }, [debounced, type]);
  return (
    <div className="space-y-2">
      <div className="relative">
        <Search
          size={14}
          className="pointer-events-none absolute left-3 top-1/2 -translate-y-1/2 text-fg-subtle"
          aria-hidden
        />
        <Input
          className="pl-8"
          value={q}
          onChange={(e) => setQ(e.currentTarget.value)}
          placeholder={placeholder}
          aria-label={t("Search titles")}
        />
      </div>
      {loading ? <p className="text-xs text-fg-muted">{t("Searching…")}</p> : null}
      {error ? <p className="text-xs text-danger">{error}</p> : null}
      {!loading && !error && debounced.trim().length >= 2 && items.length === 0 ? (
        <p className="text-xs text-fg-muted">
          {t("No titles match “{query}”.", { query: debounced })}
        </p>
      ) : null}
      {items.length ? (
        <ul className="max-h-72 space-y-1 overflow-auto" aria-label={t("Search results")}>
          {items.map((item) => {
            const added = excludeKeys?.has(item.key);
            return (
              <li key={item.key}>
                <button
                  type="button"
                  disabled={added}
                  onClick={() => onPick(item)}
                  className="flex w-full items-center gap-2.5 rounded-md p-1.5 text-left text-sm hover:bg-surface-hover disabled:opacity-60"
                >
                  <Artwork
                    src={item.poster}
                    alt=""
                    title=""
                    className="h-12 w-8 shrink-0 rounded"
                  />
                  <span className="min-w-0 flex-1">
                    <span className="block truncate text-fg">{item.title}</span>
                    <span className="block truncate text-xs text-fg-muted">
                      {titleSubtitle(item)}
                    </span>
                  </span>
                  {added ? (
                    <Badge variant="secondary">{t("Added")}</Badge>
                  ) : (
                    <Plus size={14} className="text-fg-subtle" />
                  )}
                </button>
              </li>
            );
          })}
        </ul>
      ) : null}
    </div>
  );
}

// ---- Viewers ---------------------------------------------------------------------

let viewerCache: Promise<Viewer[]> | null = null;

/** Viewers list, cached for the session. */
export function loadViewers(force = false): Promise<Viewer[]> {
  if (!viewerCache || force) {
    viewerCache = api.viewers();
    viewerCache.catch(() => {
      viewerCache = null;
    });
  }
  return viewerCache;
}

export function useViewers() {
  const [viewers, setViewers] = useState<Viewer[] | null>(null);
  const [error, setError] = useState<unknown>(null);
  useEffect(() => {
    let live = true;
    loadViewers()
      .then((v) => {
        if (live) setViewers(v);
      })
      .catch((e) => {
        if (live) setError(e);
      });
    return () => {
      live = false;
    };
  }, []);
  return { viewers, error };
}

export function viewerName(v: Pick<Viewer, "title" | "username">): string {
  return v.title || v.username;
}
