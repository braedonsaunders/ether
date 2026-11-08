import { Badge, Button, cn } from "@braedonsaunders/appkit-ui";
import { Copy, Play, Settings, Sparkles, Star } from "lucide-react";
import { Artwork } from "../../components/Artwork";
import { copyText } from "../../components/CopyButton";
import { Drawer } from "../../components/Drawer";
import { t } from "../../i18n";
import {
  clock,
  dayClock,
  duration,
  progress,
  programmeArtwork,
  type Channel,
  type Programme,
} from "./model";

export type InspectedProgramme = { channel: Channel; programme: Programme };

/**
 * A programme's details from the guide, in a drawer like every other record:
 * what it is, when it airs, and the channel's quick actions.
 */
export function ProgrammeInspector({
  open,
  inspected,
  now,
  onClose,
  onExitComplete,
  onPlay,
  onOpenChannel,
  onToggleFavorite,
}: {
  open: boolean;
  /** Kept after closing so the drawer slides out with its content. */
  inspected: InspectedProgramme | null;
  now: number;
  onClose: () => void;
  onExitComplete?: () => void;
  onPlay?: (channel: Channel) => void;
  onOpenChannel?: (channel: Channel) => void;
  onToggleFavorite?: (channel: Channel) => void;
}) {
  const channel = inspected?.channel;
  const p = inspected?.programme;
  return (
    <Drawer
      open={open && inspected !== null}
      onClose={onClose}
      onExitComplete={onExitComplete}
      size="md"
      title={p ? programmeHeading(p) : t("Programme")}
      description={channel ? `${channel.number} · ${channel.name}` : undefined}
      headerActions={
        channel && onToggleFavorite ? (
          <Button
            variant="ghost"
            size="sm"
            aria-label={
              channel.favorite
                ? t("Unfavourite {channel}", { channel: channel.name })
                : t("Favourite {channel}", { channel: channel.name })
            }
            aria-pressed={channel.favorite}
            title={channel.favorite ? t("Unfavourite") : t("Favourite")}
            onClick={() => onToggleFavorite(channel)}
          >
            <Star size={15} className={cn(channel.favorite && "fill-warning text-warning")} />
          </Button>
        ) : undefined
      }
      footer={
        channel ? (
          <div className="flex w-full flex-wrap items-center gap-2">
            {onOpenChannel ? (
              <Button variant="outline" onClick={() => onOpenChannel(channel)}>
                <Settings size={14} /> {t("Channel settings")}
              </Button>
            ) : null}
            {channel.streamUrl ? (
              <Button variant="ghost" onClick={() => void copyText(channel.streamUrl!)}>
                <Copy size={14} /> {t("Copy link")}
              </Button>
            ) : null}
            {onPlay && channel.enabled ? (
              <Button className="ml-auto" onClick={() => onPlay(channel)}>
                <Play size={14} className="fill-current" /> {t("Watch")}
              </Button>
            ) : null}
          </div>
        ) : undefined
      }
    >
      {channel && p ? <ProgrammeDetails programme={p} now={now} /> : null}
    </Drawer>
  );
}

/** The show for an episode, the programme's own title otherwise. */
function programmeHeading(p: Programme): string {
  const { title, showTitle } = p.item;
  return (showTitle && showTitle !== title ? showTitle : title) || t("Untitled");
}

function ProgrammeDetails({ programme: p, now }: { programme: Programme; now: number }) {
  const item = p.item;
  const startMs = Date.parse(p.start);
  const endMs = Date.parse(p.end);
  const airing = startMs <= now && endMs > now;
  const ended = endMs <= now;
  const heading = programmeHeading(p);
  const subtitle = [item.episodeTitle, item.title].find((s) => s && s !== heading);
  const art = programmeArtwork(item.art || item.thumb);
  return (
    <div className="space-y-5">
      {art ? (
        <Artwork
          src={art}
          alt=""
          title=""
          className="aspect-video w-full overflow-hidden rounded-xl bg-bg-subtle object-cover"
        />
      ) : null}

      <div className="space-y-2">
        <div className="flex flex-wrap items-center gap-1.5">
          {airing ? (
            <Badge variant="destructive">{t("On now")}</Badge>
          ) : ended ? (
            <Badge variant="secondary">{t("Ended")}</Badge>
          ) : (
            <Badge variant="secondary">{t("Upcoming")}</Badge>
          )}
          {item.season && item.episode ? (
            <Badge variant="outline">
              {t("S{season} E{episode}", { season: item.season, episode: item.episode })}
            </Badge>
          ) : null}
          {item.rating ? <Badge variant="outline">{item.rating}</Badge> : null}
          {item.premiere ? (
            <Badge variant="info" className="gap-1">
              <Sparkles size={11} aria-hidden /> {t("Premiere")}
            </Badge>
          ) : null}
        </div>
        {subtitle ? <p className="text-sm font-medium text-fg">{subtitle}</p> : null}
      </div>

      <div className="space-y-2 rounded-xl border border-border bg-bg-subtle/60 p-3">
        <div className="flex flex-wrap items-baseline justify-between gap-x-3 gap-y-1 text-sm">
          <span className="font-medium tabular-nums text-fg">
            {t("{start} – {end}", { start: dayClock(p.start), end: clock(p.end) })}
          </span>
          <span className="text-xs text-fg-muted">
            {airing ? t("{time} left", { time: duration(endMs - now) }) : duration(endMs - startMs)}
          </span>
        </div>
        {airing ? (
          <div
            className="h-1.5 overflow-hidden rounded-full bg-border"
            role="progressbar"
            aria-label={t("{time} left", { time: duration(endMs - now) })}
            aria-valuemin={0}
            aria-valuemax={100}
            aria-valuenow={Math.round(progress(p, now) * 100)}
          >
            <div
              className="h-full rounded-full bg-primary"
              style={{ width: `${progress(p, now) * 100}%` }}
            />
          </div>
        ) : null}
      </div>

      {item.genres?.length ? (
        <div className="flex flex-wrap gap-1.5">
          {item.genres.map((g) => (
            <span
              key={g}
              className="rounded-md bg-surface-hover px-2 py-0.5 text-xs font-medium text-fg-muted"
            >
              {g}
            </span>
          ))}
        </div>
      ) : null}

      <p className="whitespace-pre-line text-sm leading-relaxed text-fg-muted">
        {item.summary || t("No description provided.")}
      </p>
    </div>
  );
}
