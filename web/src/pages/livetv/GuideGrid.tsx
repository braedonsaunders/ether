import { programmeArtwork } from "./model";
import { Badge, Button, Checkbox, cn } from "@braedonsaunders/appkit-ui";
import {
  AlertTriangle,
  ChevronLeft,
  ChevronRight,
  Maximize2,
  Minimize2,
  Star,
  Tv,
} from "lucide-react";
import {
  useCallback,
  useEffect,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from "react";
import { Segmented } from "../../components/fields";
import { Artwork } from "../../components/Artwork";
import { getLocale, t, tn } from "../../i18n";
import { unit } from "../../lib/format";
import type { ChannelSelection } from "./ChannelBulkBar";
import { ChannelLogo } from "./ChannelLogo";
import { MINUTE, clock, hueOf, onNow, progress, type Channel, type Programme } from "./model";

const ROW = 60;
const HEADER = 36;
const CH_COL = 208;
/** The Live TV page's lineup column: a third of the guide, never cramped. */
const LINEUP_MIN = 260;
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
  onRange,
  loading,
  head,
  covered,
  onToggleFavorite,
  emptyHint,
  selection,
  bulkActions,
}: {
  /** Sparse by design: only rows near the viewport have been fetched. */
  channels: (Channel | undefined)[];
  guide: Map<string, Programme[]>;
  from: Date;
  until: Date;
  now: number;
  selected: string | null;
  onSelect: (id: string) => void;
  onShift: (hours: number) => void;
  onNow: () => void;
  onRange?: (first: number, last: number) => void;
  loading: boolean;
  /**
   * The lineup view: the channel column becomes the page's lineup (a third
   * of the width, with favourites, what's on and guide gaps) and head sits
   * above it, beside the guide's toolbar. One scroller keeps every channel
   * row level with its listings.
   */
  head?: ReactNode;
  covered?: (c: Channel) => boolean | undefined;
  onToggleFavorite?: (c: Channel) => void;
  /** Shown instead of the default when there are no rows. */
  emptyHint?: ReactNode;
  selection?: ChannelSelection;
  bulkActions?: ReactNode;
}) {
  const lineup = head !== undefined;
  const loadedChannels = useMemo(
    () => channels.filter((c): c is Channel => c !== undefined),
    [channels],
  );
  const [zoom, setZoom] = useState<Zoom>("normal");
  const ppm = ZOOM[zoom];
  const start = from.getTime();
  const minutes = (until.getTime() - start) / MINUTE;
  const width = minutes * ppm;
  const scroller = useRef<HTMLDivElement>(null);
  const [view, setView] = useState({ top: 0, height: 800, left: 0, width: 1200 });
  const chCol = lineup ? Math.max(LINEUP_MIN, Math.round(view.width / 3)) : CH_COL;
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
      setView({
        top: el.scrollTop,
        height: el.clientHeight,
        left: el.scrollLeft,
        width: el.clientWidth,
      });
    measure();
    const ro = typeof ResizeObserver !== "undefined" ? new ResizeObserver(measure) : null;
    ro?.observe(el);
    return () => ro?.disconnect();
  }, []);

  const first = Math.max(0, Math.floor((view.top - HEADER) / ROW) - 6);
  const last = Math.min(channels.length, Math.ceil((view.top + view.height) / ROW) + 6);
  useEffect(() => {
    onRange?.(first, last);
  }, [first, last, onRange]);

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

  // Full screen shows only the guide: the browser's Fullscreen API on this
  // element, or a window-filling overlay where that API is missing.
  const root = useRef<HTMLDivElement>(null);
  const [nativeFull, setNativeFull] = useState(false);
  const [pseudoFull, setPseudoFull] = useState(false);
  const full = nativeFull || pseudoFull;
  useEffect(() => {
    const sync = () => setNativeFull(document.fullscreenElement === root.current);
    document.addEventListener("fullscreenchange", sync);
    return () => document.removeEventListener("fullscreenchange", sync);
  }, []);
  useEffect(() => {
    if (!pseudoFull) return;
    const esc = (e: KeyboardEvent) => e.key === "Escape" && setPseudoFull(false);
    window.addEventListener("keydown", esc);
    return () => window.removeEventListener("keydown", esc);
  }, [pseudoFull]);
  const toggleFull = async () => {
    if (pseudoFull) return setPseudoFull(false);
    if (document.fullscreenElement) return void (await document.exitFullscreen());
    if (root.current?.requestFullscreen) {
      try {
        await root.current.requestFullscreen();
        return;
      } catch {
        // Refused (e.g. an iframe without allowfullscreen): fill the window.
      }
    }
    setPseudoFull(true);
  };

  return (
    <div
      ref={root}
      className={cn(
        "relative flex h-full min-h-[28rem] flex-col bg-bg",
        // Without the Fullscreen API (iOS Safari), fill the window instead.
        pseudoFull && "fixed inset-0 z-[100] h-dvh",
      )}
    >
      <div
        className={cn(
          "flex shrink-0",
          lineup && channels.length === 0 && "max-sm:flex-col",
          lineup && "border-b border-border",
        )}
      >
        {lineup ? (
          <div
            className="shrink-0 border-r border-border bg-bg-subtle"
            style={{ width: channels.length === 0 && view.width < 640 ? "100%" : chCol }}
          >
            {head}
          </div>
        ) : null}
        <div className={cn("flex min-w-0 flex-1", lineup && "h-[104px]")}>
          {lineup ? (
            <OnNowStrip channels={loadedChannels} guide={guide} now={now} onSelect={onSelect} />
          ) : null}
          <div
            className={cn(
              "flex min-w-0 flex-wrap items-center gap-1 bg-surface px-2 py-1.5",
              channels.length === 0 && "max-sm:hidden",
              lineup
                ? "ml-auto w-44 shrink-0 content-center justify-center border-l border-border"
                : "flex-1 border-b border-border sm:px-4",
            )}
          >
            <div
              className={cn(
                "flex min-w-0 items-baseline gap-2",
                lineup ? "w-full justify-center" : "mr-auto",
              )}
            >
              <h2 className="text-sm font-semibold text-fg">{t("Guide")}</h2>
              <span className={cn("text-xs text-fg-muted", lineup && "sr-only")}>
                {tn(channels.length, "{day} · {count} channel", "{day} · {count} channels", {
                  day: dayLabel,
                })}
              </span>
              {loading ? <span className="text-xs text-fg-subtle">{t("Updating…")}</span> : null}
            </div>
            <div className="flex items-center gap-1">
              <Button
                variant="ghost"
                size="sm"
                aria-label={t("Earlier")}
                onClick={() => onShift(-3)}
              >
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
            <Button
              variant="ghost"
              size="sm"
              aria-label={full ? t("Exit full screen") : t("Full screen guide")}
              aria-pressed={full}
              onClick={() => void toggleFull()}
            >
              {full ? <Minimize2 size={15} /> : <Maximize2 size={15} />}
            </Button>
          </div>
        </div>
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
            width: e.currentTarget.clientWidth,
          })
        }
        className="app-scroll relative min-h-0 flex-1 overflow-auto bg-bg"
      >
        {channels.length > 0 ? (
          <div
            className="relative"
            style={{
              width: chCol + width,
              height: HEADER + channels.length * ROW + (selection?.ids.size ? 100 : 0),
            }}
          >
            {/* time ruler */}
            <div
              className="sticky top-0 z-30 flex border-b border-border bg-surface/95 backdrop-blur"
              style={{ height: HEADER }}
            >
              <div
                className="sticky left-0 z-40 flex shrink-0 items-center border-r border-border bg-surface px-3 text-[11px] font-semibold uppercase tracking-wider text-fg-subtle"
                style={{ width: chCol }}
              >
                {selection ? (
                  <Checkbox
                    className="mr-3"
                    checked={selection.all}
                    ref={(el) => {
                      if (el) el.indeterminate = !selection.all && selection.ids.size > 0;
                    }}
                    disabled={selection.busy || !channels.length}
                    aria-label={t("Select all matches")}
                    onChange={selection.toggleAll}
                  />
                ) : null}
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
              style={{ left: chCol, top: HEADER, width }}
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
              if (!c)
                return (
                  <div
                    key={`pending-${i}`}
                    role="row"
                    aria-busy="true"
                    aria-rowindex={i + 1}
                    className="absolute left-0 flex border-b border-border/60 bg-bg-subtle/40"
                    style={{ top: HEADER + i * ROW, height: ROW, width: chCol + width }}
                  >
                    <div
                      className="sticky left-0 z-20 flex shrink-0 items-center border-r border-border bg-surface px-4"
                      style={{ width: chCol }}
                      aria-hidden
                    >
                      <div className="h-3 w-2/3 animate-pulse rounded bg-fg-subtle/15" />
                    </div>
                  </div>
                );
              const ps = guide.get(c.id) ?? [];
              const on = c.id === selected;
              const live = onNow(ps, now);
              return (
                <div
                  key={c.id}
                  role="row"
                  aria-selected={selection ? selection.ids.has(c.id) : undefined}
                  aria-rowindex={i + 1}
                  className={cn(
                    "absolute left-0 flex border-b border-border/60",
                    on ? "bg-primary-subtle/40" : i % 2 ? "bg-bg" : "bg-bg-subtle/40",
                  )}
                  style={{ top: HEADER + i * ROW, height: ROW, width: chCol + width }}
                >
                  {lineup ? (
                    <LineupCell
                      channel={c}
                      live={live}
                      hasGuide={covered?.(c)}
                      on={on}
                      now={now}
                      width={chCol}
                      onSelect={onSelect}
                      onToggleFavorite={onToggleFavorite}
                      selection={selection}
                    />
                  ) : (
                    <button
                      type="button"
                      role="rowheader"
                      onClick={() => onSelect(c.id)}
                      className={cn(
                        "sticky left-0 z-20 flex shrink-0 items-center gap-2.5 border-r border-border px-3 text-left transition-colors hover:bg-surface-hover",
                        on ? "bg-primary-subtle" : "bg-surface",
                      )}
                      style={{ width: chCol }}
                      aria-label={t("Channel {number} {name}", { number: c.number, name: c.name })}
                    >
                      <span className="w-7 shrink-0 text-right font-mono text-[11px] tabular-nums text-fg-subtle">
                        {c.number}
                      </span>
                      <ChannelLogo
                        name={c.name}
                        logo={c.logo}
                        seed={c.id}
                        channelId={c.id}
                        size={32}
                      />
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
                  )}
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
                      // The guide is virtual in both directions. Rendering a
                      // whole day's cards for every visible row blocks drawer
                      // transitions even though most cards are offscreen.
                      const overscan = 60 * ppm;
                      if (
                        e <= s ||
                        x(e) < view.left - overscan ||
                        x(s) > view.left + view.width - chCol + overscan
                      )
                        return null;
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
                                src={programmeArtwork(p.item.thumb)}
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
              // Above programme cards (z-10 on hover) but below the sticky
              // channel column (z-20), so scrolling never draws it over names.
              <div
                className="pointer-events-none absolute bottom-0 z-[15] w-0.5 bg-danger shadow-[0_0_8px] shadow-danger/60"
                style={{ left: chCol + nowX - 1, top: HEADER }}
                aria-hidden
              />
            ) : null}
          </div>
        ) : null}
        {channels.length === 0 && emptyHint ? (
          <div className="absolute inset-0 grid place-items-center px-6 py-10 text-center">
            <div className="max-w-sm">{emptyHint}</div>
          </div>
        ) : channels.length === 0 ? (
          <div className="absolute inset-0 grid place-items-center px-6 py-10 text-center">
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
      {bulkActions ? (
        <div className="pointer-events-none absolute inset-x-3 bottom-3 z-40 flex justify-center">
          {bulkActions}
        </div>
      ) : null}
    </div>
  );
}

/** A lineup row's channel cell: what the Live TV lineup shows for a channel. */
function LineupCell({
  channel: c,
  live,
  hasGuide,
  on,
  now,
  width,
  onSelect,
  onToggleFavorite,
  selection,
}: {
  channel: Channel;
  live: Programme | undefined;
  hasGuide: boolean | undefined;
  on: boolean;
  now: number;
  width: number;
  onSelect: (id: string) => void;
  onToggleFavorite?: (c: Channel) => void;
  selection?: ChannelSelection;
}) {
  const checked = selection?.ids.has(c.id) ?? false;
  return (
    <div
      role="rowheader"
      className={cn(
        "group sticky left-0 z-20 flex shrink-0 items-center border-r border-border",
        on || checked ? "bg-primary-subtle" : "bg-surface",
      )}
      style={{ width }}
    >
      {selection ? (
        <div className="flex w-8 shrink-0 justify-end">
          <Checkbox
            checked={checked}
            disabled={selection.busy}
            aria-label={t("Select {channel}", { channel: c.name })}
            onChange={(e) => selection.toggle(c.id, (e.nativeEvent as MouseEvent).shiftKey)}
          />
        </div>
      ) : null}
      <button
        type="button"
        onClick={() => onSelect(c.id)}
        aria-current={on ? "true" : undefined}
        aria-label={`${c.number} ${c.name}`}
        className={cn(
          "flex h-full min-w-0 flex-1 items-center gap-2.5 px-2 text-left transition-colors hover:bg-surface-hover focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-ring/50",
          !c.enabled && "opacity-55",
        )}
      >
        <span
          className={cn(
            "w-7 shrink-0 text-right font-mono text-xs tabular-nums",
            on ? "text-primary" : "text-fg-subtle",
          )}
        >
          {c.number}
        </span>
        <ChannelLogo name={c.name} logo={c.logo} seed={c.id} channelId={c.id} size={34} />
        <span className="min-w-0 flex-1">
          <span className="flex items-center gap-1">
            <span className={cn("truncate text-sm font-medium", on ? "text-primary" : "text-fg")}>
              {c.name}
            </span>
            {c.health === "unavailable" ? (
              <Badge variant="secondary">{t("Unavailable")}</Badge>
            ) : null}
            {c.favorite ? (
              <Star
                size={11}
                className="shrink-0 fill-warning text-warning"
                aria-label={t("Favourite")}
              />
            ) : null}
          </span>
          {live ? (
            <span className="block">
              <span className="block truncate text-xs text-fg-muted">{live.item.title}</span>
              <span className="mt-1 block h-0.5 overflow-hidden rounded-full bg-border">
                <span
                  className="block h-full rounded-full bg-primary/70"
                  style={{ width: `${progress(live, now) * 100}%` }}
                />
              </span>
            </span>
          ) : (
            <span className="block truncate text-xs text-fg-subtle">
              {!c.enabled
                ? t("Off air")
                : hasGuide === false
                  ? t("No guide data")
                  : c.group || (c.kind === "virtual" ? t("Virtual") : t("Live"))}
            </span>
          )}
        </span>
        {hasGuide === false && c.enabled ? (
          <AlertTriangle
            size={13}
            className="shrink-0 text-warning"
            aria-label={t("No guide data")}
          />
        ) : null}
      </button>
      {onToggleFavorite ? (
        <button
          type="button"
          onClick={() => onToggleFavorite(c)}
          aria-label={
            c.favorite
              ? t("Unfavourite {channel}", { channel: c.name })
              : t("Favourite {channel}", { channel: c.name })
          }
          aria-pressed={c.favorite}
          className="absolute right-2 top-1/2 hidden -translate-y-1/2 rounded-md bg-surface p-1 text-fg-subtle shadow-sm hover:text-warning focus-visible:block group-hover:block"
        >
          <Star size={13} className={cn(c.favorite && "fill-warning text-warning")} />
        </button>
      ) : null}
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
    const seen = new Set<string>();
    return live
      .filter(({ p }) => {
        const title = p.item.title.trim().toLowerCase();
        if (seen.has(title)) return false;
        seen.add(title);
        return true;
      })
      .slice(0, 10);
  }, [channels, guide, now]);
  if (!picks.length)
    return (
      <section
        aria-label={t("On now")}
        className="relative isolate flex min-w-0 flex-1 items-center justify-center overflow-hidden bg-bg-subtle px-5 py-3"
      >
        <div
          aria-hidden
          className="pointer-events-none absolute inset-0 bg-gradient-to-br from-primary-subtle via-surface to-bg"
        />
        <div
          aria-hidden
          className="pointer-events-none absolute -right-6 -top-12 size-44 rounded-full border border-primary/15 bg-primary/5 motion-safe:animate-pulse"
        />
        <div
          aria-hidden
          className="pointer-events-none absolute inset-x-0 top-1/2 h-px bg-gradient-to-r from-transparent via-primary/25 to-transparent"
        />
        <div className="relative flex min-w-0 flex-col items-center gap-2 text-center sm:flex-row sm:gap-4 sm:text-left">
          <span
            className="hidden size-9 shrink-0 items-center justify-center rounded-full border border-primary/20 bg-surface/60 text-primary sm:inline-flex"
            aria-hidden
          >
            <Tv size={17} />
          </span>
          <p className="text-sm font-medium text-fg">{t("Your next channel starts here.")}</p>
        </div>
      </section>
    );
  return (
    <section aria-label={t("On now")} className="flex min-w-0 flex-1 items-center bg-surface px-2">
      <div className="sr-only">
        <span className="relative flex size-2">
          <span className="absolute inline-flex size-full animate-ping rounded-full bg-danger/60" />
          <span className="relative inline-flex size-2 rounded-full bg-danger" />
        </span>
        <h2 className="text-xs font-semibold uppercase tracking-wider text-fg-subtle">
          {t("On now")}
        </h2>
      </div>
      <div className="app-scroll flex w-full gap-2 overflow-x-auto py-1 [scrollbar-gutter:auto]">
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
              {p.item.thumb || c.logo ? (
                <Artwork
                  src={programmeArtwork(p.item.thumb) || c.logo}
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
