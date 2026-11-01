import { cn } from "@braedonsaunders/appkit-ui";
import { Check, Moon } from "lucide-react";
import { t, tn } from "../../i18n";
import { Badges, EntryArt, KindIcon, ViewerStack, entryLabel } from "./parts";
import {
  STATE_META,
  addDays,
  episodeCode,
  formatDay,
  stateOn,
  type CalendarEntry,
  type CalendarViewer,
} from "./model";

/**
 * Seven rich day columns: big artwork tiles for every release. Below `lg`
 * each day becomes a horizontal shelf, Silo-style.
 */
export function WeekView({
  from,
  byDay,
  today,
  viewers,
  onOpen,
}: {
  from: string;
  byDay: Map<string, CalendarEntry[]>;
  today: string;
  viewers: Map<number, CalendarViewer>;
  onOpen: (e: CalendarEntry) => void;
}) {
  const days = Array.from({ length: 7 }, (_, i) => addDays(from, i));
  return (
    <div className="flex min-h-0 flex-1 flex-col lg:grid lg:grid-cols-7 lg:divide-x lg:divide-border">
      {days.map((day) => {
        const list = byDay.get(day) ?? [];
        const isToday = day === today;
        return (
          <section
            key={day}
            aria-label={`${formatDay.long(day)}, ${tn(list.length, "{count} release", "{count} releases")}`}
            className={cn(
              "flex min-w-0 flex-col border-b border-border lg:min-h-0 lg:border-b-0",
              isToday ? "bg-primary/[0.05]" : day < today ? "bg-bg-subtle/40" : "bg-bg",
            )}
          >
            <header
              className={cn(
                "sticky top-0 z-10 flex items-center gap-2.5 border-b border-border/70 px-3 py-2.5 backdrop-blur lg:flex-col lg:items-start lg:gap-1",
                isToday ? "bg-primary/[0.08]" : "bg-surface/90",
              )}
            >
              <span
                className={cn(
                  "text-[11px] font-semibold uppercase tracking-wider",
                  isToday ? "text-primary" : "text-fg-subtle",
                )}
              >
                {formatDay.weekday(day)}
              </span>
              <span className="flex items-center gap-2">
                <span
                  className={cn(
                    "grid size-9 place-items-center rounded-xl text-lg font-semibold tabular-nums",
                    isToday ? "bg-primary text-primary-fg shadow-md shadow-primary/30" : "text-fg",
                  )}
                  aria-current={isToday ? "date" : undefined}
                >
                  {formatDay.number(day)}
                </span>
                <span className="text-xs text-fg-muted">
                  {list.length
                    ? tn(list.length, "{count} release", "{count} releases")
                    : t("Nothing scheduled")}
                </span>
              </span>
            </header>
            {list.length === 0 ? (
              <p className="hidden items-center justify-center gap-1.5 p-6 text-xs text-fg-subtle lg:flex">
                <Moon size={13} aria-hidden /> {t("Quiet day")}
              </p>
            ) : (
              <ul className="app-scroll flex gap-2.5 overflow-x-auto p-2.5 lg:flex-col lg:overflow-y-auto lg:overflow-x-visible">
                {list.map((e) => (
                  <li key={e.id} className="w-56 shrink-0 lg:w-auto">
                    <Tile entry={e} today={today} viewers={viewers} onOpen={onOpen} />
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

function Tile({
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
  const meta = STATE_META[state];
  return (
    <button
      type="button"
      onClick={() => onOpen(entry)}
      aria-label={entryLabel(entry, state)}
      className="group block w-full overflow-hidden rounded-xl bg-surface text-left shadow-xs ring-1 ring-border transition-all hover:-translate-y-0.5 hover:shadow-lg hover:ring-border-strong focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/60"
    >
      <span className="relative block overflow-hidden">
        <EntryArt
          entry={entry}
          variant="wide"
          className="aspect-video w-full transition duration-500 group-hover:scale-[1.04]"
        />
        <span className="absolute inset-0 bg-gradient-to-t from-black/50 via-transparent to-transparent" />
        <Badges entry={entry} className="absolute left-1.5 top-1.5 right-8" />
        {entry.watched?.length ? (
          <span className="absolute right-1.5 top-1.5 grid size-5 place-items-center rounded-full bg-success text-white shadow">
            <Check size={12} strokeWidth={3} aria-hidden />
          </span>
        ) : null}
        <span className="absolute bottom-1.5 left-1.5 flex items-center gap-1 rounded bg-black/55 px-1.5 py-0.5 text-[10px] font-medium text-white backdrop-blur-sm">
          <KindIcon entry={entry} size={10} />
          {entry.kind === "episode"
            ? episodeCode(entry)
            : entry.kind === "movie"
              ? t("Movie")
              : t("Season {number}", { number: entry.season ?? 0 })}
        </span>
      </span>
      <span className="block px-2.5 pt-2">
        <span className="line-clamp-2 text-[13px] font-semibold leading-snug text-fg">
          {entry.title}
        </span>
        {entry.kind === "episode" && entry.episodeTitle ? (
          <span className="block truncate text-[11px] text-fg-muted">{entry.episodeTitle}</span>
        ) : null}
      </span>
      <span className="flex items-center justify-between gap-2 px-2.5 py-1.5">
        <span className="flex min-w-0 items-center gap-1.5 text-[11px] font-medium text-fg-muted">
          <span className={cn("size-2 shrink-0 rounded-full", meta.dot)} aria-hidden />
          <span className="truncate">{meta.label}</span>
        </span>
        <ViewerStack entry={entry} viewers={viewers} size={18} max={3} />
      </span>
    </button>
  );
}
