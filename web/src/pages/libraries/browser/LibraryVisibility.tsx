import {
  ExtendedCondition,
  conditionValue,
  browseFields,
  conditionTitle,
  newBrowseCondition,
} from "./RuleCondition";
import { PluginBrowse } from "../PluginBrowse";
import { librariesApi, type Library as PluginLibrary } from "../api";
import { Link } from "react-router-dom";
import { CatalogBrowser } from "./Catalog";
import { Button, Input, Select } from "@braedonsaunders/appkit-ui";
import {
  CheckCircle2,
  Library,
  Eye,
  EyeOff,
  ArrowDown,
  ChevronDown,
  ChevronUp,
  GripVertical,
  Pencil,
  Plus,
  Trash2,
} from "lucide-react";
import { useMemo, useState } from "react";
import { Drawer } from "../../../components/Drawer";
import { Artwork } from "../../../components/Artwork";
import { Segmented } from "../../../components/fields";
import { t, tn } from "../../../i18n";
import { apiFetch } from "../../../lib/api";
import { formatNumber } from "../../../lib/format";
import type { Rule, RuleField } from "../../../lib/contract";
import { useAsync, useDebouncedValue } from "../../../lib/hooks";
import { describeRule, fieldMeta, isRuleComplete, type FieldMeta } from "../../../lib/rules";
import { titlePosterURL } from "../../../lib/titles";
import { RuleRow } from "../../collections/RuleRow";

export type VisibilityRule = {
  name: string;
  action?: string;
  mediaType: string;
  match: string;
  rules: Rule[];
  /** "discovery" hides from browsing only; empty or "everywhere" hides everywhere. */
  scope?: string;
};

type Preview = {
  total: number;
  hidden: number;
  visible?: number;
  movies: number;
  shows: number;
  items?:
    { key: string; title: string; year?: number; poster?: string; posterPath?: string }[] | null;
  shown?:
    { key: string; title: string; year?: number; poster?: string; posterPath?: string }[] | null;
  rules?: { movies: number; shows: number }[] | null;
};

const FIELDS: RuleField[] = [
  "genre",
  "keyword",
  "studio",
  "country",
  "language",
  "contentRating",
  "year",
  "rating",
  "votes",
  "popularity",
  "runtime",
];

function metaFor(field: RuleField): FieldMeta | undefined {
  const base = fieldMeta(field);
  if (!base) return undefined;
  const extra = [...base.ops, "unknown" as const, "known" as const];
  if (field === "keyword")
    return { ...base, kind: "facet", facet: "keyword", ops: extra, defaultOp: "in" };
  if (field === "studio")
    return {
      ...base,
      label: t("Studio or network"),
      kind: "facet",
      facet: "studio",
      ops: extra,
      defaultOp: "in",
    };
  if (field === "contentRating") return base;
  if (base.kind === "facet" || base.kind === "number") return { ...base, ops: extra };
  return base;
}

const blank = (): Rule => ({ field: "genre", op: "in", value: [] });

function actionOf(rule: VisibilityRule) {
  return rule.action === "show" ? "show" : "hide";
}

function browsingOnly(rule: VisibilityRule) {
  return actionOf(rule) === "hide" && rule.scope === "discovery";
}

function conditionLine(rule: Rule): string {
  if (rule.field === "group" && Array.isArray(rule.value) && rule.value.length === 0)
    return conditionTitle(rule);
  if (rule.field === "group")
    return `${conditionTitle(rule)} (${(rule.value as Rule[]).map(conditionLine).join(" · ")})`;
  if (rule.field.startsWith("browse:") || rule.field === "viewer")
    return `${conditionTitle(rule)}: ${conditionValue(rule.value, rule.field)}`;
  if (rule.op === "in") return describeRule({ ...rule, op: "is" });
  return describeRule(rule);
}

/** "Hide · Genre is Horror, Thriller". The stored name stays in the drawer. */
function ruleLabel(rule: VisibilityRule, pinnedMediaType?: "movie" | "show") {
  const action = actionOf(rule) === "show" ? t("Show") : t("Hide");
  const bits = rule.rules.filter(isRuleComplete).map(conditionLine);
  const body =
    rule.match === "any" && bits.length > 1
      ? bits.reduce((left, right) => t("{left} or {right}", { left, right }))
      : bits.join(" · ");
  const media =
    pinnedMediaType && (rule.mediaType === pinnedMediaType || rule.mediaType === "mixed")
      ? ""
      : rule.mediaType === "movie"
        ? t("Movies")
        : rule.mediaType === "show"
          ? t("TV")
          : "";
  const scope = browsingOnly(rule) ? t("Browsing only") : "";
  return [action, body, media, scope].filter(Boolean).join(" · ");
}

export function RulesPanel({
  rules,
  fallback,
  onRules,
  onFallback,
  region,
  libraryId,
  mediaType,
  pluginLibrary,
  sources,
}: {
  sources?: string[];
  pluginLibrary?: PluginLibrary;
  rules: VisibilityRule[];
  fallback: string;
  onRules: (rules: VisibilityRule[]) => void;
  onFallback: (value: string) => void;
  region: string;
  libraryId?: number;
  mediaType?: "movie" | "show";
}) {
  const canonicalId = mediaType === "show" ? 2 : 1;
  const inherited = useAsync(
    (signal) =>
      !pluginLibrary && (Boolean(sources) || Boolean(libraryId && libraryId > 2))
        ? librariesApi.get(canonicalId, signal)
        : Promise.resolve(null),
    [canonicalId, libraryId, pluginLibrary, sources],
  );
  const inheritedPolicy = inherited.data;
  const aliases: Record<string, string[]> = {
    genre: ["genre", "genres", "tags", "tag"],
    studio: ["studio", "studios"],
    country: ["country", "countries"],
    keyword: ["keyword", "keywords"],
    language: ["language"],
    runtime: ["runtime", "duration", "length"],
    votes: ["votes"],
    popularity: ["popularity"],
  };
  const pluginFields: Record<string, string> = {};
  for (const [field, keys] of Object.entries(aliases)) {
    const key = keys.find((k) =>
      pluginLibrary?.kind?.fields?.some(
        (f) =>
          f.key === k &&
          f.indexed &&
          (["runtime", "votes", "popularity"].includes(field)
            ? ["int", "float", "duration"].includes(f.type)
            : ["string", "strings", "enum", "people"].includes(f.type)),
      ),
    );
    if (key) pluginFields[field] = key;
  }
  const allowedFields = pluginLibrary
    ? FIELDS.filter((f) => f === "year" || f === "rating" || !!pluginFields[f])
    : FIELDS;
  const newCondition = (): Rule =>
    pluginLibrary ? { field: "year", op: "gte", value: "" } : blank();
  const [browse, setBrowse] = useState<"hidden" | "shown" | null>(null);
  const [browseParams, setBrowseParams] = useState(new URLSearchParams());
  const ruleFilter = useMemo(
    () => ({
      ruleVisibility: browse ?? "shown",
      rules: JSON.stringify(rules),
      ruleFallback: fallback,
    }),
    [browse, rules, fallback],
  );
  const [open, setOpen] = useState<number | null>(null);
  const [drag, setDrag] = useState<number | null>(null);
  const [fresh, setFresh] = useState<number | null>(null);
  const complete = rules.every((g) => g.rules.length > 0 && g.rules.every(isRuleComplete));
  const spec = useMemo(
    () => ({ rules, fallback, libraryId, sources, mediaType }),
    [rules, fallback, libraryId, sources, mediaType],
  );
  const draft = useDebouncedValue(spec, 400);
  const preview = useAsync(
    async (signal) => {
      if (!complete) return null;
      return apiFetch<Preview>(
        sources && !libraryId ? "/libraries/preview" : "/catalog/visibility/preview",
        {
          method: "POST",
          body: JSON.stringify({
            ...(draft.libraryId ? { libraryId: draft.libraryId } : {}),
            ...(sources && !libraryId
              ? { rules: draft.rules, sources: draft.sources, mediaType: draft.mediaType }
              : { exclusions: draft.rules }),
            visibilityDefault: draft.fallback === "hide" ? "hide" : "show",
          }),
          signal,
        },
      );
    },
    [draft, complete],
  );
  const move = (i: number, dir: number) => {
    const j = i + dir;
    if (j < 0 || j >= rules.length) return;
    const next = rules.slice();
    const [row] = next.splice(i, 1);
    next.splice(j, 0, row!);
    onRules(next);
    setOpen((cur) => (cur === i ? j : cur === j ? i : cur));
  };
  const update = (i: number, patch: Partial<VisibilityRule>) =>
    onRules(rules.map((g, n) => (n === i ? { ...g, ...patch } : g)));
  const editing = open != null ? rules[open] : undefined;
  const hiddenItems = preview.data?.items ?? [];
  const shownItems = preview.data?.shown ?? [];
  const visible =
    preview.data?.visible ?? Math.max(0, (preview.data?.total ?? 0) - (preview.data?.hidden ?? 0));

  const renderCondition = (
    rule: Rule,
    j: number,
    change: (r: Rule) => void,
    remove: () => void,
  ): React.ReactNode =>
    rule.field.startsWith("browse:") || rule.field === "group" || rule.field === "viewer" ? (
      <ExtendedCondition
        key={j}
        rule={rule}
        onChange={change}
        onRemove={remove}
        renderChild={renderCondition}
      />
    ) : (
      <RuleRow
        key={j}
        rule={rule}
        index={j}
        kind="include"
        mediaType={mediaType ?? "mixed"}
        region={region}
        allowedFields={allowedFields}
        facetLibraryId={pluginLibrary?.id}
        facetFields={pluginFields}
        metaFor={metaFor}
        inline
        openValue={fresh === j}
        onChange={change}
        onRemove={remove}
      />
    );
  return (
    <div className="@container">
      <div className="grid grid-cols-1 gap-6 @3xl:grid-cols-[minmax(0,1fr)_300px]">
        <div className="space-y-4">
          {inheritedPolicy &&
          (inheritedPolicy.rules?.length || inheritedPolicy.visibilityDefault === "hide") ? (
            <section className="space-y-2 rounded-xl border border-border bg-surface p-4">
              <div className="flex items-center justify-between">
                <p className="font-medium">
                  {inheritedPolicy.name} · {t("Rules")}
                </p>
                <Link to={`/libraries/${canonicalId}/rules`}>{t("Edit")}</Link>
              </div>
              <ul className="space-y-1 text-sm">
                {inheritedPolicy.rules?.map((rule, i) => (
                  <li key={i}>{ruleLabel(rule, mediaType)}</li>
                ))}
              </ul>
              <p className="text-sm text-fg-muted">
                {t("Everything else")} ·{" "}
                {inheritedPolicy.visibilityDefault === "hide" ? t("Hide") : t("Show")}
              </p>
            </section>
          ) : null}
          <div className="flex items-center gap-3 rounded-xl border border-border bg-surface p-5 text-lg font-semibold">
            <Library className="shrink-0 text-primary" size={24} aria-hidden />
            {preview.loading
              ? t("Loading titles…")
              : libraryId
                ? tn(
                    preview.data?.total ?? 0,
                    "{count} title in this library",
                    "{count} titles in this library",
                  )
                : tn(
                    preview.data?.total ?? 0,
                    "{count} title in your catalog",
                    "{count} titles in your catalog",
                  )}
          </div>
          <div className="ml-6 border-l-2 border-border pl-5 space-y-4">
            <ArrowDown className="text-fg-muted" size={20} aria-hidden />
            {!pluginLibrary ? (
              <p className="text-sm text-fg-muted">
                {t("First match wins. Ratings match by age level.")}
              </p>
            ) : null}
            {rules.length === 0 ? (
              <p className="rounded-lg border border-dashed border-border px-4 py-6 text-sm text-fg-muted">
                {fallback === "hide"
                  ? t("No rules yet. Everything is hidden.")
                  : t("No rules yet. Everything is shown.")}
              </p>
            ) : null}
            <ol className="space-y-2">
              {rules.map((rule, i) => {
                const counts = preview.data?.rules?.[i];
                return (
                  <li
                    key={i}
                    draggable
                    onDragStart={() => setDrag(i)}
                    onDragOver={(e) => e.preventDefault()}
                    onDrop={() => {
                      if (drag == null || drag === i) return;
                      const next = rules.slice();
                      const [row] = next.splice(drag, 1);
                      next.splice(i, 0, row!);
                      onRules(next);
                      setDrag(null);
                    }}
                    className="grid grid-cols-[minmax(0,1fr)_repeat(4,auto)] items-center gap-1 rounded-lg border border-border bg-surface px-2 py-2 sm:flex sm:gap-2"
                  >
                    <button
                      type="button"
                      className="hidden cursor-grab px-1 text-fg-muted sm:block"
                      aria-label={t("Drag rule {n}", { n: i + 1 })}
                    >
                      <GripVertical size={16} />
                    </button>
                    <button
                      type="button"
                      className="col-span-5 min-w-0 flex-1 text-left"
                      onClick={() => setOpen(i)}
                    >
                      <span className="block text-sm font-medium sm:truncate">
                        {ruleLabel(rule, mediaType)}
                      </span>
                    </button>
                    {counts ? (
                      <span className="shrink-0 text-xs text-fg-muted">
                        {pluginLibrary ? (
                          tn(counts.movies + counts.shows, "{count} item", "{count} items")
                        ) : mediaType === "movie" ? (
                          tn(counts.movies, "{count} movie", "{count} movies")
                        ) : mediaType === "show" ? (
                          tn(counts.shows, "{count} show", "{count} shows")
                        ) : (
                          <>
                            {tn(counts.movies, "{count} movie", "{count} movies")}
                            {" · "}
                            {tn(counts.shows, "{count} show", "{count} shows")}
                          </>
                        )}
                      </span>
                    ) : null}
                    <Button
                      variant="ghost"
                      size="icon"
                      aria-label={t("Move rule {n} up", { n: i + 1 })}
                      disabled={i === 0}
                      onClick={() => move(i, -1)}
                    >
                      <ChevronUp size={16} />
                    </Button>
                    <Button
                      variant="ghost"
                      size="icon"
                      aria-label={t("Move rule {n} down", { n: i + 1 })}
                      disabled={i === rules.length - 1}
                      onClick={() => move(i, 1)}
                    >
                      <ChevronDown size={16} />
                    </Button>
                    <Button
                      variant="ghost"
                      size="icon"
                      aria-label={t("Edit rule {n}", { n: i + 1 })}
                      onClick={() => setOpen(i)}
                    >
                      <Pencil size={16} />
                    </Button>
                    <Button
                      variant="ghost"
                      size="icon"
                      aria-label={t("Delete rule {n}", { n: i + 1 })}
                      onClick={() => {
                        onRules(rules.filter((_, n) => n !== i));
                        setOpen(null);
                      }}
                    >
                      <Trash2 size={16} />
                    </Button>
                  </li>
                );
              })}
            </ol>
            <div className="flex items-center gap-3 rounded-lg border border-border bg-surface-alt px-3 py-2">
              <span className="flex-1 text-sm font-medium">{t("Everything else")}</span>
              <Segmented
                ariaLabel={t("Everything else")}
                size="sm"
                value={fallback === "hide" ? "hide" : "show"}
                onChange={onFallback}
                options={[
                  { value: "show", label: t("Show") },
                  { value: "hide", label: t("Hide") },
                ]}
              />
            </div>
            <Button
              variant="outline"
              size="sm"
              disabled={rules.length >= 32}
              onClick={() => {
                onRules([
                  ...rules,
                  {
                    name: "",
                    action: "hide",
                    mediaType: mediaType ?? "mixed",
                    match: "all",
                    rules: [newCondition()],
                  },
                ]);
                setFresh(0);
                setOpen(rules.length);
              }}
            >
              <Plus size={14} />
              {t("Add rule")}
            </Button>
            <ArrowDown className="text-fg-muted" size={20} aria-hidden />
          </div>
          <div
            className="flex items-center gap-3 rounded-xl border border-border bg-surface p-5 font-semibold"
            role="status"
          >
            <Eye className="shrink-0 text-success" size={24} aria-hidden />
            {t("{shown} shown · {hidden} hidden", {
              shown: formatNumber(visible),
              hidden: formatNumber(preview.data?.hidden ?? 0),
            })}
          </div>
        </div>
        <aside
          className="min-w-0 space-y-3 lg:sticky lg:top-0 lg:self-start"
          aria-label={t("Preview")}
        >
          <section className="min-h-fit rounded-xl border border-border bg-surface p-5 pb-6">
            <CheckCircle2 className="mb-4 text-success" size={32} aria-hidden />
            {!complete ? (
              <p className="text-sm text-fg-muted">{t("Finish each condition to preview.")}</p>
            ) : preview.error ? (
              <p className="text-sm text-fg-muted">{t("Preview isn't available.")}</p>
            ) : (
              <>
                <p className="text-3xl font-semibold">
                  {preview.loading ? "…" : formatNumber(visible)}
                </p>
                <p className="text-sm text-fg-muted">
                  {(preview.data?.hidden ?? 0) > 0
                    ? t("visible of {total}", { total: formatNumber(preview.data?.total ?? 0) })
                    : t("titles visible")}
                </p>
                {(preview.data?.hidden ?? 0) > 0 ? (
                  <p className="mt-2 text-xs text-fg-muted">
                    {pluginLibrary
                      ? t("Hidden: {count}", { count: formatNumber(preview.data?.hidden ?? 0) })
                      : mediaType === "movie"
                        ? t("Hidden: {count}", {
                            count: tn(preview.data?.movies ?? 0, "{count} movie", "{count} movies"),
                          })
                        : mediaType === "show"
                          ? t("Hidden: {count}", {
                              count: tn(preview.data?.shows ?? 0, "{count} show", "{count} shows"),
                            })
                          : t("Hidden: {movies} · {shows}", {
                              movies: tn(
                                preview.data?.movies ?? 0,
                                "{count} movie",
                                "{count} movies",
                              ),
                              shows: tn(preview.data?.shows ?? 0, "{count} show", "{count} shows"),
                            })}
                  </p>
                ) : null}
              </>
            )}
          </section>
          <PosterRow
            title={t("Hidden")}
            count={preview.data?.hidden ?? 0}
            empty={t("Nothing hidden")}
            items={hiddenItems}
            onBrowse={() => {
              setBrowseParams(new URLSearchParams());
              setBrowse("hidden");
            }}
          />
          <PosterRow
            title={t("Shown")}
            count={visible}
            empty={t("Nothing shown")}
            items={shownItems}
            onBrowse={() => {
              setBrowseParams(new URLSearchParams());
              setBrowse("shown");
            }}
          />
        </aside>
        <Drawer
          open={browse !== null}
          onClose={() => setBrowse(null)}
          title={browse === "hidden" ? t("Hidden by rules") : t("Shown by rules")}
          size="2xl"
        >
          {browse && pluginLibrary ? (
            <PluginBrowse
              library={pluginLibrary}
              ruleFilter={ruleFilter}
              controlledParams={browseParams}
              onParams={setBrowseParams}
            />
          ) : browse ? (
            <CatalogBrowser
              params={browseParams}
              setParams={setBrowseParams}
              refreshHealth={async () => {}}
              ruleFilter={ruleFilter}
              libraryScope={libraryId ? { id: libraryId, type: mediaType ?? "" } : undefined}
            />
          ) : null}
        </Drawer>
        <Drawer
          open={editing != null}
          onClose={() => setOpen(null)}
          title={editing ? editing.name.trim() || ruleLabel(editing, mediaType) : t("Rule")}
          size="lg"
        >
          {editing && open != null ? (
            <div className="space-y-4">
              <Input
                aria-label={t("Rule name")}
                placeholder={t("Name (optional)")}
                value={editing.name}
                onChange={(e) => update(open, { name: e.target.value })}
              />
              <div className="flex flex-wrap gap-3">
                <Segmented
                  ariaLabel={t("Rule action")}
                  value={actionOf(editing)}
                  onChange={(v) =>
                    update(open, v === "show" ? { action: v, scope: undefined } : { action: v })
                  }
                  options={[
                    { value: "show", label: t("Show") },
                    { value: "hide", label: t("Hide") },
                  ]}
                />
                {!mediaType && !pluginLibrary ? (
                  <Segmented
                    ariaLabel={t("Rule media")}
                    value={editing.mediaType}
                    onChange={(v) => update(open, { mediaType: v })}
                    options={[
                      { value: "mixed", label: t("Movies + TV") },
                      { value: "movie", label: t("Movies") },
                      { value: "show", label: t("TV") },
                    ]}
                  />
                ) : null}
                <Segmented
                  ariaLabel={t("Rule match")}
                  value={editing.match}
                  onChange={(v) => update(open, { match: v })}
                  options={[
                    { value: "all", label: t("All") },
                    { value: "any", label: t("Any") },
                  ]}
                />
              </div>
              {actionOf(editing) === "hide" ? (
                <div className="space-y-1">
                  <Select
                    aria-label={t("Where to hide")}
                    className="max-w-xs"
                    value={browsingOnly(editing) ? "discovery" : "everywhere"}
                    onChange={(e) =>
                      update(open, {
                        scope: e.target.value === "discovery" ? "discovery" : undefined,
                      })
                    }
                  >
                    <option value="everywhere">{t("Hide everywhere")}</option>
                    <option value="discovery">{t("Hide from browsing only")}</option>
                  </Select>
                  <p className="text-xs text-fg-muted">
                    {t(
                      "Browsing only keeps these titles out of Home, rows, recommendations, New Releases and Trending. Anyone can still search for them and play them.",
                    )}
                  </p>
                </div>
              ) : null}
              {editing.rules.some(
                (rule) => rule.field === "country" && (rule.op === "in" || rule.op === "is"),
              ) && (
                <p className="text-xs text-fg-muted">
                  {t(
                    "Hide matches only when every production country is listed. Show matches when any country is listed.",
                  )}
                </p>
              )}
              {editing.rules.map((rule, j) =>
                renderCondition(
                  rule,
                  j,
                  (v) => update(open, { rules: editing.rules.map((r, k) => (k === j ? v : r)) }),
                  () => update(open, { rules: editing.rules.filter((_, k) => k !== j) }),
                ),
              )}
              {!pluginLibrary ? (
                <Select
                  aria-label={t("Add condition")}
                  value=""
                  onChange={(e) => {
                    if (e.target.value)
                      update(open, {
                        rules: [...editing.rules, newBrowseCondition(e.target.value)],
                      });
                  }}
                >
                  <option value="">{t("More conditions")}</option>
                  {browseFields().map(([key, label]) => (
                    <option key={key} value={key}>
                      {label}
                    </option>
                  ))}
                </Select>
              ) : null}
              <Button
                size="sm"
                variant="outline"
                disabled={editing.rules.length >= 20}
                onClick={() => {
                  setFresh(editing.rules.length);
                  update(open, { rules: [...editing.rules, newCondition()] });
                }}
              >
                <Plus size={14} />
                {t("Add condition")}
              </Button>
            </div>
          ) : null}
        </Drawer>
      </div>
    </div>
  );
}

function PosterRow({
  count,
  empty,
  title,
  items,
  onBrowse,
}: {
  title: string;
  count: number;
  empty: string;
  onBrowse: () => void;
  items: { key: string; title: string; poster?: string; posterPath?: string }[];
}) {
  return (
    <section className="min-w-0 rounded-xl border border-border bg-surface p-3">
      <div className="mb-2 flex items-center justify-between gap-2">
        <h3 className="text-sm font-medium">
          {t("{title} · {count}", { title, count: formatNumber(count) })}
        </h3>
        <Button variant="ghost" size="sm" onClick={onBrowse} disabled={count === 0}>
          {t("Browse")}
        </Button>
      </div>
      {count === 0 ? (
        <div className="flex items-center gap-2 rounded-lg border border-dashed border-border p-4 text-sm text-fg-muted">
          <EyeOff size={18} aria-hidden />
          {empty}
        </div>
      ) : null}
      <div className="flex gap-2 overflow-x-auto pb-1">
        {items.slice(0, 24).map((item) => (
          <Artwork
            key={item.key}
            src={item.poster ?? titlePosterURL(item, 185)}
            alt={item.title}
            className="aspect-[2/3] w-14 shrink-0 rounded-md"
          />
        ))}
      </div>
    </section>
  );
}
