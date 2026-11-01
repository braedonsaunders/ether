import type { AvailabilityConfig } from "./types";

export type ReleaseMode = "playable" | "digital" | "immediate";

/** Map stored flags onto the three owner modes. */
export function releaseMode(cfg: AvailabilityConfig): ReleaseMode {
  if (cfg.mode === "digital" || cfg.mode === "immediate" || cfg.mode === "playable")
    return cfg.mode;
  if (cfg.disableMovieEvidenceGate) return "immediate";
  if (cfg.digitalDateEvidence) return "digital";
  return "playable";
}

/** Write engine flags for a chosen mode. Gentle/Unlimited become self-tuned. */
export function applyReleaseMode(cfg: AvailabilityConfig, mode: ReleaseMode): AvailabilityConfig {
  const next = { ...cfg, mode };
  if (mode === "digital") {
    next.monitor = false;
    next.disableMovieEvidenceGate = false;
    next.digitalDateEvidence = true;
  } else if (mode === "immediate") {
    next.monitor = false;
    next.disableMovieEvidenceGate = true;
    next.digitalDateEvidence = false;
  } else {
    next.monitor = true;
    next.disableMovieEvidenceGate = false;
    next.digitalDateEvidence = false;
  }
  if (next.effort === "gentle" || next.effort === "unlimited") next.effort = "normal";
  return next;
}

/** A blank or non-custom ceiling means Ether picks the daily limit. */
export function customRequestCeiling(cfg: AvailabilityConfig): number | null {
  if (cfg.effort === "custom" && cfg.feedRequests > 0) return cfg.feedRequests;
  return null;
}
