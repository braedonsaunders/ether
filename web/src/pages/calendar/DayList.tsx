import { cn } from "@braedonsaunders/appkit-ui";
import { Moon } from "lucide-react";
import { t, tn } from "../../i18n";
import { Badges, EntryArt, StateDot, ViewerStack, entryLabel } from "./parts";
import {
  STATE_META,
  daysBetween,
  entrySubtitle,
  formatDay,
  relativeDay,
  stateOn,
  type CalendarEntry,
  type CalendarViewer,
} from "./model";

/** One day's releases as a compact list (month popover, narrow day panel). */
export function DayList({
  day,
  entries,
  today,
  viewers,
  onOpen,
  compact,
}: {
  day: string;
  entries: CalendarEntry[];
  today: string;
  viewers: Map<number, CalendarViewer>;
  onOpen: (e: CalendarEntry) => void;
  compact?: boolean;
}) {
  const near = Math.abs(daysBetween(today, day)) <= 1;
  return (
    <div className={cn(compact ? "max-h-[26rem] overflow-y-auto" : "")}>
      <div className="sticky top-0 z-10 flex items-baseline justify-between gap-2 border-b border-border bg-surface/95 px-3 py-2 backdrop-blur">
        <p className="text-sm font-semibold text-fg">
          {near ? `${relativeDay(day, today)} · ${formatDay.medium(day)}` : formatDay.long(day)}
        </p>
        <p className="text-xs text-fg-muted">
          {tn(entries.length, "{count} release", "{count} releases")}
        </p>
      </div>
      {entries.length === 0 ? (
        <p className="flex items-center gap-2 px-3 py-4 text-sm text-fg-muted">
          <Moon size={14} aria-hidden /> {t("Nothing scheduled")}
        </p>
      ) : (
        <ul className="divide-y divide-border/70 p-1">
          {entries.map((e) => {
            const state = stateOn(e, today);
            return (
              <li key={e.id}>
                <button
                  type="button"
                  onClick={() => onOpen(e)}
                  aria-label={entryLabel(e, state)}
                  className="flex w-full items-center gap-2.5 rounded-md px-2 py-2 text-left transition-colors hover:bg-surface-hover focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/50"
                >
                  <EntryArt
                    entry={e}
                    variant="poster"
                    className="aspect-[2/3] w-8 shrink-0 rounded"
                  />
                  <span className="min-w-0 flex-1">
                    <span className="block truncate text-[13px] font-medium text-fg">
                      {e.title}
                    </span>
                    <span className="block truncate text-[11px] text-fg-muted">
                      {entrySubtitle(e)}
                    </span>
                    <Badges entry={e} className="mt-1" />
                  </span>
                  <span className="flex shrink-0 flex-col items-end gap-1">
                    <span className="flex items-center gap-1 text-[10px] font-medium text-fg-muted">
                      <StateDot state={state} />
                      {STATE_META[state].label}
                    </span>
                    <ViewerStack entry={e} viewers={viewers} size={18} max={3} />
                  </span>
                </button>
              </li>
            );
          })}
        </ul>
      )}
    </div>
  );
}
