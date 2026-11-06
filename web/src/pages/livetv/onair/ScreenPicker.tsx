import { useCanvasResolution } from "./useCanvasResolution";
import { styleCopy } from "./naming";
import { api } from "../../../lib/api";
import { useAsync } from "../../../lib/hooks";
import { onair, type Look } from "./model";
import {
  renderDocument,
  type Scene,
  styleBindings,
  loadOnAirFonts,
  releaseRenderer,
} from "./scene-renderer";
import { useEffect, useRef, useState } from "react";
import { Volume2, VolumeX } from "lucide-react";
import { cn } from "@braedonsaunders/appkit-ui";
import { t } from "../../../i18n";
import type { Channel } from "../model";
import { InfoTip } from "./RecipeTab";
export type ScreenStyle = "" | "card" | "static" | "bars" | "brb" | "testcard" | "error";

let systemScreens: Record<string, Scene> = Object.fromEntries(
  Object.entries(
    import.meta.glob<Scene>("../../../../../internal/onair/families/documents/signal-*.json", {
      eager: true,
      import: "default",
    }),
  ).map(([path, doc]) => [path.split("/").at(-1)!.slice(7, -5), doc]),
);
let screenLoad: Promise<void> | undefined;
function loadSystemScreens() {
  if (import.meta.env.MODE === "mock" || import.meta.env.MODE === "test") return Promise.resolve();
  return (screenLoad ??= import("../../../lib/api").then(async ({ apiFetch }) => {
    systemScreens = await apiFetch<Record<string, Scene>>("/onair/system-screens");
  }));
}
function ScreenCard({
  style,
  name,
  station,
  active,
  onSelect,
  look,
  fallback = "card",
}: {
  look?: Look;
  fallback?: ScreenStyle;
  style: ScreenStyle;
  name: string;
  station: string;
  active: boolean;
  onSelect: () => void;
}) {
  const canvas = useRef<HTMLCanvasElement>(null);
  const resolution = useCanvasResolution(canvas);
  const [hover, setHover] = useState(false);
  const [sound, setSound] = useState(false);
  useEffect(() => {
    if (!sound) return;
    const context = new AudioContext();
    const gain = context.createGain();
    gain.gain.value = 0.025;
    gain.connect(context.destination);
    const oscillator = context.createOscillator();
    oscillator.frequency.value = style === "static" ? 60 : 1000;
    oscillator.connect(gain);
    oscillator.start();
    if (style === "static") {
      const buffer = context.createBuffer(1, context.sampleRate * 2, context.sampleRate);
      const samples = buffer.getChannelData(0);
      for (let i = 0; i < samples.length; i++) samples[i] = Math.random() * 2 - 1;
      const noise = context.createBufferSource();
      noise.buffer = buffer;
      noise.loop = true;
      noise.connect(gain);
      noise.start();
    }
    return () => {
      void context.close();
    };
  }, [sound, style]);
  useEffect(() => {
    const ctx = canvas.current?.getContext("2d");
    if (!ctx) return;
    let frame = 0;
    const reduced = matchMedia("(prefers-reduced-motion: reduce)").matches;
    const draw = (time: number) => {
      const w = ctx.canvas.width,
        h = ctx.canvas.height;
      ctx.fillStyle = "#101827";
      ctx.fillRect(0, 0, w, h);
      if ((style === "card" || !style || style === "brb") && look) {
        const scene = look.scenes.find(
          (s) => s.enabled && s.role === (style === "brb" ? "break" : "offair"),
        );
        if (scene)
          renderDocument(
            ctx,
            scene.document,
            styleBindings(look, { channel: { name: station } }),
            hover ? (time / 1000) % scene.document.duration : scene.document.poster,
          );
      } else {
        const selected = style || fallback || "card";
        const doc = systemScreens[`screen.${selected === "error" ? "card" : selected}`];
        if (doc)
          renderDocument(
            ctx,
            doc,
            {
              tokens: { seed: 7 },
              slots: {
                "station.id": station,
                "screen.line1": selected === "error" ? t("Can't play this channel") : t("Off air"),
                "screen.line2": t("Programming will resume shortly"),
              },
            },
            hover ? time / 1000 : doc.poster,
          );
      }
      if (hover && !reduced) frame = requestAnimationFrame(draw);
    };
    void Promise.all([loadOnAirFonts(), loadSystemScreens()]).then(() => draw(0));
    return () => {
      cancelAnimationFrame(frame);
      releaseRenderer(ctx);
    };
  }, [style, station, hover, look, fallback, resolution]);
  const audible = style === "static" || style === "bars" || style === "testcard";
  return (
    <div
      className="relative min-w-0"
      onMouseEnter={() => setHover(true)}
      onMouseLeave={() => {
        setHover(false);
        setSound(false);
      }}
    >
      <button
        type="button"
        aria-pressed={active}
        onFocus={() => setHover(true)}
        onBlur={() => {
          setHover(false);
          setSound(false);
        }}
        onClick={onSelect}
        className={cn(
          "w-full overflow-hidden rounded-lg border bg-surface text-left focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary",
          active ? "border-primary ring-2 ring-primary" : "border-border",
        )}
        aria-label={name}
      >
        <canvas ref={canvas} width={320} height={180} className="aspect-video w-full" />
        <span className="block truncate px-2 py-2 text-xs font-medium text-fg">{name}</span>
      </button>
      {audible ? (
        <button
          type="button"
          aria-label={sound ? t("Mute {name}", { name }) : t("Listen to {name}", { name })}
          aria-pressed={sound}
          onClick={() => setSound((v) => !v)}
          className="absolute right-1.5 top-1.5 rounded-full bg-black/70 p-1.5 text-white"
        >
          {sound ? <Volume2 size={13} /> : <VolumeX size={13} />}
        </button>
      ) : null}
    </div>
  );
}
export function ScreenPicker({
  label,
  value,
  onChange,
  station = "",
  defaultLabel,
  allowError = false,
  look,
  fallback,
}: {
  look?: Look;
  fallback?: ScreenStyle;
  label: string;
  value: ScreenStyle;
  onChange: (value: ScreenStyle) => void;
  station?: string;
  defaultLabel?: string;
  allowError?: boolean;
}) {
  const options: { value: ScreenStyle; label: string }[] = [
    ...(defaultLabel
      ? [{ value: "" as const, label: styleCopy("Default ({style})", { style: defaultLabel }) }]
      : []),
    { value: "card", label: t("Modern card") },
    { value: "static", label: t("Analogue static") },
    { value: "bars", label: t("Colour bars") },
    { value: "brb", label: t("We'll be right back") },
    { value: "testcard", label: t("Test card") },
    ...(allowError ? [{ value: "error" as const, label: t("Show an error") }] : []),
  ];
  return (
    <div className="space-y-3">
      <h4 className="flex items-center gap-2 text-sm font-medium text-fg">
        {label}
        <InfoTip
          text={t(
            "Plays when the channel has no programme or its stream is unavailable. Sound previews are muted until you press the speaker.",
          )}
        />
      </h4>
      <div className="grid grid-cols-2 gap-3 sm:grid-cols-3">
        {options.map((o) => (
          <ScreenCard
            key={o.value}
            style={o.value}
            look={look}
            fallback={fallback}
            name={o.label}
            station={station}
            active={o.value === value}
            onSelect={() => onChange(o.value)}
          />
        ))}
      </div>
    </div>
  );
}
export function ChannelScreens({
  channel,
  onChange,
}: {
  channel: Channel;
  onChange: (patch: Partial<Channel>) => void;
}) {
  const config = useAsync(() => api.getConfig(), []);
  const looks = useAsync(() => onair.looks(), []);
  const look = looks.data?.find(
    (v) => v.id === (channel.look?.lookId || config.data?.liveTV?.defaultStyleId),
  );
  const names: Record<string, string> = {
    card: t("Modern card"),
    static: t("Analogue static"),
    bars: t("Colour bars"),
    brb: t("We'll be right back"),
    testcard: t("Test card"),
    error: t("Show an error"),
  };
  return (
    <section className="space-y-5">
      <h3 className="text-xs font-semibold uppercase tracking-wider text-fg-muted">
        {t("Screens for {name}", { name: channel.name })}
      </h3>
      <ScreenPicker
        label={t("When a channel is off air")}
        value={channel.screens?.offAir || ""}
        onChange={(offAir) => onChange({ screens: { ...channel.screens, offAir } })}
        station={
          (look?.screens?.stationId ?? config.data?.liveTV?.screens?.stationId ?? true)
            ? channel.name
            : ""
        }
        look={look}
        fallback={look?.screens?.offAir || config.data?.liveTV?.screens?.offAir || "card"}
        defaultLabel={
          names[look?.screens?.offAir || config.data?.liveTV?.screens?.offAir || "card"]
        }
      />
      <ScreenPicker
        label={t("When a channel can't play")}
        value={channel.screens?.noSignal || ""}
        onChange={(noSignal) => onChange({ screens: { ...channel.screens, noSignal } })}
        station={
          (look?.screens?.stationId ?? config.data?.liveTV?.screens?.stationId ?? true)
            ? channel.name
            : ""
        }
        look={look}
        fallback={look?.screens?.noSignal || config.data?.liveTV?.screens?.noSignal || "card"}
        defaultLabel={
          names[look?.screens?.noSignal || config.data?.liveTV?.screens?.noSignal || "card"]
        }
      />
    </section>
  );
}
