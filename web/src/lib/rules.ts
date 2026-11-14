import type {
  CollectionSort,
  CollectionSpec,
  FacetName,
  LookupKind,
  Rule,
  RuleField,
  RuleOp,
  Source,
} from "./contract";
import { localized, t, tn } from "../i18n";
import { NOT_RATED, ratingLevelLabel } from "./contentRatings";
import type { Descriptor, ExtendText } from "./extend";
import { textOf } from "./extend";

/**
 * Rule value encoding (the contract leaves `value` typed by field):
 *   in / notIn   → array of keys (facet keys as strings, lookup ids as numbers)
 *   is / isNot   → a single key, number or boolean
 *   gte / lte    → a number, or an ISO date (YYYY-MM-DD) for date fields
 *   between      → [min, max] with the same element type as gte/lte
 *   within       → a relative window "<n><unit>", unit d|w|m|y ("30d", "6m")
 * Lookup-backed rules also carry `label`: the display names joined by ", ".
 */

type ValueKind = "facet" | "lookup" | "number" | "date" | "bool";

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
    field: "latestSeasonPremiere",
    label: t("Latest season premiere"),
    group: t("Dates"),
    kind: "date",
    ops: DATE_OPS,
    defaultOp: "within",
    hint: t("Premiere of the latest aired regular season (TV shows)"),
  },
  {
    field: "latestEpisode",
    label: t("Latest aired episode"),
    group: t("Dates"),
    kind: "date",
    ops: DATE_OPS,
    defaultOp: "within",
    hint: t("Most recent regular episode air date (TV shows)"),
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

const OP_LABELS = localized<Record<RuleOp, string>>(() => ({
  is: t("is"),
  isNot: t("is not"),
  in: t("is any of"),
  notIn: t("is none of"),
  gte: t("is at least"),
  lte: t("is at most"),
  between: t("is between"),
  within: t("in the last"),
  unknown: t("is not set"),
  known: t("is set"),
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

function formatWithin(v: unknown): string {
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
      if (op === "unknown" || op === "known") return null;
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
  if (!meta) return { field, op: "is", value: "" };
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
  if (rule.field === "group")
    return Array.isArray(rule.value) && rule.value.every((child: Rule) => isRuleComplete(child));
  if (rule.op === "unknown" || rule.op === "known") return true;
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
  if (rule.op === "unknown") return t("{field} is not set", { field: label });
  if (rule.op === "known") return t("{field} is set", { field: label });
  if (meta?.field === "contentRating") {
    const values = (Array.isArray(v) ? v : [v]).filter((x) => x !== "" && x != null);
    const shown = [...new Set(values.map((x) => ratingLevelLabel(String(x))))];
    return opPhrase(rule.op, meta.kind, label, shown.join(", ") || "…");
  }
  if (rule.op === "between" && Array.isArray(v))
    return t("{field} between {min} and {max}", {
      field: label,
      min: String(v[0]),
      max: `${String(v[1])}${unit}`,
    });
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
    case "unknown":
      return t("{field} is not set", { field });
    case "known":
      return t("{field} is set", { field });
  }
}

// ---- Sources -------------------------------------------------------------------
//
// Collection source kinds are served by the descriptor registry
// (internal/collections, kind "collectionSource"); the console keeps no
// list of its own. These types mirror internal/extend/meta_sources.go.

interface ParamOption {
  value: string;
  label: ExtendText;
  media?: string[];
}

export interface ParamField {
  name: string;
  kind: string;
  label: ExtendText;
  hint?: ExtendText;
  required?: boolean;
  options?: ParamOption[];
  lookup?: string;
  placeholder?: string;
}

export interface CollectionSourceMeta {
  params?: ParamField[];
  editor?: string;
  defaults?: Record<string, unknown>;
  mediaTypes?: string[];
}

export type SourceDescriptor = Descriptor<CollectionSourceMeta>;

export function sourceDescriptor(
  descriptors: readonly SourceDescriptor[],
  kind: string,
): SourceDescriptor | undefined {
  return descriptors.find((d) => d.id === kind);
}

export function paramField(d: SourceDescriptor | undefined, name: string): ParamField | undefined {
  return d?.meta?.params?.find((p) => p.name === name);
}

/** Options of a select param, labels in the active language. */
export function paramOptions(
  field: ParamField | undefined,
): { value: string; label: string; media?: string[] }[] {
  return (field?.options ?? []).map((o) => ({
    value: o.value,
    label: textOf(o.label),
    media: o.media,
  }));
}

function optionLabel(field: ParamField | undefined, value: string): string {
  const o = field?.options?.find((x) => x.value === value);
  return o ? textOf(o.label) : value;
}

/** A fresh source of a descriptor's kind, from its served defaults. */
export function newSourceFromDescriptor(
  d: SourceDescriptor,
  mediaType: CollectionSpec["mediaType"],
  region = "US",
): Source {
  const src: Record<string, unknown> = { kind: d.id, ...(d.meta?.defaults ?? {}) };
  if ("mediaType" in src && mediaType === "show") src.mediaType = "show";
  if (paramField(d, "region") && d.meta?.editor !== "plugin") src.region = region;
  return src as Source;
}

/** A source is complete when every value it carries is filled in. Field
 * shape, not kind lists, decides, so plugin kinds work unchanged. */
export function isSourceComplete(s: Source): boolean {
  const v = s as unknown as Record<string, unknown>;
  if (s.kind === "tmdbUserList" && typeof v.url === "string")
    return /^(?:[1-9][0-9]*|https:\/\/(?:www\.)?themoviedb\.org\/list\/[0-9]+(?:-[^/?#]+)?(?:\/[^/?#]+)?\/?(?:[?#].*)?)$/.test(
      v.url.trim(),
    );
  if (typeof v.url === "string") return /^https?:\/\/\S+$/.test(v.url.trim());
  if (Array.isArray(v.keys)) return v.keys.length > 0;
  if (v.params && typeof v.params === "object") return Object.keys(v.params).length > 0;
  if (typeof v.id === "number") return v.id > 0;
  return true;
}

export function describeSource(s: Source, descriptors: readonly SourceDescriptor[] = []): string {
  const d = sourceDescriptor(descriptors, s.kind);
  const label = d ? textOf(d.label) : s.kind;
  const v = s as unknown as Record<string, unknown>;
  switch (d?.meta?.editor) {
    case "none":
      return label;
    case "tmdbChart": {
      const list = optionLabel(paramField(d, "list"), String(v.list ?? ""));
      return v.mediaType === "show" ? t("{list} (TV)", { list }) : t("{list} (movies)", { list });
    }
    case "titles": {
      const keys = Array.isArray(v.keys) ? v.keys : [];
      return tn(keys.length, "{count} hand-picked title", "{count} hand-picked titles");
    }
    case "discover": {
      const params = (v.params ?? {}) as Record<string, string>;
      return t("Discover: {params}", {
        params:
          Object.entries(params)
            .map(([k, val]) => `${k}=${val}`)
            .join("&") || t("no parameters"),
      });
    }
    case "urlList":
      return t("{source}: {detail}", { source: label, detail: String(v.url ?? "") || t("no URL") });
    case "listSelect":
      return t("{source}: {list}", {
        source: label,
        list: optionLabel(paramField(d, "list"), String(v.list ?? "")),
      });
    case "lookup": {
      const service = typeof v.label === "string" && v.label ? v.label : label;
      if (paramField(d, "region")) {
        return v.region
          ? t("{service} ({region})", { service, region: String(v.region) })
          : t("{service} (default region)", { service });
      }
      return t("{source}: {detail}", {
        source: label,
        detail: (typeof v.label === "string" && v.label) || (v.id ? `#${v.id}` : t("not chosen")),
      });
    }
    default:
      return label;
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
