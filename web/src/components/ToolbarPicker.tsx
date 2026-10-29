import { Input, Popover, cn } from "@braedonsaunders/appkit-ui";
import { Check, ChevronDown, Search } from "lucide-react";
import { useMemo, useState, type ReactNode } from "react";
import { t } from "../i18n";

export interface ToolbarOption<T extends string> {
  value: T;
  label: string;
  /** Second line in the menu (never in the trigger). */
  hint?: string;
  icon?: ReactNode;
  /** Right-aligned adornment in the menu, e.g. an override count. */
  meta?: ReactNode;
  /** Section header; consecutive options sharing it are grouped. */
  group?: string;
}

/**
 * A compact toolbar dropdown: one chip-sized trigger, a popover menu that
 * becomes searchable once it is long. Lets a whole row of context (who,
 * which page, which device, when) sit on one line and still scale to a long
 * list of choices.
 */
export function ToolbarPicker<T extends string>({
  value,
  options,
  onChange,
  ariaLabel,
  trigger,
  iconOnly,
  searchPlaceholder = t("Search…"),
  searchable,
  className,
  menuClassName,
  footer,
  side,
}: {
  value: T;
  options: ToolbarOption<T>[];
  onChange: (v: T) => void;
  ariaLabel: string;
  /** Override the trigger's content (defaults to the chosen option). */
  trigger?: ReactNode;
  /** Show only the chosen option's icon in the trigger. */
  iconOnly?: boolean;
  searchPlaceholder?: string;
  searchable?: boolean;
  className?: string;
  menuClassName?: string;
  footer?: ReactNode;
  /** Open the menu above the trigger, e.g. in a sidebar footer. */
  side?: "top" | "bottom";
}) {
  const [open, setOpen] = useState(false);
  const [q, setQ] = useState("");
  const current = options.find((o) => o.value === value);
  const showSearch = searchable ?? options.length > 8;
  const shown = useMemo(() => {
    const needle = q.trim().toLowerCase();
    if (!needle) return options;
    return options.filter((o) =>
      `${o.label} ${o.hint ?? ""} ${o.group ?? ""}`.toLowerCase().includes(needle),
    );
  }, [options, q]);

  const pick = (v: T) => {
    onChange(v);
    setOpen(false);
    setQ("");
  };

  return (
    <Popover
      open={open}
      onOpenChange={(o) => {
        setOpen(o);
        if (!o) setQ("");
      }}
      align="start"
      side={side}
      className={cn("w-72 p-1", menuClassName)}
      trigger={
        <button
          type="button"
          aria-label={ariaLabel}
          aria-haspopup="menu"
          aria-expanded={open}
          title={iconOnly ? `${ariaLabel}: ${current?.label ?? ""}` : undefined}
          onClick={() => setOpen((o) => !o)}
          className={cn(
            "inline-flex h-8 min-w-0 items-center gap-1.5 rounded-lg border border-border bg-surface px-2 text-sm font-medium text-fg shadow-xs transition-colors hover:border-border-strong hover:bg-surface-hover focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/50",
            open && "border-primary/40 bg-primary-subtle",
            className,
          )}
        >
          {trigger ?? (
            <>
              {current?.icon ? (
                <span className="grid shrink-0 place-items-center text-fg-muted">
                  {current.icon}
                </span>
              ) : null}
              {iconOnly ? null : <span className="min-w-0 truncate">{current?.label}</span>}
            </>
          )}
          <ChevronDown size={13} className="shrink-0 text-fg-subtle" aria-hidden />
        </button>
      }
    >
      <div role="menu" aria-label={ariaLabel} className="max-h-[22rem] overflow-y-auto">
        {showSearch ? (
          <div className="sticky top-0 z-10 bg-elevated p-1">
            <div className="relative">
              <Search
                size={13}
                className="pointer-events-none absolute left-2.5 top-1/2 -translate-y-1/2 text-fg-subtle"
                aria-hidden
              />
              <Input
                autoFocus
                value={q}
                onChange={(e) => setQ(e.currentTarget.value)}
                placeholder={searchPlaceholder}
                aria-label={searchPlaceholder}
                className="h-8 pl-7 text-sm"
              />
            </div>
          </div>
        ) : null}
        {shown.length === 0 ? (
          <p className="px-3 py-4 text-center text-xs text-fg-muted">{t("No matches")}</p>
        ) : null}
        {shown.map((o, i) => {
          const on = o.value === value;
          const header = o.group && o.group !== shown[i - 1]?.group ? o.group : null;
          return (
            <div key={o.value}>
              {header ? (
                <p className="px-2.5 pb-1 pt-2.5 text-[10.5px] font-semibold uppercase tracking-wider text-fg-subtle">
                  {header}
                </p>
              ) : null}
              <button
                type="button"
                role="menuitemradio"
                aria-checked={on}
                onClick={() => pick(o.value)}
                className={cn(
                  "flex w-full items-center gap-2.5 rounded-md px-2.5 py-1.5 text-left text-sm transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/50",
                  on ? "bg-primary-subtle text-primary" : "text-fg hover:bg-surface-hover",
                )}
              >
                {o.icon ? <span className="grid shrink-0 place-items-center">{o.icon}</span> : null}
                <span className="min-w-0 flex-1">
                  <span className="block truncate font-medium">{o.label}</span>
                  {o.hint ? (
                    <span className="block truncate text-xs text-fg-muted">{o.hint}</span>
                  ) : null}
                </span>
                {o.meta}
                {on ? <Check size={14} className="shrink-0" aria-hidden /> : null}
              </button>
            </div>
          );
        })}
        {footer ? <div className="border-t border-border p-1">{footer}</div> : null}
      </div>
    </Popover>
  );
}
