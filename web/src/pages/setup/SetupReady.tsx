import { Button } from "@braedonsaunders/appkit-ui";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useNavigate } from "react-router-dom";
import { t } from "../../i18n";
import { api } from "../../lib/api";
import type { EtherConfig } from "../../lib/types";
import { appPath, appRank } from "../apps/catalog";

const CONNECT_PANEL_APPS = ["jellyfin", "emby", "silo", "dlna"];

/** The finish chip opens the app that shows a server address, not the Apps overview. */
export function connectDevicePath(frontends: EtherConfig["frontends"]): string {
  const enabled = Object.entries(frontends ?? {})
    .filter(([, row]) => row?.enabled)
    .map(([id]) => id)
    .sort((a, b) => appRank(a) - appRank(b));
  const panel = enabled.find((id) => CONNECT_PANEL_APPS.includes(id));
  const id = panel ?? enabled[0];
  if (!id) return "/apps";
  if (["stremio", "webdav", "m3u"].includes(id)) return "/me";
  return `/apps/${appPath(id)}?connect=1`;
}

function enabledFrontendIds(cfg: EtherConfig | null): string[] {
  if (!cfg) return [];
  if (cfg.frontends) {
    return Object.entries(cfg.frontends)
      .filter(([, entry]) => Boolean(entry?.enabled))
      .map(([id]) => id);
  }
  return cfg.setupComplete || cfg.plex?.claimed ? ["plex"] : [];
}

/**
 * Ribbon specification for the full-viewport Continuum assembly.
 *
 * Coordinate system:
 * - The full-viewport SVG uses `viewBox="-852 -552 1800 1200"`, whose center is `(48, 48)`.
 * - The SVG is anchored inside the `112px`/`128px` (`size-28 sm:size-32`) Mark Stage with
 *   `w-[1875%] h-[1250%]` (`1800 / 96 = 18.75`, `1200 / 96 = 12.5`).
 * - Consequently, the `[0..96, 0..96]` square at the center of the SVG maps 1:1 to the
 *   `112px`/`128px` Mark Stage at every viewport size, and the assembled mark never shifts
 *   by even a single pixel when transitioning from the sweeping animation to the settled state.
 * - For `"primary"` ribbons, the SVG path is the smooth C1 concatenation of:
 *   - Part A (`approachD`): a monotone sweeping curve from the viewport edge into `[0..96, 0..96]`
 *   - Part B (`markD`): the exact canonical stroke of the Ether Continuum mark (`brand-art.json`)
 *   Animating `strokeDasharray` to `<L_B> <L_total>` and `strokeDashoffset` to `-L_A` causes the
 *   viewport-spanning ribbon to flow directly into and become the permanent mark stroke.
 */
export interface ContinuumRibbonSpec {
  id: string;
  role: "primary" | "secondary";
  approachD: string;
  markD?: string;
  strokeWidth: number;
  /** Final stroke tone once assembled into the mark. */
  color: "ink" | "teal-bright" | "teal-mid" | "teal-deep";
  delayMs: number;
  durationMs: number;
  peakOpacity?: number;
}

export const ETHER_READY_RIBBONS: readonly ContinuumRibbonSpec[] = [
  // 1. Outer Continuum U-curve (top half of `M80 17H43C24 17 11 30 11 48S24 79 43 79H80`):
  //    sweeps in from top-right into the open top-right mouth (80, 17), glides left to (43, 17),
  //    and curves down to the left apex (11, 48).
  {
    id: "right-outer-top",
    role: "primary",
    approachD: "M 960 -320 C 660 -255 380 -85 195 -6 C 135 15 104 17 80 17",
    markD: "L 43 17 C 24 17 11 30 11 48",
    strokeWidth: 9,
    color: "ink",
    delayMs: 0,
    durationMs: 2140,
  },
  // 2. Outer Continuum U-curve (bottom half of `M80 17H43C24 17 11 30 11 48S24 79 43 79H80`):
  //    sweeps in from bottom-right into the open bottom-right mouth (80, 79), glides left to (43, 79),
  //    and curves up to meet at the left apex (11, 48).
  {
    id: "right-outer-bottom",
    role: "primary",
    approachD: "M 960 416 C 660 351 380 181 195 102 C 135 81 104 79 80 79",
    markD: "L 43 79 C 24 79 11 66 11 48",
    strokeWidth: 9,
    color: "ink",
    delayMs: 45,
    durationMs: 2140,
  },
  // 3. Inner Continuum C-curve (top half of `M78 32H43C33 32 27 38 27 48S33 64 43 64H78`):
  //    sweeps in from upper-right into (78, 32), glides left to (43, 32), and curves down to (27, 48).
  {
    id: "right-inner-top",
    role: "primary",
    approachD: "M 960 -140 C 660 -100 380 -12 190 22 C 132 31 102 32 78 32",
    markD: "L 43 32 C 33 32 27 38 27 48",
    strokeWidth: 8,
    color: "teal-bright",
    delayMs: 95,
    durationMs: 2100,
  },
  // 4. Inner Continuum C-curve (bottom half of `M78 32H43C33 32 27 38 27 48S33 64 43 64H78`):
  //    sweeps in from lower-right into (78, 64), glides left to (43, 64), and curves up to (27, 48).
  {
    id: "right-inner-bottom",
    role: "primary",
    approachD: "M 960 236 C 660 196 380 108 190 74 C 132 65 102 64 78 64",
    markD: "L 43 64 C 33 64 27 58 27 48",
    strokeWidth: 8,
    color: "teal-bright",
    delayMs: 135,
    durationMs: 2100,
  },
  // 5. Central Continuum signal bar (`M45 48H80`) fed from top-left:
  //    sweeps down from top-left across the left half of the viewport into the centerline (45, 48)
  //    and locks onto (45, 48) -> (80, 48).
  {
    id: "left-core-upper",
    role: "primary",
    approachD: "M -960 -310 C -640 -240 -330 -40 -95 34 C -25 46 15 48 45 48",
    markD: "L 80 48",
    strokeWidth: 8,
    color: "teal-bright",
    delayMs: 30,
    durationMs: 2120,
  },
  // 6. Central Continuum signal bar (`M45 48H80`) fed from bottom-left:
  //    sweeps up from bottom-left across the left half of the viewport into the centerline (45, 48)
  //    and locks onto (45, 48) -> (80, 48).
  {
    id: "left-core-lower",
    role: "primary",
    approachD: "M -960 406 C -640 336 -330 136 -95 62 C -25 50 15 48 45 48",
    markD: "L 80 48",
    strokeWidth: 8,
    color: "teal-bright",
    delayMs: 75,
    durationMs: 2120,
  },
  // Secondary harmonic ribbons: softer companion curves that escort the primary
  // streams from all sides and dissolve inside the central channel as the mark locks in.
  {
    id: "whisper-left-upper",
    role: "secondary",
    approachD: "M -960 -120 C -640 -85 -330 8 -90 40 C -25 47 18 48 62 48",
    strokeWidth: 4,
    color: "teal-mid",
    delayMs: 110,
    durationMs: 1920,
    peakOpacity: 0.52,
  },
  {
    id: "whisper-left-lower",
    role: "secondary",
    approachD: "M -960 216 C -640 181 -330 88 -90 56 C -25 49 18 48 62 48",
    strokeWidth: 4,
    color: "teal-deep",
    delayMs: 155,
    durationMs: 1940,
    peakOpacity: 0.48,
  },
  {
    id: "whisper-right-center",
    role: "secondary",
    approachD: "M 980 48 C 680 48 400 48 180 48 C 120 48 85 48 45 48",
    strokeWidth: 3.5,
    color: "teal-mid",
    delayMs: 170,
    durationMs: 1900,
    peakOpacity: 0.44,
  },
];

interface ThemePalette {
  ink: string;
  tealBright: string;
  tealMid: string;
  tealDeep: string;
}

const DARK_PALETTE: ThemePalette = {
  ink: "#f0f0e9",
  tealBright: "#52dfc5",
  tealMid: "#28c5ae",
  tealDeep: "#149f8b",
};

const LIGHT_PALETTE: ThemePalette = {
  ink: "#1c2927",
  tealBright: "#087f76",
  tealMid: "#0d9488",
  tealDeep: "#14b0a2",
};

function currentPalette(): ThemePalette {
  if (typeof document === "undefined") return DARK_PALETTE;
  const root = document.documentElement;
  if (root.classList.contains("light")) return LIGHT_PALETTE;
  if (root.classList.contains("dark")) return DARK_PALETTE;
  if (
    typeof window !== "undefined" &&
    typeof window.matchMedia === "function" &&
    window.matchMedia("(prefers-color-scheme: light)").matches
  ) {
    return LIGHT_PALETTE;
  }
  return DARK_PALETTE;
}

function resolveRibbonColor(
  color: ContinuumRibbonSpec["color"],
  palette: ThemePalette,
  stage: "sweep" | "settled",
): string {
  switch (color) {
    case "ink":
      return stage === "sweep" ? palette.tealBright : palette.ink;
    case "teal-bright":
      return palette.tealBright;
    case "teal-mid":
      return palette.tealMid;
    case "teal-deep":
      return palette.tealDeep;
  }
}

function safePathLength(el: SVGPathElement | null, fallback: number): number {
  if (!el) return fallback;
  try {
    const len = el.getTotalLength();
    if (Number.isFinite(len) && len > 0) return len;
  } catch {
    // JSDOM or non-layout environments may not implement SVGGeometryElement.getTotalLength.
  }
  return fallback;
}

function prefersReducedMotion(): boolean {
  if (typeof window === "undefined" || typeof window.matchMedia !== "function") return false;
  return window.matchMedia("(prefers-reduced-motion: reduce)").matches;
}

export interface SetupReadyProps {
  initialConfig?: EtherConfig | null;
}

export function SetupReady({ initialConfig = null }: SetupReadyProps) {
  const nav = useNavigate();
  const reducedMotion = useMemo(() => prefersReducedMotion(), []);
  const [cfg, setCfg] = useState<EtherConfig | null>(initialConfig);
  const [settled, setSettled] = useState<boolean>(reducedMotion);

  useEffect(() => {
    if (initialConfig) {
      setCfg(initialConfig);
      return;
    }
    void api
      .getConfig()
      .then(setCfg)
      .catch(() => {});
  }, [initialConfig]);

  const visiblePathRefs = useRef<Record<string, SVGPathElement | null>>({});
  const glowPathRefs = useRef<Record<string, SVGPathElement | null>>({});
  const measureApproachRefs = useRef<Record<string, SVGPathElement | null>>({});
  const measureMarkRefs = useRef<Record<string, SVGPathElement | null>>({});
  const animationsRef = useRef<Animation[]>([]);
  const timersRef = useRef<number[]>([]);

  const clearScheduled = useCallback(() => {
    for (const id of timersRef.current) {
      window.clearTimeout(id);
    }
    timersRef.current = [];
  }, []);

  const snapPathsToSettled = useCallback(() => {
    for (const anim of animationsRef.current) {
      try {
        anim.cancel();
      } catch {
        // Ignore cancellation errors on detached nodes.
      }
    }
    animationsRef.current = [];

    const palette = currentPalette();

    for (const spec of ETHER_READY_RIBBONS) {
      const el = visiblePathRefs.current[spec.id];
      const glowEl = glowPathRefs.current[spec.id];
      // `left-core-lower` shares the exact same `L 80 48` mark stroke as `left-core-upper`;
      // hiding one of the two identical overlays on settle prevents doubled glow opacity.
      if (spec.role === "secondary" || spec.id === "left-core-lower") {
        if (el) el.style.opacity = "0";
        if (glowEl) glowEl.style.opacity = "0";
        continue;
      }
      const lA = safePathLength(measureApproachRefs.current[spec.id] ?? null, 1050);
      const lB = safePathLength(measureMarkRefs.current[spec.id] ?? null, 95);
      const total = lA + lB + 40;
      const finalColor = resolveRibbonColor(spec.color, palette, "settled");
      if (el) {
        el.style.strokeDasharray = `${lB.toFixed(2)} ${total.toFixed(2)}`;
        el.style.strokeDashoffset = `${(-lA).toFixed(2)}`;
        el.style.opacity = "1";
        el.style.stroke = finalColor;
      }
      if (glowEl) {
        glowEl.style.strokeDasharray = `${lB.toFixed(2)} ${total.toFixed(2)}`;
        glowEl.style.strokeDashoffset = `${(-lA).toFixed(2)}`;
        glowEl.style.opacity = "0.24";
        glowEl.style.stroke = resolveRibbonColor(spec.color, palette, "sweep");
      }
    }
  }, []);

  const skipToSettled = useCallback(() => {
    clearScheduled();
    snapPathsToSettled();
    setSettled(true);
  }, [clearScheduled, snapPathsToSettled]);

  useEffect(() => {
    if (reducedMotion) {
      snapPathsToSettled();
      setSettled(true);
      return;
    }

    const canAnimate =
      typeof Element !== "undefined" && typeof Element.prototype.animate === "function";

    if (!canAnimate) {
      snapPathsToSettled();
      const fallbackTimer = window.setTimeout(() => setSettled(true), 60);
      timersRef.current.push(fallbackTimer);
      return clearScheduled;
    }

    const palette = currentPalette();
    const createdAnimations: Animation[] = [];
    const easing = "cubic-bezier(0.32, 0.04, 0.18, 1)";

    for (const spec of ETHER_READY_RIBBONS) {
      const el = visiblePathRefs.current[spec.id];
      const glowEl = glowPathRefs.current[spec.id];
      if (!el) continue;

      const sweepColor = resolveRibbonColor(spec.color, palette, "sweep");
      const settledColor = resolveRibbonColor(spec.color, palette, "settled");

      if (spec.role === "primary") {
        const lA = safePathLength(measureApproachRefs.current[spec.id] ?? null, 1050);
        const lB = safePathLength(measureMarkRefs.current[spec.id] ?? null, 95);
        const total = lA + lB + 48;
        const ribbonWindow = Math.min(Math.max(lA * 0.44, 340), lA * 0.66);
        const midStop = -(lA * 0.52);

        // Lock final settled styles on the element so when the animation finishes
        // (or is cancelled), the assembled mark stroke remains cleanly in place.
        el.style.strokeDasharray = `${lB.toFixed(2)} ${total.toFixed(2)}`;
        el.style.strokeDashoffset = `${(-lA).toFixed(2)}`;
        el.style.opacity = spec.id === "left-core-lower" ? "0" : "1";
        el.style.stroke = settledColor;

        const keyframes: Keyframe[] = [
          {
            strokeDasharray: `1 ${total.toFixed(2)}`,
            strokeDashoffset: "0",
            opacity: 0,
            stroke: sweepColor,
            offset: 0,
          },
          {
            strokeDasharray: `${(ribbonWindow * 0.68).toFixed(2)} ${total.toFixed(2)}`,
            strokeDashoffset: `${(midStop * 0.3).toFixed(2)}`,
            opacity: 0.96,
            stroke: sweepColor,
            offset: 0.24,
          },
          {
            strokeDasharray: `${ribbonWindow.toFixed(2)} ${total.toFixed(2)}`,
            strokeDashoffset: `${midStop.toFixed(2)}`,
            opacity: 1,
            stroke: sweepColor,
            offset: 0.56,
          },
          {
            strokeDasharray: `${Math.max(lB * 1.35, 95).toFixed(2)} ${total.toFixed(2)}`,
            strokeDashoffset: `${(-(lA * 0.92)).toFixed(2)}`,
            opacity: 1,
            stroke: sweepColor,
            offset: 0.82,
          },
          {
            strokeDasharray: `${lB.toFixed(2)} ${total.toFixed(2)}`,
            strokeDashoffset: `${(-lA).toFixed(2)}`,
            opacity: spec.id === "left-core-lower" ? 0 : 1,
            stroke: settledColor,
            offset: 1,
          },
        ];

        createdAnimations.push(
          el.animate(keyframes, {
            duration: spec.durationMs,
            delay: spec.delayMs,
            easing,
            fill: "both",
          }),
        );

        if (glowEl) {
          glowEl.style.strokeDasharray = `${lB.toFixed(2)} ${total.toFixed(2)}`;
          glowEl.style.strokeDashoffset = `${(-lA).toFixed(2)}`;
          glowEl.style.opacity = spec.id === "left-core-lower" ? "0" : "0.24";
          glowEl.style.stroke = sweepColor;

          createdAnimations.push(
            glowEl.animate(
              [
                {
                  strokeDasharray: `1 ${total.toFixed(2)}`,
                  strokeDashoffset: "0",
                  opacity: 0,
                  offset: 0,
                },
                {
                  strokeDasharray: `${(ribbonWindow * 0.72).toFixed(2)} ${total.toFixed(2)}`,
                  strokeDashoffset: `${(midStop * 0.3).toFixed(2)}`,
                  opacity: 0.5,
                  offset: 0.26,
                },
                {
                  strokeDasharray: `${ribbonWindow.toFixed(2)} ${total.toFixed(2)}`,
                  strokeDashoffset: `${midStop.toFixed(2)}`,
                  opacity: 0.58,
                  offset: 0.58,
                },
                {
                  strokeDasharray: `${lB.toFixed(2)} ${total.toFixed(2)}`,
                  strokeDashoffset: `${(-lA).toFixed(2)}`,
                  opacity: spec.id === "left-core-lower" ? 0 : 0.24,
                  offset: 1,
                },
              ],
              {
                duration: spec.durationMs,
                delay: spec.delayMs,
                easing,
                fill: "both",
              },
            ),
          );
        }
      } else {
        const lA = safePathLength(measureApproachRefs.current[spec.id] ?? null, 1050);
        const total = lA + 60;
        const tailLen = Math.min(Math.max(lA * 0.35, 260), 460);
        const peak = spec.peakOpacity ?? 0.42;

        el.style.strokeDasharray = `0 ${total.toFixed(2)}`;
        el.style.strokeDashoffset = `${(-lA).toFixed(2)}`;
        el.style.opacity = "0";
        el.style.stroke = sweepColor;

        createdAnimations.push(
          el.animate(
            [
              {
                strokeDasharray: `1 ${total.toFixed(2)}`,
                strokeDashoffset: "0",
                opacity: 0,
                offset: 0,
              },
              {
                strokeDasharray: `${tailLen.toFixed(2)} ${total.toFixed(2)}`,
                strokeDashoffset: `${(-(lA * 0.28)).toFixed(2)}`,
                opacity: peak,
                offset: 0.34,
              },
              {
                strokeDasharray: `${(tailLen * 0.65).toFixed(2)} ${total.toFixed(2)}`,
                strokeDashoffset: `${(-(lA * 0.78)).toFixed(2)}`,
                opacity: peak * 0.65,
                offset: 0.76,
              },
              {
                strokeDasharray: `1 ${total.toFixed(2)}`,
                strokeDashoffset: `${(-lA).toFixed(2)}`,
                opacity: 0,
                offset: 1,
              },
            ],
            {
              duration: spec.durationMs,
              delay: spec.delayMs,
              easing,
              fill: "both",
            },
          ),
        );

        if (glowEl) {
          glowEl.style.strokeDasharray = `0 ${total.toFixed(2)}`;
          glowEl.style.strokeDashoffset = `${(-lA).toFixed(2)}`;
          glowEl.style.opacity = "0";
          glowEl.style.stroke = sweepColor;

          createdAnimations.push(
            glowEl.animate(
              [
                {
                  strokeDasharray: `1 ${total.toFixed(2)}`,
                  strokeDashoffset: "0",
                  opacity: 0,
                  offset: 0,
                },
                {
                  strokeDasharray: `${tailLen.toFixed(2)} ${total.toFixed(2)}`,
                  strokeDashoffset: `${(-(lA * 0.28)).toFixed(2)}`,
                  opacity: peak * 0.55,
                  offset: 0.36,
                },
                {
                  strokeDasharray: `1 ${total.toFixed(2)}`,
                  strokeDashoffset: `${(-lA).toFixed(2)}`,
                  opacity: 0,
                  offset: 1,
                },
              ],
              {
                duration: spec.durationMs,
                delay: spec.delayMs,
                easing,
                fill: "both",
              },
            ),
          );
        }
      }
    }

    animationsRef.current = createdAnimations;

    // Trigger the settled glow pulse and staggered welcome reveal right as the
    // primary ribbons finish locking into the Continuum mark (~2.22s).
    const settleTimer = window.setTimeout(() => {
      snapPathsToSettled();
      setSettled(true);
    }, 2220);
    timersRef.current.push(settleTimer);

    return () => {
      clearScheduled();
      for (const anim of createdAnimations) {
        try {
          anim.cancel();
        } catch {
          // Ignore cleanup errors.
        }
      }
    };
  }, [clearScheduled, reducedMotion, snapPathsToSettled]);

  // Keep settled stroke colors in sync if the user toggles the theme while on the ready screen.
  useEffect(() => {
    if (!settled || typeof MutationObserver === "undefined" || typeof document === "undefined") {
      return;
    }
    const observer = new MutationObserver(() => {
      snapPathsToSettled();
    });
    observer.observe(document.documentElement, {
      attributes: true,
      attributeFilter: ["class"],
    });
    return () => observer.disconnect();
  }, [settled, snapPathsToSettled]);

  useEffect(() => {
    if (settled) return;
    const onKeyDown = (e: KeyboardEvent) => {
      if (e.key === "Escape") {
        e.preventDefault();
        skipToSettled();
      }
    };
    window.addEventListener("keydown", onKeyDown);
    return () => window.removeEventListener("keydown", onKeyDown);
  }, [settled, skipToSettled]);

  const leaveTo = useCallback(
    (destination: string) => {
      clearScheduled();
      nav(destination);
    },
    [clearScheduled, nav],
  );

  // Build contextual next-step pills underneath "Get started".
  const enabledFrontends = enabledFrontendIds(cfg);
  const plexEnabled = enabledFrontends.includes("plex");
  const plexClaimed = Boolean(cfg?.plex?.claimed || cfg?.plex?.token);
  const debridOn = (cfg?.debrid?.providers ?? []).some(
    (p) => p.enabled && p.apiKey.trim().length > 0,
  );
  const usenetOn = (cfg?.usenet?.providers ?? []).some(
    (p) => p.enabled && p.host.trim().length > 0,
  );
  const anyApp = enabledFrontends.length > 0;

  const pills: { label: string; to: string }[] = [];
  if (!debridOn && !usenetOn) {
    pills.push({ label: t("Add a debrid account"), to: "/sources" });
  }
  if (plexEnabled && !plexClaimed) {
    pills.push({ label: t("Connect Plex"), to: "/apps/plex/account" });
  }
  if (anyApp) {
    pills.push({ label: t("Connect a device"), to: connectDevicePath(cfg?.frontends) });
  }
  pills.push({ label: t("Add viewers"), to: "/viewers" });
  pills.push({ label: t("Choose playback quality"), to: "/releases/quality" });

  const summaryLine =
    debridOn || usenetOn
      ? t(
          "Your settings are saved and your server is running — open Overview or pick a next step below.",
        )
      : t("Your settings are saved — add a debrid or Usenet account next so titles can play.");

  const palette = currentPalette();

  return (
    <section
      aria-label={t("Ether is ready")}
      data-ready-state={settled ? "settled" : "animating"}
      onClick={!settled ? skipToSettled : undefined}
      className={`ether-ready-stage relative isolate -mx-4 -my-5 flex min-h-[calc(100dvh-3.5rem)] w-[calc(100%+2rem)] flex-1 flex-col items-center justify-center overflow-hidden px-6 py-12 select-none sm:-mx-6 sm:-my-6 sm:w-[calc(100%+3rem)] lg:-mx-10 lg:w-[calc(100%+5rem)] ${
        !settled ? "cursor-pointer" : ""
      }`}
    >
      {/* Subtle atmospheric radial glow across the canvas */}
      <div
        aria-hidden="true"
        className="ether-ready-atmosphere pointer-events-none absolute inset-0"
      />

      <div className="relative z-10 flex w-full max-w-lg flex-col items-center text-center">
        {/* Mark Stage: 112px on mobile, 128px on desktop.
            The full-viewport SVG is anchored to the exact center of this box so [0..96, 0..96]
            maps 1:1 to this stage across all viewport sizes. */}
        <div className="relative size-28 shrink-0 sm:size-32">
          <div
            aria-hidden="true"
            className={`ether-ready-pulse pointer-events-none absolute -inset-10 rounded-full ${
              settled ? "ether-ready-pulse-active" : ""
            }`}
          />

          <svg
            aria-hidden="true"
            viewBox="-852 -552 1800 1200"
            className="ether-ready-svg pointer-events-none absolute top-1/2 left-1/2 h-[1250%] w-[1875%] max-w-none -translate-x-1/2 -translate-y-1/2 overflow-visible"
            fill="none"
          >
            {/* Hidden geometry measurement paths so getTotalLength() gives exact Part A and Part B lengths */}
            <defs>
              {ETHER_READY_RIBBONS.map((spec) => (
                <g key={`measure-${spec.id}`}>
                  <path
                    ref={(el) => {
                      measureApproachRefs.current[spec.id] = el;
                    }}
                    d={spec.approachD}
                  />
                  {spec.markD && (
                    <path
                      ref={(el) => {
                        measureMarkRefs.current[spec.id] = el;
                      }}
                      d={`M ${spec.approachD.split(/\s+/).slice(-2).join(" ")} ${spec.markD}`}
                    />
                  )}
                </g>
              ))}
            </defs>

            {/* Luminous under-glow layer */}
            {ETHER_READY_RIBBONS.map((spec) => {
              const fullD = spec.markD ? `${spec.approachD} ${spec.markD}` : spec.approachD;
              return (
                <path
                  key={`glow-${spec.id}`}
                  ref={(el) => {
                    glowPathRefs.current[spec.id] = el;
                  }}
                  d={fullD}
                  stroke={resolveRibbonColor(spec.color, palette, "sweep")}
                  strokeWidth={spec.strokeWidth * 2.15}
                  strokeLinecap="round"
                  strokeLinejoin="round"
                  className="ether-ready-ribbon-glow"
                />
              );
            })}

            {/* Crisp foreground ribbons that converge from viewport edges and lock into the Ether mark */}
            {ETHER_READY_RIBBONS.map((spec) => {
              const fullD = spec.markD ? `${spec.approachD} ${spec.markD}` : spec.approachD;
              return (
                <path
                  key={spec.id}
                  ref={(el) => {
                    visiblePathRefs.current[spec.id] = el;
                  }}
                  data-ribbon-id={spec.id}
                  data-ribbon-role={spec.role}
                  d={fullD}
                  stroke={resolveRibbonColor(spec.color, palette, settled ? "settled" : "sweep")}
                  strokeWidth={spec.strokeWidth}
                  strokeLinecap="round"
                  strokeLinejoin="round"
                />
              );
            })}
          </svg>
        </div>

        {/* Welcome typography, explanatory sentence, primary action, and secondary pills.
            Always stays in DOM flow so the Mark Stage never jumps vertically on settle. */}
        <div
          aria-hidden={!settled}
          inert={!settled ? true : undefined}
          className={`mt-8 flex flex-col items-center ${
            settled ? "ether-ready-content-visible" : "ether-ready-content-hidden"
          }`}
        >
          <h1 className="ether-ready-title text-2xl font-semibold tracking-tight text-fg sm:text-3xl">
            {t("Ether is ready")}
          </h1>

          <p className="ether-ready-subtitle mt-2.5 max-w-md text-sm leading-relaxed text-fg-muted">
            {summaryLine}
          </p>

          <div className="ether-ready-cta mt-6">
            <Button
              size="lg"
              className="min-w-48 px-8"
              onClick={(e) => {
                e.stopPropagation();
                leaveTo("/");
              }}
            >
              {t("Get started")}
            </Button>
          </div>

          {pills.length > 0 && (
            <div className="ether-ready-pills mt-5 flex flex-wrap items-center justify-center gap-2">
              {pills.map((pill) => (
                <button
                  key={`${pill.to}-${pill.label}`}
                  type="button"
                  onClick={(e) => {
                    e.stopPropagation();
                    leaveTo(pill.to);
                  }}
                  className="rounded-full border border-border/80 bg-surface/60 px-3.5 py-1.5 text-xs font-medium text-fg-muted backdrop-blur-xs transition hover:border-border hover:bg-raised/70 hover:text-fg focus:outline-none focus-visible:ring-2 focus-visible:ring-accent"
                >
                  {pill.label}
                </button>
              ))}
            </div>
          )}
        </div>
      </div>

      {/* Subtle Skip affordance anchored near the bottom edge while the ribbon convergence plays */}
      {!settled && (
        <button
          type="button"
          onClick={(e) => {
            e.stopPropagation();
            skipToSettled();
          }}
          className="absolute bottom-7 left-1/2 z-20 -translate-x-1/2 rounded-full px-3 py-1 text-xs font-medium text-fg-subtle transition hover:text-fg-muted focus:outline-none focus-visible:ring-2 focus-visible:ring-accent"
        >
          {t("Skip")}
        </button>
      )}
    </section>
  );
}

export default SetupReady;
