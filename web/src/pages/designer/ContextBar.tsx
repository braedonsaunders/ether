import { Popover, cn } from "@braedonsaunders/appkit-ui";
import { ChevronDown, Globe, Laptop, Smartphone, Tv } from "lucide-react";
import { useState } from "react";
import { ToolbarPicker } from "../../components/ToolbarPicker";
import { localized, t } from "../../i18n";
import type { Device, LayoutSection } from "../../lib/contract";
import { WEEKDAYS, WEEKDAYS_LONG, formatHour } from "../../lib/format";
import { SECTIONS } from "../../lib/layout";

/** The context a Home is previewed in (the viewer is chosen separately). */
export interface ViewContext {
  device: Device;
  day: number;
  hour: number;
  section: LayoutSection;
}

export const DEVICE_OPTIONS = localized(() => [
  { value: "tv" as const, label: t("TV"), icon: <Tv size={14} /> },
  { value: "mobile" as const, label: t("Phone"), icon: <Smartphone size={14} /> },
  { value: "web" as const, label: t("Web"), icon: <Globe size={14} /> },
  { value: "desktop" as const, label: t("Desktop"), icon: <Laptop size={14} /> },
]);

/**
 * Page, device and time as three compact pickers on one line, so the
 * controls below them start near the top of the panel.
 */
export function ContextBar({
  ctx,
  onChange,
  className,
}: {
  ctx: ViewContext;
  onChange: (c: ViewContext) => void;
  className?: string;
}) {
  return (
    <div
      className={cn("flex min-w-0 items-center gap-1", className)}
      role="group"
      aria-label={t("Preview context")}
    >
      <ToolbarPicker
        ariaLabel={t("Page")}
        value={ctx.section}
        onChange={(s) => onChange({ ...ctx, section: s })}
        options={SECTIONS.map((s) => ({ value: s.value, label: s.label }))}
        menuClassName="w-40"
      />
      <ToolbarPicker
        ariaLabel={t("Device")}
        iconOnly
        value={ctx.device}
        onChange={(d) => onChange({ ...ctx, device: d })}
        options={DEVICE_OPTIONS}
        menuClassName="w-40"
      />
      <WhenPicker ctx={ctx} onChange={onChange} />
    </div>
  );
}

function WhenPicker({ ctx, onChange }: { ctx: ViewContext; onChange: (c: ViewContext) => void }) {
  const [open, setOpen] = useState(false);
  const label = t("{day} {time}", { day: WEEKDAYS[ctx.day] ?? "", time: formatHour(ctx.hour) });
  return (
    <Popover
      open={open}
      onOpenChange={setOpen}
      align="start"
      className="w-[17rem] p-3"
      trigger={
        <button
          type="button"
          aria-label={t("Preview time: {day} {time}", {
            day: WEEKDAYS_LONG[ctx.day] ?? "",
            time: formatHour(ctx.hour),
          })}
          aria-expanded={open}
          onClick={() => setOpen((o) => !o)}
          className={cn(
            "inline-flex h-8 shrink-0 items-center gap-1.5 rounded-lg border border-border bg-surface px-2 text-sm font-medium text-fg shadow-xs transition-colors hover:border-border-strong hover:bg-surface-hover focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/50",
            open && "border-primary/40 bg-primary-subtle",
          )}
        >
          <span className="whitespace-nowrap tabular-nums">{label}</span>
          <ChevronDown size={13} className="text-fg-subtle" aria-hidden />
        </button>
      }
    >
      <div className="space-y-3">
        <div className="flex items-center justify-between">
          <p className="text-xs font-semibold uppercase tracking-wider text-fg-subtle">
            {t("Preview at")}
          </p>
          <button
            type="button"
            className="text-xs font-medium text-primary hover:underline"
            onClick={() => {
              const now = new Date();
              onChange({ ...ctx, day: now.getDay(), hour: now.getHours() });
              setOpen(false);
            }}
          >
            {t("Now")}
          </button>
        </div>
        <div role="radiogroup" aria-label={t("Day")} className="grid grid-cols-7 gap-1">
          {WEEKDAYS.map((d, i) => (
            <button
              key={d}
              type="button"
              role="radio"
              aria-checked={ctx.day === i}
              aria-label={WEEKDAYS_LONG[i]}
              onClick={() => onChange({ ...ctx, day: i })}
              className={cn(
                "h-8 rounded-md text-xs font-medium transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/50",
                ctx.day === i
                  ? "bg-primary text-primary-fg"
                  : "bg-bg-subtle text-fg-muted hover:text-fg",
              )}
            >
              {d.slice(0, 2)}
            </button>
          ))}
        </div>
        <div role="radiogroup" aria-label={t("Time")} className="grid grid-cols-6 gap-1">
          {Array.from({ length: 24 }, (_, h) => (
            <button
              key={h}
              type="button"
              role="radio"
              aria-checked={ctx.hour === h}
              aria-label={formatHour(h)}
              onClick={() => {
                onChange({ ...ctx, hour: h });
                setOpen(false);
              }}
              className={cn(
                "h-7 rounded-md text-[11px] font-medium tabular-nums transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/50",
                ctx.hour === h
                  ? "bg-primary text-primary-fg"
                  : "text-fg-muted hover:bg-surface-hover hover:text-fg",
              )}
            >
              {formatHour(h).replace(":00", "").replace(" ", "").toLowerCase()}
            </button>
          ))}
        </div>
      </div>
    </Popover>
  );
}
