import { useEffect, useState } from "react";
import { Button, Input, Popover } from "@braedonsaunders/appkit-ui";
import {
  Volume2,
  VolumeX,
  Headphones,
  MoreHorizontal,
  Trash2,
  Diamond,
  Music2,
} from "lucide-react";
import { ToolButton } from "./InsertBar";
import { t } from "../../../../i18n";
import { studio, type SceneSound } from "../studio-model";
export function SoundLane({
  sound,
  duration,
  onChange,
  onRemove,
  muted,
  onMute,
  onSolo,
  solo,
}: {
  sound: SceneSound;
  duration: number;
  onChange(s: SceneSound): void;
  onRemove(): void;
  muted: boolean;
  onMute(): void;
  onSolo(): void;
  solo: boolean;
}) {
  const [open, setOpen] = useState(false),
    [name, setName] = useState("");
  const [peaks, setPeaks] = useState<number[]>([]),
    [length, setLength] = useState(duration);
  useEffect(() => {
    void studio
      .asset(sound.assetDigest)
      .then((asset) => setName(asset.name))
      .catch(() => setName(""));
    void studio
      .waveform(sound.assetDigest)
      .then((w) => {
        setPeaks(w.peaks);
        setLength(w.durationMs / 1000);
      })
      .catch(() => setPeaks([]));
  }, [sound.assetDigest]);
  const drag = (
    e: React.PointerEvent,
    initial: number,
    field: "at" | "start" | "end" | "fadeIn" | "fadeOut",
  ) => {
    e.stopPropagation();
    const node = e.currentTarget,
      x = e.clientX,
      width = node.closest("[data-track]")!.getBoundingClientRect().width;
    node.setPointerCapture(e.pointerId);
    const move = (event: PointerEvent) => {
      const value =
        initial + ((event.clientX - x) / width) * duration * (field === "fadeOut" ? -1 : 1);
      onChange({
        ...sound,
        [field]: Math.max(
          0,
          field === "start"
            ? Math.min(value, (sound.end ?? length) - 0.01)
            : field === "end"
              ? Math.min(length, Math.max(value, sound.start + 0.01))
              : field === "at"
                ? Math.min(duration, value)
                : Math.min(value, (sound.end ?? length) - sound.start),
        ),
      });
    };
    const end = () => node.removeEventListener("pointermove", move as EventListener);
    node.addEventListener("pointermove", move as EventListener);
    node.addEventListener("pointerup", end, { once: true });
  };
  return (
    <section className="flex items-center gap-2 border-b border-border py-1 text-xs">
      <div className="flex w-40 shrink-0 items-center gap-1">
        <Music2 size={12} className="shrink-0 text-fg-muted" />
        <span className="min-w-0 flex-1 truncate text-[10px]">{name || t("Sound track")}</span>
        <ToolButton label={t("Mute sound")} active={muted} onClick={onMute}>
          {muted ? <VolumeX size={12} /> : <Volume2 size={12} />}
        </ToolButton>
        <ToolButton label={t("Solo sound")} active={solo} onClick={onSolo}>
          <Headphones size={12} />
        </ToolButton>
      </div>
      <div
        data-track
        className={`relative h-8 min-w-24 flex-1 overflow-hidden rounded bg-current/5 ${muted ? "opacity-40" : ""}`}
      >
        <div
          className="absolute h-full bg-purple-500/20"
          style={{
            left: `${(sound.at / duration) * 100}%`,
            width: `${(((sound.end ?? length) - sound.start) / duration) * 100}%`,
          }}
          onPointerDown={(e) => drag(e, sound.at, "at")}
        >
          <svg
            viewBox="0 0 1000 40"
            className="h-8 w-full text-primary"
            aria-label={t("Sound waveform")}
          >
            {peaks
              .slice(
                Math.floor((sound.start / length) * peaks.length),
                Math.ceil(((sound.end ?? length) / length) * peaks.length),
              )
              .map((p, i, arr) => (
                <path
                  key={i}
                  stroke="currentColor"
                  d={`M${(i * 1000) / arr.length} ${20 - p * 20}v${p * 40}`}
                />
              ))}
          </svg>
          <button
            className="absolute left-0 top-0 h-full"
            aria-label={t("Trim sound start")}
            onPointerDown={(e) => drag(e, sound.start, "start")}
          >
            <span className="block h-full w-1 rounded bg-primary/60" />
          </button>
          <button
            className="absolute right-0 top-0 h-full"
            aria-label={t("Trim sound end")}
            onPointerDown={(e) => drag(e, sound.end ?? length, "end")}
          >
            <span className="block h-full w-1 rounded bg-primary/60" />
          </button>
          <button
            className="absolute top-0"
            style={{ left: `${(sound.fadeIn / ((sound.end ?? length) - sound.start)) * 100}%` }}
            aria-label={t("Fade in")}
            onPointerDown={(e) => drag(e, sound.fadeIn, "fadeIn")}
          >
            <Diamond size={10} fill="currentColor" />
          </button>
          <button
            className="absolute bottom-0"
            style={{ right: `${(sound.fadeOut / ((sound.end ?? length) - sound.start)) * 100}%` }}
            aria-label={t("Fade out")}
            onPointerDown={(e) => drag(e, sound.fadeOut, "fadeOut")}
          >
            <Diamond size={10} fill="currentColor" />
          </button>
        </div>
      </div>
      <Popover
        open={open}
        onOpenChange={setOpen}
        className="w-64 p-3"
        trigger={
          <button
            className="rounded p-1 text-fg-muted hover:bg-surface-hover"
            aria-label={t("Sound settings")}
            onClick={() => setOpen(!open)}
          >
            <MoreHorizontal size={14} />
          </button>
        }
      >
        <h4 className="mb-3 text-xs font-semibold">{name || t("Sound track")}</h4>
        <div className="grid grid-cols-2 gap-2">
          {(["at", "start", "end", "fadeIn", "fadeOut", "volume"] as const).map((prop) => (
            <label key={prop} className="text-xs">
              {
                {
                  at: t("Scene offset"),
                  start: t("Trim start"),
                  end: t("Trim end"),
                  fadeIn: t("Fade in"),
                  fadeOut: t("Fade out"),
                  volume: t("Volume"),
                }[prop]
              }
              <Input
                className="h-8 text-xs"
                type="number"
                step={0.05}
                min={0}
                max={prop === "volume" ? 2 : prop === "at" ? duration : length}
                value={sound[prop] ?? length}
                onChange={(e) => {
                  const value = Number(e.target.value);
                  onChange({ ...sound, [prop]: value });
                }}
              />
            </label>
          ))}
        </div>
        <Button
          size="sm"
          variant="ghost"
          className="mt-3 w-full justify-start gap-2 text-danger"
          onClick={onRemove}
        >
          <Trash2 size={13} />
          {t("Remove sound")}
        </Button>
      </Popover>
    </section>
  );
}
