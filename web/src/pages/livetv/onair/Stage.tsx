import { useCanvasResolution } from "./useCanvasResolution";
import { useAssetAudio } from "./useAssetAudio";
import { useEffect, useRef, useState } from "react";
import { Button, cn } from "@braedonsaunders/appkit-ui";
import { Play, Pause, Repeat } from "lucide-react";
import { t } from "../../../i18n";
import { renderLook, loadStyleFonts, releaseRenderer, documentFor } from "./scene-renderer";
import { MOMENTS, type Look, type Moment, type Bindings } from "./model";
const EMPTY_BINDINGS: Bindings = {};
let activeTile: HTMLCanvasElement | null = null;
export function LookTile({
  look,
  moment = "ident",
  channel,
  bindings,
  size,
  playOnHover = true,
}: {
  look: Look;
  moment?: Moment;
  channel?: string;
  bindings?: Bindings;
  size?: string;
  playOnHover?: boolean;
}) {
  const ref = useRef<HTMLCanvasElement>(null);
  const resolution = useCanvasResolution(ref);
  const [hover, setHover] = useState(false);
  useEffect(() => {
    const c = ref.current;
    const ctx = c?.getContext("2d");
    if (!c || !ctx) return;
    let frame = 0,
      mounted = true;
    const start = performance.now();
    const still = look.moments[moment]?.poster ?? documentFor(look.family, moment)?.poster ?? 5.5;
    const draw = () => {
      renderLook(
        ctx,
        look,
        moment,
        {
          channel: { name: channel || look.name },
          ...bindings,
          viewer:
            bindings?.viewer ||
            (look.family === "personal" && channel
              ? { name: channel.replace(/ TV$/, "") }
              : undefined),
        },
        hover && activeTile === c ? (still + (performance.now() - start) / 1000) % 8 : still,
      );
      if (hover && activeTile === c) frame = requestAnimationFrame(draw);
    };
    draw();
    void loadStyleFonts(look).then(() => {
      if (mounted && !hover) draw();
    });
    return () => {
      releaseRenderer(ctx);
      mounted = false;
      cancelAnimationFrame(frame);
    };
  }, [look, moment, hover, channel, bindings, resolution]);
  const begin = () => {
    if (playOnHover && !matchMedia("(prefers-reduced-motion: reduce)").matches) {
      activeTile = ref.current;
      setHover(true);
    }
  };
  return (
    <canvas
      ref={ref}
      width={640}
      height={360}
      className={cn("block aspect-video min-w-0 w-full max-w-full rounded-lg bg-bg-subtle", size)}
      onMouseEnter={begin}
      onMouseLeave={() => setHover(false)}
      onFocus={begin}
      onBlur={() => setHover(false)}
      tabIndex={playOnHover ? 0 : undefined}
      aria-label={t("{name} preview", { name: look.name })}
    />
  );
}
export const layerBoxes = [
  { id: "Title", x: 0.2, y: 0.28, w: 0.6, h: 0.34 },
  { id: "Tagline", x: 0.2, y: 0.76, w: 0.6, h: 0.15 },
  { id: "Atmosphere", x: 0, y: 0, w: 1, h: 1 },
];
export function LookStage({
  look,
  moment = "ident",
  bindings = EMPTY_BINDINGS,
  at,
  viewer,
  onSelectLayer,
  onMomentChange,
  compact = false,
}: {
  look: Look;
  moment?: Moment;
  bindings?: Bindings;
  at?: string;
  viewer?: string;
  onSelectLayer?: (id: string) => void;
  onMomentChange?: (m: Moment) => void;
  compact?: boolean;
}) {
  const canvas = useRef<HTMLCanvasElement>(null);
  const resolution = useCanvasResolution(canvas);
  const [selected, setSelected] = useState(moment);
  const video = useRef<HTMLVideoElement>(null);
  const authoredVideoSource = useAssetAudio(look.moments[selected]?.assetDigest ?? "");
  const [playing, setPlaying] = useState(
    () => !matchMedia("(prefers-reduced-motion: reduce)").matches,
  );
  const [time, setTime] = useState(3);
  const clock = useRef(3);
  const [loop, setLoop] = useState(true);
  const [slow, setSlow] = useState(false);
  const [safe, setSafe] = useState(false);
  const audioSource = useAssetAudio(look.brand.sting);
  const sting = useRef<HTMLAudioElement | null>(null);
  const [sound, setSound] = useState(false);
  const [corner, setCorner] = useState(false);
  const [hover, setHover] = useState<string | null>(null);
  const duration = look.moments[selected]?.duration || 8;
  useEffect(() => setSelected(moment), [moment]);
  useEffect(() => {
    if (!sound) return;
    if (!audioSource) return;
    const audio = new Audio(audioSource);
    sting.current = audio;
    audio.volume = look.brand.volume;
    audio.playbackRate = slow ? 0.5 : 1;
    const sync = () => {
      if (!Number.isFinite(audio.duration)) return;
      audio.currentTime = Math.min(clock.current, audio.duration);
      if (playing && clock.current < audio.duration) void audio.play().catch(() => {});
    };
    audio.addEventListener("loadedmetadata", sync, { once: true });
    if (audio.readyState >= 1) sync();
    return () => {
      sting.current = null;
      audio.pause();
      audio.removeAttribute("src");
    };
  }, [sound, selected, audioSource, look.brand.volume, playing, slow]);
  useEffect(() => {
    let frame = 0,
      last = performance.now(),
      lastDrawTime = -1;
    const draw = (now: number) => {
      const dt = Math.min(0.1, (now - last) / 1000);
      last = now;
      if (playing) {
        clock.current += dt * (slow ? 0.5 : 1);
        if (clock.current >= duration) {
          clock.current = loop ? 0 : duration;
          if (!loop) setPlaying(false);
        }
      }
      const audio = sting.current;
      if (audio && playing && audio.readyState >= 1 && Number.isFinite(audio.duration)) {
        if (Math.abs(audio.currentTime - clock.current) > 0.2)
          audio.currentTime = Math.min(clock.current, audio.duration);
        if (clock.current < audio.duration && audio.paused) void audio.play().catch(() => {});
        if (clock.current >= audio.duration) audio.pause();
      }
      if (!playing && clock.current === lastDrawTime) {
        frame = requestAnimationFrame(draw);
        return;
      }
      lastDrawTime = clock.current;
      const ctx = canvas.current?.getContext("2d");
      if (ctx) {
        renderLook(
          ctx,
          look,
          corner ? "lowerthird" : selected,
          { ...bindings, ...(viewer ? { viewer: { name: viewer } } : {}) },
          clock.current,
          safe,
        );
        if (safe) {
          ctx.save();
          ctx.setTransform(canvas.current!.width, 0, 0, canvas.current!.height, 0, 0);
          ctx.strokeStyle = "#ffffff88";
          ctx.lineWidth = 0.001;
          ctx.setLineDash([0.008, 0.008]);
          ctx.strokeRect(0.05, 0.05, 0.9, 0.9);
          ctx.strokeRect(0.1, 0.1, 0.8, 0.8);
          ctx.restore();
        }
      }
      setTime(clock.current);
      frame = requestAnimationFrame(draw);
    };
    frame = requestAnimationFrame(draw);
    void loadStyleFonts(look).then(() => {
      lastDrawTime = -1;
    });
    return () => {
      cancelAnimationFrame(frame);
      const ctx = canvas.current?.getContext("2d");
      if (ctx) releaseRenderer(ctx);
    };
  }, [
    look,
    selected,
    bindings,
    playing,
    slow,
    loop,
    safe,
    corner,
    duration,
    at,
    viewer,
    resolution,
  ]);
  useEffect(() => {
    const v = video.current;
    if (!v) return;
    v.playbackRate = slow ? 0.5 : 1;
    v.volume = look.brand.volume;
    v.loop = loop;
    if (playing) void v.play().catch(() => {});
    else v.pause();
  }, [playing, slow, loop, authoredVideoSource, look.brand.volume]);
  const step = (n: number) => {
    setPlaying(false);
    clock.current = Math.max(0, Math.min(duration, clock.current + n));
    setTime(clock.current);
    if (video.current) video.current.currentTime = clock.current;
  };
  return (
    <div
      className="space-y-4"
      tabIndex={0}
      onKeyDown={(e) => {
        if ((e.target as HTMLElement).matches("input,textarea,select")) return;
        if (e.code === "Space") {
          e.preventDefault();
          setPlaying((p) => !p);
        }
        if (e.key === "ArrowRight" || e.key === "ArrowLeft") {
          e.preventDefault();
          step((e.key === "ArrowRight" ? 1 : -1) * (e.shiftKey ? 1 : 1 / 30));
        }
      }}
    >
      <div
        className={cn(
          "relative overflow-hidden rounded-xl border border-border shadow-lg",
          compact && "mx-auto max-w-[560px]",
        )}
        onMouseMove={(e) => {
          const r = e.currentTarget.getBoundingClientRect(),
            x = (e.clientX - r.left) / r.width,
            y = (e.clientY - r.top) / r.height;
          setHover(
            layerBoxes.find((b) => x >= b.x && x <= b.x + b.w && y >= b.y && y <= b.y + b.h)?.id ||
              null,
          );
        }}
        onMouseLeave={() => setHover(null)}
        onClick={() => hover && onSelectLayer?.(hover)}
      >
        <canvas
          ref={canvas}
          width={1280}
          height={720}
          className="aspect-video w-full"
          aria-label={t("On-air stage")}
        />
        {authoredVideoSource ? (
          <video
            ref={video}
            src={authoredVideoSource}
            muted={!sound}
            playsInline
            aria-label={t("Bumper video")}
            className="absolute inset-0 h-full w-full object-contain"
            style={{
              opacity: look.moments[selected]?.crossfade
                ? Math.max(0, Math.min(1, time / 0.25, (duration - time) / 0.25))
                : 1,
            }}
          />
        ) : null}
        {hover && onSelectLayer ? (
          <div
            className="pointer-events-none absolute border border-primary"
            style={{
              left: `${layerBoxes.find((b) => b.id === hover)!.x * 100}%`,
              top: `${layerBoxes.find((b) => b.id === hover)!.y * 100}%`,
              width: `${layerBoxes.find((b) => b.id === hover)!.w * 100}%`,
              height: `${layerBoxes.find((b) => b.id === hover)!.h * 100}%`,
            }}
          >
            <span className="bg-primary px-2 text-xs text-white">{t(hover)}</span>
          </div>
        ) : null}
      </div>
      <div className="flex items-center gap-2">
        <Button
          size="sm"
          variant="ghost"
          aria-label={playing ? t("Pause") : t("Play")}
          onClick={() => setPlaying(!playing)}
        >
          {playing ? <Pause size={16} /> : <Play size={16} />}
        </Button>
        <input
          className="min-w-0 flex-1 accent-primary"
          type="range"
          min="0"
          max={duration}
          step="0.033333"
          value={time}
          aria-label={t("Scrub preview")}
          onChange={(e) => step(Number(e.target.value) - clock.current)}
        />
        <span className="whitespace-nowrap text-xs tabular-nums text-fg-muted">
          {time.toFixed(1)} / {duration.toFixed(1)}s
        </span>
        <Button
          variant="ghost"
          size="sm"
          aria-label={t("Loop")}
          aria-pressed={loop}
          onClick={() => setLoop(!loop)}
        >
          <Repeat size={15} />
        </Button>
        <Button variant="ghost" size="sm" aria-pressed={slow} onClick={() => setSlow(!slow)}>
          ½×
        </Button>
      </div>
      <div
        className={
          compact
            ? "flex gap-2 overflow-x-auto [&>button]:w-20 [&>button]:shrink-0"
            : "grid grid-cols-3 gap-2 sm:grid-cols-5"
        }
      >
        {MOMENTS.filter((m) => (look.family === "personal" ? m !== "offair" : m !== "welcome")).map(
          (m) => (
            <button
              key={m}
              className={cn(
                "min-w-0 rounded-lg border p-1 text-left",
                selected === m
                  ? "border-primary bg-primary-subtle"
                  : "border-border hover:bg-surface-hover",
              )}
              onClick={() => {
                setSelected(m);
                clock.current = 3;
                onMomentChange?.(m);
              }}
              onDoubleClick={() => onSelectLayer?.("Title")}
            >
              <LookTile look={look} moment={m} bindings={bindings} playOnHover={false} />
              <span className="block truncate px-1 pt-1 text-xs text-fg-muted">
                {t(
                  {
                    ident: "Ident",
                    tonight: "Tonight",
                    upnext: "Up next",
                    break: "Break",
                    back: "Back",
                    offair: "Off air",
                    welcome: "Welcome",
                    bug: "Corner logo",
                    lowerthird: "Lower third",
                  }[m],
                )}
              </span>
            </button>
          ),
        )}
      </div>
      <div className="flex flex-wrap gap-2">
        {[
          { on: safe, set: setSafe, label: t("Safe area") },
          { on: sound, set: setSound, label: t("Sound") },
          { on: corner, set: setCorner, label: t("Corner logo") },
        ].map((o) => (
          <Button
            key={o.label}
            size="sm"
            variant={o.on ? "secondary" : "ghost"}
            aria-pressed={o.on}
            onClick={() => o.set(!o.on)}
          >
            {o.label}
          </Button>
        ))}
      </div>
    </div>
  );
}
