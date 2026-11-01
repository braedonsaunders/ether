import { Button, Input, cn } from "@braedonsaunders/appkit-ui";
import { ChevronDown, ChevronRight, RotateCcw } from "lucide-react";
import { useState } from "react";
import { t } from "../../i18n";
import type { Device, HomeLayout, LayoutSection, Viewer } from "../../lib/contract";
import { DEVICES, SECTIONS, lengthIn, setPageLength, type LayerKey } from "../../lib/layout";

/**
 * Page length (rows the engine arranges; Plex's own rows not counted), for
 * every device or per device. A viewer layer shows what it inherits and can
 * override it.
 */
export function PageSettings({
  draft,
  onChange,
  layer,
  section,
  defaults,
}: {
  draft: HomeLayout;
  onChange: (d: HomeLayout) => void;
  layer: LayerKey;
  section: LayoutSection;
  viewers: Viewer[];
  defaults: Record<LayoutSection, number>;
}) {
  const sectionLabel = SECTIONS.find((s) => s.value === section)?.label ?? section;
  const anyDevice = DEVICES.some((d) => lengthIn(draft, layer, section, d.value));
  const [perDevice, setPerDevice] = useState(false);
  const showDevices = perDevice || anyDevice;
  // What a setting falls back to when this layer leaves it empty.
  const inherited = (device: Device | null): { n: number; from: string } => {
    if (layer !== "global") {
      if (device) {
        const own = lengthIn(draft, layer, section, null);
        if (own) return { n: own, from: t("this viewer's page length") };
        const gd = lengthIn(draft, "global", section, device);
        if (gd) return { n: gd, from: t("Everyone") };
      }
      const g = lengthIn(draft, "global", section, null);
      if (g) return { n: g, from: t("Everyone") };
      return { n: defaults[section], from: t("engine default") };
    }
    if (device) {
      const g = lengthIn(draft, "global", section, null);
      if (g) return { n: g, from: t("all devices") };
    }
    return { n: defaults[section], from: t("engine default") };
  };
  const row = (device: Device | null, label: string) => {
    const own = lengthIn(draft, layer, section, device);
    const inh = inherited(device);
    const name = device
      ? t("{section} rows on {device}", { section: sectionLabel, device: label })
      : t("{section} rows", { section: sectionLabel });
    return (
      <div key={device ?? "all"} className="flex items-center gap-2">
        <span
          className={cn("w-24 shrink-0 text-xs", device ? "text-fg-muted" : "font-medium text-fg")}
        >
          {label}
        </span>
        <Input
          type="number"
          inputMode="numeric"
          min={1}
          max={50}
          aria-label={name}
          className="h-8 w-20 tabular-nums"
          value={own ?? ""}
          placeholder={String(inh.n)}
          onChange={(e) => {
            const n = e.currentTarget.valueAsNumber;
            onChange(
              setPageLength(
                draft,
                layer,
                section,
                device,
                Number.isFinite(n) && n > 0 ? Math.min(50, n) : undefined,
              ),
            );
          }}
        />
        {own ? (
          <Button
            variant="ghost"
            size="sm"
            className="h-7 px-2 text-xs"
            aria-label={t("Reset {name}", { name })}
            onClick={() => onChange(setPageLength(draft, layer, section, device, undefined))}
          >
            <RotateCcw size={12} /> {layer === "global" ? t("Default") : t("Reset")}
          </Button>
        ) : (
          <span className="text-[11px] text-fg-subtle">{inh.from}</span>
        )}
      </div>
    );
  };
  return (
    <section className="space-y-2" aria-label={t("Page length")}>
      <div>
        <h2 className="text-xs font-semibold uppercase tracking-wider text-fg-subtle">
          {t("Page length")}
        </h2>
        <p className="mt-0.5 text-xs text-fg-muted">
          {layer !== "global"
            ? t("All rows on {section}. Empty fields follow Global.", { section: sectionLabel })
            : t("All rows on {section}.", { section: sectionLabel })}
        </p>
      </div>
      <div className="space-y-1.5 rounded-lg border border-border bg-surface p-2.5">
        {row(null, t("All devices"))}
        <button
          type="button"
          className="flex items-center gap-1 text-[11px] font-medium text-fg-muted hover:text-fg disabled:opacity-60"
          aria-expanded={showDevices}
          disabled={anyDevice}
          onClick={() => setPerDevice((o) => !o)}
        >
          {showDevices ? <ChevronDown size={12} /> : <ChevronRight size={12} />} {t("Per device")}
        </button>
        {showDevices ? DEVICES.map((d) => row(d.value, d.label)) : null}
      </div>
    </section>
  );
}
