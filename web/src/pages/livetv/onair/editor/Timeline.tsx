import { useEffect, useRef, useState } from "react";
import { Button, Input, Select, Popover } from "@braedonsaunders/appkit-ui";
import {
  Play,
  Pause,
  Repeat,
  ChevronDown,
  Sparkles,
  Music2,
  Upload,
  ChevronRight,
  Gauge,
} from "lucide-react";
import { ToolButton } from "./InsertBar";
import { t } from "../../../../i18n";
import { studio, type StyleScene, type LibraryAsset } from "../studio-model";
import { renderScene, type Layer } from "../scene-renderer";
import { isAnimated, moveKeyframe, removeKeyframe, setEase, type Ease } from "./keyframes";
import { applyAnimation, presets, type AnimationPreset } from "./animation-presets";
import { SoundLane } from "./SoundLane";

import { onair } from "../model";
import { LibraryPicker } from "../../library/LibraryPicker";
type Timed = Layer & { name?: string; start?: number; end?: number };
export function Timeline(p: {
  scene: StyleScene;
  duration: number;
  time: number;
  playing: boolean;
  onPlayingChange?(p: boolean): void;
  loop?: boolean;
  onLoopChange?(v: boolean): void;
  rate?: number;
  onRateChange?(v: number): void;
  onPreviewMixChange?(muted: string[], solo: string): void;
  onTime(t: number): void;
  selected: string[];
  onSelect(ids: string[]): void;
  onChange(scene: StyleScene): void;
}) {
  const [localLoop, setLoop] = useState(true),
    [localHalf, setHalf] = useState(false),
    [zoom, setZoom] = useState(1),
    [expanded, setExpanded] = useState<string[]>([]),
    [animateOpen, setAnimateOpen] = useState(false),
    [picker, setPicker] = useState(false),
    [muted, setMuted] = useState<string[]>([]),
    [solo, setSolo] = useState(""),
    [error, setError] = useState("");
  const loop = p.loop ?? localLoop,
    half = p.rate !== undefined ? p.rate === 0.5 : localHalf;
  const edit = (id: string, fn: (l: Timed) => Layer) => {
    const visit = (layers: Layer[]): Layer[] =>
      layers.map((l) =>
        l.id === id ? fn(l) : { ...l, layers: l.layers ? visit(l.layers) : undefined },
      );
    p.onChange({
      ...p.scene,
      document: { ...p.scene.document, layers: visit(p.scene.document.layers) },
    });
  };
  const snap = (v: number) => {
    const points = [p.time, ...Array.from({ length: Math.ceil(p.duration) + 1 }, (_, i) => i)];
    const visit = (layers: Layer[]) =>
      layers.forEach((l) => {
        Object.values(l).forEach((v) => {
          if (isAnimated(v)) points.push(...v.keys.map((k) => k[0]));
        });
        if (l.layers) visit(l.layers);
      });
    visit(p.scene.document.layers);
    const nearest = points.sort((a, b) => Math.abs(a - v) - Math.abs(b - v))[0] ?? v;
    return Math.max(0, Math.min(p.duration, Math.abs(nearest - v) < 0.12 / zoom ? nearest : v));
  };
  const drag = (e: React.PointerEvent, initial: number, change: (v: number) => void) => {
    e.stopPropagation();
    const x = e.clientX,
      width = (e.currentTarget.closest("[data-track]") ??
        e.currentTarget.parentElement)!.getBoundingClientRect().width;
    const node = e.currentTarget;
    node.setPointerCapture(e.pointerId);
    const move = (event: PointerEvent) =>
      change(snap(initial + ((event.clientX - x) / width) * p.duration));
    const end = () => {
      node.removeEventListener("pointermove", move as EventListener);
      node.removeEventListener("pointerup", end);
    };
    node.addEventListener("pointermove", move as EventListener);
    node.addEventListener("pointerup", end, { once: true });
  };
  const rows = (layers: Layer[], depth = 0): React.ReactNode =>
    layers.map((layer) => {
      const l = layer as Timed,
        start = l.start || 0,
        end = l.end || p.duration;
      return (
        <div key={l.id}>
          <div
            className={`flex h-7 items-center gap-1 border-b border-border/40 ${p.selected.includes(l.id) ? "bg-primary-subtle" : ""}`}
          >
            <Button
              size="sm"
              variant="ghost"
              className="h-6 min-w-6 px-1"
              aria-label={t("Expand animation tracks")}
              aria-expanded={expanded.includes(l.id)}
              onClick={() =>
                setExpanded(
                  expanded.includes(l.id)
                    ? expanded.filter((id) => id !== l.id)
                    : [...expanded, l.id],
                )
              }
            >
              <ChevronRight size={11} className={expanded.includes(l.id) ? "rotate-90" : ""} />
            </Button>
            <button
              className="w-32 shrink-0 truncate text-left text-[10px]"
              style={{ paddingLeft: depth * 12 }}
              onClick={() => p.onSelect([l.id])}
              aria-pressed={p.selected.includes(l.id)}
            >
              {l.name || t("Layer")}
            </button>
            <div data-track className="relative h-5 grow bg-current/5">
              <span
                aria-hidden="true"
                className="pointer-events-none absolute inset-y-0 w-px bg-amber-400 z-10"
                style={{ left: `${(p.time / p.duration) * 100}%` }}
              />
              <div
                className="absolute h-5 rounded-sm bg-primary/25 flex justify-between"
                role="slider"
                tabIndex={0}
                aria-label={t("Layer timing")}
                aria-valuemin={0}
                aria-valuemax={p.duration}
                aria-valuenow={start}
                onKeyDown={(e) => {
                  if (e.key !== "ArrowLeft" && e.key !== "ArrowRight") return;
                  e.preventDefault();
                  const next = Math.max(
                    0,
                    Math.min(
                      p.duration - (end - start),
                      start + (e.key === "ArrowLeft" ? -0.1 : 0.1),
                    ),
                  );
                  edit(l.id, (old) => ({ ...old, start: next, end: next + end - start }));
                }}
                style={{
                  left: `${(start / p.duration) * 100}%`,
                  width: `${((end - start) / p.duration) * 100}%`,
                }}
                onPointerDown={(e) =>
                  drag(e, start, (v) =>
                    edit(l.id, (old) => ({
                      ...old,
                      start: Math.min(v, p.duration - (end - start)),
                      end: Math.min(p.duration, v + end - start),
                    })),
                  )
                }
              >
                <button
                  aria-label={t("Trim layer start")}
                  onKeyDown={(e) => {
                    if (e.key === "ArrowLeft" || e.key === "ArrowRight") {
                      e.stopPropagation();
                      edit(l.id, (old) => ({
                        ...old,
                        start: Math.max(
                          0,
                          Math.min(end - 0.01, start + (e.key === "ArrowLeft" ? -0.1 : 0.1)),
                        ),
                      }));
                    }
                  }}
                  onPointerDown={(e) =>
                    drag(e, start, (v) =>
                      edit(l.id, (old) => ({ ...old, start: Math.min(v, end - 0.01) })),
                    )
                  }
                >
                  ┃
                </button>
                <button
                  aria-label={t("Trim layer end")}
                  onKeyDown={(e) => {
                    if (e.key === "ArrowLeft" || e.key === "ArrowRight") {
                      e.stopPropagation();
                      edit(l.id, (old) => ({
                        ...old,
                        end: Math.max(
                          start + 0.01,
                          Math.min(p.duration, end + (e.key === "ArrowLeft" ? -0.1 : 0.1)),
                        ),
                      }));
                    }
                  }}
                  onPointerDown={(e) =>
                    drag(e, end, (v) =>
                      edit(l.id, (old) => ({ ...old, end: Math.max(start + 0.01, v) })),
                    )
                  }
                >
                  ┃
                </button>
              </div>
            </div>
          </div>
          {expanded.includes(l.id) && (
            <div className="pl-12">
              {Object.entries(l)
                .filter(([, v]) => isAnimated(v))
                .map(([key, v]) => (
                  <div key={key} className="flex h-9 items-center">
                    <span className="w-32">{propertyName(key)}</span>
                    <div data-track className="relative min-w-48 grow h-8">
                      {isAnimated(v) &&
                        v.keys.map((k) => (
                          <button
                            key={k[0]}
                            className="absolute text-blue-500 touch-none"
                            style={{ left: `${(k[0] / p.duration) * 100}%` }}
                            aria-label={t("Keyframe")}
                            onClick={() => {
                              p.onTime(k[0]);
                              p.onSelect([l.id]);
                            }}
                            onPointerDown={(e) =>
                              drag(e, k[0], (time) =>
                                edit(l.id, (old) =>
                                  moveKeyframe(old, key as keyof Layer, k[0], time),
                                ),
                              )
                            }
                            onKeyDown={(e) => {
                              if (e.key === "Delete")
                                edit(l.id, (old) => removeKeyframe(old, key as keyof Layer, k[0]));
                              if (e.key === "ArrowLeft" || e.key === "ArrowRight")
                                edit(l.id, (old) =>
                                  moveKeyframe(
                                    old,
                                    key as keyof Layer,
                                    k[0],
                                    snap(k[0] + (e.key === "ArrowLeft" ? -0.1 : 0.1)),
                                  ),
                                );
                            }}
                            onContextMenu={(e) => {
                              e.preventDefault();
                              p.onTime(k[0]);
                              p.onSelect([l.id]);
                            }}
                          >
                            ◆
                          </button>
                        ))}
                    </div>
                    <Select
                      className="w-32 shrink-0"
                      aria-label={t("Keyframe easing")}
                      disabled={!isAnimated(v) || !v.keys.some((k) => k[0] === p.time)}
                      value={
                        isAnimated(v)
                          ? (v.keys.find((k) => k[0] === p.time)?.[2] ?? "linear")
                          : "linear"
                      }
                      onChange={(e) =>
                        edit(l.id, (old) =>
                          setEase(old, key as keyof Layer, p.time, e.target.value as Ease),
                        )
                      }
                    >
                      {["linear", "in", "out", "inOut", "back", "elastic", "spring"].map((e) => (
                        <option key={e} value={e}>
                          {propertyName(e)}
                        </option>
                      ))}
                    </Select>
                  </div>
                ))}
              {l.behave?.map((b, i) =>
                Object.entries(b).map(([kind, params]) => (
                  <div key={i} className="flex flex-wrap gap-2 rounded border p-2">
                    <span>{propertyName(kind)}</span>
                    <Button
                      variant="ghost"
                      onClick={() =>
                        edit(l.id, (old) => ({
                          ...old,
                          behave: old.behave?.filter((_, index) => index !== i),
                        }))
                      }
                    >
                      {t("Remove animation")}
                    </Button>
                    {(["start", "amplitude", "speed"] as const).map((field) => (
                      <label key={field}>
                        {propertyName(field)}
                        <Input
                          type="number"
                          value={params[field] ?? 0}
                          onChange={(e) =>
                            edit(l.id, (old) => ({
                              ...old,
                              behave: old.behave?.map((v, j) =>
                                i === j
                                  ? { [kind]: { ...params, [field]: Number(e.target.value) } }
                                  : v,
                              ),
                            }))
                          }
                        />
                      </label>
                    ))}
                  </div>
                )),
              )}
            </div>
          )}
          {l.layers && rows(l.layers, depth + 1)}
        </div>
      );
    });
  const addSound = (a: LibraryAsset) => {
    if (p.scene.sounds.length >= 4) return;
    p.onChange({
      ...p.scene,
      sounds: [
        ...p.scene.sounds,
        {
          id: crypto.randomUUID(),
          assetDigest: a.digest,
          at: p.time,
          start: 0,
          end: a.durationMs ? a.durationMs / 1000 : undefined,
          volume: 1,
          fadeIn: 0,
          fadeOut: 0,
        },
      ],
    });
    setPicker(false);
  };
  return (
    <section className="space-y-1 text-xs" aria-label={t("Timeline")}>
      <div className="sticky top-0 z-20 flex h-8 items-center gap-1 border-b border-border bg-surface pb-1">
        <ToolButton
          label={p.playing ? t("Pause") : t("Play")}
          shortcut="Space"
          onClick={() => p.onPlayingChange?.(!p.playing)}
        >
          {p.playing ? <Pause size={13} /> : <Play size={13} />}
        </ToolButton>
        <ToolButton
          label={t("Loop")}
          active={loop}
          onClick={() => {
            setLoop(!loop);
            p.onLoopChange?.(!loop);
          }}
        >
          <Repeat size={13} />
        </ToolButton>
        <ToolButton
          label={t("Half speed")}
          active={half}
          onClick={() => {
            setHalf(!half);
            p.onRateChange?.(half ? 1 : 0.5);
          }}
        >
          <Gauge size={13} />
        </ToolButton>
        <output className="mx-2 min-w-14 font-mono text-[10px] tabular-nums text-fg-muted">
          {t("{time} / {duration}s", { time: p.time.toFixed(2), duration: p.duration })}
        </output>
        <span className="mx-1 h-4 border-l border-border" />
        <Popover
          open={animateOpen}
          onOpenChange={setAnimateOpen}
          className="max-h-[65vh] w-80 overflow-y-auto p-3"
          trigger={
            <Button
              size="sm"
              variant="ghost"
              className="h-7 gap-1.5 text-xs"
              onClick={() => setAnimateOpen(!animateOpen)}
            >
              <Sparkles size={13} />
              {t("Animate")}
              <ChevronDown size={11} />
            </Button>
          }
        >
          <h3 className="mb-3 text-xs font-semibold">{t("Animation presets")}</h3>
          {!p.selected.length && (
            <p className="mb-3 text-xs text-fg-muted">{t("Select a layer to give it motion.")}</p>
          )}
          <div className="grid grid-cols-3 gap-2">
            {presets.map((id) => (
              <button
                key={id}
                disabled={!p.selected.length}
                className="space-y-1 overflow-hidden rounded-lg border border-border p-1 text-[10px] hover:border-primary disabled:opacity-40"
                onClick={() => {
                  const visit = (layers: Layer[]): Layer[] =>
                    layers.map((l) => ({
                      ...(p.selected.includes(l.id) ? applyAnimation(l, id, p.duration) : l),
                      layers: l.layers ? visit(l.layers) : undefined,
                    }));
                  p.onChange({
                    ...p.scene,
                    document: { ...p.scene.document, layers: visit(p.scene.document.layers) },
                  });
                  setAnimateOpen(false);
                }}
              >
                <PresetPreview preset={id} />
                <span className="block pb-1">{presetName(id)}</span>
              </button>
            ))}
          </div>
        </Popover>
        <ToolButton
          label={t("Add sound")}
          disabled={p.scene.sounds.length >= 4}
          onClick={() => setPicker(true)}
        >
          <Music2 size={13} />
        </ToolButton>
        <label className="ml-auto flex items-center gap-2 text-[10px] text-fg-muted">
          {t("Zoom")}
          <input
            type="range"
            className="w-16 accent-primary"
            min="1"
            max="4"
            step=".25"
            value={zoom}
            aria-label={t("Timeline zoom")}
            onChange={(e) => setZoom(Number(e.target.value))}
          />
        </label>
      </div>
      <div className="overflow-x-auto">
        <div style={{ minWidth: 600 * zoom }}>
          <Input
            className="accent-primary border-0 bg-transparent px-0 py-0"
            style={{
              marginLeft: 160,
              width: "calc(100% - 160px)",
              height: 12,
              accentColor: "var(--color-primary)",
            }}
            aria-label={t("Playhead")}
            type="range"
            min={0}
            max={p.duration}
            step={0.01}
            value={p.time}
            onChange={(e) => p.onTime(Number(e.target.value))}
          />
          <div className="ml-40 flex justify-between text-[9px] text-fg-muted">
            {Array.from({ length: Math.floor(p.duration) + 1 }, (_, i) => (
              <span key={i}>{i}</span>
            ))}
          </div>
          {rows(p.scene.document.layers)}
        </div>
      </div>
      {p.scene.sounds.map((sound) => (
        <SoundLane
          key={sound.id}
          sound={sound}
          duration={p.duration}
          muted={muted.includes(sound.id)}
          solo={solo === sound.id}
          onMute={() => {
            const next = muted.includes(sound.id)
              ? muted.filter((id) => id !== sound.id)
              : [...muted, sound.id];
            setMuted(next);
            p.onPreviewMixChange?.(next, solo);
          }}
          onSolo={() => {
            const next = solo === sound.id ? "" : sound.id;
            setSolo(next);
            p.onPreviewMixChange?.(muted, next);
          }}
          onChange={(s) =>
            p.onChange({
              ...p.scene,
              sounds: p.scene.sounds.map((old) => (old.id === s.id ? s : old)),
            })
          }
          onRemove={() =>
            p.onChange({ ...p.scene, sounds: p.scene.sounds.filter((s) => s.id !== sound.id) })
          }
        />
      ))}
      <LibraryPicker
        open={picker}
        kinds={["audio"]}
        onPick={addSound}
        onClose={() => setPicker(false)}
      />
      <label className="inline-flex cursor-pointer items-center gap-1 rounded px-2 py-1 text-[10px] text-fg-muted hover:bg-surface-hover">
        <Upload size={11} />
        {t("Upload sound")}
        <Input
          className="sr-only"
          aria-label={t("Upload sound")}
          type="file"
          accept="audio/*"
          disabled={p.scene.sounds.length >= 4}
          onChange={(e) => {
            const file = e.target.files?.[0];
            if (file)
              void onair
                .uploadAsset(file, "audio")
                .then((a) => studio.asset(a.digest))
                .then(addSound)
                .catch((e) => setError(String(e)));
          }}
        />
      </label>
      {error && <p role="alert">{error}</p>}
    </section>
  );
}

function presetName(id: AnimationPreset) {
  return {
    "fade-in": t("Fade in"),
    pop: t("Pop"),
    "slide-left": t("Slide from left"),
    "slide-right": t("Slide from right"),
    "slide-up": t("Slide from above"),
    "slide-down": t("Slide from below"),
    typewriter: t("Typewriter"),
    "wipe-in": t("Wipe in"),
    glitch: t("Glitch"),
    float: t("Float"),
    pulse: t("Pulse"),
    flicker: t("Flicker"),
    glint: t("Glint"),
    wiggle: t("Wiggle"),
    drift: t("Drift"),
    "fade-out": t("Fade out"),
    drop: t("Drop"),
    "wipe-out": t("Wipe out"),
  }[id];
}
function PresetPreview({ preset }: { preset: AnimationPreset }) {
  const canvas = useRef<HTMLCanvasElement>(null);
  useEffect(() => {
    const ctx = canvas.current?.getContext("2d");
    if (!ctx) return;
    let frame = 0;
    const layer = applyAnimation(
      { id: "sample", type: "text", text: "Aa", x: 780, y: 650, size: 420, fill: "#60a5fa" },
      preset,
      2,
    );
    const doc = { v: 1, moment: "ident", duration: 2, poster: 0, layers: [layer] };
    const tick = (now: number) => {
      renderScene(ctx, doc, { tokens: {}, slots: {} }, (now / 1000) % 2);
      frame = requestAnimationFrame(tick);
    };
    frame = requestAnimationFrame(tick);
    return () => cancelAnimationFrame(frame);
  }, [preset]);
  return (
    <canvas
      ref={canvas}
      width={160}
      height={90}
      className="aspect-video w-full rounded bg-black/10"
      aria-hidden="true"
    />
  );
}

export function propertyName(key: string): string {
  return (
    (
      {
        x: t("X position"),
        y: t("Y position"),
        w: t("Width"),
        h: t("Height"),
        scale: t("Scale"),
        scaleX: t("Horizontal scale"),
        scaleY: t("Vertical scale"),
        rotate: t("Rotation"),
        opacity: t("Opacity"),
        fill: t("Fill"),
        stroke: t("Stroke"),
        strokeWidth: t("Stroke width"),
        text: t("Text"),
        font: t("Font"),
        size: t("Font size"),
        track: t("Tracking"),
        src: t("Source"),
        tint: t("Tint"),
        start: t("Start"),
        amplitude: t("Amplitude"),
        speed: t("Speed"),
        linear: t("Linear"),
        in: t("Ease in"),
        out: t("Ease out"),
        inOut: t("Ease in and out"),
        back: t("Back"),
        elastic: t("Elastic"),
        spring: t("Spring"),
        typewriter: t("Typewriter"),
        flicker: t("Flicker"),
        wiggle: t("Wiggle"),
        bounce: t("Bounce"),
        glint: t("Glint"),
        chroma: t("Chroma"),
        drift: t("Drift"),
        pop: t("Pop"),
        squash: t("Squash"),
      } as Record<string, string>
    )[key] ?? key
  );
}
