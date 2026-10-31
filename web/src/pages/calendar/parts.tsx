import { cn } from "@braedonsaunders/appkit-ui";
import { Check, Clapperboard, Film, Sparkles, Tv } from "lucide-react";
import type { ReactNode } from "react";
import { Artwork } from "../../components/Artwork";
import { initials } from "../../lib/format";
import { t, tn } from "../../i18n";
import {
  BADGE_LABEL,
  STATE_META,
  entrySubtitle,
  formatDay,
  hueOf,
  type CalendarEntry,
  type CalendarViewer,
  type EntryState,
} from "./model";

/** Wide artwork for an entry: the episode still, else the backdrop. */
export function wideArt(e: CalendarEntry): string | undefined {
  return e.still || e.backdrop || undefined;
}

export function KindIcon({ entry, size = 12 }: { entry: CalendarEntry; size?: number }) {
  if (entry.kind === "movie") return <Film size={size} aria-hidden />;
  if (entry.kind === "premiere") return <Sparkles size={size} aria-hidden />;
  return <Tv size={size} aria-hidden />;
}

/** Artwork with a gradient fallback seeded by the title, never a blank box. */
export function EntryArt({
  entry,
  variant,
  className,
  eager,
}: {
  entry: CalendarEntry;
  variant: "poster" | "wide";
  className?: string;
  eager?: boolean;
}) {
  const src = variant === "poster" ? entry.poster || undefined : wideArt(entry);
  const hue = hueOf(entry.open);
  return (
    <div
      className={cn("relative overflow-hidden", className)}
      style={{
        background: `linear-gradient(135deg, hsl(${hue} 55% 32%), hsl(${(hue + 50) % 360} 60% 14%))`,
      }}
    >
      {src ? (
        <Artwork
          src={src}
          alt=""
          title=""
          icon={<span />}
          eager={eager}
          className="absolute inset-0 bg-transparent"
        />
      ) : (
        <span className="absolute inset-0 grid place-items-center text-white/70">
          {variant === "poster" ? (
            <span className="px-1 text-center text-[10px] font-semibold leading-tight">
              {initials(entry.title)}
            </span>
          ) : entry.kind === "movie" ? (
            <Clapperboard size={20} aria-hidden />
          ) : (
            <Tv size={20} aria-hidden />
          )}
        </span>
      )}
    </div>
  );
}

export function StateDot({ state, className }: { state: EntryState; className?: string }) {
  return (
    <span
      className={cn("inline-block size-2 shrink-0 rounded-full", STATE_META[state].dot, className)}
      aria-hidden
    />
  );
}

export function StateChip({ state, className }: { state: EntryState; className?: string }) {
  const m = STATE_META[state];
  return (
    <span
      className={cn(
        "inline-flex shrink-0 items-center gap-1.5 whitespace-nowrap rounded-full px-2 py-0.5 text-[11px] font-semibold ring-1 ring-inset",
        m.chip,
        className,
      )}
      title={m.hint}
    >
      {state === "today" ? (
        <span className="relative flex size-1.5">
          <span className="absolute inline-flex size-full animate-ping rounded-full bg-primary/70" />
          <span className="relative inline-flex size-1.5 rounded-full bg-primary" />
        </span>
      ) : (
        <StateDot state={state} className="size-1.5" />
      )}
      {m.label}
    </span>
  );
}

export function Badges({ entry, className }: { entry: CalendarEntry; className?: string }) {
  if (!entry.badges?.length) return null;
  return (
    <span className={cn("flex flex-wrap gap-1", className)}>
      {entry.badges.map((b) => (
        <span
          key={b}
          className={cn(
            "whitespace-nowrap rounded px-1.5 py-px text-[10px] font-bold uppercase tracking-wider",
            b === "finale" ? "bg-danger/90 text-white" : "bg-primary text-primary-fg",
          )}
        >
          {BADGE_LABEL[b]}
        </span>
      ))}
    </span>
  );
}

function Avatar({
  viewer,
  watched,
  size,
}: {
  viewer: CalendarViewer;
  watched: boolean;
  size: number;
}) {
  const hue = hueOf(String(viewer.id) + viewer.name);
  return (
    <span
      className="relative inline-grid shrink-0 place-items-center overflow-hidden rounded-full font-semibold text-white ring-2 ring-surface"
      style={{ width: size, height: size, fontSize: size * 0.4, background: `hsl(${hue} 45% 40%)` }}
      title={
        watched
          ? t("{name} has watched this", { name: viewer.name })
          : t("On {name}'s calendar", { name: viewer.name })
      }
    >
      {viewer.thumb ? (
        <img src={viewer.thumb} alt="" className="absolute inset-0 size-full object-cover" />
      ) : (
        initials(viewer.name)
      )}
      {watched ? (
        <span className="absolute inset-0 grid place-items-center bg-success/80">
          <Check size={size * 0.6} strokeWidth={3} aria-hidden />
        </span>
      ) : null}
    </span>
  );
}

/** Who an entry is for, with a tick on those who have watched it. */
export function ViewerStack({
  entry,
  viewers,
  size = 20,
  max = 4,
  className,
}: {
  entry: CalendarEntry;
  viewers: Map<number, CalendarViewer>;
  size?: number;
  max?: number;
  className?: string;
}) {
  const list = entry.viewers.map((id) => viewers.get(id)).filter(Boolean) as CalendarViewer[];
  if (!list.length) return null;
  const watched = new Set(entry.watched ?? []);
  const shown = list.slice(0, max);
  const label =
    list.length === viewers.size && viewers.size > 1
      ? t("For everyone")
      : list.map((v) => v.name).join(", ");
  return (
    <span className={cn("flex items-center", className)} aria-label={label} role="img">
      <span className="flex -space-x-1.5">
        {shown.map((v) => (
          <Avatar key={v.id} viewer={v} watched={watched.has(v.id)} size={size} />
        ))}
      </span>
      {list.length > max ? (
        <span className="ml-1 text-[10px] font-medium text-fg-muted">+{list.length - max}</span>
      ) : null}
    </span>
  );
}

/** Full accessible description of an entry, used as its button label. */
export function entryLabel(e: CalendarEntry, state: EntryState): string {
  const parts = [
    e.title,
    entrySubtitle(e),
    formatDay.long(e.date),
    STATE_META[state].label,
    ...(e.badges ?? []).map((b) => BADGE_LABEL[b]),
  ];
  if (e.watched?.length)
    parts.push(tn(e.watched.length, "Watched by {count} viewer", "Watched by {count} viewers"));
  return parts.filter(Boolean).join(", ");
}

export function EmptyNote({
  icon,
  title,
  children,
}: {
  icon: ReactNode;
  title: string;
  children?: ReactNode;
}) {
  return (
    <div className="mx-auto flex max-w-md flex-col items-center gap-3 px-6 py-14 text-center">
      <span className="grid size-14 place-items-center rounded-2xl bg-primary/10 text-primary ring-1 ring-primary/20">
        {icon}
      </span>
      <p className="text-base font-semibold text-fg">{title}</p>
      {children ? <div className="space-y-3 text-sm text-fg-muted">{children}</div> : null}
    </div>
  );
}
