import { cn } from "@braedonsaunders/appkit-ui";
import { useEffect, useRef, useState, type CSSProperties } from "react";
import { t } from "../i18n";
import art from "../lib/brand-art.json";

type Motion = "static" | "intro" | "loop";
const delay = (seconds: number) => ({ "--ether-delay": `${seconds}s` }) as CSSProperties;

function MarkArt() {
  return (
    <g transform="translate(16 16)" fill="none" strokeLinecap="round" strokeLinejoin="round">
      {art.mark.map((p) => (
        <path
          key={p.d}
          d={p.d}
          pathLength={1}
          strokeWidth={p.width}
          className={cn(
            "ether-trace",
            p.color === "accent" ? "ether-accent" : "ether-ink",
            p.pulse && "ether-signal",
          )}
          style={delay(p.delay)}
        />
      ))}
    </g>
  );
}

/** Continuum: one geometry source for the app, README SVGs and platform icons. */
export function EtherMark({
  size = 28,
  className,
  motion = "static",
}: {
  size?: number;
  className?: string;
  motion?: Motion;
}) {
  return (
    <svg
      width={size}
      height={size}
      viewBox="16 16 96 96"
      aria-hidden
      className={cn("ether-logo shrink-0", className)}
      data-motion={motion}
    >
      <MarkArt />
    </svg>
  );
}

export function EtherLogo({
  className,
  motion = "static",
  label = "Ether",
}: {
  className?: string;
  motion?: Motion;
  label?: string;
}) {
  return (
    <svg
      viewBox="0 0 510 128"
      role="img"
      aria-label={label}
      className={cn("ether-logo block h-auto w-32", className)}
      data-motion={motion}
    >
      <MarkArt />
      <g
        transform="translate(142 23) scale(1.32)"
        fill="none"
        strokeWidth={7}
        strokeLinecap="round"
        strokeLinejoin="round"
      >
        {art.word.map((p) => (
          <path
            key={p.x}
            transform={`translate(${p.x} 0)`}
            d={p.d}
            pathLength={1}
            className="ether-trace ether-ink"
            style={delay(p.delay)}
          />
        ))}
      </g>
    </svg>
  );
}

export function Brand({ serverName }: { serverName?: string }) {
  return (
    <span className="flex min-w-0 flex-col gap-1">
      <EtherLogo className="w-[116px]" />
      {serverName && serverName.toLowerCase() !== "ether" && (
        <span className="block truncate text-[10px] leading-tight text-fg-muted">{serverName}</span>
      )}
    </span>
  );
}

/** Root-only reveal: wait for bootstrap and the entrance, then retire until reload. */
export function BrandSplash({ ready }: { ready: boolean }) {
  const [phase, setPhase] = useState<"visible" | "fading" | "gone">("visible");
  const shownAt = useRef(performance.now());
  useEffect(() => {
    if (!ready) return;
    const reduced = window.matchMedia("(prefers-reduced-motion: reduce)").matches;
    let finish: ReturnType<typeof setTimeout> | undefined;
    const start = setTimeout(
      () => {
        setPhase("fading");
        finish = setTimeout(() => setPhase("gone"), reduced ? 0 : 400);
      },
      Math.max(0, (reduced ? 350 : 2000) - (performance.now() - shownAt.current)),
    );
    return () => {
      clearTimeout(start);
      clearTimeout(finish);
    };
  }, [ready]);
  if (phase === "gone") return null;
  return (
    <div
      className={cn("ether-splash", phase === "fading" && "ether-splash-fading")}
      aria-hidden
      data-brand-splash
    >
      <div className="ether-splash-center">
        <EtherLogo motion="intro" className="w-full" />
        <p>{t("Your Plex. Beyond the static library.")}</p>
        <div className="ether-splash-progress" />
      </div>
    </div>
  );
}
