import { cn, useElementSize } from "@braedonsaunders/appkit-ui";
import { useId, useMemo, useState } from "react";
import { t } from "../i18n";
import { fixed, formatHour, formatPercent } from "../lib/format";

/**
 * Small token-driven charts. Each is single-series (identity is named by the
 * chart title), uses thin marks with rounded data ends anchored to the
 * baseline, recessive gridlines, text in text tokens, and a hover/focus
 * tooltip. Colors come from semantic tokens so light and dark both work.
 */

function roundedTopBar(x: number, y: number, w: number, h: number, r: number): string {
  if (h <= 0) return "";
  const rr = Math.min(r, w / 2, h);
  return `M${x},${y + h}V${y + rr}Q${x},${y} ${x + rr},${y}H${x + w - rr}Q${x + w},${y} ${x + w},${y + rr}V${y + h}Z`;
}

function niceMax(v: number): number {
  if (v <= 0) return 1;
  const p = 10 ** Math.floor(Math.log10(v));
  const n = v / p;
  const step = n <= 1 ? 1 : n <= 2 ? 2 : n <= 5 ? 5 : 10;
  return step * p;
}

export interface BarDatum {
  key: string;
  label: string;
  value: number;
  detail?: string;
}

/** Vertical bars over time (e.g. plays per day). */
export function TimeBars({
  data,
  height: fixedHeight = 160,
  fill = false,
  ariaLabel,
  valueFormat = (v) => String(v),
  className,
}: {
  data: BarDatum[];
  height?: number;
  /** Take the container's height instead of `height` (give it a definite height). */
  fill?: boolean;
  ariaLabel: string;
  valueFormat?: (v: number) => string;
  className?: string;
}) {
  const [hover, setHover] = useState<number | null>(null);
  const [ref, size] = useElementSize<HTMLDivElement>();
  const width = Math.max(240, Math.round(size.width || 600));
  const height = fill ? Math.max(96, Math.round(size.height || fixedHeight)) : fixedHeight;
  const padL = 32;
  const padB = 22;
  const padT = 8;
  const max = niceMax(Math.max(0, ...data.map((d) => d.value)));
  const plotH = height - padB - padT;
  const slot = data.length ? (width - padL) / data.length : 0;
  const gap = Math.min(2, slot * 0.25);
  const barW = Math.max(1, slot - gap);
  const ticks = [0, max / 2, max];
  const labelIdx = data.length ? [0, Math.floor((data.length - 1) / 2), data.length - 1] : [];
  const h = hover !== null ? data[hover] : undefined;
  return (
    <div ref={ref} className={cn("relative", fill && "h-full min-h-0", className)}>
      <svg
        viewBox={`0 0 ${width} ${height}`}
        width="100%"
        height={height}
        className={cn("block overflow-visible", fill && "absolute inset-0")}
        role="img"
        aria-label={ariaLabel}
        onMouseLeave={() => setHover(null)}
      >
        {ticks.map((tick) => {
          const y = padT + plotH - (tick / max) * plotH;
          return (
            <g key={tick}>
              <line
                x1={padL}
                x2={width}
                y1={y}
                y2={y}
                className="stroke-border-subtle"
                strokeWidth={1}
                vectorEffect="non-scaling-stroke"
              />
              <text
                x={padL - 6}
                y={y + 3}
                textAnchor="end"
                className="fill-fg-subtle text-[10px] tabular-nums"
              >
                {valueFormat(tick)}
              </text>
            </g>
          );
        })}
        {data.map((d, i) => {
          const bh = (d.value / max) * plotH;
          const x = padL + i * slot + gap / 2;
          return (
            <g key={d.key}>
              <rect
                x={padL + i * slot}
                y={padT}
                width={slot}
                height={plotH}
                fill="transparent"
                onMouseEnter={() => setHover(i)}
                onFocus={() => setHover(i)}
                onBlur={() => setHover(null)}
                tabIndex={0}
                aria-label={`${d.label}: ${valueFormat(d.value)}${d.detail ? `, ${d.detail}` : ""}`}
                className="focus:outline-none"
              />
              <path
                d={roundedTopBar(x, padT + plotH - bh, barW, bh, 4)}
                className={cn(
                  "pointer-events-none transition-opacity",
                  hover === null || hover === i ? "fill-primary" : "fill-primary opacity-40",
                )}
              />
            </g>
          );
        })}
        <line
          x1={padL}
          x2={width}
          y1={padT + plotH}
          y2={padT + plotH}
          className="stroke-border"
          strokeWidth={1}
          vectorEffect="non-scaling-stroke"
        />
        {labelIdx.map((i, n) => {
          const d = data[i];
          if (!d || (n > 0 && i === labelIdx[n - 1])) return null;
          const x = padL + i * slot + slot / 2;
          return (
            <text
              key={`l${i}`}
              x={x}
              y={height - 6}
              textAnchor={n === 0 ? "start" : n === 2 ? "end" : "middle"}
              className="fill-fg-subtle text-[10px]"
            >
              {d.label}
            </text>
          );
        })}
      </svg>
      {h && hover !== null ? (
        <div
          className="pointer-events-none absolute top-0 z-10 -translate-x-1/2 whitespace-nowrap rounded-md border border-border bg-elevated px-2.5 py-1.5 text-xs shadow-md"
          style={{ left: `${((padL + hover * slot + slot / 2) / width) * 100}%` }}
        >
          <div className="font-medium text-fg">{h.label}</div>
          <div className="tabular-nums text-fg-muted">
            {valueFormat(h.value)}
            {h.detail ? ` · ${h.detail}` : ""}
          </div>
        </div>
      ) : null}
    </div>
  );
}

export interface WeightDatum {
  key: string;
  label: string;
  weight: number;
  hint?: string;
}

/**
 * Horizontal diverging bars for weights in -1..1: likes extend right in the
 * brand hue, dislikes extend left in a contrasting categorical hue, with a
 * neutral zero line. Values are printed in text tokens.
 */
export function DivergingBars({
  data,
  ariaLabel,
  onSelect,
  selectedKey,
}: {
  data: WeightDatum[];
  ariaLabel: string;
  onSelect?: (d: WeightDatum) => void;
  selectedKey?: string;
}) {
  return (
    <ul className="space-y-1.5" aria-label={ariaLabel}>
      {data.map((d) => {
        const w = Math.max(-1, Math.min(1, d.weight));
        const pct = Math.abs(w) * 50;
        const Row = onSelect ? "button" : "div";
        return (
          <li key={d.key}>
            <Row
              {...(onSelect ? { type: "button" as const, onClick: () => onSelect(d) } : {})}
              title={d.hint}
              className={cn(
                "grid w-full grid-cols-[minmax(5rem,9rem)_1fr_2.75rem] items-center gap-2 rounded-md px-1 py-0.5 text-left",
                onSelect &&
                  "hover:bg-surface-hover focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/50",
                selectedKey === d.key && "bg-primary-subtle",
              )}
            >
              <span className="truncate text-sm text-fg">{d.label}</span>
              <span className="relative h-3" aria-hidden>
                <span className="absolute inset-y-0 left-1/2 w-px bg-border-strong" />
                <span
                  className={cn(
                    "absolute inset-y-0.5",
                    w >= 0 ? "left-1/2 rounded-r bg-primary" : "right-1/2 rounded-l bg-accent-rose",
                  )}
                  style={{ width: `${pct}%` }}
                />
              </span>
              <span className="text-right text-xs tabular-nums text-fg-muted">
                {w > 0 ? "+" : ""}
                {fixed(w, 2)}
              </span>
            </Row>
          </li>
        );
      })}
    </ul>
  );
}

/** Sequential single-hue columns (e.g. decades), magnitude by height. */
export function Columns({
  data,
  ariaLabel,
  height = 96,
}: {
  data: { key: string; label: string; value: number; hint?: string }[];
  ariaLabel: string;
  height?: number;
}) {
  const max = Math.max(0.0001, ...data.map((d) => Math.abs(d.value)));
  const [hover, setHover] = useState<string | null>(null);
  return (
    <div role="img" aria-label={ariaLabel} className="flex items-end gap-1.5" style={{ height }}>
      {data.map((d) => {
        const v = Math.max(0, d.value);
        const pct = (v / max) * 100;
        return (
          <div
            key={d.key}
            className="flex min-w-0 flex-1 flex-col items-center gap-1"
            onMouseEnter={() => setHover(d.key)}
            onMouseLeave={() => setHover(null)}
            title={d.hint}
          >
            <div className="relative flex w-full flex-1 items-end">
              {hover === d.key ? (
                <span className="absolute -top-5 left-1/2 -translate-x-1/2 text-[10px] tabular-nums text-fg-muted">
                  {fixed(d.value, 2)}
                </span>
              ) : null}
              <div
                className={cn(
                  "w-full rounded-t bg-primary transition-opacity",
                  d.value <= 0 && "bg-border",
                )}
                style={{
                  height: `${Math.max(pct, d.value <= 0 ? 4 : 2)}%`,
                  opacity: d.value <= 0 ? 1 : 0.35 + 0.65 * (v / max),
                }}
              />
            </div>
            <span className="truncate text-[10px] text-fg-subtle">{d.label}</span>
          </div>
        );
      })}
    </div>
  );
}

/** 24 cells, one per hour; peak hours filled. */
export function HourStrip({ peaks, ariaLabel }: { peaks: number[]; ariaLabel: string }) {
  const set = useMemo(() => new Set(peaks.map((h) => ((h % 24) + 24) % 24)), [peaks]);
  return (
    <div
      role="img"
      aria-label={`${ariaLabel}: ${peaks.length ? peaks.map(formatHour).join(", ") : t("no clear peak")}`}
    >
      <div className="grid grid-cols-24 gap-0.5">
        {Array.from({ length: 24 }, (_, h) => (
          <div
            key={h}
            title={formatHour(h)}
            className={cn(
              "h-5 rounded-sm",
              set.has(h) ? "bg-primary" : "bg-bg-subtle ring-1 ring-inset ring-border-subtle",
            )}
          />
        ))}
      </div>
      <div className="mt-1 flex justify-between text-[10px] text-fg-subtle">
        <span>{formatHour(0)}</span>
        <span>{formatHour(6)}</span>
        <span>{formatHour(12)}</span>
        <span>{formatHour(18)}</span>
        <span>{formatHour(24)}</span>
      </div>
    </div>
  );
}

/** Thin horizontal meter (0..1) with a label and value. */
export function Meter({
  value,
  label,
  valueLabel,
  tone = "primary",
  onScrim = false,
}: {
  value: number;
  label?: string;
  valueLabel?: string;
  tone?: "primary" | "warning" | "danger";
  /** Drawn over artwork: text and track use the on-scrim colour. */
  onScrim?: boolean;
}) {
  const id = useId();
  const v = Math.max(0, Math.min(1, Number.isFinite(value) ? value : 0));
  return (
    <div>
      {label || valueLabel ? (
        <div className="mb-1 flex items-baseline justify-between gap-2 text-xs">
          <span id={id} className={onScrim ? "text-on-scrim/80" : "text-fg-muted"}>
            {label}
          </span>
          <span className={cn("tabular-nums", onScrim ? "text-on-scrim" : "text-fg")}>
            {valueLabel ?? formatPercent(v)}
          </span>
        </div>
      ) : null}
      <div
        className={cn(
          "h-1.5 overflow-hidden rounded-full",
          onScrim ? "bg-on-scrim/25" : "bg-bg-subtle ring-1 ring-inset ring-border-subtle",
        )}
        role="meter"
        aria-labelledby={label ? id : undefined}
        aria-valuemin={0}
        aria-valuemax={100}
        aria-valuenow={Math.round(v * 100)}
      >
        <div
          className={cn(
            "h-full rounded-full transition-[width] duration-500",
            tone === "primary" ? "bg-primary" : tone === "warning" ? "bg-warning" : "bg-danger",
          )}
          style={{ width: `${v * 100}%` }}
        />
      </div>
    </div>
  );
}
