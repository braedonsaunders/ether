import { Button, cn } from "@braedonsaunders/appkit-ui";
import { ChevronLeft, ChevronRight, Star, Tv } from "lucide-react";
import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";
import { Segmented } from "../../components/fields";
import { Artwork } from "../../components/Artwork";
import { getLocale, t, tn } from "../../i18n";
import { unit } from "../../lib/format";
import { ChannelLogo } from "./ChannelLogo";
import { MINUTE, clock, hueOf, onNow, progress, type Channel, type Programme } from "./model";

const ROW = 60;
const HEADER = 36;
const CH_COL = 208;
const ZOOM = { wide: 5, normal: 3.5, tight: 2.2 } as const;
export type Zoom = keyof typeof ZOOM;

/**
 * The classic EPG grid, done properly: channels down the left, time across
 * the top, the live line running through it. Rows are virtualised so an
 * IPTV lineup of thousands still scrolls smoothly.
 */
export function GuideGrid({
  channels,
  guide,
  from,
  until,
  now,
  selected,
  onSelect,
  onShift,
  onNow: jumpNow,
  loading,
}: {
  channels: Channel[];
  guide: Map<string, Programme[]>;
  from: Date;
  until: Date;
  now: number;
  selected: string | null;
  onSelect: (id: string) => void;
  onShift: (hours: number) => void;
  onNow: () => void;
  loading: boolean;
}) {
  const [zoom, setZoom] = useState<Zoom>("normal");
  const ppm = ZOOM[zoom];
  const start = from.getTime();
  const minutes = (until.getTime() - start) / MINUTE;
  const width = minutes * ppm;
  const scroller = useRef<HTMLDivElement>(null);
  const [view, setView] = useState({ top: 0, height: 800, left: 0 });
  const x = (ms: number) => ((ms - start) / MINUTE) * ppm;
  const nowX = x(now);
  const nowVisible = now >= start && now <= until.getTime();

  const scrollToNow = useCallback(
    (smooth = false) => {
      const el = scroller.current;
      if (!el) return;
      const left = Math.max(0, nowX - 45 * ppm);
      if (typeof el.scrollTo === "function")
        el.scrollTo({ left, behavior: smooth ? "smooth" : "auto" });
      else el.scrollLeft = left;
    },
    [nowX, ppm],
  );
  // Land on "now" whenever the window or zoom changes.
  useLayoutEffect(() => {
    scrollToNow();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [start, zoom]);

  useEffect(() => {
    const el = scroller.current;
    if (!el) return;
    const measure = () =>
      setView({ top: el.scrollTop, height: el.clientHeight, left: el.scrollLeft });
    measure();
    const ro = typeof ResizeObserver !== "undefined" ? new ResizeObserver(measure) : null;
    ro?.observe(el);
    return () => ro?.disconnect();
  }, []);

  const first = Math.max(0, Math.floor((view.top - HEADER) / ROW) - 6);
  const last = Math.min(channels.length, Math.ceil((view.top + view.height) / ROW) + 6);

  const ticks = useMemo(() => {
    const out: number[] = [];
    const step = 30 * MINUTE;
    for (let tick = Math.ceil(start / step) * step; tick <= until.getTime(); tick += step)
      out.push(tick);
    return out;
  }, [start, until]);

  const dayLabel = new Date(Math.max(start, now)).toLocaleDateString(getLocale(), {
    weekday: "long",
    month: "short",
    day: "numeric",
  });

  return (
    <div className="flex h-full min-h-[28rem] flex-col">
      <div className="flex shrink-0 flex-wrap items-center gap-2 border-b border-border bg-surface px-3 py-2 sm:px-4">
        <div className="mr-auto flex items-baseline gap-2">
          <h2 className="text-sm font-semibold text-fg">{t("Guide")}</h2>
          <span className="text-xs text-fg-muted">
            {tn(channels.length, "{day} · {count} channel", "{day} · {count} channels", {
              day: dayLabel,
            })}
          </span>
          {loading ? <span className="text-xs text-fg-subtle">{t("Updating…")}</span> : null}
        </div>
        <div className="flex items-center gap-1">
          <Button variant="ghost" size="sm" aria-label={t("Earlier")} onClick={() => onShift(-3)}>
            <ChevronLeft size={15} />
          </Button>
          <Button
            variant="outline"
            size="sm"
            onClick={() => {
              jumpNow();
              scrollToNow(true);
            }}
          >
            <span className="relative mr-1 flex size-2">
              <span className="absolute inline-flex size-full animate-ping rounded-full bg-danger/60" />
              <span className="relative inline-flex size-2 rounded-full bg-danger" />
            </span>
            {t("Now")}
          </Button>
          <Button variant="ghost" size="sm" aria-label={t("Later")} onClick={() => onShift(3)}>
            <ChevronRight size={15} />
          </Button>
        </div>
        <Segmented
          ariaLabel={t("Zoom")}
          size="sm"
          value={zoom}
          onChange={setZoom}
          options={[
            { value: "tight", label: unit(6, "hour") },
            { value: "normal", label: unit(4, "hour") },
            { value: "wide", label: unit(3, "hour") },
          ]}
        />
      </div>

      <div
        ref={scroller}
        role="grid"
        aria-label={t("Programme guide")}
        aria-rowcount={channels.length}
        onScroll={(e) =>
          setView({
            top: e.currentTarget.scrollTop,
            height: e.currentTarget.clientHeight,
            left: e.currentTarget.scrollLeft,
          })
        }
        className="app-scroll relative min-h-0 flex-1 overflow-auto bg-bg"
      >
        <div
          className="relative"
          style={{ width: CH_COL + width, height: HEADER + channels.length * ROW }}
        >
          {/* time ruler */}
          <div
            className="sticky top-0 z-30 flex border-b border-border bg-surface/95 backdrop-blur"
            style={{ height: HEADER }}
          >
            <div
              className="sticky left-0 z-40 flex shrink-0 items-center border-r border-border bg-surface px-3 text-[11px] font-semibold uppercase tracking-wider text-fg-subtle"
              style={{ width: CH_COL }}
            >
              {t("Channel")}
            </div>
            <div className="relative" style={{ width }}>
              {ticks.map((tick) => {
                const hour = new Date(tick).getMinutes() === 0;
                return (
                  <div
                    key={tick}
                    className="absolute inset-y-0 flex items-center border-l border-border/70 pl-2"
                    style={{ left: x(tick) }}
                  >
                    <span
                      className={cn(
                        "whitespace-nowrap text-xs tabular-nums",
                        hour ? "font-semibold text-fg" : "text-fg-subtle",
                      )}
                    >
                      {clock(new Date(tick))}
                    </span>
                  </div>
                );
              })}
              {nowVisible ? (
                <div
                  className="absolute bottom-0 z-10 -translate-x-1/2 rounded-t-md bg-danger px-1.5 py-0.5 text-[10px] font-bold uppercase tracking-wider text-white"
                  style={{ left: nowX }}
                >
                  {t("Live · {time}", { time: clock(new Date(now)) })}
                </div>
              ) : null}
            </div>
          </div>

          {/* half-hour gridlines */}
          <div
            className="pointer-events-none absolute bottom-0 z-0"
            style={{ left: CH_COL, top: HEADER, width }}
            aria-hidden
          >
            {ticks.map((tick) => (
              <div
                key={tick}
                className="absolute inset-y-0 border-l border-border/40"
                style={{ left: x(tick) }}
              />
            ))}
          </div>

          {channels.slice(first, last).map((c, k) => {
            const i = first + k;
            const ps = guide.get(c.id) ?? [];
            const on = c.id === selected;
            const live = onNow(ps, now);
            return (
              <div
                key={c.id}
                role="row"
                aria-rowindex={i + 1}
                className={cn(
                  "absolute left-0 flex border-b border-border/60",
                  on ? "bg-primary-subtle/40" : i % 2 ? "bg-bg" : "bg-bg-subtle/40",
                )}
                style={{ top: HEADER + i * ROW, height: ROW, width: CH_COL + width }}
              >
                <button
                  type="button"
                  role="rowheader"
                  onClick={() => onSelect(c.id)}
                  className={cn(
                    "sticky left-0 z-20 flex shrink-0 items-center gap-2.5 border-r border-border px-3 text-left transition-colors hover:bg-surface-hover",
                    on ? "bg-primary-subtle" : "bg-surface",
                  )}
                  style={{ width: CH_COL }}
                  aria-label={t("Channel {number} {name}", { number: c.number, name: c.name })}
                >
                  <span className="w-7 shrink-0 text-right font-mono text-[11px] tabular-nums text-fg-subtle">
                    {c.number}
                  </span>
                  <ChannelLogo name={c.name} logo={c.logo} seed={c.id} size={32} />
                  <span className="min-w-0 flex-1">
                    <span className="flex items-center gap-1">
                      <span className="truncate text-[13px] font-medium text-fg">{c.name}</span>
                      {c.favorite ? (
                        <Star size={10} className="shrink-0 fill-warning text-warning" />
                      ) : null}
                    </span>
                    <span className="block truncate text-[11px] text-fg-subtle">
                      {live
                        ? t("{time} left", {
                            time: unit(
                              Math.max(1, Math.round((Date.parse(live.end) - now) / MINUTE)),
                              "minute",
                            ),
                          })
                        : c.group || (c.kind === "virtual" ? t("Virtual") : t("Live"))}
                    </span>
                  </span>
                </button>
                <div className="relative" style={{ width }}>
                  {ps.length === 0 ? (
                    <div
                      className="absolute inset-1 flex items-center rounded-md border border-dashed border-border px-3 text-xs text-fg-subtle"
                      style={{ paddingLeft: Math.max(12, view.left + 12) }}
                    >
                      {c.kind === "iptv"
                        ? t("No guide data — map a guide channel in Guide data")
                        : t("No guide data — no programming")}
                    </div>
                  ) : null}
                  {ps.map((p) => {
                    const s = Math.max(Date.parse(p.start), start);
                    const e = Math.min(Date.parse(p.end), until.getTime());
                    if (e <= s) return null;
                    const left = x(s);
                    const w = Math.max(2, x(e) - left);
                    const airing = live?.id === p.id;
                    const past = Date.parse(p.end) <= now;
                    // Slide the label right so it stays readable beside the
                    // sticky channel column when the programme began earlier.
                    const inset = Math.max(0, Math.min(view.left - left, w - 90));
                    return (
                      <button
                        key={p.id}
                        type="button"
                        role="gridcell"
                        onClick={() => onSelect(c.id)}
                        aria-label={t("{channel}: {title}, {start} to {end}", {
                          channel: c.name,
                          title: p.item.title,
                          start: clock(p.start),
                          end: clock(p.end),
                        })}
                        title={`${p.item.title}\n${clock(p.start)} – ${clock(p.end)}${p.item.summary ? `\n\n${p.item.summary}` : ""}`}
                        className={cn(
                          "group absolute inset-y-1 overflow-hidden rounded-md border px-2 py-1 text-left transition-all hover:z-10 hover:shadow-md focus-visible:z-10 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/50",
                          airing
                            ? "border-primary/40 bg-surface shadow-sm"
                            : past
                              ? "border-transparent bg-surface/50 opacity-60"
                              : "border-border/70 bg-surface hover:border-border-strong",
                        )}
                        style={{ left: left + 1, width: w - 2 }}
                      >
                        {airing ? (
                          <span
                            className="absolute inset-y-0 left-0 bg-primary/10"
                            style={{ width: `${progress(p, now) * 100}%` }}
                            aria-hidden
                          />
                        ) : null}
                        <span
                          className="absolute inset-y-1 left-0 w-0.5 rounded-full"
                          style={{ background: `hsl(${hueOf(c.id)} 65% 55%)` }}
                          aria-hidden
                        />
                        <span
                          className="relative flex h-full items-center gap-2"
                          style={inset ? { paddingLeft: inset } : undefined}
                        >
                          {p.item.thumb && w > 220 ? (
                            <Artwork
                              src={p.item.thumb}
                              alt=""
                              title=""
                              className="h-10 w-7 shrink-0 rounded"
                            />
                          ) : null}
                          <span className="min-w-0">
                            <span
                              className={cn(
                                "block truncate text-[13px] font-medium",
                                airing ? "text-fg" : "text-fg",
                              )}
                            >
                              {p.item.title || t("Untitled")}
                            </span>
                            {w > 70 ? (
                              <span className="block truncate text-[11px] tabular-nums text-fg-muted">
                                {clock(p.start)} – {clock(p.end)}
                                {p.item.rating ? ` · ${p.item.rating}` : ""}
                              </span>
                            ) : null}
                          </span>
                        </span>
                      </button>
                    );
                  })}
                </div>
              </div>
            );
          })}

          {nowVisible ? (
            <div
              className="pointer-events-none absolute bottom-0 z-[25] w-0.5 bg-danger shadow-[0_0_8px] shadow-danger/60"
              style={{ left: CH_COL + nowX - 1, top: HEADER }}
              aria-hidden
            />
          ) : null}
        </div>
        {channels.length === 0 ? (
          <div className="absolute inset-0 top-9 grid place-items-center p-8 text-center">
            <div className="max-w-sm space-y-2">
              <Tv className="mx-auto text-fg-subtle" size={28} />
              <p className="text-sm font-medium text-fg">{t("No channels match")}</p>
              <p className="text-xs text-fg-muted">
                {t(
                  "Clear the search or filter on the left, create a virtual channel, or add an IPTV source.",
                )}
              </p>
            </div>
          </div>
        ) : null}
      </div>
    </div>
  );
}

/** "On now" spotlight: big cards for favourites (or the first channels). */
export function OnNowStrip({
  channels,
  guide,
  now,
  onSelect,
}: {
  channels: Channel[];
  guide: Map<string, Programme[]>;
  now: number;
  onSelect: (id: string) => void;
}) {
  const picks = useMemo(() => {
    const live = channels
      .filter((c) => c.enabled)
      .map((c) => ({ c, p: onNow(guide.get(c.id), now) }))
      .filter((x): x is { c: Channel; p: Programme } => Boolean(x.p));
    live.sort(
      (a, b) =>
        Number(b.c.favorite) - Number(a.c.favorite) ||
        Number(Boolean(b.p.item.thumb)) - Number(Boolean(a.p.item.thumb)),
    );
    return live.slice(0, 10);
  }, [channels, guide, now]);
  if (!picks.length) return null;
  return (
    <section
      aria-label={t("On now")}
      className="shrink-0 border-b border-border bg-surface px-3 pb-3 pt-2.5 sm:px-4"
    >
      <div className="mb-2 flex items-center gap-2">
        <span className="relative flex size-2">
          <span className="absolute inline-flex size-full animate-ping rounded-full bg-danger/60" />
          <span className="relative inline-flex size-2 rounded-full bg-danger" />
        </span>
        <h2 className="text-xs font-semibold uppercase tracking-wider text-fg-subtle">
          {t("On now")}
        </h2>
      </div>
      <div className="app-scroll -mx-1 flex gap-2.5 overflow-x-auto px-1 pb-1">
        {picks.map(({ c, p }) => {
          const hue = hueOf(c.id);
          return (
            <button
              key={c.id}
              type="button"
              onClick={() => onSelect(c.id)}
              aria-label={t("{channel} on now: {title}", { channel: c.name, title: p.item.title })}
              className="group relative flex h-[5.5rem] w-64 shrink-0 overflow-hidden rounded-xl border border-border text-left shadow-xs transition-all hover:-translate-y-0.5 hover:shadow-md focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/50"
              style={{
                background: `linear-gradient(120deg, hsl(${hue} 60% 22%), hsl(${(hue + 50) % 360} 55% 12%))`,
              }}
            >
              {p.item.thumb ? (
                <Artwork
                  src={p.item.thumb}
                  alt=""
                  title=""
                  className="absolute inset-y-0 right-0 w-24 opacity-90 [mask-image:linear-gradient(to_left,black_40%,transparent)]"
                  imgClassName="object-cover"
                />
              ) : null}
              <span className="relative flex min-w-0 flex-1 flex-col justify-between p-2.5">
                <span className="flex items-center gap-2">
                  <ChannelLogo
                    name={c.name}
                    logo={c.logo}
                    seed={c.id}
                    size={22}
                    className="rounded-md"
                  />
                  <span className="truncate text-[11px] font-medium text-white/80">
                    {c.number} · {c.name}
                  </span>
                </span>
                <span className="min-w-0 pr-16">
                  <span className="block truncate text-sm font-semibold text-white">
                    {p.item.title}
                  </span>
                  <span className="block text-[11px] tabular-nums text-white/70">
                    {t("until {time}", { time: clock(p.end) })}
                  </span>
                </span>
              </span>
              <span className="absolute inset-x-0 bottom-0 h-1 bg-white/15">
                <span
                  className="block h-full bg-white/80"
                  style={{ width: `${progress(p, now) * 100}%` }}
                />
              </span>
            </button>
          );
        })}
      </div>
    </section>
  );
}
