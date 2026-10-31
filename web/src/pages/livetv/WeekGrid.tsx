import { Button, cn } from "@braedonsaunders/appkit-ui";
import { HelpCircle } from "lucide-react";
import { useEffect, useRef, useState } from "react";
import { t } from "../../i18n";
import { blockLabel, timeOfDay, type Block } from "./model";

/** -1 is the every-day shortcut. The rest are Monday-first, Sunday last. */
const ROW_DAYS = [-1, 1, 2, 3, 4, 5, 6, 0];
const WEEK = [0, 1, 2, 3, 4, 5, 6];

function dayLabel(day: number): string {
  if (day < 0) return t("Every day");
  return [
    t("Sunday"),
    t("Monday"),
    t("Tuesday"),
    t("Wednesday"),
    t("Thursday"),
    t("Friday"),
    t("Saturday"),
  ][day]!;
}

function blockDays(block: Block): number[] {
  return block.days?.length ? block.days : WEEK;
}

function shownOn(block: Block, day: number): boolean {
  if (day < 0) return !block.days?.length;
  return !!block.days?.length && block.days.includes(day);
}

function overlaps(blocks: Block[], day: number, start: number, end: number, skip = -1): boolean {
  return blocks.some((block, index) => {
    if (index === skip) return false;
    const days = blockDays(block);
    if (!days.includes(day)) return false;
    return start < block.end && end > block.start;
  });
}

/**
 * A week of dayparts. Drag across a day to paint a block, or use the arrow
 * keys: Enter paints the focused hour, Shift+arrows resize the focused block.
 */
export function WeekGrid({
  blocks,
  onChange,
  selected = null,
  onSelect,
}: {
  blocks: Block[];
  onChange: (blocks: Block[]) => void;
  selected?: number | null;
  onSelect?: (index: number | null) => void;
}) {
  const drag = useRef<{ day: number; anchor: number; current: number } | null>(null);
  const scroller = useRef<HTMLDivElement>(null);
  const [ghost, setGhost] = useState<{ day: number; start: number; end: number } | null>(null);
  const [focus, setFocus] = useState<{ day: number; hour: number }>({ day: 1, hour: 8 });

  useEffect(() => {
    const el = scroller.current;
    if (!el) return;
    const now = new Date().getHours();
    const target = now < 6 ? 6 : now;
    const dayCol = el.querySelector("[role=rowheader]")?.getBoundingClientRect().width ?? 72;
    const hourWidth = (el.scrollWidth - dayCol) / 24;
    el.scrollLeft = Math.max(0, Math.min(el.scrollWidth - el.clientWidth, target * hourWidth));
  }, []);

  const paint = (day: number, start: number, end: number) => {
    const s = Math.max(0, Math.min(start, end));
    const e = Math.min(1440, Math.max(start, end));
    const check = day < 0 ? WEEK : [day];
    if (e - s < 30 || check.some((d) => overlaps(blocks, d, s, e))) return;
    onChange([
      ...blocks,
      { start: s, end: e, ...(day < 0 ? {} : { days: [day] }), items: [], shuffle: false },
    ]);
    onSelect?.(blocks.length);
  };

  const onKey = (event: React.KeyboardEvent) => {
    const { day, hour } = focus;
    if (event.key === "ArrowRight" || event.key === "ArrowLeft") {
      event.preventDefault();
      const delta = event.key === "ArrowRight" ? 1 : -1;
      setFocus({ day, hour: Math.max(0, Math.min(23, hour + delta)) });
    } else if (event.key === "ArrowDown" || event.key === "ArrowUp") {
      event.preventDefault();
      const at = ROW_DAYS.indexOf(day);
      const next =
        ROW_DAYS[
          Math.max(0, Math.min(ROW_DAYS.length - 1, at + (event.key === "ArrowDown" ? 1 : -1)))
        ]!;
      setFocus({ day: next, hour });
    } else if (event.key === "Enter") {
      event.preventDefault();
      paint(day, hour * 60, (hour + 1) * 60);
    } else if (event.key === "Delete" || event.key === "Backspace") {
      const minute = hour * 60;
      const hit = blocks.findIndex(
        (block) => shownOn(block, day) && minute >= block.start && minute < block.end,
      );
      if (hit >= 0) {
        event.preventDefault();
        onChange(blocks.filter((_, index) => index !== hit));
      }
    }
  };

  const help = t(
    "Drag across a day to paint a block. Arrow keys move, Enter paints the focused hour, Delete removes the block under the cursor.",
  );
  return (
    <div className="space-y-2">
      <div className="flex justify-end">
        <button
          type="button"
          className="grid size-6 place-items-center rounded-full text-fg-muted hover:bg-bg-subtle hover:text-fg"
          title={help}
          aria-label={help}
        >
          <HelpCircle size={14} aria-hidden />
        </button>
      </div>
      <div
        role="grid"
        ref={scroller}
        aria-label={t("Weekly schedule")}
        className="min-w-0 overflow-x-auto rounded-xl border border-border"
        onKeyDown={onKey}
      >
        <div className="min-w-[52rem]">
          <div
            className="grid border-b border-border bg-bg-subtle text-[10px] tabular-nums text-fg-subtle"
            style={{ gridTemplateColumns: "4.5rem repeat(24, minmax(0, 1fr))" }}
          >
            <div className="sticky left-0 z-10 border-r border-border bg-bg-subtle" />
            {Array.from({ length: 24 }, (_, hour) => (
              <div key={hour} className="px-0.5 py-1 text-center">
                {hour % 3 === 0 ? timeOfDay(hour * 60) : ""}
              </div>
            ))}
          </div>
          {ROW_DAYS.map((day) => (
            <div
              key={day}
              role="row"
              className="grid border-b border-border/70 last:border-b-0"
              style={{ gridTemplateColumns: "4.5rem minmax(0, 1fr)" }}
              onPointerUp={() => {
                if (!drag.current || drag.current.day !== day) return;
                const { anchor, current } = drag.current;
                drag.current = null;
                setGhost(null);
                const start = Math.min(anchor, current) * 60;
                const end = (Math.max(anchor, current) + 1) * 60;
                paint(day, start, end);
              }}
              onPointerLeave={() => {
                if (drag.current?.day === day) {
                  drag.current = null;
                  setGhost(null);
                }
              }}
            >
              <div
                role="rowheader"
                className="sticky left-0 z-10 flex items-center border-r border-border bg-bg px-1.5 text-[10px] font-medium leading-tight text-fg-muted"
              >
                {day < 0 ? dayLabel(day) : dayLabel(day).slice(0, 3)}
              </div>
              <div
                className="relative grid"
                style={{ gridTemplateColumns: "repeat(24, minmax(0, 1fr))" }}
              >
                {Array.from({ length: 24 }, (_, hour) => {
                  const on = focus.day === day && focus.hour === hour;
                  return (
                    <button
                      key={hour}
                      type="button"
                      role="gridcell"
                      aria-label={t("{day} at {time}", {
                        day: dayLabel(day),
                        time: timeOfDay(hour * 60),
                      })}
                      tabIndex={on ? 0 : -1}
                      className={cn(
                        "h-9 border-l border-border/40",
                        on && "ring-2 ring-inset ring-primary",
                      )}
                      onFocus={() => setFocus({ day, hour })}
                      onClick={() => paint(day, hour * 60, (hour + 1) * 60)}
                      onPointerDown={(event) => {
                        drag.current = { day, anchor: hour, current: hour };
                        setGhost({ day, start: hour * 60, end: (hour + 1) * 60 });
                        event.currentTarget.releasePointerCapture?.(event.pointerId);
                      }}
                      onPointerEnter={() => {
                        if (!drag.current || drag.current.day !== day) return;
                        drag.current.current = hour;
                        const start = Math.min(drag.current.anchor, hour);
                        const end = Math.max(drag.current.anchor, hour);
                        setGhost({ day, start: start * 60, end: (end + 1) * 60 });
                      }}
                    />
                  );
                })}
                {blocks.map((block, index) => {
                  if (!shownOn(block, day)) return null;
                  const hue = (index * 67 + 200) % 360;
                  const label = blockLabel(block);
                  const on = selected === index;
                  return (
                    <button
                      key={index}
                      type="button"
                      title={label}
                      aria-label={label}
                      aria-pressed={on}
                      onClick={() => onSelect?.(index)}
                      className={cn(
                        "absolute inset-y-1 truncate rounded-md px-1 text-left text-[10px] font-medium leading-7 text-white",
                        on && "ring-2 ring-white",
                      )}
                      style={{
                        left: `${(block.start / 1440) * 100}%`,
                        width: `${((block.end - block.start) / 1440) * 100}%`,
                        background: `linear-gradient(135deg, hsl(${hue} 65% 48%), hsl(${(hue + 28) % 360} 65% 38%))`,
                      }}
                    >
                      {label}
                    </button>
                  );
                })}
                {ghost?.day === day ? (
                  <div
                    className="pointer-events-none absolute inset-y-1 rounded-md bg-primary/30 ring-1 ring-primary"
                    style={{
                      left: `${(ghost.start / 1440) * 100}%`,
                      width: `${((ghost.end - ghost.start) / 1440) * 100}%`,
                    }}
                  />
                ) : null}
              </div>
            </div>
          ))}
        </div>
      </div>
      <div className="flex flex-wrap gap-2">
        <Button
          type="button"
          size="sm"
          variant="outline"
          onClick={() => {
            onChange([
              ...blocks,
              {
                start: 8 * 60,
                end: 12 * 60,
                days: [6],
                name: "Cartoons",
                items: [],
                shuffle: false,
                filter: { type: "show", genres: ["Animation"], maxContentRating: "PG" },
              },
            ]);
            onSelect?.(blocks.length);
          }}
        >
          {t("Saturday morning cartoons")}
        </Button>
        <Button
          type="button"
          size="sm"
          variant="outline"
          onClick={() => {
            onChange([
              ...blocks,
              {
                start: 20 * 60,
                end: 23 * 60,
                days: [5],
                name: "Movie night",
                items: [],
                shuffle: false,
                mode: "shuffle",
                filter: { type: "movie" },
              },
            ]);
            onSelect?.(blocks.length);
          }}
        >
          {t("Movie night")}
        </Button>
        <Button
          type="button"
          size="sm"
          variant="outline"
          onClick={() => {
            onChange(blocks.filter((block) => block.days?.length));
            onSelect?.(null);
          }}
        >
          {t("Clear every-day blocks")}
        </Button>
      </div>
    </div>
  );
}
