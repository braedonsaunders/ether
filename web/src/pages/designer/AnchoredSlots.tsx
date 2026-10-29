import { Badge, Button, SearchSelect } from "@braedonsaunders/appkit-ui";
import { Anchor, Plus, X } from "lucide-react";
import { useState } from "react";
import { DragHandle, SortableList } from "../../components/Sortable";
import { t } from "../../i18n";
import type {
  AvailableRow,
  HomeLayout,
  LayoutSection,
  RowPolicy,
  Viewer,
} from "../../lib/contract";
import {
  FRONT_END_ROWS,
  anchorsOf,
  effectivePolicy,
  freeSlot,
  policyIn,
  reorderAnchors,
  setPolicy,
  type LayerKey,
} from "../../lib/layout";

export function rowTitle(available: AvailableRow[], row: string): string {
  return available.find((a) => a.hubId === row || a.kind === row)?.title ?? row;
}

/**
 * The page structure: rows anchored to fixed slots in this layer, in slot
 * order. Drag to swap their slots; the slots in between are personalized.
 */
export function AnchoredSlots({
  draft,
  onChange,
  layer,
  section,
  available,
}: {
  draft: HomeLayout;
  onChange: (d: HomeLayout) => void;
  layer: LayerKey;
  section: LayoutSection;
  viewers: Viewer[];
  available: AvailableRow[];
}) {
  const [adding, setAdding] = useState("");
  const own = anchorsOf(draft, layer, section);
  // Global anchors a viewer inherits (unless they override that row).
  const inherited =
    layer === "global"
      ? []
      : anchorsOf(draft, "global", section).filter((g) => {
          const eff = effectivePolicy(draft, layer, section, g.row);
          return eff?.layer === "global";
        });
  const unanchor = (p: RowPolicy) =>
    onChange(setPolicy(draft, layer, section, { ...p, slot: undefined, status: "always" }));
  const anchor = (row: string) => {
    const base = policyIn(draft, layer, section, row) ??
      effectivePolicy(draft, layer === "global" ? null : layer, section, row)?.policy ?? {
        row,
        status: "always" as const,
      };
    onChange(
      setPolicy(draft, layer, section, {
        ...base,
        row,
        status: "always",
        slot: freeSlot(draft, layer, section),
        minSlot: undefined,
        maxSlot: undefined,
      }),
    );
  };
  const options = available
    .filter(
      (a) => !FRONT_END_ROWS.has(a.kind) && (section === "home" || a.kind !== "continue-watching"),
    )
    .filter((a) => !own.some((p) => p.row === a.hubId))
    .map((a) => ({ value: a.hubId, label: a.title, hint: a.description }));
  return (
    <section className="space-y-2" aria-label={t("Anchored slots")}>
      <div>
        <h2 className="text-xs font-semibold uppercase tracking-wider text-fg-subtle">
          {t("Page structure")}
        </h2>
        <p className="mt-0.5 text-xs text-fg-muted">
          {layer === "global"
            ? t(
                "Anchored rows hold their slot for everyone; personalization fills the slots between them. Drag to swap.",
              )
            : t(
                "Anchored rows hold their slot for this viewer; personalization fills the slots between them. Drag to swap.",
              )}
        </p>
      </div>
      {own.length === 0 && inherited.length === 0 ? (
        <p className="rounded-lg border border-dashed border-border px-3 py-2.5 text-xs text-fg-muted">
          {t("No anchored slots: every position is personalized.")}
        </p>
      ) : null}
      {own.length ? (
        <SortableList
          items={own}
          getId={(p) => p.row}
          onReorder={(next) =>
            onChange(
              reorderAnchors(
                draft,
                layer,
                section,
                next.map((p) => p.row),
              ),
            )
          }
          className="space-y-1"
          renderItem={(p, handle) => (
            <div className="flex items-center gap-2 rounded-lg border border-border bg-surface py-1 pl-1 pr-1.5">
              <DragHandle
                handle={handle}
                label={t("Move anchored row {row}", { row: rowTitle(available, p.row) })}
              />
              <span
                className="grid size-6 shrink-0 place-items-center rounded-md bg-primary-subtle text-xs font-semibold tabular-nums text-primary"
                aria-label={t("Slot {slot}", { slot: p.slot ?? "" })}
              >
                {p.slot}
              </span>
              <span className="min-w-0 flex-1 truncate text-sm text-fg">
                {rowTitle(available, p.row)}
              </span>
              {layer !== "global" ? <Badge variant="info">{t("Override")}</Badge> : null}
              <Button
                variant="ghost"
                size="icon"
                className="size-7 text-fg-muted"
                aria-label={t("Unanchor {row}", { row: rowTitle(available, p.row) })}
                onClick={() => unanchor(p)}
              >
                <X size={14} />
              </Button>
            </div>
          )}
        />
      ) : null}
      {inherited.length ? (
        <ul className="space-y-1" aria-label={t("Anchors from everyone's Home")}>
          {inherited.map((p) => (
            <li
              key={p.row}
              className="flex items-center gap-2 rounded-lg border border-dashed border-border px-2 py-1.5 text-fg-muted"
            >
              <span className="grid size-6 shrink-0 place-items-center rounded-md bg-bg-subtle text-xs font-semibold tabular-nums">
                {p.slot}
              </span>
              <span className="min-w-0 flex-1 truncate text-sm">{rowTitle(available, p.row)}</span>
              <Badge variant="secondary">{t("Everyone")}</Badge>
            </li>
          ))}
        </ul>
      ) : null}
      <div className="flex gap-2">
        <div className="min-w-0 flex-1">
          <SearchSelect
            value={adding}
            onChange={setAdding}
            options={options}
            ariaLabel={t("Row to anchor")}
            placeholder={t("Anchor a row…")}
            searchPlaceholder={t("Search…")}
            noMatchesLabel={t("No matches")}
            selectLabel={t("Select")}
            closeLabel={t("Close")}
          />
        </div>
        <Button
          variant="outline"
          size="sm"
          className="h-10"
          disabled={!adding}
          onClick={() => {
            anchor(adding);
            setAdding("");
          }}
        >
          <Plus size={14} />
          <Anchor size={13} /> {t("Anchor")}
        </Button>
      </div>
    </section>
  );
}
