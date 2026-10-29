import { Drawer } from "../../components/Drawer";
import { Badge, Button, Input, cn } from "@braedonsaunders/appkit-ui";
import { ChevronDown, Plus, RotateCcw, Search, SlidersHorizontal } from "lucide-react";
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
  Viewer,
} from "../../lib/contract";
import { api } from "../../lib/api";
import { localized, t, tn } from "../../i18n";
import { WEEKDAYS, formatNumber } from "../../lib/format";
import {
  FRONT_END_ROWS,
  STATUS_HINT,
  STATUS_LABEL,
  pageOf,
  policyChips,
  policyIn,
  removePolicy,
  rowFits,
  setPolicy,
  viewersOverriding,
  type LayerKey,
} from "../../lib/layout";
import { PolicyEditor } from "./PolicyEditor";
import { useLocale } from "../../i18n/react";

interface PoolEntry {
  row: string;
  title: string;
  description: string;
  group: RowGroup | "custom";
  kind?: string;
}

const GROUPS = localized<{ key: RowGroup | "custom"; label: string; hint: string }[]>(() => [
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
  const [q, setQ] = useState("");
  const [custom, setCustom] = useState("");
  const [patternOpen, setPatternOpen] = useState(false);
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
  const entries = useMemo<PoolEntry[]>(() => {
    const out: PoolEntry[] = available
      .filter((a) => !FRONT_END_ROWS.has(a.kind) && rowFits(a, section))
      .map((a) => ({
        row: a.hubId,
        title: a.title,
        description: a.description,
        group: a.group ?? "picks",
        kind: a.kind,
      }));
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
  }, [available, draft, layer, section, locale]);
  const needle = q.trim().toLowerCase();
  const shown = needle
    ? entries.filter(
        (e) => e.title.toLowerCase().includes(needle) || e.row.toLowerCase().includes(needle),
      )
    : entries;
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
      {GROUPS.map((g) => {
        const rows = shown.filter((e) => e.group === g.key);
        if (!rows.length) return null;
        return (
          <div key={g.key} className="space-y-1.5 pt-1" role="group" aria-label={g.label}>
            <div className="flex items-baseline gap-2 px-0.5">
              <h3 className="text-[11px] font-semibold uppercase tracking-wider text-fg-subtle">
                {g.label}
              </h3>
              <span className="truncate text-[11px] text-fg-subtle/80">{g.hint}</span>
            </div>
            <ul className="space-y-1.5">
              {rows.map((e) => (
                <PoolItem
                  key={e.row}
                  entry={e}
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
          </div>
        );
      })}
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
  const own = policyIn(draft, layer, section, entry.row);
  const global = layer === "global" ? undefined : policyIn(draft, "global", section, entry.row);
  const effective: RowPolicy = own ?? global ?? { row: entry.row, status: "eligible" };
  const source: "override" | "global" | "default" = own
    ? layer === "global"
      ? "global"
      : "override"
    : global
      ? "global"
      : "default";
  const edit = (p: RowPolicy) => onChange(setPolicy(draft, layer, section, p));
  const reset = () => onChange(removePolicy(draft, layer, section, entry.row));
  const overriding =
    layer === "global"
      ? viewersOverriding(draft, section, entry.row).filter((id) =>
          viewers.some((v) => v.id === id),
        )
      : [];
  const chips = policyChips(effective, WEEKDAYS);
  const statuses: RowStatus[] = ["always", "eligible", "off"];
  return (
    <li
      ref={ref}
      aria-label={t("Row {title}", { title: entry.title })}
      className={cn(
        "rounded-lg border bg-surface p-2.5",
        source === "override" ? "border-info/40" : "border-border",
        open && "ring-2 ring-primary/30",
      )}
    >
      <div className="flex items-start gap-2">
        <div className="min-w-0 flex-1">
          <div className="flex flex-wrap items-center gap-1.5">
            <span className="truncate text-sm font-medium text-fg" title={entry.description}>
              {entry.title}
            </span>
            {source === "override" ? (
              <Badge variant="info">{t("Override")}</Badge>
            ) : source === "global" && layer !== "global" ? (
              <Badge variant="secondary">{t("From Everyone")}</Badge>
            ) : source === "default" ? (
              <span className="text-[11px] text-fg-subtle">{t("Default")}</span>
            ) : null}
          </div>
          <p className="mt-0.5 line-clamp-2 text-xs text-fg-muted">{entry.description}</p>
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
      </div>
      <div className="mt-2 flex items-center justify-between gap-2">
        <p className="text-xs text-fg-muted">{STATUS_LABEL[effective.status]}</p>
        <Engagement insight={insight} />
      </div>
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
            onChange={(s) =>
              edit({ ...effective, status: s, slot: s === "off" ? undefined : effective.slot })
            }
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
                layer === "global"
                  ? t("Back to default: {title}", { title: entry.title })
                  : t("Reset to Everyone: {title}", { title: entry.title })
              }
            >
              <RotateCcw size={12} /> {layer === "global" ? t("Default") : t("Reset to Everyone")}
            </Button>
          ) : null}
        </div>
        <PolicyEditor policy={effective} title={entry.title} onChange={edit} />
      </Drawer>
    </li>
  );
}
