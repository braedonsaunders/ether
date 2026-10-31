import type { ReactNode } from "react";
import debridlink from "../../assets/brands/debridlink.svg?no-inline";
import stremio from "../../assets/brands/stremio.svg?no-inline";

/**
 * Vendored Simple Icons marks, same treatment as integration-brands.
 * The caller passes the mark; nothing is fetched at runtime.
 */
function BrandMark({ src, color }: { src: string; color: string }) {
  return (
    <span
      aria-hidden
      className="block size-7 bg-current"
      style={{
        color,
        maskImage: `url("${src}")`,
        maskPosition: "center",
        maskSize: "contain",
        maskRepeat: "no-repeat",
        WebkitMaskImage: `url("${src}")`,
        WebkitMaskPosition: "center",
        WebkitMaskSize: "contain",
        WebkitMaskRepeat: "no-repeat",
      }}
    />
  );
}

export function DebridLinkMark() {
  return <BrandMark src={debridlink} color="#264E70" />;
}

export function StremioMark() {
  return <BrandMark src={stremio} color="#685CEE" />;
}

/** A real mark when Simple Icons publishes one for this debrid service. */
export function debridMark(kind: string): ReactNode | null {
  if (kind === "debridlink") return <DebridLinkMark />;
  return null;
}
