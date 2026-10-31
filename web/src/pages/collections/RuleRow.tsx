import { Button, Input, SearchSelect, Slider, cn } from "@braedonsaunders/appkit-ui";
import { AlertCircle, Trash2 } from "lucide-react";
import { useEffect, useRef, type ReactNode } from "react";
import { Segmented, NumberInput } from "../../components/fields";
import { FacetPicker, FacetSelect, LookupPicker, type Picked } from "../../components/pickers";
import { localized, t } from "../../i18n";
import { NOT_RATED } from "../../lib/contentRatings";
import { searchSelectLabels } from "../../i18n/appkit-labels";
import type { Rule, RuleField, RuleOp } from "../../lib/contract";
import {
  FIELDS,
  WITHIN_UNITS,
  describeRule,
  fieldMeta,
  isRuleComplete,
  opLabel,
  parseWithin,
  valueFor,
  withField,
  withOp,
  type FieldMeta,
} from "../../lib/rules";

const FIELD_OPTIONS = localized(() =>
  FIELDS.map((f) => ({
    value: f.field,
    label: f.label,
    hint: f.hint,
    group: f.group,
  })),
);

/** One condition: field, operator and a value editor that fits the field. */
export function RuleRow({
  rule,
  onChange,
  onRemove,
  mediaType,
  region,
  index,
  kind,
  allowedFields,
  metaFor,
  inline,
  openValue,
}: {
  allowedFields?: RuleField[];
  /** Visibility rules add operators (is not set) and facet-backed studio/keyword. */
  metaFor?: (field: RuleField) => FieldMeta | undefined;
  /** Field, operator and value on one line, read as a sentence. */
  inline?: boolean;
  /** Open the value picker once, when this condition was just added. */
  openValue?: boolean;
  rule: Rule;
  onChange: (r: Rule) => void;
  onRemove: () => void;
  mediaType: string;
  region: string;
  index: number;
  kind: "include" | "exclude";
}) {
  const meta = metaFor?.(rule.field) ?? fieldMeta(rule.field);
  const complete = isRuleComplete(rule);
  const n = index + 1;
  const label = kind === "exclude" ? t("Exclusion {n}", { n }) : t("Rule {n}", { n });
  return (
    <div
      className={cn(
        "rounded-lg border bg-surface p-3",
        complete ? "border-border" : "border-warning/40",
      )}
      role="group"
      aria-label={label}
    >
      <div className={cn("flex flex-wrap items-center gap-2", inline && "items-center")}>
        <div className={inline ? "w-36 shrink-0" : "w-full sm:w-48"}>
          <SearchSelect
            value={rule.field}
            onChange={(v) => {
              if (!v) return;
              const field = v as RuleField;
              const custom = metaFor?.(field);
              if (!custom) onChange(withField(rule, field));
              else
                onChange({
                  field,
                  op: custom.defaultOp,
                  value: valueFor(custom, custom.defaultOp),
                });
            }}
            options={
              allowedFields
                ? FIELD_OPTIONS.filter((f) => allowedFields.includes(f.value))
                : FIELD_OPTIONS
            }
            ariaLabel={t("{label} field", { label })}
            searchable
            {...searchSelectLabels()}
          />
        </div>
        {meta ? (
          <div className={inline ? "w-32 shrink-0" : "w-full sm:w-40"}>
            <SearchSelect
              value={rule.op}
              onChange={(v) => v && onChange(withOp(rule, v as RuleOp))}
              options={meta.ops.map((op) => ({ value: op, label: opLabel(op, meta.kind) }))}
              ariaLabel={t("{label} condition", { label })}
              searchable={false}
              {...searchSelectLabels()}
            />
          </div>
        ) : null}
        {inline && meta ? (
          <OpenOnMount active={openValue} className="min-w-40 flex-1">
            <ValueEditor
              rule={rule}
              meta={meta}
              onChange={onChange}
              mediaType={mediaType}
              region={region}
              label={label}
            />
          </OpenOnMount>
        ) : null}
        <Button
          type="button"
          variant="ghost"
          size="icon"
          className="ml-auto size-9 shrink-0 text-fg-muted hover:text-danger"
          onClick={onRemove}
          aria-label={
            kind === "exclude" ? t("Remove exclusion {n}", { n }) : t("Remove rule {n}", { n })
          }
        >
          <Trash2 size={15} />
        </Button>
      </div>
      {inline ? null : (
      <div className="mt-2">
        {meta ? (
          <ValueEditor
            rule={rule}
            meta={meta}
            onChange={onChange}
            mediaType={mediaType}
            region={region}
            label={label}
          />
        ) : (
          <RawValue rule={rule} onChange={onChange} label={label} />
        )}
      </div>
      )}
      {inline && complete ? null : (
      <p
        className={cn(
          "mt-2 flex items-center gap-1.5 text-xs",
          complete ? "text-fg-muted" : "text-warning",
        )}
      >
        {complete ? null : <AlertCircle size={12} aria-hidden />}
        {complete
          ? describeRule(rule)
          : t("Pick a value to apply this rule. It's left out of the preview until then.")}
      </p>
      )}
    </div>
  );
}

function OpenOnMount({
  active,
  className,
  children,
}: {
  active?: boolean;
  className?: string;
  children: ReactNode;
}) {
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (!active) return;
    // Delay past StrictMode's setup/cleanup pair so the picker is opened once.
    const id = window.setTimeout(() => {
      const target = ref.current?.querySelector<HTMLElement>("button, [role='combobox'], input");
      if (!target || target.getAttribute("aria-expanded") === "true") return;
      target.click();
      if (target instanceof HTMLInputElement) target.focus();
    }, 0);
    return () => window.clearTimeout(id);
  }, [active]);
  return (
    <div ref={ref} className={className}>
      {children}
    </div>
  );
}

function toPicked(rule: Rule): Picked[] {
  const ids = (
    Array.isArray(rule.value)
      ? rule.value
      : rule.value === "" || rule.value == null
        ? []
        : [rule.value]
  )
    .map(Number)
    .filter((n) => Number.isFinite(n) && n > 0);
  const names = rule.label ? rule.label.split(", ") : [];
  return ids.map((id, i) => ({
    id,
    name: names.length === ids.length ? (names[i] ?? `#${id}`) : `#${id}`,
  }));
}

function ValueEditor({
  rule,
  meta,
  onChange,
  mediaType,
  region,
  label,
}: {
  rule: Rule;
  meta: FieldMeta;
  onChange: (r: Rule) => void;
  mediaType: string;
  region: string;
  label: string;
}) {
  if (rule.op === "unknown" || rule.op === "known") return null;
  const multi = rule.op === "in" || rule.op === "notIn";
  const facetType = mediaType === "show" ? "show" : mediaType === "movie" ? "movie" : "";
  switch (meta.kind) {
    case "facet":
      return multi ? (
        <FacetPicker
          type={facetType}
          facet={meta.facet!}
          value={(Array.isArray(rule.value) ? rule.value : []).map(String)}
          onChange={(v) => onChange({ ...rule, value: v })}
          ariaLabel={t("{label} values", { label })}
        />
      ) : (
        <FacetSelect
          type={facetType}
          facet={meta.facet!}
          value={String(rule.value ?? "")}
          onChange={(v) => onChange({ ...rule, value: v })}
          ariaLabel={t("{label} value", { label })}
          omit={rule.op === "gte" || rule.op === "lte" ? [NOT_RATED] : undefined}
        />
      );
    case "lookup":
      return (
        <LookupPicker
          kind={meta.lookup!}
          region={region}
          multiple={multi}
          value={toPicked(rule)}
          ariaLabel={t("{label} search", { label })}
          onChange={(picked) =>
            onChange({
              ...rule,
              value: multi ? picked.map((p) => p.id) : (picked[0]?.id ?? ""),
              label: picked.map((p) => p.name).join(", ") || undefined,
            })
          }
        />
      );
    case "bool":
      return (
        <Segmented
          ariaLabel={t("{label} value", { label })}
          size="sm"
          value={rule.value === false ? "no" : "yes"}
          onChange={(v) => onChange({ ...rule, value: v === "yes" })}
          options={[
            { value: "yes", label: t("Yes") },
            { value: "no", label: t("No") },
          ]}
        />
      );
    case "number": {
      if (rule.op === "between") {
        const [lo, hi] = Array.isArray(rule.value) ? rule.value : [0, 0];
        return (
          <div className="flex items-center gap-2">
            <NumberInput
              aria-label={t("{label} minimum", { label })}
              value={Number(lo)}
              min={meta.min}
              max={meta.max}
              step={meta.step}
              onChange={(n) => onChange({ ...rule, value: [n, Number(hi)] })}
            />
            <span className="text-sm text-fg-muted">{t("and")}</span>
            <NumberInput
              aria-label={t("{label} maximum", { label })}
              value={Number(hi)}
              min={meta.min}
              max={meta.max}
              step={meta.step}
              onChange={(n) => onChange({ ...rule, value: [Number(lo), n] })}
            />
            {meta.unit ? <span className="shrink-0 text-sm text-fg-muted">{meta.unit}</span> : null}
          </div>
        );
      }
      return (
        <div className="flex items-center gap-2">
          <NumberInput
            aria-label={t("{label} value", { label })}
            value={Number(rule.value)}
            min={meta.min}
            max={meta.max}
            step={meta.step}
            onChange={(n) => onChange({ ...rule, value: n })}
            className="max-w-40"
          />
          {meta.unit ? <span className="text-sm text-fg-muted">{meta.unit}</span> : null}
          {meta.field === "rating" ? (
            <Slider
              min={0}
              max={10}
              step={0.1}
              value={Number(rule.value)}
              aria-label={t("{label} slider", { label })}
              onChange={(e) => onChange({ ...rule, value: e.currentTarget.valueAsNumber })}
              className="ml-2 flex-1"
            />
          ) : null}
        </div>
      );
    }
    case "date": {
      if (rule.op === "within") {
        const w = parseWithin(rule.value);
        return (
          <div className="flex items-center gap-2">
            <NumberInput
              aria-label={t("{label} amount", { label })}
              value={w.n}
              min={1}
              step={1}
              className="max-w-24"
              onChange={(n) =>
                onChange({ ...rule, value: `${Math.max(1, Math.round(n))}${w.unit}` })
              }
            />
            <div className="w-32">
              <SearchSelect
                value={w.unit}
                onChange={(u) => u && onChange({ ...rule, value: `${w.n}${u}` })}
                options={WITHIN_UNITS.map((u) => ({ value: u.value, label: u.label }))}
                ariaLabel={t("{label} unit", { label })}
                searchable={false}
                {...searchSelectLabels()}
              />
            </div>
          </div>
        );
      }
      if (rule.op === "between") {
        const [a, b] = Array.isArray(rule.value) ? rule.value.map(String) : ["", ""];
        return (
          <div className="flex flex-wrap items-center gap-2">
            <Input
              type="date"
              aria-label={t("{label} from", { label })}
              value={a}
              onChange={(e) => onChange({ ...rule, value: [e.currentTarget.value, b] })}
              className="w-44"
            />
            <span className="text-sm text-fg-muted">{t("and")}</span>
            <Input
              type="date"
              aria-label={t("{label} to", { label })}
              value={b}
              onChange={(e) => onChange({ ...rule, value: [a, e.currentTarget.value] })}
              className="w-44"
            />
          </div>
        );
      }
      return (
        <Input
          type="date"
          aria-label={t("{label} date", { label })}
          value={String(rule.value ?? "")}
          onChange={(e) => onChange({ ...rule, value: e.currentTarget.value })}
          className="w-44"
        />
      );
    }
  }
}

/** Fallback for a field this UI doesn't know (e.g. a newer server template). */
function RawValue({
  rule,
  onChange,
  label,
}: {
  rule: Rule;
  onChange: (r: Rule) => void;
  label: string;
}) {
  const text = JSON.stringify(rule.value);
  return (
    <Input
      key={text}
      defaultValue={text}
      aria-label={t("{label} raw value", { label })}
      className="font-mono text-xs"
      onBlur={(e) => {
        try {
          onChange({ ...rule, value: JSON.parse(e.currentTarget.value) });
        } catch {
          /* keep previous */
        }
      }}
    />
  );
}
