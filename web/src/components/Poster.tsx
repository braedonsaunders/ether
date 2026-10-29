import { Button, cn } from "@braedonsaunders/appkit-ui";
import { ChevronLeft, ChevronRight, Star } from "lucide-react";
import { useRef, type ReactNode } from "react";
import { t } from "../i18n";
import type { TitleCard } from "../lib/contract";
import { fixed } from "../lib/format";
import { Artwork } from "./Artwork";

export function titleSubtitle(card: TitleCard): string {
  if (card.type === "episode") {
    const se =
      card.season != null && card.episode != null
        ? `S${String(card.season).padStart(2, "0")}E${String(card.episode).padStart(2, "0")}`
        : "";
    return [card.showTitle, se].filter(Boolean).join(" · ");
  }
  return [card.year, card.type === "show" ? t("TV") : null].filter(Boolean).join(" · ");
}

/** A 2:3 poster with title and year, Plex-style. `reason` appears on hover/focus. */
export function PosterCard({
  item,
  reason,
  badge,
  onClick,
  className,
  showMeta = true,
  actions,
  eager,
}: {
  item: TitleCard;
  reason?: string;
  badge?: ReactNode;
  onClick?: () => void;
  className?: string;
  showMeta?: boolean;
  actions?: ReactNode;
  eager?: boolean;
}) {
  const interactive = Boolean(onClick);
  const body = (
    <>
      <div className="relative aspect-[2/3] w-full overflow-hidden rounded-md shadow-sm ring-1 ring-border-subtle transition duration-200 group-hover:shadow-md group-hover:ring-border-strong motion-safe:group-hover:-translate-y-0.5">
        <Artwork
          src={item.poster}
          alt={item.title}
          title={item.title}
          className="size-full"
          eager={eager}
        />
        {badge ? <div className="absolute left-1.5 top-1.5">{badge}</div> : null}
        {typeof item.rating === "number" && item.rating > 0 ? (
          <span className="absolute right-1.5 top-1.5 inline-flex items-center gap-0.5 rounded bg-overlay/70 px-1 py-0.5 text-[10px] font-semibold tabular-nums text-on-scrim">
            <Star size={9} aria-hidden /> {fixed(item.rating, 1)}
          </span>
        ) : null}
        {reason ? (
          <div className="scrim-bottom pointer-events-none absolute inset-x-0 bottom-0 translate-y-1 p-2 opacity-0 transition duration-200 group-hover:translate-y-0 group-hover:opacity-100 group-focus-within:translate-y-0 group-focus-within:opacity-100">
            <p className="line-clamp-4 text-[11px] leading-snug text-on-scrim">{reason}</p>
          </div>
        ) : null}
        {actions ? (
          <div className="absolute bottom-1.5 right-1.5 flex gap-1 opacity-0 transition group-hover:opacity-100 group-focus-within:opacity-100">
            {actions}
          </div>
        ) : null}
      </div>
      {showMeta ? (
        <div className="mt-1.5 min-w-0 px-0.5">
          <p className="truncate text-xs font-medium text-fg" title={item.title}>
            {item.title}
          </p>
          <p className="truncate text-[11px] text-fg-muted">{titleSubtitle(item)}</p>
        </div>
      ) : null}
    </>
  );
  if (interactive) {
    return (
      <button
        type="button"
        onClick={onClick}
        title={reason}
        className={cn(
          "group block w-full text-left focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/60 rounded-md",
          className,
        )}
      >
        {body}
      </button>
    );
  }
  return (
    <div
      className={cn("group block w-full", className)}
      tabIndex={reason ? 0 : undefined}
      title={reason}
      aria-label={reason ? `${item.title}: ${reason}` : undefined}
    >
      {body}
    </div>
  );
}

/** A horizontally scrolling shelf of posters with arrow buttons (a Plex row). */
export function Shelf({
  items,
  reasons,
  posterWidth = "w-28 sm:w-32",
  ariaLabel,
  empty,
  renderBadge,
}: {
  items: TitleCard[];
  reasons?: Record<string, string>;
  posterWidth?: string;
  ariaLabel: string;
  empty?: ReactNode;
  renderBadge?: (item: TitleCard) => ReactNode;
}) {
  const ref = useRef<HTMLDivElement>(null);
  const scroll = (dir: 1 | -1) => {
    const el = ref.current;
    if (!el) return;
    el.scrollBy({ left: dir * Math.max(200, el.clientWidth * 0.8), behavior: "smooth" });
  };
  if (items.length === 0)
    return <div className="py-4 text-sm text-fg-muted">{empty ?? t("No titles")}</div>;
  return (
    <div className="group/shelf relative">
      <div
        ref={ref}
        className="shelf flex gap-3 overflow-x-auto pb-1"
        role="list"
        aria-label={ariaLabel}
      >
        {items.map((it, i) => (
          <div key={`${it.key}-${i}`} role="listitem" className={cn("shrink-0", posterWidth)}>
            <PosterCard item={it} reason={reasons?.[it.key]} badge={renderBadge?.(it)} />
          </div>
        ))}
      </div>
      {items.length > 4 ? (
        <>
          <Button
            type="button"
            variant="outline"
            size="icon"
            aria-label={t("Scroll left")}
            onClick={() => scroll(-1)}
            className="absolute -left-3 top-[35%] hidden size-8 rounded-full opacity-0 shadow-md transition group-hover/shelf:opacity-100 focus-visible:opacity-100 sm:inline-flex"
          >
            <ChevronLeft size={16} />
          </Button>
          <Button
            type="button"
            variant="outline"
            size="icon"
            aria-label={t("Scroll right")}
            onClick={() => scroll(1)}
            className="absolute -right-3 top-[35%] hidden size-8 rounded-full opacity-0 shadow-md transition group-hover/shelf:opacity-100 focus-visible:opacity-100 sm:inline-flex"
          >
            <ChevronRight size={16} />
          </Button>
        </>
      ) : null}
    </div>
  );
}

/** Responsive poster grid for previews and search results. */
export function PosterGrid({
  items,
  reasons,
  onSelect,
  renderBadge,
  className,
}: {
  items: TitleCard[];
  reasons?: Record<string, string>;
  onSelect?: (item: TitleCard) => void;
  renderBadge?: (item: TitleCard) => ReactNode;
  className?: string;
}) {
  return (
    <div
      className={cn(
        "grid grid-cols-[repeat(auto-fill,minmax(7rem,1fr))] gap-x-3 gap-y-4",
        className,
      )}
      role="list"
    >
      {items.map((it, i) => (
        <div key={`${it.key}-${i}`} role="listitem">
          <PosterCard
            item={it}
            reason={reasons?.[it.key]}
            badge={renderBadge?.(it)}
            onClick={onSelect ? () => onSelect(it) : undefined}
            eager={i < 12}
          />
        </div>
      ))}
    </div>
  );
}
