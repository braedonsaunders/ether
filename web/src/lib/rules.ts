import type {
  CollectionSort,
  CollectionSpec,
  FacetName,
  LookupKind,
  LookupSourceKind,
  Rule,
  RuleField,
  RuleOp,
  Source,
  SourceKind,
  StevenLuList,
  TmdbListName,
} from "./contract";
import { localized, t, tn } from "../i18n";
import { NOT_RATED, ratingLevelLabel } from "./contentRatings";

/**
 * Rule value encoding (the contract leaves `value` typed by field):
 *   in / notIn   → array of keys (facet keys as strings, lookup ids as numbers)
 *   is / isNot   → a single key, number or boolean
 *   gte / lte    → a number, or an ISO date (YYYY-MM-DD) for date fields
 *   between      → [min, max] with the same element type as gte/lte
 *   within       → a relative window "<n><unit>", unit d|w|m|y ("30d", "6m")
 * Lookup-backed rules also carry `label`: the display names joined by ", ".
 */

export type ValueKind = "facet" | "lookup" | "number" | "date" | "bool";

export interface FieldMeta {
  field: RuleField;
  label: string;
  /** Display heading for the field's group in pickers. */
  group: string;
  kind: ValueKind;
  ops: RuleOp[];
  defaultOp: RuleOp;
  facet?: FacetName;
  lookup?: LookupKind;
  unit?: string;
  min?: number;
  max?: number;
  step?: number;
  hint: string;
  /** Description of a `false` boolean rule, e.g. "Not watched by the viewer". */
  negated?: string;
}

const SET_OPS: RuleOp[] = ["in", "notIn"];
const NUM_OPS: RuleOp[] = ["gte", "lte", "between", "is"];
const DATE_OPS: RuleOp[] = ["within", "gte", "lte", "between"];

export const FIELDS = localized<FieldMeta[]>(() => [
  {
    field: "genre",
    label: t("Genre"),
    group: t("Content"),
    kind: "facet",
    facet: "genre",
    ops: SET_OPS,
    defaultOp: "in",
    hint: t("TMDB genres"),
  },
  {
    field: "keyword",
    label: t("Keyword"),
    group: t("Content"),
    kind: "lookup",
    lookup: "keyword",
    ops: SET_OPS,
    defaultOp: "in",
    hint: t("Themes like “time travel” or “heist”"),
  },
  {
    field: "decade",
    label: t("Decade"),
    group: t("Content"),
    kind: "facet",
    facet: "decade",
    ops: SET_OPS,
    defaultOp: "in",
    hint: t("Release decade"),
  },
  {
    field: "contentRating",
    label: t("Content rating"),
    group: t("Content"),
    kind: "facet",
    facet: "contentRating",
    ops: ["in", "notIn", "lte", "gte"],
    defaultOp: "in",
    hint: t("Age level: PG also covers TV-PG and other countries' equivalents"),
  },
  {
    field: "country",
    label: t("Country"),
    group: t("Content"),
    kind: "facet",
    facet: "country",
    ops: SET_OPS,
    defaultOp: "in",
    hint: t("Production country"),
  },
  {
    field: "language",
    label: t("Original language"),
    group: t("Content"),
    kind: "facet",
    facet: "language",
    ops: SET_OPS,
    defaultOp: "in",
    hint: t("Original spoken language"),
  },
  {
    field: "collection",
    label: t("TMDB collection"),
    group: t("Content"),
    kind: "lookup",
    lookup: "collection",
    ops: SET_OPS,
    defaultOp: "in",
    hint: t("Franchise collections such as “Alien”"),
  },
  {
    field: "watchProvider",
    label: t("Streaming on"),
    group: t("Content"),
    kind: "lookup",
    lookup: "watchProvider",
    ops: SET_OPS,
    defaultOp: "in",
    hint: t("Where the title streams in your region"),
  },
  {
    field: "person",
    label: t("Person"),
    group: t("People & companies"),
    kind: "lookup",
    lookup: "person",
    ops: SET_OPS,
    defaultOp: "in",
    hint: t("Cast or crew"),
  },
  {
    field: "studio",
    label: t("Studio"),
    group: t("People & companies"),
    kind: "lookup",
    lookup: "company",
    ops: SET_OPS,
    defaultOp: "in",
    hint: t("Production company"),
  },
  {
    field: "network",
    label: t("Network"),
    group: t("People & companies"),
    kind: "lookup",
    lookup: "network",
    ops: SET_OPS,
    defaultOp: "in",
    hint: t("Original TV network"),
  },
  {
    field: "year",
    label: t("Year"),
    group: t("Numbers"),
    kind: "number",
    ops: NUM_OPS,
    defaultOp: "gte",
    min: 1890,
    max: 2100,
    step: 1,
    hint: t("Release year"),
  },
  {
    field: "rating",
    label: t("Rating"),
    group: t("Numbers"),
    kind: "number",
    ops: NUM_OPS,
    defaultOp: "gte",
    min: 0,
    max: 10,
    step: 0.1,
    unit: "/ 10",
    hint: t("TMDB vote average"),
  },
  {
    field: "votes",
    label: t("Votes"),
    group: t("Numbers"),
    kind: "number",
    ops: NUM_OPS,
    defaultOp: "gte",
    min: 0,
    step: 50,
    hint: t("Number of TMDB votes"),
  },
  {
    field: "popularity",
    label: t("Popularity"),
    group: t("Numbers"),
    kind: "number",
    ops: NUM_OPS,
    defaultOp: "gte",
    min: 0,
    step: 1,
    hint: t("TMDB popularity score"),
  },
  {
    field: "runtime",
    label: t("Runtime"),
    group: t("Numbers"),
    kind: "number",
    ops: NUM_OPS,
    defaultOp: "lte",
    min: 0,
    max: 600,
    step: 5,
    unit: t("min"),
    hint: t("Minutes (episode length for shows)"),
  },
  {
    field: "released",
    label: t("Released"),
    group: t("Dates"),
    kind: "date",
    ops: DATE_OPS,
    defaultOp: "within",
    hint: t("First release or air date"),
  },
  {
    field: "addedAt",
    label: t("Added to Ether"),
    group: t("Dates"),
    kind: "date",
    ops: DATE_OPS,
    defaultOp: "within",
    hint: t("When the title entered your catalogue"),
  },
  {
    field: "watched",
    label: t("Watched by the viewer"),
    group: t("Viewer"),
    kind: "bool",
    ops: ["is"],
    defaultOp: "is",
    hint: t("Evaluated per viewer"),
    negated: t("Not watched by the viewer"),
  },
  {
    field: "inProgress",
    label: t("In progress for the viewer"),
    group: t("Viewer"),
    kind: "bool",
    ops: ["is"],
    defaultOp: "is",
    hint: t("Started but not finished"),
    negated: t("Not in progress for the viewer"),
  },
]);

export const FIELD_BY_NAME: Record<RuleField, FieldMeta> = localized(
  () => Object.fromEntries(FIELDS.map((f) => [f.field, f])) as Record<RuleField, FieldMeta>,
);

export function fieldMeta(field: string): FieldMeta | undefined {
  return (FIELD_BY_NAME as Record<string, FieldMeta | undefined>)[field];
}

export const OP_LABELS = localized<Record<RuleOp, string>>(() => ({
  is: t("is"),
  isNot: t("is not"),
  in: t("is any of"),
  notIn: t("is none of"),
  gte: t("is at least"),
  lte: t("is at most"),
  between: t("is between"),
  within: t("in the last"),
}));

export function opLabel(op: RuleOp, kind?: ValueKind): string {
  if (kind === "date") {
    if (op === "gte") return t("on or after");
    if (op === "lte") return t("on or before");
  }
  return OP_LABELS[op];
}

export const WITHIN_UNITS = localized(
  () =>
    [
      { value: "d", label: t("days") },
      { value: "w", label: t("weeks") },
      { value: "m", label: t("months") },
      { value: "y", label: t("years") },
    ] as const,
);

export function parseWithin(v: unknown): { n: number; unit: string } {
  const m = typeof v === "string" ? /^(\d+)\s*([dwmy])$/.exec(v.trim()) : null;
  return m ? { n: Number(m[1]), unit: m[2] ?? "d" } : { n: 30, unit: "d" };
}

export function formatWithin(v: unknown): string {
  const { n, unit } = parseWithin(v);
  if (unit === "w") return tn(n, "{count} week", "{count} weeks");
  if (unit === "m") return tn(n, "{count} month", "{count} months");
  if (unit === "y") return tn(n, "{count} year", "{count} years");
  return tn(n, "{count} day", "{count} days");
}

function today(): string {
  return new Date().toISOString().slice(0, 10);
}

/** A fresh value for the field/op pair, reusing what's compatible from `prev`. */
export function valueFor(meta: FieldMeta, op: RuleOp, prev?: unknown): unknown {
  const list = Array.isArray(prev)
    ? prev
    : prev === undefined || prev === null || prev === ""
      ? []
      : [prev];
  switch (meta.kind) {
    case "bool":
      return typeof prev === "boolean" ? prev : true;
    case "facet":
    case "lookup":
      if (op === "in" || op === "notIn") return list;
      // A rating bound needs a rated level, never "Not rated".
      if (meta.field === "contentRating" && (op === "gte" || op === "lte"))
        return list.find((x) => x !== NOT_RATED) ?? "";
      return list[0] ?? "";
    case "number": {
      const first = typeof list[0] === "number" ? list[0] : undefined;
      const fallback =
        meta.field === "year"
          ? new Date().getFullYear() - 10
          : meta.field === "rating"
            ? 7
            : meta.field === "runtime"
              ? 100
              : 0;
      if (op === "between") {
        const lo = first ?? fallback;
        const hi =
          typeof list[1] === "number"
            ? list[1]
            : meta.field === "year"
              ? new Date().getFullYear()
              : (meta.max ?? lo);
        return [lo, hi];
      }
      return first ?? fallback;
    }
    case "date": {
      if (op === "within")
        return typeof prev === "string" && /^\d+[dwmy]$/.test(prev) ? prev : "30d";
      const first =
        typeof list[0] === "string" && /^\d{4}-\d{2}-\d{2}/.test(list[0]) ? list[0] : today();
      if (op === "between")
        return [first, typeof list[1] === "string" && /^\d{4}/.test(list[1]) ? list[1] : today()];
      return first;
    }
  }
}

export function newRule(field: RuleField): Rule {
  const meta = FIELD_BY_NAME[field];
  return { field, op: meta.defaultOp, value: valueFor(meta, meta.defaultOp) };
}

export function withOp(rule: Rule, op: RuleOp): Rule {
  const meta = fieldMeta(rule.field);
  if (!meta) return { ...rule, op };
  return { ...rule, op, value: valueFor(meta, op, rule.value) };
}

export function withField(rule: Rule, field: RuleField): Rule {
  if (field === rule.field) return rule;
  return newRule(field);
}

/** Incomplete rules (no values picked yet) are left out of previews. */
export function isRuleComplete(rule: Rule): boolean {
  const meta = fieldMeta(rule.field);
  if (!meta) return true;
  const v = rule.value;
  if (meta.kind === "bool") return typeof v === "boolean";
  if (Array.isArray(v)) {
    if (rule.op === "between")
      return v.length === 2 && v.every((x) => x !== "" && x !== null && x !== undefined);
    return v.length > 0;
  }
  return v !== "" && v !== null && v !== undefined && !(typeof v === "number" && Number.isNaN(v));
}

function listText(rule: Rule, values: unknown[]): string {
  const labels = rule.label ? rule.label.split(", ").filter(Boolean) : [];
  const shown = labels.length === values.length ? labels : values.map(String);
  if (shown.length <= 3) return shown.join(", ");
  return tn(shown.length - 2, "{names} and {count} more", "{names} and {count} more", {
    names: shown.slice(0, 2).join(", "),
  });
}

/** One-line human description, e.g. "Genre is any of Horror, Thriller". */
export function describeRule(rule: Rule): string {
  const meta = fieldMeta(rule.field);
  const label = meta?.label ?? rule.field;
  const v = rule.value;
  if (meta?.kind === "bool")
    return v === false
      ? (meta.negated ??
          t("Not {field}", { field: label.charAt(0).toLowerCase() + label.slice(1) }))
      : label;
  // Ratings read "7 / 10" in the editor but bare in the summary.
  const unit = meta?.unit && meta.field !== "rating" ? ` ${meta.unit}` : "";
  if (rule.op === "within")
    return t("{field} in the last {window}", { field: label, window: formatWithin(v) });
  if (rule.op === "between" && Array.isArray(v))
    return t("{field} between {min} and {max}", {
      field: label,
      min: String(v[0]),
      max: `${String(v[1])}${unit}`,
    });
  if (meta?.field === "contentRating") {
    const values = (Array.isArray(v) ? v : [v]).filter((x) => x !== "" && x != null);
    const shown = values.map((x) => ratingLevelLabel(String(x)));
    return opPhrase(rule.op, meta.kind, label, shown.join(", ") || "…");
  }
  if (Array.isArray(v)) return opPhrase(rule.op, meta?.kind, label, listText(rule, v) || "…");
  const text = rule.label || (v === "" || v === undefined || v === null ? "…" : String(v));
  return opPhrase(rule.op, meta?.kind, label, `${text}${typeof v === "number" ? unit : ""}`);
}

/** "{field} {operator} {value}" as one translatable phrase per operator. */
function opPhrase(op: RuleOp, kind: ValueKind | undefined, field: string, value: string): string {
  const values = { field, value };
  switch (op) {
    case "is":
      return t("{field} is {value}", values);
    case "isNot":
      return t("{field} is not {value}", values);
    case "in":
      return t("{field} is any of {value}", values);
    case "notIn":
      return t("{field} is none of {value}", values);
    case "gte":
      return kind === "date"
        ? t("{field} on or after {value}", values)
        : t("{field} is at least {value}", values);
    case "lte":
      return kind === "date"
        ? t("{field} on or before {value}", values)
        : t("{field} is at most {value}", values);
    case "between":
      return t("{field} is between {value}", values);
    case "within":
      return t("{field} in the last {value}", values);
  }
}

// ---- Sources -------------------------------------------------------------------

export interface SourceMeta {
  kind: SourceKind;
  label: string;
  description: string;
  lookup?: LookupKind;
}

export const SOURCE_KINDS = localized<SourceMeta[]>(() => [
  { kind: "catalog", label: t("Whole catalogue"), description: t("Everything browsable in Ether") },
  {
    kind: "tmdbList",
    label: t("TMDB chart"),
    description: t("Trending, popular, top rated, now playing…"),
  },
  {
    kind: "titles",
    label: t("Hand-picked titles"),
    description: t("Choose titles one by one, in your order"),
  },
  {
    kind: "tmdbPerson",
    label: t("Person's filmography"),
    description: t("Everything an actor or director made"),
    lookup: "person",
  },
  {
    kind: "tmdbCollection",
    label: t("TMDB collection"),
    description: t("A franchise such as “The Lord of the Rings”"),
    lookup: "collection",
  },
  {
    kind: "tmdbKeyword",
    label: t("Keyword"),
    description: t("Titles tagged with a TMDB keyword"),
    lookup: "keyword",
  },
  {
    kind: "tmdbCompany",
    label: t("Studio"),
    description: t("Titles from a production company"),
    lookup: "company",
  },
  {
    kind: "tmdbNetwork",
    label: t("TV network"),
    description: t("Shows from a network such as HBO"),
    lookup: "network",
  },
  {
    kind: "watchProvider",
    label: t("Streaming service"),
    description: t("What's on a service in your region"),
    lookup: "watchProvider",
  },
  {
    kind: "tmdbDiscover",
    label: t("TMDB discover query"),
    description: t("Raw TMDB discover parameters"),
  },
  { kind: "traktList", label: t("Trakt list"), description: t("A public Trakt list URL") },
  {
    kind: "letterboxdList",
    label: t("Letterboxd list"),
    description: t("A public Letterboxd list URL"),
  },
  { kind: "mdblist", label: t("MDBList"), description: t("A public MDBList URL") },
  {
    kind: "anilistList",
    label: t("AniList list"),
    description: t("A public AniList anime list URL"),
  },
  {
    kind: "stevenlu",
    label: t("StevenLu popular movies"),
    description: t("Movies people are talking about"),
  },
]);

export function sourceMeta(kind: string): SourceMeta | undefined {
  return SOURCE_KINDS.find((s) => s.kind === kind);
}

export const TMDB_LISTS = localized<
  { value: TmdbListName; label: string; media: ("movie" | "show")[] }[]
>(() => [
  { value: "trending_day", label: t("Trending today"), media: ["movie", "show"] },
  { value: "trending_week", label: t("Trending this week"), media: ["movie", "show"] },
  { value: "popular", label: t("Popular"), media: ["movie", "show"] },
  { value: "top_rated", label: t("Top rated"), media: ["movie", "show"] },
  { value: "now_playing", label: t("Now in cinemas"), media: ["movie"] },
  { value: "upcoming", label: t("Upcoming"), media: ["movie"] },
  { value: "airing_today", label: t("Airing today"), media: ["show"] },
  { value: "on_the_air", label: t("On the air"), media: ["show"] },
]);

export const STEVENLU_LISTS = localized<{ value: StevenLuList; label: string }[]>(() => [
  { value: "movies", label: t("All popular movies") },
  { value: "movies-imdb-min7", label: "IMDb 7+" },
  { value: "movies-imdb-min8", label: "IMDb 8+" },
  { value: "movies-metacritic-min50", label: "Metacritic 50+" },
  { value: "movies-metacritic-min60", label: "Metacritic 60+" },
  { value: "movies-metacritic-min70", label: "Metacritic 70+" },
  { value: "movies-metacritic-min80", label: "Metacritic 80+" },
  { value: "movies-rottentomatoes-min70", label: "Rotten Tomatoes 70+" },
  { value: "movies-rottentomatoes-min80", label: "Rotten Tomatoes 80+" },
]);

export function newSource(
  kind: SourceKind,
  mediaType: CollectionSpec["mediaType"],
  region = "US",
): Source {
  switch (kind) {
    case "catalog":
      return { kind };
    case "tmdbList":
      return { kind, list: "trending_week", mediaType: mediaType === "show" ? "show" : "movie" };
    case "tmdbCollection":
      return { kind, id: 0 };
    case "tmdbDiscover":
      return { kind, params: {} };
    case "tmdbKeyword":
    case "tmdbCompany":
    case "tmdbNetwork":
    case "tmdbPerson":
      return { kind: kind as LookupSourceKind, id: 0 };
    case "watchProvider":
      return { kind, id: 0, region };
    case "traktList":
    case "letterboxdList":
    case "mdblist":
    case "anilistList":
      return { kind, url: "" };
    case "stevenlu":
      return { kind, list: "movies" };
    case "titles":
      return { kind, keys: [] };
  }
}

export function isSourceComplete(s: Source): boolean {
  switch (s.kind) {
    case "tmdbCollection":
    case "tmdbKeyword":
    case "tmdbCompany":
    case "tmdbNetwork":
    case "tmdbPerson":
    case "watchProvider":
      return s.id > 0;
    case "traktList":
    case "letterboxdList":
    case "mdblist":
    case "anilistList":
      return /^https?:\/\/\S+$/.test(s.url.trim());
    case "titles":
      return s.keys.length > 0;
    case "tmdbDiscover":
      return Object.keys(s.params).length > 0;
    default:
      return true;
  }
}

export function describeSource(s: Source): string {
  switch (s.kind) {
    case "catalog":
      return t("Whole catalogue");
    case "tmdbList": {
      const list = TMDB_LISTS.find((l) => l.value === s.list)?.label ?? s.list;
      return s.mediaType === "show" ? t("{list} (TV)", { list }) : t("{list} (movies)", { list });
    }
    case "titles":
      return tn(s.keys.length, "{count} hand-picked title", "{count} hand-picked titles");
    case "tmdbDiscover":
      return t("Discover: {params}", {
        params:
          Object.entries(s.params)
            .map(([k, v]) => `${k}=${v}`)
            .join("&") || t("no parameters"),
      });
    case "traktList":
    case "letterboxdList":
    case "mdblist":
    case "anilistList":
      return t("{source}: {detail}", {
        source: String(sourceMeta(s.kind)?.label),
        detail: s.url || t("no URL"),
      });
    case "stevenlu":
      return t("StevenLu: {list}", {
        list: STEVENLU_LISTS.find((l) => l.value === s.list)?.label ?? s.list,
      });
    case "watchProvider": {
      const service = s.label || t("Streaming service");
      return s.region
        ? t("{service} ({region})", { service, region: s.region })
        : t("{service} (default region)", { service });
    }
    default:
      return t("{source}: {detail}", {
        source: String(sourceMeta(s.kind)?.label),
        detail: s.label || (s.id ? `#${s.id}` : t("not chosen")),
      });
  }
}

// ---- Sort, schedule ---------------------------------------------------------------

export const SORTS = localized<{ value: CollectionSort; label: string; hint: string }[]>(() => [
  { value: "popularity", label: t("Popularity"), hint: t("Most popular on TMDB right now") },
  { value: "rating", label: t("Rating"), hint: t("TMDB vote average") },
  { value: "released", label: t("Release date"), hint: t("Newest or oldest first") },
  {
    value: "latestEpisode",
    label: t("Latest aired episode"),
    hint: t("Latest regular episode from available metadata; unknown dates last"),
  },
  {
    value: "latestSeasonPremiere",
    label: t("Latest season premiere"),
    hint: t("Premiere of the latest aired regular season; unknown dates last"),
  },
  { value: "added", label: t("Date added"), hint: t("When titles entered Ether") },
  { value: "title", label: t("Title"), hint: t("Alphabetical") },
  {
    value: "personal",
    label: t("Personal fit"),
    hint: t("Ordered for each viewer by their taste"),
  },
  { value: "random", label: t("Shuffle"), hint: t("A fresh order on every refresh") },
  {
    value: "source",
    label: t("Source order"),
    hint: t("Keep the order of the list or your picks"),
  },
]);

export const REFRESH_OPTIONS = localized<
  { value: CollectionSpec["refresh"]; label: string; hint: string }[]
>(() => [
  { value: "live", label: t("Live"), hint: t("Evaluated whenever Plex asks") },
  { value: "hourly", label: t("Hourly"), hint: t("Rebuilt every hour") },
  { value: "daily", label: t("Daily"), hint: t("Rebuilt overnight") },
  { value: "weekly", label: t("Weekly"), hint: t("Rebuilt once a week") },
]);

export function blankSpec(mediaType: CollectionSpec["mediaType"] = "movie"): CollectionSpec {
  return {
    name: "",
    description: "",
    mediaType,
    sources: [{ kind: "catalog" }],
    match: "all",
    rules: [],
    exclude: [],
    sort: "popularity",
    sortDesc: true,
    limit: 50,
    refresh: "daily",
    rotate: null,
    artwork: { mode: "auto" },
    targets: {
      collectionsTab: true,
      homeRow: false,
      sectionRow: false,
      users: "all",
      schedule: null,
    },
    enabled: false,
  };
}

/** Fill in anything a server or template omitted so the editor never meets undefined. */
export function normalizeSpec(spec: Partial<CollectionSpec>): CollectionSpec {
  const base = blankSpec(spec.mediaType ?? "movie");
  return {
    ...base,
    ...spec,
    sources: Array.isArray(spec.sources) && spec.sources.length ? spec.sources : base.sources,
    rules: Array.isArray(spec.rules) ? spec.rules : [],
    exclude: Array.isArray(spec.exclude) ? spec.exclude : [],
    artwork: { ...base.artwork, ...(spec.artwork ?? {}) },
    targets: { ...base.targets, ...(spec.targets ?? {}) },
  };
}

/** The spec sent to /collections/preview: incomplete rules and sources are dropped. */
export function previewableSpec(spec: CollectionSpec): CollectionSpec {
  const sources = spec.sources.filter(isSourceComplete);
  return {
    ...spec,
    sources: sources.length ? sources : [{ kind: "catalog" }],
    rules: spec.rules.filter(isRuleComplete),
    exclude: spec.exclude.filter(isRuleComplete),
  };
}

/** Problems that block saving. */
export function specProblems(spec: CollectionSpec): string[] {
  const out: string[] = [];
  if (spec.releaseMode === "upcoming" || spec.releaseMode === "mixed") {
    if (!spec.targets.collectionsTab || spec.targets.homeRow || spec.targets.sectionRow)
      out.push(
        t("Upcoming collections require the Collections tab and cannot use Home or library rows."),
      );
    if (spec.localOnly || spec.sources.length !== 1 || spec.sources[0]?.kind !== "catalog")
      out.push(t("Upcoming titles require the whole catalogue source and remote TMDB discovery."));
    if (
      !Number.isInteger(spec.lookaheadDays) ||
      (spec.lookaheadDays ?? 0) < 1 ||
      (spec.lookaheadDays ?? 0) > 366
    )
      out.push(t("Look ahead must be between 1 and 366 days."));
  }
  if (!spec.name.trim()) out.push(t("Give the collection a name."));
  if (spec.sources.length === 0) out.push(t("Add at least one source."));
  const badSources = spec.sources.filter((s) => !isSourceComplete(s));
  if (badSources.length)
    out.push(
      tn(
        badSources.length,
        "Finish or remove the incomplete source.",
        "Finish or remove {count} incomplete sources.",
      ),
    );
  const badRules = [...spec.rules, ...spec.exclude].filter((r) => !isRuleComplete(r));
  if (badRules.length)
    out.push(
      tn(
        badRules.length,
        "Finish or remove the incomplete rule.",
        "Finish or remove {count} incomplete rules.",
      ),
    );
  if (spec.artwork.mode === "custom" && !spec.artwork.url?.trim())
    out.push(t("Add an image URL for custom artwork."));
  if (spec.rotate && (spec.rotate.size < 1 || !Number.isFinite(spec.rotate.size)))
    out.push(t("Rotation needs a subset size of at least 1."));
  return out;
}

/** Where the collection appears, in words. */
export function describeTargets(spec: CollectionSpec): string[] {
  const out: string[] = [];
  if (spec.targets.collectionsTab) out.push(t("Collections tab"));
  if (spec.targets.homeRow) out.push(t("Home row"));
  if (spec.targets.sectionRow) out.push(t("Library row"));
  return out;
}
