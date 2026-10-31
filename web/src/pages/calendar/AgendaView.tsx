import { cn } from "@braedonsaunders/appkit-ui";
import { Check, Moon, Star } from "lucide-react";
import { fixed } from "../../lib/format";
import { t } from "../../i18n";
import { Badges, EntryArt, KindIcon, StateChip, ViewerStack, entryLabel } from "./parts";
import {
  REASON_LABEL,
  addDays,
  daysBetween,
  entrySubtitle,
  formatDay,
  relativeDay,
  stateOn,
  type CalendarEntry,
  type CalendarViewer,
} from "./model";

/**
 * Day-by-day list of everything in the period, with big artwork, the
 * episode's synopsis and why it is on the calendar. Days without releases
 * are skipped, except today.
 */
export function AgendaView({
  from,
  to,
  byDay,
  today,
  viewers,
  onOpen,
}: {
  from: string;
  to: string;
  byDay: Map<string, CalendarEntry[]>;
  today: string;
  viewers: Map<number, CalendarViewer>;
  onOpen: (e: CalendarEntry) => void;
}) {
  const days: string[] = [];
  for (let d = from; d <= to; d = addDays(d, 1)) if (byDay.has(d) || d === today) days.push(d);
  if (!days.length)
    return (
      <p className="flex items-center justify-center gap-2 p-12 text-sm text-fg-muted">
        <Moon size={15} aria-hidden /> {t("Nothing scheduled in these weeks.")}
      </p>
    );
  return (
    <div className="mx-auto w-full max-w-5xl px-3 pb-10 sm:px-5">
      {days.map((day) => {
        const list = byDay.get(day) ?? [];
        const isToday = day === today;
        const near = Math.abs(daysBetween(today, day)) <= 1;
        return (
          <section key={day} aria-label={formatDay.long(day)} className="pt-4">
            <header className="sticky top-0 z-10 -mx-3 flex items-center gap-3 bg-bg/95 px-3 py-2 backdrop-blur sm:-mx-5 sm:px-5">
              <span
                className={cn(
                  "grid size-11 shrink-0 place-items-center rounded-xl text-center leading-none ring-1",
                  isToday
                    ? "bg-primary text-primary-fg ring-primary shadow-md shadow-primary/30"
                    : "bg-surface text-fg ring-border",
                )}
                aria-current={isToday ? "date" : undefined}
              >
                <span>
                  <span className="block text-[9px] font-bold uppercase tracking-wider opacity-80">
                    {formatDay.weekday(day)}
                  </span>
                  <span className="block text-lg font-semibold tabular-nums">
                    {formatDay.number(day)}
                  </span>
                </span>
              </span>
              <span className="min-w-0">
                <span
                  className={cn(
                    "block text-sm font-semibold",
                    isToday ? "text-primary" : "text-fg",
                  )}
                >
                  {near ? relativeDay(day, today) : formatDay.weekdayLong(day)}
                </span>
                <span className="block text-xs text-fg-muted">
                  {near ? formatDay.long(day) : formatDay.monthDay(day)}
                </span>
              </span>
              <span className="ml-auto h-px flex-1 bg-border" aria-hidden />
            </header>
            {list.length === 0 ? (
              <p className="ml-14 flex items-center gap-2 py-3 text-sm text-fg-muted">
                <Moon size={14} aria-hidden /> {t("Nothing airs today.")}
              </p>
            ) : (
              <ul className="space-y-2 pl-0 sm:pl-14">
                {list.map((e) => (
                  <li key={e.id}>
                    <Row entry={e} today={today} viewers={viewers} onOpen={onOpen} />
                  </li>
                ))}
              </ul>
            )}
          </section>
        );
      })}
    </div>
  );
}

function Row({
  entry,
  today,
  viewers,
  onOpen,
}: {
  entry: CalendarEntry;
  today: string;
  viewers: Map<number, CalendarViewer>;
  onOpen: (e: CalendarEntry) => void;
}) {
  const state = stateOn(entry, today);
  const why = entry.collections?.length
    ? t("In {collection}", { collection: entry.collections.join(", ") })
    : entry.reasons.map((r) => REASON_LABEL[r]).join(" · ");
  return (
    <button
      type="button"
      onClick={() => onOpen(entry)}
      aria-label={entryLabel(entry, state)}
      className="group flex w-full gap-3 overflow-hidden rounded-xl bg-surface p-2 text-left shadow-xs ring-1 ring-border transition-all hover:shadow-md hover:ring-border-strong focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/60 sm:p-2.5"
    >
      <span className="relative shrink-0 overflow-hidden rounded-lg">
        <EntryArt
          entry={entry}
          variant="wide"
          className="aspect-video w-32 transition duration-500 group-hover:scale-105 sm:w-48"
        />
        {entry.watched?.length ? (
          <span className="absolute right-1 top-1 grid size-5 place-items-center rounded-full bg-success text-white shadow">
            <Check size={12} strokeWidth={3} aria-hidden />
          </span>
        ) : null}
      </span>
      <span className="flex min-w-0 flex-1 flex-col gap-1">
        <span className="flex flex-wrap items-center gap-x-2 gap-y-1">
          <span className="truncate text-sm font-semibold text-fg">{entry.title}</span>
          <Badges entry={entry} />
        </span>
        <span className="flex items-center gap-1.5 text-xs text-fg-muted">
          <KindIcon entry={entry} />
          <span className="truncate">{entrySubtitle(entry)}</span>
          {entry.runtimeMinutes ? (
            <span className="hidden shrink-0 sm:inline">
              · {t("{minutes} min", { minutes: entry.runtimeMinutes })}
            </span>
          ) : null}
          {entry.contentRating ? (
            <span className="hidden shrink-0 rounded border border-border px-1 text-[10px] sm:inline">
              {entry.contentRating}
            </span>
          ) : null}
          {entry.rating ? (
            <span className="hidden shrink-0 items-center gap-0.5 sm:inline-flex">
              <Star size={10} aria-hidden /> {fixed(entry.rating, 1)}
            </span>
          ) : null}
        </span>
        {entry.overview ? (
          <span className="line-clamp-2 hidden text-xs text-fg-muted sm:block">
            {entry.overview}
          </span>
        ) : null}
        <span className="mt-auto flex flex-wrap items-center gap-2 pt-1">
          <StateChip state={state} />
          {why ? <span className="truncate text-[11px] text-fg-subtle">{why}</span> : null}
        </span>
      </span>
      <span className="hidden shrink-0 flex-col items-end justify-end sm:flex">
        <ViewerStack entry={entry} viewers={viewers} size={22} />
      </span>
    </button>
  );
}
