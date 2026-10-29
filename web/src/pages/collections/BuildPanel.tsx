import { Button, SearchSelect, Textarea, type ContextMenuEntry } from "@braedonsaunders/appkit-ui";
import {
  ArrowDownWideNarrow,
  ArrowUpNarrowWide,
  Filter,
  FilterX,
  ListPlus,
  Plus,
} from "lucide-react";
import { ActionMenu } from "../../components/ActionMenu";
import { SectionHeading } from "../../components/feedback";
import { ChipGroup, Field, NumberInput, Segmented } from "../../components/fields";
import type { CollectionSpec, Rule, RuleField, SourceKind } from "../../lib/contract";
import { t, tx } from "../../i18n";
import { FIELDS, SORTS, SOURCE_KINDS, newRule, newSource } from "../../lib/rules";
import { RuleRow } from "./RuleRow";
import { SourceEditor } from "./SourceEditor";

export type SpecUpdate = (patch: Partial<CollectionSpec>, coalesceKey?: string) => void;

function fieldMenu(onPick: (f: RuleField) => void): ContextMenuEntry[] {
  const out: ContextMenuEntry[] = [];
  let group = "";
  for (const f of FIELDS) {
    if (f.group !== group) {
      if (group) out.push({ key: `sep-${f.group}`, separator: true });
      group = f.group;
    }
    out.push({ key: f.field, label: f.label, onSelect: () => onPick(f.field) });
  }
  return out;
}

export function BuildPanel({
  spec,
  update,
  region,
}: {
  spec: CollectionSpec;
  update: SpecUpdate;
  region: string;
}) {
  const setRules = (key: "rules" | "exclude", rules: Rule[]) => update({ [key]: rules });
  // The whole catalogue already contains every other source, so adding a
  // specific source replaces a lone "Whole catalogue".
  const addSource = (kind: SourceKind) => {
    const keep =
      spec.sources.length === 1 && spec.sources[0]?.kind === "catalog" && kind !== "catalog"
        ? []
        : spec.sources;
    update({ sources: [...keep, newSource(kind, spec.mediaType, region)] });
  };
  const sourceItems: ContextMenuEntry[] = SOURCE_KINDS.filter(
    (k) => k.kind !== "catalog" || !spec.sources.some((s) => s.kind === "catalog"),
  ).map((k) => ({ key: k.kind, label: k.label, onSelect: () => addSource(k.kind) }));
  const limitPresets = [25, 50, 100, 0];

  return (
    <div className="space-y-7">
      <section className="space-y-4">
        <Field
          label={t("Description")}
          hint={t("Shown in Plex under the collection title.")}
          htmlFor="collection-description"
        >
          <Textarea
            id="collection-description"
            rows={2}
            value={spec.description ?? ""}
            placeholder={t("What makes this collection special?")}
            onChange={(e) => update({ description: e.currentTarget.value }, "description")}
          />
        </Field>
        <Field label={t("Contains")}>
          <Segmented
            ariaLabel={t("Media type")}
            value={spec.mediaType}
            onChange={(m) => update({ mediaType: m })}
            options={[
              { value: "movie", label: t("Movies") },
              { value: "show", label: t("TV shows") },
              { value: "mixed", label: t("Both") },
            ]}
          />
        </Field>
        <Field
          label={t("Release dates")}
          hint={t(
            "Upcoming shows are new series by first air date; future episodes of existing shows are not included. Dates can change.",
          )}
        >
          <Segmented
            ariaLabel={t("Release dates")}
            value={spec.releaseMode ?? "released"}
            onChange={(releaseMode) =>
              update({ releaseMode, lookaheadDays: spec.lookaheadDays || 90 })
            }
            options={[
              { value: "released", label: t("Released") },
              { value: "upcoming", label: t("Upcoming") },
              { value: "mixed", label: t("Both") },
            ]}
          />
        </Field>
        {spec.releaseMode && spec.releaseMode !== "released" && (
          <Field
            label={t("Look ahead (days)")}
            htmlFor="lookahead-days"
            hint={t(
              "1–366 days. Upcoming titles use the whole catalogue source and TMDB discovery; playback is unavailable before the scheduled release date.",
            )}
          >
            <NumberInput
              id="lookahead-days"
              value={spec.lookaheadDays ?? 90}
              min={1}
              max={366}
              onChange={(lookaheadDays) => update({ lookaheadDays }, "lookaheadDays")}
            />
          </Field>
        )}
      </section>

      <section aria-labelledby="sources-h">
        <label className="mb-4 flex gap-3 rounded-lg border border-border p-4 text-sm">
          <input
            type="checkbox"
            checked={Boolean(spec.localOnly)}
            onChange={(e) =>
              update({
                localOnly: e.target.checked,
                ...(e.target.checked ? { sources: [{ kind: "catalog" as const }] } : {}),
              })
            }
          />
          <span>
            <strong>{t("Local catalogue only")}</strong>
            <span className="mt-1 block text-fg-muted">
              {t(
                "Live matching without remote lookups. Ideal for genre, country and network collections.",
              )}
            </span>
          </span>
        </label>
        <SectionHeading
          id="sources-h"
          title={t("Sources")}
          description={t("Where candidate titles come from. Several sources are combined.")}
          actions={
            <ActionMenu items={sourceItems} variant="outline" size="sm" label={t("Add source")}>
              <ListPlus size={14} /> {t("Add source")}
            </ActionMenu>
          }
        />
        <div className="space-y-2">
          {spec.sources.map((s, i) => (
            <SourceEditor
              key={i}
              index={i}
              source={s}
              region={region}
              removable={spec.sources.length > 1}
              onChange={(next) =>
                update({ sources: spec.sources.map((x, j) => (j === i ? next : x)) }, `source-${i}`)
              }
              onRemove={() => update({ sources: spec.sources.filter((_, j) => j !== i) })}
            />
          ))}
        </div>
      </section>

      <section aria-labelledby="rules-h">
        <SectionHeading
          id="rules-h"
          title={t("Rules")}
          description={t("Keep only titles that match.")}
          actions={
            <ActionMenu
              items={fieldMenu((f) => setRules("rules", [...spec.rules, newRule(f)]))}
              variant="outline"
              size="sm"
              label={t("Add rule")}
            >
              <Filter size={14} /> {t("Add rule")}
            </ActionMenu>
          }
        />
        {spec.rules.length > 1 ? (
          <div className="mb-3 flex flex-wrap items-center gap-2 text-sm text-fg-muted">
            {tx("Include titles that match {mode} of these rules", {
              mode: (
                <Segmented
                  ariaLabel={t("Match mode")}
                  size="sm"
                  value={spec.match}
                  onChange={(m) => update({ match: m })}
                  options={[
                    { value: "all", label: t("all") },
                    { value: "any", label: t("any") },
                  ]}
                />
              ),
            })}
          </div>
        ) : null}
        {spec.rules.length === 0 ? (
          <EmptyHint
            text={t("No rules: every title from the sources is included.")}
            action={
              <Button
                type="button"
                variant="ghost"
                size="sm"
                onClick={() => setRules("rules", [...spec.rules, newRule("genre")])}
              >
                <Plus size={14} /> {t("Add a genre rule")}
              </Button>
            }
          />
        ) : (
          <div className="space-y-2">
            {spec.rules.map((r, i) => (
              <RuleRow
                key={i}
                index={i}
                kind="include"
                rule={r}
                mediaType={spec.mediaType}
                region={region}
                onChange={(next) =>
                  setRules(
                    "rules",
                    spec.rules.map((x, j) => (j === i ? next : x)),
                  )
                }
                onRemove={() =>
                  setRules(
                    "rules",
                    spec.rules.filter((_, j) => j !== i),
                  )
                }
              />
            ))}
          </div>
        )}
      </section>

      <section aria-labelledby="exclude-h">
        <SectionHeading
          id="exclude-h"
          title={t("Exclude")}
          description={t("Drop titles matching any of these, whatever the rules say.")}
          actions={
            <ActionMenu
              items={fieldMenu((f) => setRules("exclude", [...spec.exclude, newRule(f)]))}
              variant="outline"
              size="sm"
              label={t("Add exclusion")}
            >
              <FilterX size={14} /> {t("Add exclusion")}
            </ActionMenu>
          }
        />
        {spec.exclude.length === 0 ? (
          <EmptyHint text={t("Nothing excluded.")} />
        ) : (
          <div className="space-y-2">
            {spec.exclude.map((r, i) => (
              <RuleRow
                key={i}
                index={i}
                kind="exclude"
                rule={r}
                mediaType={spec.mediaType}
                region={region}
                onChange={(next) =>
                  setRules(
                    "exclude",
                    spec.exclude.map((x, j) => (j === i ? next : x)),
                  )
                }
                onRemove={() =>
                  setRules(
                    "exclude",
                    spec.exclude.filter((_, j) => j !== i),
                  )
                }
              />
            ))}
          </div>
        )}
      </section>

      <section aria-labelledby="sort-h" className="space-y-4">
        <SectionHeading id="sort-h" title={t("Order and size")} />
        <div className="flex flex-wrap items-end gap-3">
          <Field label={t("Sort by")} className="min-w-52 flex-1">
            <SearchSelect
              value={spec.sort}
              onChange={(v) => v && update({ sort: v as CollectionSpec["sort"] })}
              options={SORTS.map((s) => ({ value: s.value, label: s.label, hint: s.hint }))}
              ariaLabel={t("Sort by")}
              searchable={false}
              placeholder={t("Select…")}
              selectLabel={t("Select")}
              closeLabel={t("Close")}
            />
          </Field>
          {spec.sort !== "random" && spec.sort !== "source" && spec.sort !== "personal" ? (
            <Segmented
              ariaLabel={t("Sort direction")}
              value={spec.sortDesc ? "desc" : "asc"}
              onChange={(d) => update({ sortDesc: d === "desc" })}
              options={[
                {
                  value: "desc",
                  label: spec.sort === "title" ? t("Z–A") : t("Highest"),
                  icon: <ArrowDownWideNarrow size={14} />,
                },
                {
                  value: "asc",
                  label: spec.sort === "title" ? t("A–Z") : t("Lowest"),
                  icon: <ArrowUpNarrowWide size={14} />,
                },
              ]}
            />
          ) : null}
        </div>
        <Field
          label={t("Limit")}
          hint={t("The most titles the collection holds. 0 keeps every match.")}
        >
          <div className="flex flex-wrap items-center gap-3">
            <NumberInput
              aria-label={t("Limit")}
              value={spec.limit}
              min={0}
              step={5}
              className="w-28"
              onChange={(n) => update({ limit: Math.max(0, Math.round(n)) }, "limit")}
            />
            <ChipGroup
              ariaLabel={t("Limit presets")}
              size="sm"
              value={[spec.limit]}
              onChange={(v) => {
                const last = v[v.length - 1];
                if (last !== undefined) update({ limit: last });
              }}
              options={limitPresets.map((n) => ({
                value: n,
                label: n === 0 ? t("No limit") : String(n),
              }))}
            />
          </div>
        </Field>
      </section>
    </div>
  );
}

function EmptyHint({ text, action }: { text: string; action?: React.ReactNode }) {
  return (
    <div className="flex flex-wrap items-center justify-between gap-2 rounded-lg border border-dashed border-border px-3 py-2.5 text-sm text-fg-muted">
      {text}
      {action}
    </div>
  );
}
