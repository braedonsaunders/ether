import { Button } from "@braedonsaunders/appkit-ui";
import { memo, useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useNavigate } from "react-router-dom";
import { t } from "../../i18n";
import { api } from "../../lib/api";
import type { EtherConfig } from "../../lib/types";
import { connectDevicePath } from "./connect-path";

export { connectDevicePath };

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
 * - Each `"primary"` ribbon owns a distinct, non-overlapping segment of the canonical
 *   Ether Continuum mark (`brand-art.json`), so no two primary strokes ever stack or
 *   toggle opacity against one another during settle.
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
    durationMs: 2100,
  },
  // 2. Outer Continuum U-curve (bottom half of `M80 17H43C24 17 11 30 11 48S24 79 43 79H80`):
  //    sweeps in from bottom-right into the open bottom-right mouth (80, 79), glides left to (43, 79),
  //    and curves up to meet at the left apex (11, 48) at the exact same millisecond (2100ms).
  {
    id: "right-outer-bottom",
    role: "primary",
    approachD: "M 960 416 C 660 351 380 181 195 102 C 135 81 104 79 80 79",
    markD: "L 43 79 C 24 79 11 66 11 48",
    strokeWidth: 9,
    color: "ink",
    delayMs: 20,
    durationMs: 2080,
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
    delayMs: 70,
    durationMs: 2050,
  },
  // 4. Inner Continuum C-curve (bottom half of `M78 32H43C33 32 27 38 27 48S33 64 43 64H78`):
  //    sweeps in from lower-right into (78, 64), glides left to (43, 64), and curves up to (27, 48)
  //    at the exact same millisecond (2120ms).
  {
    id: "right-inner-bottom",
    role: "primary",
    approachD: "M 960 236 C 660 196 380 108 190 74 C 132 65 102 64 78 64",
    markD: "L 43 64 C 33 64 27 58 27 48",
    strokeWidth: 8,
    color: "teal-bright",
    delayMs: 90,
    durationMs: 2030,
  },
  // 5. Central Continuum signal bar (`M45 48H80`) fed from top-left:
  //    sweeps across the left half of the viewport into the centerline (45, 48)
  //    and locks onto (45, 48) -> (80, 48) as the single permanent owner of the middle bar.
  {
    id: "left-core-signal",
    role: "primary",
    approachD: "M -960 -260 C -640 -195 -330 -28 -95 35 C -25 46 15 48 45 48",
    markD: "L 80 48",
    strokeWidth: 8,
    color: "teal-bright",
    delayMs: 40,
    durationMs: 2080,
  },
  // Secondary harmonic ribbons from the left: escort the primary stream and taper
  // smoothly into the outer/inner left apexes before the mark settles, never crossing
  // over the central signal bar.
  {
    id: "whisper-left-lower",
    role: "secondary",
    approachD: "M -960 360 C -640 295 -340 120 -110 60 C -55 49 -15 48 11 48",
    strokeWidth: 5,
    color: "teal-mid",
    delayMs: 60,
    durationMs: 1860,
    peakOpacity: 0.56,
  },
  {
    id: "whisper-left-mid",
    role: "secondary",
    approachD: "M -960 88 C -640 78 -340 58 -110 49 C -50 48 -8 48 27 48",
    strokeWidth: 4,
    color: "teal-deep",
    delayMs: 110,
    durationMs: 1820,
    peakOpacity: 0.46,
  },
];

interface ThemePalette {
  ink: string;
  inkMid: string;
  inkNear: string;
  tealBright: string;
  tealMid: string;
  tealDeep: string;
}

const DARK_PALETTE: ThemePalette = {
  ink: "#f0f0e9",
  inkMid: "#a6ece0",
  inkNear: "#d8f3ec",
  tealBright: "#52dfc5",
  tealMid: "#28c5ae",
  tealDeep: "#149f8b",
};

const LIGHT_PALETTE: ThemePalette = {
  ink: "#1c2927",
  inkMid: "#115e57",
  inkNear: "#173f3b",
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
  stage: "sweep" | "mid" | "near" | "settled",
): string {
  switch (color) {
    case "ink":
      if (stage === "sweep") return palette.tealBright;
      if (stage === "mid") return palette.inkMid;
      if (stage === "near") return palette.inkNear;
      return palette.ink;
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

function SetupReadyInner({ initialConfig = null }: SetupReadyProps) {
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

  const svgRef = useRef<SVGSVGElement | null>(null);
  const pulseRef = useRef<HTMLDivElement | null>(null);
  const markBloomRef = useRef<HTMLDivElement | null>(null);
  const titleRef = useRef<HTMLHeadingElement | null>(null);
  const subtitleRef = useRef<HTMLParagraphElement | null>(null);
  const ctaRef = useRef<HTMLDivElement | null>(null);
  const pillsRef = useRef<HTMLDivElement | null>(null);
  const skipBtnRef = useRef<HTMLButtonElement | null>(null);
  const animationsRef = useRef<Animation[]>([]);
  const timersRef = useRef<number[]>([]);

  const clearScheduled = useCallback(() => {
    for (const id of timersRef.current) {
      window.clearTimeout(id);
    }
    timersRef.current = [];
  }, []);

  const applySettledStyles = useCallback((cancelRunning: boolean) => {
    if (cancelRunning) {
      for (const anim of animationsRef.current) {
        try {
          anim.cancel();
        } catch {
          // Ignore cancellation errors on detached nodes.
        }
      }
      animationsRef.current = [];
    }

    const svg = svgRef.current;
    const palette = currentPalette();

    if (svg) {
      for (const spec of ETHER_READY_RIBBONS) {
        const el = svg.querySelector<SVGPathElement>(`path[data-ribbon-id="${spec.id}"]`);
        if (spec.role === "secondary") {
          if (el) el.style.opacity = "0";
          continue;
        }
        const mA = svg.querySelector<SVGPathElement>(`path[data-measure-approach="${spec.id}"]`);
        const mB = svg.querySelector<SVGPathElement>(`path[data-measure-mark="${spec.id}"]`);
        const lA = safePathLength(mA, 1050);
        const lB = safePathLength(mB, 95);
        const total = lA + lB + 48;
        const finalColor = resolveRibbonColor(spec.color, palette, "settled");
        if (el) {
          el.style.strokeDasharray = `${lB.toFixed(2)} ${total.toFixed(2)}`;
          el.style.strokeDashoffset = `${(-lA).toFixed(2)}`;
          el.style.opacity = "1";
          el.style.stroke = finalColor;
        }
      }
    }

    if (pulseRef.current) {
      pulseRef.current.style.opacity = "0.48";
      pulseRef.current.style.transform = "scale3d(1, 1, 1)";
    }
    if (markBloomRef.current) {
      markBloomRef.current.style.opacity = "0.34";
      markBloomRef.current.style.transform = "scale3d(1, 1, 1)";
    }
    for (const node of [titleRef.current, subtitleRef.current, ctaRef.current, pillsRef.current]) {
      if (node) {
        node.style.opacity = "1";
        node.style.transform = "translate3d(0, 0, 0)";
      }
    }
  }, []);

  const skipToSettled = useCallback(() => {
    clearScheduled();
    applySettledStyles(true);
    setSettled(true);
  }, [applySettledStyles, clearScheduled]);

  useEffect(() => {
    if (reducedMotion) {
      applySettledStyles(true);
      setSettled(true);
      return;
    }

    const canAnimate =
      typeof Element !== "undefined" && typeof Element.prototype.animate === "function";

    if (!canAnimate) {
      applySettledStyles(true);
      const fallbackTimer = window.setTimeout(() => setSettled(true), 60);
      timersRef.current.push(fallbackTimer);
      return clearScheduled;
    }

    const svg = svgRef.current;
    const palette = currentPalette();
    const createdAnimations: Animation[] = [];
    const ribbonEasing = "cubic-bezier(0.25, 0.1, 0.2, 1)";

    if (svg) {
      for (const spec of ETHER_READY_RIBBONS) {
        const el = svg.querySelector<SVGPathElement>(`path[data-ribbon-id="${spec.id}"]`);
        if (!el) continue;

        const sweepColor = resolveRibbonColor(spec.color, palette, "sweep");
        const midColor = resolveRibbonColor(spec.color, palette, "mid");
        const nearColor = resolveRibbonColor(spec.color, palette, "near");
        const settledColor = resolveRibbonColor(spec.color, palette, "settled");

        const mA = svg.querySelector<SVGPathElement>(`path[data-measure-approach="${spec.id}"]`);
        const mB = svg.querySelector<SVGPathElement>(`path[data-measure-mark="${spec.id}"]`);

        if (spec.role === "primary") {
          const lA = safePathLength(mA, 1050);
          const lB = safePathLength(mB, 95);
          const total = lA + lB + 48;

          // Strictly monotonic [s_tail, s_head] progression along the combined path:
          // neither s_tail nor s_head ever retracts or overshoots for any (lA, lB).
          const stops = [
            { offset: 0, tail: 0, head: 2, opacity: 0, stroke: sweepColor },
            {
              offset: 0.18,
              tail: 0.04 * lA,
              head: 0.36 * lA,
              opacity: 0.94,
              stroke: sweepColor,
            },
            {
              offset: 0.5,
              tail: 0.34 * lA,
              head: 0.78 * lA,
              opacity: 1,
              stroke: sweepColor,
            },
            {
              offset: 0.78,
              tail: 0.76 * lA,
              head: lA + 0.72 * lB,
              opacity: 1,
              stroke: midColor,
            },
            {
              offset: 0.92,
              tail: 0.94 * lA,
              head: lA + 0.96 * lB,
              opacity: 1,
              stroke: nearColor,
            },
            {
              offset: 1,
              tail: lA,
              head: lA + lB,
              opacity: 1,
              stroke: settledColor,
            },
          ];

          // Lock final settled styles underneath so when the animation completes,
          // the element already matches its final state without needing anim.cancel().
          el.style.strokeDasharray = `${lB.toFixed(2)} ${total.toFixed(2)}`;
          el.style.strokeDashoffset = `${(-lA).toFixed(2)}`;
          el.style.opacity = "1";
          el.style.stroke = settledColor;

          createdAnimations.push(
            el.animate(
              stops.map((s) => ({
                strokeDasharray: `${Math.max(1, s.head - s.tail).toFixed(2)} ${total.toFixed(2)}`,
                strokeDashoffset: `${(-s.tail).toFixed(2)}`,
                opacity: s.opacity,
                stroke: s.stroke,
                offset: s.offset,
              })),
              {
                duration: spec.durationMs,
                delay: spec.delayMs,
                easing: ribbonEasing,
                fill: "both",
              },
            ),
          );
        } else {
          const lA = safePathLength(mA, 1050);
          const total = lA + 60;
          const peak = spec.peakOpacity ?? 0.46;

          const secondaryStops = [
            { offset: 0, tail: 0, head: 2, opacity: 0 },
            { offset: 0.22, tail: 0.06 * lA, head: 0.4 * lA, opacity: peak },
            { offset: 0.55, tail: 0.38 * lA, head: 0.76 * lA, opacity: peak * 0.88 },
            { offset: 0.82, tail: 0.74 * lA, head: 0.96 * lA, opacity: peak * 0.42 },
            { offset: 1, tail: lA, head: lA + 0.5, opacity: 0 },
          ];

          el.style.strokeDasharray = `0.5 ${total.toFixed(2)}`;
          el.style.strokeDashoffset = `${(-lA).toFixed(2)}`;
          el.style.opacity = "0";
          el.style.stroke = sweepColor;

          createdAnimations.push(
            el.animate(
              secondaryStops.map((s) => ({
                strokeDasharray: `${Math.max(0.5, s.head - s.tail).toFixed(2)} ${total.toFixed(2)}`,
                strokeDashoffset: `${(-s.tail).toFixed(2)}`,
                opacity: s.opacity,
                offset: s.offset,
              })),
              {
                duration: spec.durationMs,
                delay: spec.delayMs,
                easing: ribbonEasing,
                fill: "both",
              },
            ),
          );
        }
      }
    }

    // Drive the pre-rendered radial glow halo on the exact same WAAPI timeline:
    // swells smoothly as the ribbons enter the mark (1350ms..2050ms) and eases
    // gently into its resting glow (2050ms..2650ms) via GPU opacity + scale3d.
    if (pulseRef.current) {
      pulseRef.current.style.opacity = "0.48";
      pulseRef.current.style.transform = "scale3d(1, 1, 1)";
      createdAnimations.push(
        pulseRef.current.animate(
          [
            { opacity: 0, transform: "scale3d(0.78, 0.78, 1)", offset: 0 },
            { opacity: 0.28, transform: "scale3d(0.92, 0.92, 1)", offset: 0.28 },
            { opacity: 0.74, transform: "scale3d(1.05, 1.05, 1)", offset: 0.56 },
            { opacity: 0.56, transform: "scale3d(1.01, 1.01, 1)", offset: 0.8 },
            { opacity: 0.48, transform: "scale3d(1, 1, 1)", offset: 1 },
          ],
          {
            delay: 1350,
            duration: 1300,
            easing: "cubic-bezier(0.22, 1, 0.36, 1)",
            fill: "both",
          },
        ),
      );
    }

    // Drive the pre-rendered blurred Continuum mark bloom layer via GPU opacity + scale3d
    // (never animating filter or strokeDasharray on a blurred element).
    if (markBloomRef.current) {
      markBloomRef.current.style.opacity = "0.34";
      markBloomRef.current.style.transform = "scale3d(1, 1, 1)";
      createdAnimations.push(
        markBloomRef.current.animate(
          [
            { opacity: 0, transform: "scale3d(0.92, 0.92, 1)", offset: 0 },
            { opacity: 0.24, transform: "scale3d(0.97, 0.97, 1)", offset: 0.26 },
            { opacity: 0.82, transform: "scale3d(1.03, 1.03, 1)", offset: 0.56 },
            { opacity: 0.46, transform: "scale3d(1.005, 1.005, 1)", offset: 0.82 },
            { opacity: 0.34, transform: "scale3d(1, 1, 1)", offset: 1 },
          ],
          {
            delay: 1400,
            duration: 1250,
            easing: "cubic-bezier(0.22, 1, 0.36, 1)",
            fill: "both",
          },
        ),
      );
    }

    // Drive the welcome copy and actions on the same continuous WAAPI timeline
    // so no React state re-render or CSS class flip happens during the glow peak.
    const revealItems: { el: HTMLElement | null; delay: number }[] = [
      { el: titleRef.current, delay: 1920 },
      { el: subtitleRef.current, delay: 1990 },
      { el: ctaRef.current, delay: 2070 },
      { el: pillsRef.current, delay: 2150 },
    ];
    for (const item of revealItems) {
      if (!item.el) continue;
      item.el.style.opacity = "1";
      item.el.style.transform = "translate3d(0, 0, 0)";
      createdAnimations.push(
        item.el.animate(
          [
            { opacity: 0, transform: "translate3d(0, 10px, 0)", offset: 0 },
            { opacity: 1, transform: "translate3d(0, 0, 0)", offset: 1 },
          ],
          {
            delay: item.delay,
            duration: 620,
            easing: "cubic-bezier(0.16, 1, 0.3, 1)",
            fill: "both",
          },
        ),
      );
    }

    if (skipBtnRef.current) {
      createdAnimations.push(
        skipBtnRef.current.animate(
          [
            { opacity: 1, offset: 0 },
            { opacity: 1, offset: 0.78 },
            { opacity: 0, offset: 1 },
          ],
          {
            delay: 0,
            duration: 2100,
            easing: "ease-out",
            fill: "both",
          },
        ),
      );
    }

    animationsRef.current = createdAnimations;

    // Mark the scene settled once the unified WAAPI timeline has finished,
    // without cancelling or snapping any animations.
    const settleTimer = window.setTimeout(() => {
      setSettled(true);
    }, 2780);
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
  }, [applySettledStyles, clearScheduled, reducedMotion]);

  // Keep settled stroke colors in sync if the user toggles the theme while on the ready screen.
  useEffect(() => {
    if (!settled || typeof MutationObserver === "undefined" || typeof document === "undefined") {
      return;
    }
    const observer = new MutationObserver(() => {
      applySettledStyles(false);
    });
    observer.observe(document.documentElement, {
      attributes: true,
      attributeFilter: ["class"],
    });
    return () => observer.disconnect();
  }, [applySettledStyles, settled]);

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
  const homeProfile = cfg?.quality?.default?.trim() || "1080p";
  pills.push({
    label: t("Playback quality: {profile} at home · change", { profile: homeProfile }),
    to: "/releases/quality",
  });

  const summaryLine =
    debridOn || usenetOn
      ? t("Your settings are saved and Ether is running. Pick a next step below.")
      : t("Your settings are saved. Add a debrid or Usenet account so titles can play.");

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
          {/* Layer 1: Wide radial ambient halo animated purely via GPU opacity + scale3d */}
          <div
            ref={pulseRef}
            aria-hidden="true"
            className="ether-ready-pulse pointer-events-none absolute -inset-16 rounded-full sm:-inset-20"
          />

          {/* Layer 2: Pre-rendered static blurred Continuum mark bloom layer.
              Blurred once as a static 128x128 GPU texture; only its container opacity
              and scale3d animate on the WAAPI timeline (never animating filter or dashes). */}
          <div
            ref={markBloomRef}
            aria-hidden="true"
            className="ether-ready-bloom pointer-events-none absolute inset-0"
          >
            <svg viewBox="0 0 96 96" className="size-full overflow-visible" fill="none">
              <path
                d="M80 17H43C24 17 11 30 11 48S24 79 43 79H80"
                stroke={palette.tealBright}
                strokeWidth="12"
                strokeLinecap="round"
                strokeLinejoin="round"
              />
              <path
                d="M78 32H43C33 32 27 38 27 48S33 64 43 64H78"
                stroke={palette.tealBright}
                strokeWidth="11"
                strokeLinecap="round"
                strokeLinejoin="round"
              />
              <path
                d="M45 48H80"
                stroke={palette.tealBright}
                strokeWidth="11"
                strokeLinecap="round"
                strokeLinejoin="round"
              />
            </svg>
          </div>

          {/* Layer 3: Crisp full-viewport vector ribbons (zero CSS filter on any path) */}
          <svg
            ref={svgRef}
            aria-hidden="true"
            viewBox="-852 -552 1800 1200"
            className="ether-ready-svg pointer-events-none absolute top-1/2 left-1/2 h-[1250%] w-[1875%] max-w-none -translate-x-1/2 -translate-y-1/2 overflow-visible"
            fill="none"
          >
            {/* Hidden geometry measurement paths so getTotalLength() gives exact Part A and Part B lengths */}
            <defs>
              {ETHER_READY_RIBBONS.map((spec) => (
                <g key={`measure-${spec.id}`}>
                  <path data-measure-approach={spec.id} d={spec.approachD} />
                  {spec.markD && (
                    <path
                      data-measure-mark={spec.id}
                      d={`M ${spec.approachD.split(/\s+/).slice(-2).join(" ")} ${spec.markD}`}
                    />
                  )}
                </g>
              ))}
            </defs>

            {/* Crisp foreground ribbons that converge from viewport edges and lock into the Ether mark */}
            {ETHER_READY_RIBBONS.map((spec) => {
              const fullD = spec.markD ? `${spec.approachD} ${spec.markD}` : spec.approachD;
              return (
                <path
                  key={spec.id}
                  data-ribbon-id={spec.id}
                  data-ribbon-role={spec.role}
                  d={fullD}
                  stroke={resolveRibbonColor(spec.color, palette, "settled")}
                  strokeWidth={spec.strokeWidth}
                  strokeLinecap="round"
                  strokeLinejoin="round"
                />
              );
            })}
          </svg>
        </div>

        {/* Welcome typography, explanatory sentence, primary action, and secondary pills.
            Driven on the same single WAAPI timeline so no React re-render interrupts the glow. */}
        <div className="mt-8 flex flex-col items-center">
          <h1
            ref={titleRef}
            className="ether-ready-title text-2xl font-semibold tracking-tight text-fg sm:text-3xl"
          >
            {t("Ether is ready")}
          </h1>

          <p
            ref={subtitleRef}
            className="ether-ready-subtitle mt-2.5 max-w-md text-sm leading-relaxed text-fg-muted"
          >
            {summaryLine}
          </p>

          <div ref={ctaRef} className="ether-ready-cta mt-6">
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
            <div
              ref={pillsRef}
              className="ether-ready-pills mt-5 flex flex-wrap items-center justify-center gap-2"
            >
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
          ref={skipBtnRef}
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

export const SetupReady = memo(SetupReadyInner);
export default SetupReady;
