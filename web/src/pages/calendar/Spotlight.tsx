import { cn } from "@braedonsaunders/appkit-ui";
import { Artwork } from "../../components/Artwork";
import { t } from "../../i18n";
import { EntryArt, KindIcon, ViewerStack, entryLabel, wideArt } from "./parts";
import {
  BADGE_LABEL,
  entrySubtitle,
  formatDay,
  hueOf,
  relativeDay,
  stateOn,
  type CalendarEntry,
  type CalendarViewer,
} from "./model";

/**
 * "Up next": the next releases as big backdrop cards, today's first, so the
 * owner sees what lands tonight without reading the grid.
 */
export function Spotlight({
  entries,
  today,
  viewers,
  onOpen,
  rail,
}: {
  entries: CalendarEntry[];
  today: string;
  viewers: Map<number, CalendarViewer>;
  onOpen: (e: CalendarEntry) => void;
  /** A vertical side rail (wide screens) instead of a horizontal strip. */
  rail?: boolean;
}) {
  const picks = entries.filter((e) => e.date >= today).slice(0, rail ? 16 : 12);
  if (!picks.length) return null;
  if (rail)
    return (
      <section className="space-y-2 p-3">
        <Heading />
        {picks.map((e) => (
          <Card
            key={e.id}
            entry={e}
            today={today}
            viewers={viewers}
            onOpen={onOpen}
            className="w-full"
          />
        ))}
      </section>
    );
  return (
    <section
      aria-label={t("Up next")}
      className="shrink-0 border-b border-border bg-surface px-3 pb-3 pt-2.5 sm:px-5"
    >
      <Heading />
      <div className="app-scroll -mx-1 flex snap-x gap-2.5 overflow-x-auto px-1 pb-1">
        {picks.map((e) => (
          <Card
            key={e.id}
            entry={e}
            today={today}
            viewers={viewers}
            onOpen={onOpen}
            className="w-72 snap-start"
          />
        ))}
      </div>
    </section>
  );
}

function Heading() {
  return (
    <div className="mb-2 flex items-center gap-2">
      <span className="relative flex size-2">
        <span className="absolute inline-flex size-full animate-ping rounded-full bg-primary/60" />
        <span className="relative inline-flex size-2 rounded-full bg-primary" />
      </span>
      <h2 className="text-xs font-semibold uppercase tracking-wider text-fg-subtle">
        {t("Up next")}
      </h2>
    </div>
  );
}

function Card({
  entry: e,
  today,
  viewers,
  onOpen,
  className,
}: {
  entry: CalendarEntry;
  today: string;
  viewers: Map<number, CalendarViewer>;
  onOpen: (e: CalendarEntry) => void;
  className?: string;
}) {
  const hue = hueOf(e.open);
  const state = stateOn(e, today);
  const when = relativeDay(e.date, today);
  const badge = e.badges?.[0];
  return (
    <button
      key={e.id}
      type="button"
      onClick={() => onOpen(e)}
      aria-label={entryLabel(e, state)}
      className={cn(
        "group relative flex h-[6.5rem] shrink-0 overflow-hidden rounded-xl border border-border text-left shadow-xs transition-all hover:-translate-y-0.5 hover:shadow-md focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/50",
        className,
      )}
      style={{
        background: `linear-gradient(120deg, hsl(${hue} 55% 22%), hsl(${(hue + 50) % 360} 55% 10%))`,
      }}
    >
      {wideArt(e) ? (
        <Artwork
          src={wideArt(e)}
          alt=""
          title=""
          icon={<span />}
          className="absolute inset-y-0 right-0 w-44 bg-transparent opacity-90 [mask-image:linear-gradient(to_left,black_45%,transparent)]"
          imgClassName="object-cover transition duration-500 group-hover:scale-105"
        />
      ) : null}
      <span className="relative flex min-w-0 flex-1 gap-2.5 p-2.5">
        <EntryArt
          entry={e}
          variant="poster"
          className="aspect-[2/3] h-full shrink-0 rounded-md shadow-lg ring-1 ring-white/10"
        />
        <span className="flex min-w-0 flex-1 flex-col justify-between pr-10">
          <span className="flex items-center gap-1.5">
            <span
              className={
                state === "today"
                  ? "shrink-0 whitespace-nowrap rounded bg-primary px-1.5 py-px text-[10px] font-bold uppercase tracking-wider text-primary-fg"
                  : "shrink-0 whitespace-nowrap rounded bg-white/15 px-1.5 py-px text-[10px] font-bold uppercase tracking-wider text-white"
              }
            >
              {when}
            </span>
            {badge ? (
              <span className="truncate text-[10px] font-semibold uppercase tracking-wider text-white/75">
                {BADGE_LABEL[badge]}
              </span>
            ) : null}
          </span>
          <span className="min-w-0">
            <span className="block truncate text-sm font-semibold text-white">{e.title}</span>
            <span className="flex items-center gap-1 truncate text-[11px] text-white/70">
              <KindIcon entry={e} size={11} />
              <span className="truncate">{entrySubtitle(e)}</span>
            </span>
          </span>
          <span className="flex items-center justify-between gap-2">
            <span className="text-[11px] tabular-nums text-white/60">
              {formatDay.medium(e.date)}
            </span>
            <ViewerStack entry={e} viewers={viewers} size={18} max={3} />
          </span>
        </span>
      </span>
    </button>
  );
}
