import { Button, Input } from "@braedonsaunders/appkit-ui";
import { ChevronDown, ChevronUp, GripVertical, Pencil, Plus, Trash2 } from "lucide-react";
import { useMemo, useState } from "react";
import { Drawer } from "../../components/Drawer";
import { Artwork } from "../../components/Artwork";
import { Segmented } from "../../components/fields";
import { ErrorPanel } from "../../components/feedback";
import { t, tn } from "../../i18n";
import { apiFetch } from "../../lib/api";
import { formatNumber } from "../../lib/format";
import type { Rule, RuleField } from "../../lib/contract";
import { useAsync, useDebouncedValue } from "../../lib/hooks";
import { describeRule, fieldMeta, isRuleComplete, type FieldMeta } from "../../lib/rules";
import { RuleRow } from "../collections/RuleRow";

export type VisibilityRule = {
  name: string;
  action?: string;
  mediaType: string;
  match: string;
  rules: Rule[];
};

type Preview = {
  total: number;
  hidden: number;
  visible?: number;
  movies: number;
  shows: number;
  items?: { key: string; title: string; year?: number; poster?: string; posterPath?: string }[] | null;
  shown?: { key: string; title: string; year?: number; poster?: string; posterPath?: string }[] | null;
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

function conditionLine(rule: Rule) {
  if (rule.op === "in") return describeRule({ ...rule, op: "is" });
  return describeRule(rule);
}

/** "Hide · Genre is Horror, Thriller". The stored name stays in the drawer. */
export function ruleLabel(rule: VisibilityRule) {
  const action = actionOf(rule) === "show" ? t("Show") : t("Hide");
  const bits = rule.rules.filter(isRuleComplete).map(conditionLine);
  const body =
    rule.match === "any" && bits.length > 1
      ? bits.reduce((left, right) => t("{left} or {right}", { left, right }))
      : bits.join(" · ");
  const media = rule.mediaType === "movie" ? t("Movies") : rule.mediaType === "show" ? t("TV") : "";
  return [action, body, media].filter(Boolean).join(" · ");
}

export function RulesPanel({
  rules,
  fallback,
  onRules,
  onFallback,
  region,
}: {
  rules: VisibilityRule[];
  fallback: string;
  onRules: (rules: VisibilityRule[]) => void;
  onFallback: (value: string) => void;
  region: string;
}) {
  const [open, setOpen] = useState<number | null>(null);
  const [drag, setDrag] = useState<number | null>(null);
  const [fresh, setFresh] = useState<number | null>(null);
  const complete = rules.every((g) => g.rules.length > 0 && g.rules.every(isRuleComplete));
  const spec = useMemo(() => ({ rules, fallback }), [rules, fallback]);
  const draft = useDebouncedValue(spec, 400);
  const preview = useAsync(
    async (signal) => {
      if (!complete) return null;
      return apiFetch<Preview>("/catalog/visibility/preview", {
        method: "POST",
        body: JSON.stringify({
          exclusions: draft.rules,
          visibilityDefault: draft.fallback === "hide" ? "hide" : "show",
        }),
        signal,
      });
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
    preview.data?.visible ??
    Math.max(0, (preview.data?.total ?? 0) - (preview.data?.hidden ?? 0));
  return (
    <div className="grid gap-6 lg:grid-cols-[minmax(0,1fr)_300px]">
      <div className="space-y-2">
        <p className="text-sm text-fg-muted">
          {t("First match wins. Ratings match by age level.")}
        </p>
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
                className="flex items-center gap-2 rounded-lg border border-border bg-surface px-2 py-2"
              >
                <button
                  type="button"
                  className="cursor-grab px-1 text-fg-muted"
                  aria-label={t("Drag rule {n}", { n: i + 1 })}
                >
                  <GripVertical size={16} />
                </button>
                <button
                  type="button"
                  className="min-w-0 flex-1 text-left"
                  onClick={() => setOpen(i)}
                >
                  <span className="block truncate text-sm font-medium">{ruleLabel(rule)}</span>
                </button>
                {counts ? (
                  <span className="shrink-0 text-xs text-fg-muted">
                    {tn(counts.movies, "{count} movie", "{count} movies")}
                    {" · "}
                    {tn(counts.shows, "{count} show", "{count} shows")}
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
                mediaType: "mixed",
                match: "all",
                rules: [blank()],
              },
            ]);
            setFresh(0);
            setOpen(rules.length);
          }}
        >
          <Plus size={14} />
          {t("Add rule")}
        </Button>
      </div>
      <aside className="space-y-3 lg:sticky lg:top-0 lg:self-start" aria-label={t("Preview")}>
        <div className="rounded-lg border border-border bg-surface p-4">
          {preview.error ? <ErrorPanel error={preview.error} onRetry={preview.reload} /> : null}
          {!complete ? (
            <p className="text-sm text-fg-muted">{t("Finish each condition to preview.")}</p>
          ) : (
            <>
              <p className="text-3xl font-semibold">{preview.loading ? "…" : formatNumber(visible)}</p>
              <p className="text-sm text-fg-muted">
                {tn(visible, "{count} title visible", "{count} titles visible")}
              </p>
              {(preview.data?.hidden ?? 0) > 0 ? (
                <p className="mt-2 text-xs text-fg-muted">
                  {t("Hidden: {movies} · {shows}", {
                    movies: tn(preview.data?.movies ?? 0, "{count} movie", "{count} movies"),
                    shows: tn(preview.data?.shows ?? 0, "{count} show", "{count} shows"),
                  })}
                </p>
              ) : null}
            </>
          )}
        </div>
        <PosterRow title={t("Hidden")} items={hiddenItems} />
        <PosterRow title={t("Shown")} items={shownItems} />
      </aside>
      <Drawer
        open={editing != null}
        onClose={() => setOpen(null)}
        title={editing ? editing.name.trim() || ruleLabel(editing) : t("Rule")}
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
                onChange={(v) => update(open, { action: v })}
                options={[
                  { value: "show", label: t("Show") },
                  { value: "hide", label: t("Hide") },
                ]}
              />
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
            {editing.rules.map((rule, j) => (
              <RuleRow
                key={j}
                rule={rule}
                index={j}
                kind="include"
                mediaType={editing.mediaType}
                region={region}
                allowedFields={FIELDS}
                metaFor={metaFor}
                inline
                openValue={fresh === j}
                onChange={(v) =>
                  update(open, { rules: editing.rules.map((r, k) => (k === j ? v : r)) })
                }
                onRemove={() => update(open, { rules: editing.rules.filter((_, k) => k !== j) })}
              />
            ))}
            <Button
              size="sm"
              variant="outline"
              disabled={editing.rules.length >= 20}
              onClick={() => {
                setFresh(editing.rules.length);
                update(open, { rules: [...editing.rules, blank()] });
              }}
            >
              <Plus size={14} />
              {t("Add condition")}
            </Button>
          </div>
        ) : null}
      </Drawer>
    </div>
  );
}

function PosterRow({
  title,
  items,
}: {
  title: string;
  items: { key: string; title: string; poster?: string; posterPath?: string }[];
}) {
  if (!items.length) return null;
  return (
    <div>
      <h3 className="mb-2 text-xs font-medium text-fg-muted">{title}</h3>
      <div className="grid grid-cols-4 gap-2">
        {items.slice(0, 4).map((item) => (
          <Artwork
            key={item.key}
            src={
              item.poster ??
              (item.posterPath ? `https://image.tmdb.org/t/p/w185${item.posterPath}` : undefined)
            }
            alt={item.title}
            className="aspect-[2/3] rounded-md"
          />
        ))}
      </div>
    </div>
  );
}
