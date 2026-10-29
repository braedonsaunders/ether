import { cn, useElementSize } from "@braedonsaunders/appkit-ui";
import { useId, useState, type PointerEvent } from "react";
import { t } from "../i18n";

export interface LiveSeries {
  key: string;
  label: string;
  values: number[];
  /** Tailwind stroke/fill/background classes for this series' identity. */
  stroke: string;
  fill: string;
  swatch: string;
}

function niceMax(v: number): number {
  if (v <= 0) return 1;
  const p = 10 ** Math.floor(Math.log10(v));
  const n = v / p;
  return (n <= 1 ? 1 : n <= 2 ? 2 : n <= 5 ? 5 : 10) * p;
}

function ago(iso: string | undefined, now: number): string {
  if (!iso) return "";
  const s = Math.max(0, Math.round((now - Date.parse(iso)) / 1000));
  if (s < 5) return t("now");
  if (s < 60) return t("{seconds} s ago", { seconds: s });
  const m = Math.floor(s / 60);
  const r = s % 60;
  return r
    ? t("{minutes} min {seconds} s ago", { minutes: m, seconds: r })
    : t("{minutes} min ago", { minutes: m });
}

/**
 * A live time series that fills its container: thin 2 px lines on one
 * y-scale, an optional soft area for a single series, a recessive scale
 * line, and a crosshair tooltip with every series' value at that moment.
 */
export function LiveChart({
  series,
  times,
  max,
  format,
  area = false,
  ariaLabel,
  className,
}: {
  series: LiveSeries[];
  /** ISO timestamps, one per value. */
  times: string[];
  /** Fixed top of scale (100 for percentages); otherwise a nice max of the data. */
  max?: number;
  format: (v: number) => string;
  area?: boolean;
  ariaLabel: string;
  className?: string;
}) {
  const [ref, size] = useElementSize<HTMLDivElement>();
  const [hover, setHover] = useState<number | null>(null);
  const clip = useId();
  const n = Math.max(...series.map((s) => s.values.length), 0);
  const w = Math.max(1, Math.round(size.width));
  const h = Math.max(1, Math.round(size.height));
  const top = max ?? niceMax(Math.max(0, ...series.flatMap((s) => s.values)) * 1.1);
  const padT = 14;
  const plotH = Math.max(1, h - padT - 1);
  const x = (i: number) => (n <= 1 ? w : (i / (n - 1)) * w);
  const y = (v: number) => padT + plotH - (Math.min(v, top) / top) * plotH;
  const path = (vals: number[]) =>
    vals.map((v, i) => `${i ? "L" : "M"}${x(i).toFixed(1)},${y(v).toFixed(1)}`).join("");

  const onMove = (e: PointerEvent<SVGSVGElement>) => {
    if (n < 2) return;
    const r = e.currentTarget.getBoundingClientRect();
    const i = Math.round(((e.clientX - r.left) / r.width) * (n - 1));
    setHover(Math.max(0, Math.min(n - 1, i)));
  };
  const hx = hover !== null ? x(hover) : 0;
  const now = Date.now();

  return (
    <div ref={ref} role="img" aria-label={ariaLabel} className={cn("relative min-h-0", className)}>
      {n >= 2 && size.width > 0 ? (
        <svg
          width={w}
          height={h}
          className="absolute inset-0 block touch-none overflow-visible"
          aria-hidden
          onPointerMove={onMove}
          onPointerDown={onMove}
          onPointerLeave={() => setHover(null)}
        >
          <defs>
            <clipPath id={clip}>
              <rect x={0} y={0} width={w} height={h} />
            </clipPath>
          </defs>
          <line
            x1={0}
            x2={w}
            y1={padT}
            y2={padT}
            className="stroke-border-subtle"
            strokeWidth={1}
          />
          <text x={0} y={padT - 4} className="fill-fg-subtle text-[10px]">
            {format(top)}
          </text>
          <line x1={0} x2={w} y1={h - 0.5} y2={h - 0.5} className="stroke-border" strokeWidth={1} />
          <g clipPath={`url(#${clip})`}>
            {area && series.length === 1 ? (
              <path
                d={`${path(series[0]!.values)}L${x(n - 1)},${h}L${x(0)},${h}Z`}
                className={cn(series[0]!.fill, "opacity-15")}
              />
            ) : null}
            {series.map((s) => (
              <path
                key={s.key}
                d={path(s.values)}
                fill="none"
                strokeWidth={2}
                strokeLinejoin="round"
                strokeLinecap="round"
                className={s.stroke}
              />
            ))}
          </g>
          {hover !== null ? (
            <g className="pointer-events-none">
              <line
                x1={hx}
                x2={hx}
                y1={padT}
                y2={h}
                className="stroke-border-strong"
                strokeWidth={1}
              />
              {series.map((s) => (
                <circle
                  key={s.key}
                  cx={hx}
                  cy={y(s.values[hover] ?? 0)}
                  r={4}
                  strokeWidth={2}
                  className={cn(s.fill, "stroke-surface")}
                />
              ))}
            </g>
          ) : null}
        </svg>
      ) : null}
      {hover !== null ? (
        <div
          role="tooltip"
          className="pointer-events-none absolute z-10 min-w-28 rounded-md border border-border bg-elevated px-2.5 py-1.5 text-xs shadow-md"
          style={{
            top: 0,
            left: Math.min(Math.max(0, hx - 56), Math.max(0, w - 120)),
          }}
        >
          <p className="text-fg-subtle">{ago(times[hover], now)}</p>
          {series.map((s) => (
            <p key={s.key} className="flex items-center justify-between gap-3 text-fg">
              <span className="flex items-center gap-1.5 text-fg-muted">
                <span className={cn("size-2 rounded-full", s.swatch)} />
                {s.label}
              </span>
              <span className="font-medium tabular-nums">{format(s.values[hover] ?? 0)}</span>
            </p>
          ))}
        </div>
      ) : null}
    </div>
  );
}
