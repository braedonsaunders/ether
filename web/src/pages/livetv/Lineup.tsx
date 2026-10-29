import { Badge, Button, cn } from "@braedonsaunders/appkit-ui";
import {
  AlertTriangle,
  CalendarRange,
  ChevronRight,
  KeyRound,
  ListVideo,
  Radio,
  Sparkles,
  Star,
} from "lucide-react";
import { useState } from "react";
import { t, tn } from "../../i18n";
import { formatNumber, relativeTime } from "../../lib/format";
import { ChannelLogo } from "./ChannelLogo";
import {
  clock,
  dayClock,
  duration,
  onNow,
  progress,
  type Channel,
  type Programme,
  type Source,
} from "./model";

export type Selection =
  | { kind: "guide" }
  | { kind: "channel"; id: string }
  | { kind: "new" }
  | { kind: "source"; id: string }
  | { kind: "coverage" };

const PAGE = 120;

/** The channel lineup: virtual channels, then each IPTV source's channels. */
export function ChannelList({
  channels,
  sources,
  guide,
  covered,
  selected,
  onSelect,
  onToggleFavorite,
  now,
  emptyHint,
}: {
  channels: Channel[];
  sources: Source[];
  guide: Map<string, Programme[]>;
  covered: (c: Channel) => boolean | undefined;
  selected: string | null;
  onSelect: (id: string) => void;
  onToggleFavorite: (c: Channel) => void;
  now: number;
  emptyHint: React.ReactNode;
}) {
  const groups: { key: string; title: string; icon: React.ReactNode; items: Channel[] }[] = [];
  const virtual = channels.filter((c) => c.kind === "virtual");
  if (virtual.length)
    groups.push({
      key: "virtual",
      title: t("Virtual channels"),
      icon: <Sparkles size={12} />,
      items: virtual,
    });
  const bySource = new Map<string, Channel[]>();
  for (const c of channels) {
    if (c.kind !== "iptv") continue;
    const k = c.sourceId ?? "";
    bySource.set(k, [...(bySource.get(k) ?? []), c]);
  }
  for (const [id, items] of bySource) {
    groups.push({
      key: `src-${id}`,
      title: sources.find((s) => s.id === id)?.name ?? "IPTV",
      icon: <Radio size={12} />,
      items,
    });
  }
  if (!channels.length) return <div className="px-4 py-10 text-center">{emptyHint}</div>;
  return (
    <div className="pb-6">
      {groups.map((g) => (
        <ChannelGroup
          key={g.key}
          title={g.title}
          icon={g.icon}
          items={g.items}
          guide={guide}
          covered={covered}
          selected={selected}
          onSelect={onSelect}
          onToggleFavorite={onToggleFavorite}
          now={now}
        />
      ))}
    </div>
  );
}

function ChannelGroup({
  title,
  icon,
  items,
  ...rest
}: {
  title: string;
  icon: React.ReactNode;
  items: Channel[];
  guide: Map<string, Programme[]>;
  covered: (c: Channel) => boolean | undefined;
  selected: string | null;
  onSelect: (id: string) => void;
  onToggleFavorite: (c: Channel) => void;
  now: number;
}) {
  const [open, setOpen] = useState(true);
  const [limit, setLimit] = useState(PAGE);
  return (
    <section aria-label={title}>
      <button
        type="button"
        aria-expanded={open}
        onClick={() => setOpen((o) => !o)}
        className="sticky top-0 z-10 flex w-full items-center gap-1.5 border-b border-border/60 bg-bg-subtle/95 px-4 py-1.5 text-left text-[11px] font-semibold uppercase tracking-wider text-fg-subtle backdrop-blur hover:text-fg"
      >
        <ChevronRight size={12} className={cn("transition-transform", open && "rotate-90")} />
        {icon}
        <span className="min-w-0 flex-1 truncate">{title}</span>
        <span className="tabular-nums">{formatNumber(items.length)}</span>
      </button>
      {open ? (
        <ul className="px-2 py-1">
          {items.slice(0, limit).map((c) => (
            <ChannelRow key={c.id} channel={c} {...rest} />
          ))}
          {items.length > limit ? (
            <li className="px-2 py-1">
              <Button
                variant="ghost"
                size="sm"
                className="w-full text-xs"
                onClick={() => setLimit((l) => l + PAGE * 4)}
              >
                {t("Show {count} more", {
                  count: formatNumber(Math.min(items.length - limit, PAGE * 4)),
                })}
              </Button>
            </li>
          ) : null}
        </ul>
      ) : null}
    </section>
  );
}

function ChannelRow({
  channel: c,
  guide,
  covered,
  selected,
  onSelect,
  onToggleFavorite,
  now,
}: {
  channel: Channel;
  guide: Map<string, Programme[]>;
  covered: (c: Channel) => boolean | undefined;
  selected: string | null;
  onSelect: (id: string) => void;
  onToggleFavorite: (c: Channel) => void;
  now: number;
}) {
  const on = selected === c.id;
  const live = onNow(guide.get(c.id), now);
  const hasGuide = covered(c);
  return (
    <li className="group relative">
      <button
        type="button"
        onClick={() => onSelect(c.id)}
        aria-current={on ? "true" : undefined}
        aria-label={`${c.number} ${c.name}`}
        className={cn(
          "flex w-full items-center gap-2.5 rounded-lg px-2 py-1.5 text-left transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/50",
          on ? "bg-primary-subtle ring-1 ring-inset ring-primary/30" : "hover:bg-surface-hover",
          !c.enabled && "opacity-55",
        )}
      >
        <span
          className={cn(
            "w-9 shrink-0 text-right font-mono text-xs tabular-nums",
            on ? "text-primary" : "text-fg-subtle",
          )}
        >
          {c.number}
        </span>
        <ChannelLogo name={c.name} logo={c.logo} seed={c.id} size={34} />
        <span className="min-w-0 flex-1">
          <span className="flex items-center gap-1">
            <span className={cn("truncate text-sm font-medium", on ? "text-primary" : "text-fg")}>
              {c.name}
            </span>
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
      {live ? (
        <span className="sr-only">
          {t("On now: {title} until {time}", { title: live.item.title, time: clock(live.end) })}
        </span>
      ) : null}
    </li>
  );
}

/** Guide-data health and every source, on the left panel's Sources tab. */
export function SourceList({
  sources,
  channels,
  missing,
  selected,
  onSelect,
}: {
  sources: Source[];
  channels: Channel[];
  missing: number | null;
  selected: Selection;
  onSelect: (s: Selection) => void;
}) {
  const live = channels.filter((c) => c.enabled);
  const coveredCount = missing === null ? null : Math.max(0, live.length - missing);
  const pct = coveredCount === null || !live.length ? 1 : coveredCount / live.length;
  const r = 15.5;
  const circ = 2 * Math.PI * r;
  return (
    <div className="space-y-4 p-3 sm:p-4">
      <button
        type="button"
        onClick={() => onSelect({ kind: "coverage" })}
        aria-label={t("Guide data coverage")}
        className={cn(
          "flex w-full items-center gap-3 rounded-xl border bg-surface p-3 text-left shadow-xs transition-colors hover:border-border-strong",
          selected.kind === "coverage"
            ? "border-primary/40 ring-1 ring-primary/30"
            : "border-border",
        )}
      >
        <svg viewBox="0 0 36 36" className="size-12 shrink-0 -rotate-90" aria-hidden>
          <circle cx="18" cy="18" r={r} fill="none" strokeWidth="3.5" className="stroke-border" />
          <circle
            cx="18"
            cy="18"
            r={r}
            fill="none"
            strokeWidth="3.5"
            strokeLinecap="round"
            strokeDasharray={`${pct * circ} ${circ}`}
            className={
              pct >= 0.999 ? "stroke-success" : pct > 0.6 ? "stroke-primary" : "stroke-warning"
            }
          />
        </svg>
        <span className="min-w-0 flex-1">
          <span className="block text-sm font-semibold text-fg">{t("Guide data")}</span>
          <span className="block text-xs text-fg-muted">
            {coveredCount === null
              ? t("Checking coverage…")
              : missing
                ? t("{covered} of {total} channels have a guide · {missing} missing", {
                    covered: formatNumber(coveredCount),
                    total: formatNumber(live.length),
                    missing: formatNumber(missing),
                  })
                : tn(
                    live.length,
                    "All {count} live channel has a guide",
                    "All {count} live channels have a guide",
                  )}
          </span>
        </span>
        <ChevronRight size={15} className="shrink-0 text-fg-subtle" />
      </button>

      <div className="space-y-1.5">
        <h2 className="px-1 text-[11px] font-semibold uppercase tracking-wider text-fg-subtle">
          {t("Sources")}
        </h2>
        {sources.length === 0 ? (
          <p className="px-1 text-xs text-fg-muted">
            {t(
              "No sources yet. Add an M3U playlist for IPTV channels, or an XMLTV guide on its own to give channels programme data.",
            )}
          </p>
        ) : null}
        {sources.map((s) => {
          const on = selected.kind === "source" && selected.id === s.id;
          const epgOnly =
            s.hasPlaylist === false ||
            (!s.playlistUrl && !s.channelCount && (s.guideChannelCount ?? 0) > 0);
          return (
            <button
              key={s.id}
              type="button"
              onClick={() => onSelect({ kind: "source", id: s.id })}
              aria-current={on ? "true" : undefined}
              className={cn(
                "flex w-full items-start gap-3 rounded-xl border bg-surface p-3 text-left transition-colors hover:border-border-strong",
                on ? "border-primary/40 ring-1 ring-primary/30" : "border-border",
              )}
            >
              <span
                className={cn(
                  "grid size-9 shrink-0 place-items-center rounded-lg",
                  epgOnly ? "bg-info/15 text-info" : "bg-primary-subtle text-primary",
                )}
              >
                {epgOnly ? <CalendarRange size={17} /> : <ListVideo size={17} />}
              </span>
              <span className="min-w-0 flex-1 space-y-1">
                <span className="flex items-center gap-2">
                  <span className="truncate text-sm font-medium text-fg">{s.name}</span>
                  {s.auth?.type ? (
                    <KeyRound
                      size={12}
                      className="shrink-0 text-fg-subtle"
                      aria-label={s.auth.type === "xtream" ? t("Xtream login") : t("Signs in")}
                    />
                  ) : null}
                  {s.lastError ? (
                    <Badge variant="destructive" className="shrink-0">
                      {t("Refresh failed")}
                    </Badge>
                  ) : null}
                </span>
                <span className="flex flex-wrap gap-x-2 gap-y-0.5 text-xs text-fg-muted">
                  <span>
                    {epgOnly
                      ? t("Guide only")
                      : tn(s.channelCount ?? 0, "{count} channel", "{count} channels")}
                  </span>
                  {s.guideChannelCount ? (
                    <span>
                      · {tn(s.guideChannelCount, "{count} guide channel", "{count} guide channels")}
                    </span>
                  ) : null}
                  {s.guideUntil ? (
                    <span>· {t("through {time}", { time: dayClock(s.guideUntil) })}</span>
                  ) : null}
                </span>
                <span className="block text-[11px] text-fg-subtle">
                  {s.guideFrom && s.guideUntil
                    ? t("Refreshed {when} · {span} of listings", {
                        when: relativeTime(s.updatedAt),
                        span: duration(Date.parse(s.guideUntil) - Date.parse(s.guideFrom)),
                      })
                    : t("Refreshed {when}", { when: relativeTime(s.updatedAt) })}
                </span>
              </span>
            </button>
          );
        })}
      </div>
    </div>
  );
}
