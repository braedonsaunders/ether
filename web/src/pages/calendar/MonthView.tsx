import { Popover, cn } from "@braedonsaunders/appkit-ui";
import { Check } from "lucide-react";
import { useEffect, useRef, useState, type KeyboardEvent } from "react";
import { t, tn } from "../../i18n";
import { DayList } from "./DayList";
import { EntryArt, StateDot, entryLabel } from "./parts";
import {
  addDays,
  episodeCode,
  formatDay,
  stateOn,
  type CalendarEntry,
  type CalendarViewer,
} from "./model";

/** Chip height plus the gap above it, and the day header with padding. */
const CHIP = 28;
const CELL_CHROME = 42;

/**
 * Six weeks around the month, Sonarr-style: each day lists its releases as
 * colour-coded chips (green available, teal today, blue upcoming, amber not
 * playable yet). Arrow keys move between days; Enter opens a day.
 */
export function MonthView({
  from,
  month,
  byDay,
  today,
  viewers,
  focusDay,
  onFocusDay,
  selectedDay,
  onSelectDay,
  onOpen,
  wide,
}: {
  from: string;
  /** YYYY-MM of the month being shown. */
  month: string;
  byDay: Map<string, CalendarEntry[]>;
  today: string;
  viewers: Map<number, CalendarViewer>;
  focusDay: string;
  onFocusDay: (day: string, opts?: { focus?: boolean }) => void;
  selectedDay: string | null;
  onSelectDay: (day: string | null) => void;
  onOpen: (e: CalendarEntry) => void;
  /** Wide screens show chips and day popovers; narrow ones dots and a day panel. */
  wide: boolean;
}) {
  const days = Array.from({ length: 42 }, (_, i) => addDays(from, i));
  const weekdays = days.slice(0, 7);
  const grid = useRef<HTMLDivElement>(null);
  const [focusPending, setFocusPending] = useState(false);
  // As many chips per day as the rows have room for; the rest go behind "+N more".
  const [fit, setFit] = useState(3);
  useEffect(() => {
    const el = grid.current;
    if (!el || typeof ResizeObserver === "undefined") return;
    const measure = () => {
      const row = el.clientHeight / 6;
      if (row > 0) setFit(Math.max(1, Math.floor((row - CELL_CHROME) / CHIP)));
    };
    measure();
    const ro = new ResizeObserver(measure);
    ro.observe(el);
    return () => ro.disconnect();
  }, []);

  // Move DOM focus with the roving day once the grid shows it.
  useEffect(() => {
    if (!focusPending) return;
    const el = grid.current?.querySelector<HTMLElement>(`[data-day="${focusDay}"]`);
    if (el) {
      el.focus();
      setFocusPending(false);
    }
  }, [focusDay, focusPending, from]);

  const move = (day: string) => {
    setFocusPending(true);
    onFocusDay(day, { focus: true });
  };

  const onKey = (e: KeyboardEvent<HTMLDivElement>, day: string) => {
    if (e.target !== e.currentTarget) return;
    const dow = days.indexOf(day) % 7;
    const step: Record<string, number> = {
      ArrowLeft: -1,
      ArrowRight: 1,
      ArrowUp: -7,
      ArrowDown: 7,
      Home: -dow,
      End: 6 - dow,
    };
    if (e.key in step) {
      e.preventDefault();
      move(addDays(day, step[e.key]!));
    } else if (e.key === "Enter" || e.key === " ") {
      e.preventDefault();
      onSelectDay(day);
    }
  };

  return (
    <div className="flex flex-1 flex-col">
      <div className="grid shrink-0 grid-cols-7 border-b border-border bg-surface/95" aria-hidden>
        {weekdays.map((d) => (
          <div
            key={d}
            className="px-2 py-2 text-[11px] font-semibold uppercase tracking-wider text-fg-subtle"
          >
            {formatDay.weekday(d)}
          </div>
        ))}
      </div>
      <div
        ref={grid}
        role="grid"
        aria-label={t("Release calendar")}
        className="grid flex-1 grid-cols-7 grid-rows-[repeat(6,minmax(4.5rem,1fr))] gap-px bg-border/60 sm:grid-rows-[repeat(6,minmax(6.5rem,1fr))]"
      >
        {Array.from({ length: 6 }, (_, w) => (
          <div key={w} role="row" className="contents">
            {days.slice(w * 7, w * 7 + 7).map((day) => {
              const list = byDay.get(day) ?? [];
              const inMonth = day.startsWith(month);
              const isToday = day === today;
              const past = day < today;
              const focused = day === focusDay;
              const shown = list.length <= fit ? list.length : Math.max(1, fit - 1);
              const extra = list.length - shown;
              const label =
                day.endsWith("-01") && !isToday
                  ? `${formatDay.monthShort(day)} ${formatDay.number(day)}`
                  : formatDay.number(day);
              const dayNumber = cn(
                "grid h-6 min-w-6 place-items-center rounded-full px-1 text-xs font-semibold tabular-nums",
                isToday
                  ? "bg-primary text-primary-fg shadow-sm shadow-primary/40"
                  : inMonth
                    ? "text-fg"
                    : "text-fg-subtle",
              );
              return (
                <div
                  key={day}
                  role="gridcell"
                  data-day={day}
                  tabIndex={focused ? 0 : -1}
                  aria-selected={selectedDay === day}
                  aria-current={isToday ? "date" : undefined}
                  aria-label={`${formatDay.long(day)}${list.length ? `, ${tn(list.length, "{count} release", "{count} releases")}` : ""}`}
                  onKeyDown={(e) => onKey(e, day)}
                  onFocus={() => {
                    if (!focused) onFocusDay(day);
                  }}
                  onClick={(e) => {
                    if (e.target === e.currentTarget) onSelectDay(day);
                  }}
                  className={cn(
                    "group/day relative flex min-h-0 min-w-0 flex-col gap-1 overflow-hidden p-1.5 outline-none transition-colors focus-visible:z-10 focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-ring/60",
                    "bg-bg",
                    !inMonth && "[&>*]:opacity-55",
                    isToday && "bg-primary/[0.06]",
                    selectedDay === day && "ring-2 ring-inset ring-primary/50",
                  )}
                >
                  <div className="flex items-center justify-between gap-1">
                    {list.length && wide ? (
                      <Popover
                        open={selectedDay === day}
                        onOpenChange={(o) => onSelectDay(o ? day : null)}
                        align="start"
                        className="w-80 p-0"
                        trigger={
                          <button
                            type="button"
                            tabIndex={-1}
                            aria-label={t("Show {day}", { day: formatDay.long(day) })}
                            onClick={() => onSelectDay(selectedDay === day ? null : day)}
                            className={cn(dayNumber, "hover:ring-2 hover:ring-border-strong")}
                          >
                            {label}
                          </button>
                        }
                      >
                        <DayList
                          day={day}
                          entries={list}
                          today={today}
                          viewers={viewers}
                          onOpen={(e) => {
                            onSelectDay(null);
                            onOpen(e);
                          }}
                          compact
                        />
                      </Popover>
                    ) : (
                      <span className={cn(dayNumber, "pointer-events-none")}>{label}</span>
                    )}
                    {list.length ? (
                      <span className="pointer-events-none hidden text-[10px] font-medium tabular-nums text-fg-subtle sm:inline">
                        {list.length}
                      </span>
                    ) : null}
                  </div>
                  {/* Narrow screens: a dot per release; tap the day for its list. */}
                  <div className="pointer-events-none flex flex-wrap gap-1 sm:hidden">
                    {list.slice(0, 6).map((e) => (
                      <StateDot key={e.id} state={stateOn(e, today)} />
                    ))}
                  </div>
                  <ul className={cn("hidden min-h-0 flex-col gap-1 sm:flex", past && "opacity-80")}>
                    {list.slice(0, shown).map((e) => (
                      <li key={e.id}>
                        <Chip entry={e} today={today} onOpen={onOpen} />
                      </li>
                    ))}
                  </ul>
                  {extra > 0 ? (
                    <button
                      type="button"
                      tabIndex={-1}
                      onClick={() => onSelectDay(selectedDay === day ? null : day)}
                      className="hidden w-full rounded-md px-1.5 py-0.5 text-left text-[11px] font-semibold text-fg-muted hover:bg-surface-hover hover:text-fg sm:block"
                    >
                      {t("+{count} more", { count: extra })}
                    </button>
                  ) : null}
                </div>
              );
            })}
          </div>
        ))}
      </div>
    </div>
  );
}

function Chip({
  entry,
  today,
  onOpen,
}: {
  entry: CalendarEntry;
  today: string;
  onOpen: (e: CalendarEntry) => void;
}) {
  const state = stateOn(entry, today);
  const code = entry.kind === "episode" ? episodeCode(entry) : "";
  return (
    <button
      type="button"
      tabIndex={-1}
      onClick={() => onOpen(entry)}
      aria-label={entryLabel(entry, state)}
      title={entryLabel(entry, state)}
      className="group/chip relative flex h-6 w-full items-center gap-1.5 overflow-hidden rounded-md bg-surface pl-1.5 pr-1 text-left shadow-xs ring-1 ring-border transition hover:-translate-y-px hover:shadow-md hover:ring-border-strong"
    >
      <StateBar state={state} />
      <EntryArt entry={entry} variant="poster" className="h-5 w-[0.85rem] shrink-0 rounded-[3px]" />
      <span className="min-w-0 flex-1 truncate text-[11px] font-medium text-fg">{entry.title}</span>
      {code ? (
        <span className="hidden shrink-0 font-mono text-[10px] text-fg-subtle 2xl:inline">
          {code}
        </span>
      ) : null}
      {entry.badges?.includes("finale") || entry.badges?.some((b) => b !== "finale") ? (
        <span
          className={cn(
            "size-1.5 shrink-0 rounded-full",
            entry.badges?.includes("finale") ? "bg-danger" : "bg-primary",
          )}
          aria-hidden
        />
      ) : null}
      {entry.watched?.length ? (
        <Check size={11} className="shrink-0 text-success" aria-hidden />
      ) : null}
    </button>
  );
}

function StateBar({ state }: { state: ReturnType<typeof stateOn> }) {
  return (
    <span
      className={cn(
        "absolute inset-y-0 left-0 w-[3px]",
        state === "available"
          ? "bg-success"
          : state === "today"
            ? "bg-primary"
            : state === "upcoming"
              ? "bg-info"
              : "bg-warning",
      )}
      aria-hidden
    />
  );
}
