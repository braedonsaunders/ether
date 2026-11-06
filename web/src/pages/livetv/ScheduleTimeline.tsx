import { memo, useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";
import { createPortal } from "react-dom";
import { Button } from "@braedonsaunders/appkit-ui";
import { Minus, Plus, Pin } from "lucide-react";
import { ApiError, apiFetch } from "../../lib/api";
import { t, getLocale } from "../../i18n";
import {
  HOUR,
  flattenElements,
  daypartBands,
  TIMELINE_KINDS,
  kindLabel,
  layoutElements,
  tooltipContent,
  timeLabel,
  type TimelineElement,
  type TimelineRange,
  type TimelineResponse,
} from "./schedule-layout";
import "./schedule-timeline.css";
export type { TimelineElement, TimelineRange, TimelineResponse } from "./schedule-layout";
function validElement(e: TimelineElement, depth = 0): boolean {
  return (
    !!e &&
    depth <= 8 &&
    typeof e.id === "string" &&
    typeof e.title === "string" &&
    typeof e.rule === "string" &&
    TIMELINE_KINDS.includes(e.kind) &&
    Number.isFinite(e.start) &&
    Number.isFinite(e.end) &&
    e.end > e.start &&
    (e.children === undefined ||
      (Array.isArray(e.children) && e.children.every((child) => validElement(child, depth + 1)))) &&
    (e.breakPoints === undefined ||
      (Array.isArray(e.breakPoints) &&
        e.breakPoints.every(
          (b) =>
            b &&
            Number.isFinite(b.atMs) &&
            typeof b.source === "string" &&
            (b.confidence === undefined || Number.isFinite(b.confidence)),
        )))
  );
}
const NowMarker = memo(function NowMarker({
  from,
  to,
  scale,
}: {
  from: number;
  to: number;
  scale: number;
}) {
  const [now, setNow] = useState(Date.now());
  useEffect(() => {
    const timer = setInterval(() => setNow(Date.now()), 15000);
    return () => clearInterval(timer);
  }, []);
  return now >= from && now < to ? (
    <div className="schedule-now" style={{ left: (now - from) * scale }} aria-label={t("Now")}>
      <span>{t("Now")}</span>
    </div>
  ) : null;
});
export function ScheduleTimeline({
  channelId,
  range,
  onSelect,
  onAdd,
  selectedId,
}: {
  channelId: string;
  range: TimelineRange;
  onSelect?: (element: TimelineElement) => void;
  onAdd?: (value: { start: number; blockId?: string }) => void;
  selectedId?: string;
}) {
  const root = useRef<HTMLDivElement>(null),
    scroll = useRef<HTMLDivElement>(null);
  const [width, setWidth] = useState(1),
    [left, setLeft] = useState(0),
    [hours, setHours] = useState(2);
  const [data, setData] = useState<TimelineResponse>(),
    [error, setError] = useState("");
  const [active, setActive] = useState<string>(),
    [tip, setTip] = useState<{ el: TimelineElement; x: number; y: number }>();
  const pending = useRef<{ at: number; fraction: number } | undefined>(undefined),
    pointers = useRef(new Map<number, number>()),
    pinch = useRef<number | undefined>(undefined);
  useLayoutEffect(() => {
    const el = root.current;
    if (!el) return;
    const measure = () => setWidth(Math.max(1, el.clientWidth));
    measure();
    const ro = new ResizeObserver(measure);
    ro.observe(el);
    return () => ro.disconnect();
  }, []);
  useEffect(() => {
    let live = true;
    setData(undefined);
    setError("");
    const controller = new AbortController();
    let timer: ReturnType<typeof setTimeout>;
    const load = () =>
      apiFetch<TimelineResponse>(
        `/livetv/channels/${encodeURIComponent(channelId)}/timeline?from=${range.from}&to=${range.to}`,
        { signal: controller.signal },
      )
        .then((d) => {
          if (
            !d ||
            typeof d !== "object" ||
            !Array.isArray(d.elements) ||
            !Array.isArray(d.dayparts) ||
            typeof d.timezone !== "string" ||
            !d.timezone ||
            !d.dayparts.every(
              (b) =>
                b && Number.isFinite(b.start) && Number.isFinite(b.end) && Array.isArray(b.days),
            )
          )
            throw new Error("Invalid timeline snapshot");
          new Intl.DateTimeFormat("en", { timeZone: d.timezone }).format(0);
          // An element kind this console doesn't know yet is skipped, so it can't
          // hide the whole schedule; a malformed element of a known kind is still
          // an invalid snapshot.
          const elements = d.elements.filter((e) => !!e && TIMELINE_KINDS.includes(e.kind));
          if (!elements.every((e) => validElement(e))) throw new Error("Invalid timeline snapshot");
          if (live) setData({ ...d, elements });
        })
        .catch((error) => {
          if (!live) return;
          if (error instanceof ApiError && error.status === 503) timer = setTimeout(load, 2000);
          else setError(t("Unable to load schedule"));
        });
    void load();
    return () => {
      live = false;
      controller.abort();
      clearTimeout(timer);
    };
  }, [channelId, range.from, range.to]);
  const scale = width / (hours * HOUR),
    total = Math.max(width, (range.to - range.from) * scale);
  const items = useMemo(
    () => layoutElements(data?.elements ?? [], range, width, hours),
    [data, range.from, range.to, width, hours],
  );
  const bands = useMemo(
    () => (data ? daypartBands(data, range) : []),
    [data, range.from, range.to],
  );
  const visible = items.filter(
    (i) => i.left + i.width >= left - 100 && i.left <= left + width + 100,
  );
  const zoom = (factor: number, fraction = 0.5) => {
    pending.current = { at: range.from + (left + width * fraction) / scale, fraction };
    setHours((h) => Math.max(1, Math.min(168, h * factor)));
    setTip(undefined);
  };
  useLayoutEffect(() => {
    if (pending.current && scroll.current) {
      scroll.current.scrollLeft =
        (pending.current.at - range.from) * scale - width * pending.current.fraction;
      setLeft(scroll.current.scrollLeft);
      pending.current = undefined;
    }
  }, [scale, width, range.from]);
  useEffect(() => {
    const el = scroll.current;
    if (!el) return;
    const wheel = (e: WheelEvent) => {
      if (e.ctrlKey || e.metaKey) {
        e.preventDefault();
        zoom(Math.exp(e.deltaY * 0.005), (e.clientX - el.getBoundingClientRect().left) / width);
      }
    };
    el.addEventListener("wheel", wheel, { passive: false });
    return () => el.removeEventListener("wheel", wheel);
  });
  const jump = (at: number) => {
    if (scroll.current) {
      scroll.current.scrollLeft = (at - range.from) * scale - width / 3;
      setLeft(scroll.current.scrollLeft);
    }
  };
  const focusItem = (index: number) => {
    const item = items[Math.max(0, Math.min(items.length - 1, index))];
    if (!item) return;
    setActive(item.el.id);
    jump(item.el.start);
    requestAnimationFrame(() =>
      root.current
        ?.querySelector<HTMLButtonElement>(`[data-element-id="${CSS.escape(item.el.id)}"]`)
        ?.focus(),
    );
  };
  const showTip = (el: TimelineElement, target: HTMLElement) => {
    const r = target.getBoundingClientRect();
    setTip({
      el,
      x: Math.max(8, Math.min(r.left, document.documentElement.clientWidth - 328)),
      y: Math.max(8, Math.min(r.bottom + 8, document.documentElement.clientHeight - 280)),
    });
  };
  const add = (start: number) =>
    onAdd?.({
      start,
      blockId: data?.elements.find((e) => e.start <= start && e.end > start)?.blockId,
    });
  const ticks = [];
  const tickHours = Math.max(1, Math.ceil((hours * 70) / width));
  for (
    let at = Math.ceil((range.from + left / scale) / HOUR) * HOUR;
    at < Math.min(range.to, range.from + (left + width + 100) / scale);
    at += HOUR
  )
    ticks.push(at);
  const preview =
    tip?.el ??
    items.find((i) => i.el.id === (active ?? selectedId))?.el ??
    visible.find((i) => i.lane === 0)?.el;
  const locale = getLocale(),
    timezone = data?.timezone ?? "UTC";
  const dayFormat = new Intl.DateTimeFormat(locale, {
    timeZone: timezone,
    weekday: "short",
    day: "numeric",
    month: "short",
  });
  const hourFormat = new Intl.DateTimeFormat(locale, {
    timeZone: timezone,
    hour: "numeric",
    minute: "2-digit",
  });
  const midnightFormat = new Intl.DateTimeFormat("en-US", {
    timeZone: timezone,
    hour: "numeric",
    hourCycle: "h23",
  });
  return (
    <div
      ref={root}
      className="schedule-timeline min-w-0 space-y-3"
      data-viewport-width={width}
      data-visible-hours={hours}
      onKeyDown={(e) => {
        if (e.key === "+" || e.key === "=") {
          e.preventDefault();
          zoom(0.5);
        }
        if (e.key === "-") {
          e.preventDefault();
          zoom(2);
        }
        if (e.key === "Home") {
          e.preventDefault();
          jump(Date.now());
        }
        if (["ArrowRight", "ArrowDown", "ArrowLeft", "ArrowUp"].includes(e.key)) {
          e.preventDefault();
          focusItem(
            items.findIndex((i) => i.el.id === active) +
              (["ArrowLeft", "ArrowUp"].includes(e.key) ? -1 : 1),
          );
        }
        if (e.key === "Escape") setTip(undefined);
      }}
    >
      <div className="flex flex-wrap items-center justify-between gap-2">
        <span className="text-xs text-fg-muted">{timezone}</span>
        <div className="flex items-center gap-1">
          <Button size="sm" variant="ghost" onClick={() => jump(Date.now())}>
            {t("Now")}
          </Button>
          <Button
            size="sm"
            variant="ghost"
            aria-label={t("Zoom out")}
            disabled={hours === 168}
            onClick={() => zoom(2)}
          >
            <Minus size={14} />
          </Button>
          <output className="min-w-14 text-center text-xs tabular-nums">
            {t("{hours} h", { hours: Math.round(hours * 10) / 10 })}
          </output>
          <Button
            size="sm"
            variant="ghost"
            aria-label={t("Zoom in")}
            disabled={hours === 1}
            onClick={() => zoom(0.5)}
          >
            <Plus size={14} />
          </Button>
          {onAdd && (
            <Button
              size="sm"
              variant="secondary"
              onClick={() =>
                add(Math.max(range.from, Math.min(range.to - 1, range.from + left / scale)))
              }
            >
              <Plus size={14} />
              {t("Add")}
            </Button>
          )}
        </div>
      </div>
      {preview && (
        <div className="min-w-0 rounded-lg border border-border bg-surface px-3 py-2">
          <p className="text-sm font-medium text-fg">{preview.title}</p>
          <p className="text-xs text-fg-muted">{preview.subtitle || kindLabel(preview.kind)}</p>
          <p className="text-xs tabular-nums text-fg-subtle">
            {timeLabel(preview.start, timezone, locale)} –{" "}
            {timeLabel(preview.end, timezone, locale)}
          </p>
        </div>
      )}
      <div
        ref={scroll}
        className="schedule-scroll rounded-xl border border-border bg-bg-subtle"
        role="region"
        aria-label={t("Channel schedule")}
        tabIndex={0}
        onScroll={(e) => {
          setLeft(e.currentTarget.scrollLeft);
          setTip(undefined);
        }}
        onPointerDown={(e) => {
          pointers.current.set(e.pointerId, e.clientX);
          if (pointers.current.size === 2)
            pinch.current = Math.abs(
              [...pointers.current.values()][0]! - [...pointers.current.values()][1]!,
            );
        }}
        onPointerMove={(e) => {
          if (!pointers.current.has(e.pointerId)) return;
          pointers.current.set(e.pointerId, e.clientX);
          if (pointers.current.size === 2) {
            const p = [...pointers.current.values()],
              distance = Math.abs(p[0]! - p[1]!);
            if (pinch.current && distance > 0) zoom(pinch.current / distance);
            pinch.current = distance;
          }
        }}
        onPointerUp={(e) => {
          pointers.current.delete(e.pointerId);
          pinch.current = undefined;
        }}
        onPointerCancel={() => {
          pointers.current.clear();
          pinch.current = undefined;
        }}
      >
        <div
          className="schedule-surface"
          style={{ width: total }}
          onClick={(e) => {
            if (e.target === e.currentTarget)
              add(range.from + (e.clientX - e.currentTarget.getBoundingClientRect().left) / scale);
          }}
        >
          {bands
            .filter(
              (b) =>
                (b.end - range.from) * scale > left &&
                (b.start - range.from) * scale < left + width,
            )
            .map((b) => (
              <div
                key={`${b.id}-${b.start}`}
                className="schedule-band"
                style={{ left: (b.start - range.from) * scale, width: (b.end - b.start) * scale }}
              >
                <span style={{ position: "sticky", left: 8 }}>{b.name}</span>
              </div>
            ))}
          {ticks.map((at) => (
            <div
              key={at}
              className={`schedule-grid ${midnightFormat.format(at) === "00" ? "schedule-day" : ""}`}
              style={{ left: (at - range.from) * scale }}
            >
              <span
                style={{
                  visibility:
                    (hours <= 48 && new Date(at).getUTCHours() % tickHours === 0) ||
                    midnightFormat.format(at) === "00"
                      ? "visible"
                      : "hidden",
                }}
              >
                {dayFormat.format(at)}
              </span>
              <small
                style={{
                  visibility:
                    (hours <= 48 && new Date(at).getUTCHours() % tickHours === 0) ||
                    midnightFormat.format(at) === "00"
                      ? "visible"
                      : "hidden",
                }}
              >
                {hourFormat.format(at)}
              </small>
            </div>
          ))}
          {visible.map(({ el, left: x, width: w, detail, lane }) => (
            <button
              key={el.id}
              data-element-id={el.id}
              data-label-detail={detail}
              className={`schedule-element schedule-${el.kind}`}
              style={{
                left: x,
                width: w,
                padding: w < 64 || lane > 0 ? 0 : "3px 6px",
                top: lane === 0 ? 30 : lane === 1 ? 66 : 80,
                height: lane === 0 ? 32 : 11,
              }}
              type="button"
              tabIndex={active === el.id || (!active && items[0]?.el.id === el.id) ? 0 : -1}
              aria-label={tooltipContent(el, timezone, [], locale)}
              aria-describedby={tip?.el.id === el.id ? "schedule-tooltip" : undefined}
              aria-pressed={selectedId === el.id}
              onClick={() => onSelect?.(el)}
              onFocus={(e) => {
                setActive(el.id);
                showTip(el, e.currentTarget);
              }}
              onBlur={() => setTip(undefined)}
              onMouseEnter={(e) => showTip(el, e.currentTarget)}
              onMouseLeave={() => setTip(undefined)}
            >
              <span
                className="schedule-label"
                style={{
                  maxWidth: Math.max(0, Math.min(w - 12, width - 24)),
                  marginLeft: Math.max(0, left - x),
                }}
              >
                {el.pinned && <Pin size={12} className="inline" />}
                {el.title}
                {detail === "subtitle" && <small>{el.subtitle || kindLabel(el.kind)}</small>}
              </span>
              {el.breakPoints?.map((b, i) => (
                <span
                  key={i}
                  className="schedule-breakpoint"
                  style={{ left: Math.max(0, (el.start + b.atMs - range.from) * scale - x) }}
                  title={t("Break point · {confidence}%", {
                    confidence: Math.round(b.confidence * 100),
                  })}
                  aria-label={t("Break point · {confidence}%", {
                    confidence: Math.round(b.confidence * 100),
                  })}
                />
              ))}
            </button>
          ))}
          <NowMarker from={range.from} to={range.to} scale={scale} />
          {!data && (
            <p className="sticky left-0 w-fit px-4 pt-9 text-xs text-fg-muted" role="status">
              {error || t("Loading…")}
            </p>
          )}
        </div>
      </div>
      <div
        className="flex flex-wrap gap-x-3 gap-y-1 text-xs text-fg-muted"
        aria-label={t("Legend")}
      >
        {TIMELINE_KINDS.map((k) => (
          <span key={k} className="inline-flex items-center gap-1">
            <i className={`schedule-key schedule-${k}`} />
            {kindLabel(k)}
          </span>
        ))}
      </div>
      {tip &&
        createPortal(
          <div
            id="schedule-tooltip"
            role="tooltip"
            className="fixed z-[200] max-h-64 w-80 max-w-[calc(100vw-16px)] overflow-auto whitespace-pre-line rounded-lg border border-border bg-elevated p-3 text-xs text-fg shadow-lg"
            style={{ left: tip.x, top: tip.y }}
            onMouseEnter={() => setTip(tip)}
          >
            {tooltipContent(
              tip.el,
              timezone,
              flattenElements(data?.elements ?? []).filter(
                (e) => e.id !== tip.el.id && e.start >= tip.el.start && e.end <= tip.el.end,
              ) ?? [],
              locale,
            )}
          </div>,
          document.body,
        )}
    </div>
  );
}
