import type { Layer, Value } from "../scene-renderer";
import { valueAt } from "./keyframes";
export const presets = [
  "fade-in",
  "pop",
  "slide-left",
  "slide-right",
  "slide-up",
  "slide-down",
  "typewriter",
  "wipe-in",
  "glitch",
  "float",
  "pulse",
  "flicker",
  "glint",
  "wiggle",
  "drift",
  "fade-out",
  "drop",
  "wipe-out",
] as const;
export type AnimationPreset = (typeof presets)[number];
export function applyAnimation(layer: Layer, preset: AnimationPreset, duration: number): Layer {
  const timing = layer as Layer & { start?: number; end?: number };
  const start = timing.start || 0,
    end = timing.end || duration,
    span = Math.min(0.8, (end - start) / 3);
  const out = { ...layer };
  const animate = (prop: keyof Layer, a: Value, b: Value, exit = false) => {
    Object.assign(out, {
      [prop]: {
        keys: [
          [exit ? end - span : start, a],
          [exit ? end : start + span, b, "out"],
        ],
      },
    });
  };
  const n = (prop: keyof Layer, fallback: number) =>
    Number(valueAt(layer[prop] as Value, start) ?? fallback);
  if (preset === "fade-in") animate("opacity", 0, n("opacity", 1));
  else if (preset === "fade-out") animate("opacity", n("opacity", 1), 0, true);
  else if (preset === "pop") animate("scale", 0, n("scale", 1));
  else if (preset.startsWith("slide-")) {
    const horizontal = /left|right/.test(preset);
    const prop = horizontal ? "x" : "y";
    const v = n(prop, 0);
    animate(prop, v + (/left|up/.test(preset) ? -160 : 160), v);
  } else if (preset === "drop") animate("y", n("y", 0), n("y", 0) + 1080, true);
  else if (preset.startsWith("wipe-"))
    out.reveal = {
      wipe: preset === "wipe-out" ? "rtl" : "ltr",
      from: preset === "wipe-out" ? end - span : start,
      to: preset === "wipe-out" ? end : start + span,
      ease: "out",
      ...(preset === "wipe-out" ? { out: true } : {}),
    };
  else if (preset === "pulse" || preset === "float") {
    const prop = preset === "pulse" ? "scale" : "y";
    const v = n(prop, preset === "pulse" ? 1 : 0);
    Object.assign(out, {
      [prop]: {
        keys: Array.from({ length: 9 }, (_, i) => [
          start + ((end - start) * i) / 8,
          v + (i % 2 ? (preset === "pulse" ? 0.08 : -18) : 0),
          "inOut",
        ]),
      },
    });
  } else {
    const kind = preset === "glitch" ? "chroma" : preset;
    out.behave = [
      ...(layer.behave ?? []),
      {
        [kind]: {
          start,
          amplitude: kind === "drift" ? 12 : kind === "wiggle" ? 4 : 1,
          speed: 1,
          step: 0.04,
          seed: 17,
        },
      },
    ];
  }
  return out;
}
