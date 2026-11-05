import { Link } from "react-router-dom";
import { newCollectionHref } from "../collections/links";
import { Drawer } from "../../components/Drawer";
import { Badge, Button, Checkbox, Input, cn } from "@braedonsaunders/appkit-ui";
import {
  ChevronDown,
  ChevronRight,
  Plus,
  RotateCcw,
  Search,
  SlidersHorizontal,
  Trash2,
  X,
} from "lucide-react";
import { useEffect, useMemo, useRef, useState } from "react";
import { Segmented } from "../../components/fields";
import type {
  AvailableRow,
  HomeLayout,
  LayoutSection,
  RowGroup,
  RowInsight,
  RowPolicy,
  RowStatus,
  TargetSchedule,
  Viewer,
} from "../../lib/contract";
import { api } from "../../lib/api";
import { localized, t, tn, tx } from "../../i18n";
import { WEEKDAYS, formatNumber } from "../../lib/format";
import {
  FRONT_END_ROWS,
  collectionIdOf,
  collectionSchedule,
  effectivePolicy,
  matchRank,
  setCollectionSchedule,
  STATUS_HINT,
  STATUS_LABEL,
  pageOf,
  policyChips,
  policyIn,
  removePolicies,
  removePolicy,
  rowFits,
  setPolicy,
  setStatuses,
  viewersOverriding,
  type LayerKey,
} from "../../lib/layout";
import { PolicyEditor, type ScheduleSource } from "./PolicyEditor";

import { isPluginDescriptor, textOf, useRegistry, type Descriptor } from "../../lib/extend";
import { useLocale } from "../../i18n/react";
import { useNavigate } from "react-router-dom";
import { blankSpec, newSourceFromDescriptor, type CollectionSourceMeta } from "../../lib/rules";

interface PoolEntry {
  row: string;
  title: string;
  description: string;
  group: PoolGroup;
  kind?: string;
  owner?: string;
  collectionSource?: string;
}

/** Mirrors internal/extend.RowSourceMeta. */
interface RowSourceMeta {
  hubId: string;
  rowKind?: string;
  sections?: string[];
  source?: string;
  collectionSource?: string;
}

type PoolGroup = RowGroup | "custom";
type StatusFilter = RowStatus | "all" | "changed";

/** A Collections group longer than this starts folded, so it doesn't bury the other rows. */
const FOLD_AT = 12;

/** Where a row's status comes from in the layer being edited. */
interface Resolved {
  policy: RowPolicy;
  origin: "own" | "global" | "pattern" | "default";
  /** The pattern (or kind) rule that reaches the row, for origin "pattern". */
  via?: string;
}

/**
 * A row's policy as this layer sees it, matching the server: its own rule,
 * else the most specific rule that reaches it (Everyone's, or a pattern's),
 * else the default.
 */
function resolve(
  draft: HomeLayout,
  layer: LayerKey,
  section: LayoutSection,
  e: PoolEntry,
): Resolved {
  const own = policyIn(draft, layer, section, e.row);
  if (own) return { policy: own, origin: "own" };
  const eff = effectivePolicy(
    draft,
    layer === "global" ? null : layer,
    section,
    e.row,
    e.kind ?? e.row,
  );
  if (eff && eff.policy.row !== e.row)
    return {
      policy: { row: e.row, status: eff.policy.status },
      origin: "pattern",
      via: eff.policy.row,
    };
  if (eff) return { policy: eff.policy, origin: "global" };
  return { policy: { row: e.row, status: "eligible" }, origin: "default" };
}

const GROUPS = localized<{ key: PoolGroup; label: string; hint: string }[]>(() => [
  { key: "picks", label: t("Picks for you"), hint: t("Personal, high-confidence rows") },
  { key: "explore", label: t("Explore"), hint: t("Lenses into the catalogue") },
  { key: "shows", label: t("Series"), hint: t("Built around shows and seasons") },
  { key: "household", label: t("Household"), hint: t("What the server is watching") },
  { key: "collections", label: t("Collections"), hint: t("Your Collection Studio rows") },
  { key: "plex", label: "Plex", hint: t("Rows Plex itself provides") },
  { key: "custom", label: t("Custom patterns"), hint: t("Hub ids, kinds or globs") },
]);

/**
 * The row catalogue: every row the engine, Plex or the owner's collections
 * can put on the page, with its status and constraints in this layer.
 */
export function RowPool({
  draft,
  onChange,
  layer,
  section,
  viewers,
  available,
  focusRow,
  onFocusRow,
}: {
  draft: HomeLayout;
  onChange: (d: HomeLayout) => void;
  layer: LayerKey;
  section: LayoutSection;
  viewers: Viewer[];
  available: AvailableRow[];
  focusRow: string | null;
  onFocusRow: (row: string | null) => void;
}) {
  const navigate = useNavigate();
  const collectionKinds = useRegistry<CollectionSourceMeta>("collectionSource");
  const [q, setQ] = useState("");
  const [custom, setCustom] = useState("");
  const [patternOpen, setPatternOpen] = useState(false);
  const [filter, setFilter] = useState<StatusFilter>("all");
  const [picked, setPicked] = useState<Set<string>>(() => new Set());
  const [folded, setFolded] = useState<Partial<Record<PoolGroup, boolean>>>({});
  // A selection belongs to what it was made in: switching layer or page, or
  // changing what the list shows, clears it so no hidden row gets a bulk edit.
  const scope = JSON.stringify([layer, section, q, filter]);
  const [pickScope, setPickScope] = useState(scope);
  if (pickScope !== scope) {
    setPickScope(scope);
    setPicked(new Set());
  }
  const [insights, setInsights] = useState<RowInsight[]>([]);
  useEffect(() => {
    let live = true;
    api
      .rowInsights()
      .then((r) => live && setInsights(r))
      .catch(() => undefined);
    return () => {
      live = false;
    };
  }, []);
  const { locale } = useLocale();
  // Row kinds come from the descriptor registry: built-in titles and
  // descriptions render from it, and plugin row sources join the catalogue
  // with a badge. Dynamic rows (owner collections, extra hubs) still
  // arrive through the layout API in `available`.
  const registry = useRegistry<RowSourceMeta>("rowSource");
  const entries = useMemo<PoolEntry[]>(() => {
    const byKind = new Map<string, Descriptor<RowSourceMeta>>(
      registry.descriptors.map((d) => [d.id, d]),
    );
    const out: PoolEntry[] = available
      .filter(
        (a) =>
          !FRONT_END_ROWS.has(a.kind) &&
          !byKind.get(a.kind)?.meta?.collectionSource &&
          rowFits(a, section),
      )
      .map((a) => {
        const d = byKind.get(a.kind);
        return {
          row: a.hubId,
          title: d ? textOf(d.label) : a.title,
          description: d ? textOf(d.hint) : a.description,
          group: a.group ?? "picks",
          kind: a.kind,
        };
      });
    for (const d of registry.descriptors) {
      if (!isPluginDescriptor(d)) continue;
      const hubId = d.meta?.hubId ?? d.id;
      if (out.some((e) => e.row === hubId)) continue;
      if (!rowFits({ sections: d.meta?.sections as LayoutSection[] | undefined }, section))
        continue;
      out.push({
        row: hubId,
        title: textOf(d.label),
        description: textOf(d.hint),
        group: "collections",
        kind: d.meta?.rowKind ?? d.id,
        owner: d.owner,
        collectionSource: d.meta?.collectionSource,
      });
    }
    // Policies for patterns or hub ids that are not in the catalogue.
    for (const layerKey of ["global", layer] as LayerKey[]) {
      for (const p of pageOf(draft, layerKey, section)?.rows ?? []) {
        if (!out.some((e) => e.row === p.row))
          out.push({
            row: p.row,
            title: p.row,
            description: t("Custom row pattern (hub id, kind or glob)."),
            group: "custom",
          });
      }
    }
    return out;
  }, [available, draft, layer, section, locale, registry.descriptors]);
  const needle = q.trim().toLowerCase();
  const resolved = useMemo(
    () => new Map(entries.map((e) => [e.row, resolve(draft, layer, section, e)])),
    [entries, draft, layer, section],
  );
  const statusOf = (e: PoolEntry) => resolved.get(e.row)!.policy.status;
  const shown = entries.filter(
    (e) =>
      (!needle || e.title.toLowerCase().includes(needle) || e.row.toLowerCase().includes(needle)) &&
      (filter === "all" ||
        (!e.collectionSource &&
          (filter === "changed" ? resolved.get(e.row)!.origin === "own" : statusOf(e) === filter))),
  );
  const pickedRows = entries
    .filter((e) => !e.collectionSource && picked.has(e.row))
    .map((e) => e.row);
  const pick = (rows: string[], on: boolean) =>
    setPicked((cur) => {
      const next = new Set(cur);
      for (const r of rows) {
        if (on) next.add(r);
        else next.delete(r);
      }
      return next;
    });
  const bulk = (status: RowStatus | null) => {
    onChange(
      status
        ? setStatuses(draft, layer, section, pickedRows, status, (row) => resolved.get(row)?.policy)
        : removePolicies(draft, layer, section, pickedRows),
    );
    setPicked(new Set());
  };
  const matchesOf = (pattern: string) =>
    entries.filter(
      (e) =>
        e.group !== "custom" &&
        !e.collectionSource &&
        matchRank(pattern, e.row, e.kind ?? e.row) > 0,
    ).length;
  const filtering = Boolean(needle) || filter !== "all";
  const statuses: RowStatus[] = ["always", "eligible", "off"];
  return (
    <section className="space-y-2" aria-label={t("Row catalogue")}>
      <div>
        <h2 className="text-xs font-semibold uppercase tracking-wider text-fg-subtle">
          {t("Row catalogue")}
        </h2>
        <p className="mt-0.5 text-xs text-fg-muted">
          {layer === "global"
            ? t(
                "Always: on the page whenever it has titles. Eligible: personalization decides. Off: never.",
              )
            : t(
                "Changes here override Everyone for this viewer only; reset to follow Everyone again.",
              )}
        </p>
      </div>
      <div className="relative">
        <Search
          size={14}
          className="pointer-events-none absolute left-2.5 top-1/2 -translate-y-1/2 text-fg-subtle"
        />
        <Input
          value={q}
          onChange={(e) => setQ(e.currentTarget.value)}
          placeholder={t("Filter rows…")}
          aria-label={t("Filter rows")}
          className="h-8 pl-8"
        />
      </div>
      <Segmented
        ariaLabel={t("Show rows by status")}
        size="sm"
        value={filter}
        onChange={setFilter}
        options={[
          { value: "all", label: t("All") },
          ...statuses.map((s) => ({ value: s, label: STATUS_LABEL[s] })),
          { value: "changed", label: layer === "global" ? t("Changed") : t("Overridden") },
        ]}
      />
      {pickedRows.length ? (
        <div
          role="toolbar"
          aria-label={t("Selected rows")}
          className="sticky top-0 z-10 flex flex-wrap items-center gap-1.5 rounded-lg border border-primary/40 bg-surface p-2 shadow-sm"
        >
          <span className="text-xs font-medium text-fg">
            {t("{count} selected", { count: formatNumber(pickedRows.length) })}
          </span>
          <span className="flex-1" />
          {statuses.map((s) => (
            <Button
              key={s}
              variant="outline"
              size="sm"
              className="h-7 px-2 text-xs"
              aria-label={t("Set the selected rows to {status}", { status: STATUS_LABEL[s] })}
              onClick={() => bulk(s)}
            >
              {STATUS_LABEL[s]}
            </Button>
          ))}
          <Button
            variant="ghost"
            size="sm"
            className="h-7 px-2 text-xs"
            aria-label={
              layer === "global"
                ? t("Set the selected rows back to default")
                : t("Reset the selected rows to Everyone")
            }
            onClick={() => bulk(null)}
          >
            <RotateCcw size={12} /> {layer === "global" ? t("Default") : t("Reset to Everyone")}
          </Button>
          <Button
            variant="ghost"
            size="icon"
            className="size-7"
            aria-label={t("Clear selection")}
            onClick={() => setPicked(new Set())}
          >
            <X size={14} />
          </Button>
        </div>
      ) : null}
      <Link
        to={newCollectionHref({ source: "addonCatalog" })}
        className="block rounded-lg border border-border bg-surface p-3 text-sm text-primary"
      >
        {t("Create a Home row from a Stremio catalog")}
      </Link>
      {GROUPS.map((g) => {
        const rows = shown.filter((e) => e.group === g.key);
        if (!rows.length) return null;
        const selectable = rows.filter((e) => !e.collectionSource).map((e) => e.row);
        const allPicked = selectable.length > 0 && selectable.every((r) => picked.has(r));
        // A search, a filter or the focused row holds a group open.
        const held = filtering || rows.some((e) => e.row === focusRow);
        const open = held || !(folded[g.key] ?? (g.key === "collections" && rows.length > FOLD_AT));
        const off = rows.filter((e) => !e.collectionSource && statusOf(e) === "off").length;
        return (
          <div key={g.key} className="space-y-1.5 pt-1" role="group" aria-label={g.label}>
            <div className="flex items-center gap-2 px-0.5">
              {selectable.length ? (
                <Checkbox
                  aria-label={t("Select every row in {group}", { group: g.label })}
                  checked={allPicked}
                  onChange={(ev) => pick(selectable, ev.currentTarget.checked)}
                />
              ) : null}
              <button
                type="button"
                aria-expanded={open}
                disabled={held}
                onClick={() => {
                  setFolded({ ...folded, [g.key]: open });
                  if (open) pick(selectable, false);
                }}
                className="flex min-w-0 flex-1 items-center gap-1.5 text-left disabled:cursor-default"
              >
                <ChevronRight
                  size={13}
                  className={cn(
                    "shrink-0 text-fg-subtle transition-transform",
                    open && "rotate-90",
                  )}
                />
                <h3 className="text-[11px] font-semibold uppercase tracking-wider text-fg-subtle">
                  {g.label}
                </h3>
                <span className="truncate text-[11px] text-fg-subtle/80">
                  {off
                    ? t("{rows} · {off}", {
                        rows: tn(rows.length, "{count} row", "{count} rows"),
                        off: tn(off, "{count} off", "{count} off"),
                      })
                    : tn(rows.length, "{count} row", "{count} rows")}
                </span>
              </button>
            </div>
            {open ? (
              <>
                <p className="px-0.5 text-[11px] text-fg-subtle/80">{g.hint}</p>
                <ul className="space-y-1.5">
                  {rows.map((e) => (
                    <PoolItem
                      key={e.row}
                      entry={e}
                      resolved={resolved.get(e.row)!}
                      matches={e.group === "custom" ? matchesOf(e.row) : undefined}
                      picked={picked.has(e.row)}
                      onPick={(on) => pick([e.row], on)}
                      onCreate={
                        e.collectionSource
                          ? () => {
                              const descriptor = collectionKinds.descriptors.find(
                                (d) => d.id === e.collectionSource,
                              );
                              if (!descriptor) return;
                              const media = section === "shows" ? "show" : "movie";
                              const spec = blankSpec(media);
                              spec.name = e.title;
                              spec.sources = [newSourceFromDescriptor(descriptor, media)];
                              spec.targets.homeRow = true;
                              spec.enabled = true;
                              navigate(newCollectionHref(), { state: { spec } });
                            }
                          : undefined
                      }
                      insight={insightFor(insights, e)}
                      draft={draft}
                      onChange={onChange}
                      layer={layer}
                      section={section}
                      viewers={viewers}
                      open={focusRow === e.row}
                      onToggle={() => onFocusRow(focusRow === e.row ? null : e.row)}
                    />
                  ))}
                </ul>
              </>
            ) : null}
          </div>
        );
      })}
      {registry.error ? (
        <p role="alert" className="px-1 py-2 text-xs text-danger">
          {t("Row kinds could not be loaded.")}{" "}
          <Button type="button" variant="ghost" size="sm" onClick={registry.reload}>
            {t("Try again")}
          </Button>
        </p>
      ) : null}
      {shown.length === 0 ? (
        <p className="px-1 py-3 text-center text-xs text-fg-muted">{t("No rows match.")}</p>
      ) : null}
      <Button
        variant="ghost"
        size="sm"
        className="h-8 text-xs text-fg-muted"
        onClick={() => setPatternOpen(true)}
      >
        <Plus size={14} /> {t("Add custom pattern")}
      </Button>
      <Drawer
        open={patternOpen}
        onClose={() => {
          setPatternOpen(false);
          setCustom("");
        }}
        title={t("Custom row pattern")}
        description={t("Advanced: target row IDs, kinds or glob patterns.")}
        size="md"
      >
        <form
          className="flex gap-2"
          onSubmit={(ev) => {
            ev.preventDefault();
            const row = custom.trim();
            if (!row) return;
            if (!policyIn(draft, layer, section, row))
              onChange(setPolicy(draft, layer, section, { row, status: "eligible" }));
            setCustom("");
            setPatternOpen(false);
            onFocusRow(row);
          }}
        >
          <Input
            value={custom}
            onChange={(e) => setCustom(e.currentTarget.value)}
            placeholder={t("Pattern, e.g. because.*")}
            aria-label={t("Custom row pattern")}
            className="h-8 flex-1 font-mono text-xs"
          />
          <Button
            type="submit"
            variant="outline"
            size="sm"
            className="h-8"
            disabled={!custom.trim()}
          >
            <Plus size={14} /> {t("Add")}
          </Button>
        </form>
      </Drawer>
    </section>
  );
}

/** The 7-day engagement of a row (by hub id, else by kind). */
function insightFor(all: RowInsight[], e: PoolEntry): RowInsight | undefined {
  return (
    all.find((i) => i.hubId === e.row) ??
    (e.kind ? all.find((i) => i.hubId.endsWith(`.${e.kind}`) || i.hubId === e.kind) : undefined)
  );
}

function Engagement({ insight }: { insight?: RowInsight }) {
  if (!insight || insight.impressions7d < 5) return null;
  const pct = Math.round(insight.playRate * 1000) / 10;
  return (
    <span
      className="inline-flex shrink-0 items-center gap-1.5 text-[11px] tabular-nums text-fg-muted"
      title={tn(
        insight.plays7d,
        "{count} play from {shown} times shown in the last 7 days",
        "{count} plays from {shown} times shown in the last 7 days",
        { shown: formatNumber(insight.impressions7d) },
      )}
    >
      <span className="relative h-1 w-10 overflow-hidden rounded-full bg-border">
        <span
          className="absolute inset-y-0 left-0 rounded-full bg-primary"
          style={{ width: `${Math.min(100, insight.playRate * 400)}%` }}
        />
      </span>
      {t("{pct}% played", { pct: formatNumber(pct, 1) })}
    </span>
  );
}

function PoolItem({
  entry,
  resolved,
  matches,
  picked,
  onPick,
  onCreate,
  insight,
  draft,
  onChange,
  layer,
  section,
  viewers,
  open,
  onToggle,
}: {
  entry: PoolEntry;
  resolved: Resolved;
  /** For a pattern: how many catalogue rows it reaches. */
  matches?: number;
  picked: boolean;
  onPick: (on: boolean) => void;
  onCreate?: () => void;
  insight?: RowInsight;
  draft: HomeLayout;
  onChange: (d: HomeLayout) => void;
  layer: LayerKey;
  section: LayoutSection;
  viewers: Viewer[];
  open: boolean;
  onToggle: () => void;
}) {
  const ref = useRef<HTMLLIElement>(null);
  useEffect(() => {
    if (open) ref.current?.scrollIntoView?.({ block: "nearest", behavior: "smooth" });
  }, [open]);
  const own = resolved.origin === "own";
  const effective = resolved.policy;
  const edit = (p: RowPolicy) => onChange(setPolicy(draft, layer, section, p));
  const reset = () => onChange(removePolicy(draft, layer, section, entry.row));
  const overriding =
    layer === "global"
      ? viewersOverriding(draft, section, entry.row).filter((id) =>
          viewers.some((v) => v.id === id),
        )
      : [];
  // A collection row's schedule is its collection's; a viewer override may set its own.
  const collectionId = collectionIdOf(entry.row);
  const ownWhen = layer !== "global" && own ? effective.when : undefined;
  const schedule: ScheduleSource | undefined = collectionId
    ? ownWhen
      ? {
          when: ownWhen,
          onChange: (w) => edit({ ...effective, when: w }),
          note: t("This viewer's own schedule, instead of the collection's."),
        }
      : {
          when: collectionSchedule(draft, collectionId),
          onChange: (w) =>
            layer === "global"
              ? onChange(
                  setCollectionSchedule(draft, collectionId, w as TargetSchedule | undefined),
                )
              : edit({ ...effective, when: w }),
          note: tx("The collection's schedule, also set in {link}.", {
            link: (
              <Link
                to={`/collections/${encodeURIComponent(collectionId)}?section=audience`}
                className="text-primary underline-offset-2 hover:underline"
              >
                {t("Collections")}
              </Link>
            ),
          }),
        }
    : undefined;
  const chips = policyChips(schedule ? { ...effective, when: schedule.when } : effective, WEEKDAYS);
  const statuses: RowStatus[] = ["always", "eligible", "off"];
  const setStatus = (s: RowStatus) =>
    edit({ ...effective, status: s, slot: s === "off" ? undefined : effective.slot });
  const pattern = matches !== undefined;
  const resetLabel = pattern
    ? t("Remove rule")
    : layer === "global"
      ? t("Default")
      : t("Reset to Everyone");
  return (
    <li
      ref={ref}
      aria-label={t("Row {title}", { title: entry.title })}
      className={cn(
        "rounded-lg border bg-surface p-2.5",
        own && layer !== "global" ? "border-info/40" : "border-border",
        picked && "border-primary/50 bg-primary-subtle/30",
        open && "ring-2 ring-primary/30",
      )}
    >
      <div className="flex items-start gap-2">
        {!onCreate ? (
          <Checkbox
            className="mt-0.5"
            aria-label={t("Select {title}", { title: entry.title })}
            checked={picked}
            onChange={(ev) => onPick(ev.currentTarget.checked)}
          />
        ) : null}
        <div className="min-w-0 flex-1">
          <div className="flex flex-wrap items-center gap-1.5">
            <span
              className={cn("truncate text-sm font-medium text-fg", pattern && "font-mono text-xs")}
              title={entry.description}
            >
              {entry.title}
            </span>
            {entry.owner ? (
              <Badge variant="secondary">{t("From {owner}", { owner: entry.owner })}</Badge>
            ) : null}
            {onCreate ? null : own && layer !== "global" ? (
              <Badge variant="info">{t("Override")}</Badge>
            ) : resolved.origin === "global" && layer !== "global" ? (
              <Badge variant="secondary">{t("From Everyone")}</Badge>
            ) : resolved.origin === "pattern" ? (
              <Badge variant="outline" className="font-mono font-normal">
                {t("via {pattern}", { pattern: resolved.via ?? "" })}
              </Badge>
            ) : resolved.origin === "default" ? (
              <span className="text-[11px] text-fg-subtle">{t("Default")}</span>
            ) : null}
          </div>
          <p className="mt-0.5 line-clamp-1 text-xs text-fg-muted">
            {pattern
              ? tn(matches, "Applies to {count} row", "Applies to {count} rows")
              : entry.description}
          </p>
          {chips.length ? (
            <div className="mt-1 flex flex-wrap gap-1">
              {chips.map((c) => (
                <Badge key={c} variant="outline" className="font-normal">
                  {c}
                </Badge>
              ))}
            </div>
          ) : null}
        </div>
        {!onCreate && (
          <Button
            variant="ghost"
            size="icon"
            className="size-7 shrink-0"
            aria-expanded={open}
            aria-label={t("Constraints for {title}", { title: entry.title })}
            onClick={onToggle}
          >
            {open ? <ChevronDown size={15} /> : <SlidersHorizontal size={14} />}
          </Button>
        )}
        {pattern && own ? (
          <Button
            variant="ghost"
            size="icon"
            className="size-7 shrink-0"
            aria-label={t("Remove the rule {title}", { title: entry.title })}
            onClick={reset}
          >
            <Trash2 size={14} />
          </Button>
        ) : null}
      </div>
      {onCreate ? (
        <Button type="button" variant="outline" size="sm" onClick={onCreate}>
          {t("Create collection")}
        </Button>
      ) : (
        <div className="mt-2 flex flex-wrap items-center justify-between gap-2">
          <Segmented
            ariaLabel={t("Status of {title}", { title: entry.title })}
            size="sm"
            value={effective.status}
            onChange={setStatus}
            options={statuses.map((s) => ({
              value: s,
              label: <span title={STATUS_HINT[s]}>{STATUS_LABEL[s]}</span>,
            }))}
          />
          <Engagement insight={insight} />
        </div>
      )}
      {layer === "global" && own ? (
        <p className="mt-1.5 text-[11px] text-fg-muted">
          {tn(
            viewers.length - overriding.length,
            "Affects {count} viewer",
            "Affects {count} viewers",
          )}
          {overriding.length
            ? ` · ${tn(overriding.length, "{count} viewer overrides it", "{count} viewers override it")}`
            : ""}
        </p>
      ) : null}
      <Drawer
        open={open}
        onClose={onToggle}
        title={entry.title}
        description={t(
          "Adjust this row in the current Home draft. Save the Home layout to apply it.",
        )}
        size="lg"
      >
        {" "}
        <div className="mt-2 flex flex-wrap items-center gap-2">
          <Segmented
            ariaLabel={t("Status of {title}", { title: entry.title })}
            size="sm"
            value={effective.status}
            onChange={setStatus}
            options={statuses.map((s) => ({
              value: s,
              label: <span title={STATUS_HINT[s]}>{STATUS_LABEL[s]}</span>,
            }))}
          />
          {own ? (
            <Button
              variant="ghost"
              size="sm"
              className="h-7 px-2 text-xs"
              onClick={reset}
              aria-label={
                pattern
                  ? t("Remove the rule {title}", { title: entry.title })
                  : layer === "global"
                    ? t("Back to default: {title}", { title: entry.title })
                    : t("Reset to Everyone: {title}", { title: entry.title })
              }
            >
              {pattern ? <Trash2 size={12} /> : <RotateCcw size={12} />} {resetLabel}
            </Button>
          ) : null}
        </div>
        {resolved.origin === "pattern" ? (
          <p className="mt-2 text-xs text-fg-muted">
            {t("Set by the rule {pattern}. Changing it here gives this row its own setting.", {
              pattern: resolved.via ?? "",
            })}
          </p>
        ) : null}
        <PolicyEditor policy={effective} title={entry.title} onChange={edit} schedule={schedule} />
      </Drawer>
    </li>
  );
}
