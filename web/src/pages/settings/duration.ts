export type DurationUnit = "s" | "m" | "h" | "d";
export function parseDuration(raw: string): { amount: string; unit: DurationUnit } {
  if (!raw) return { amount: "", unit: "h" };
  const factors: Record<string, number> = {
    ns: 1e-9,
    us: 1e-6,
    µs: 1e-6,
    ms: 0.001,
    s: 1,
    m: 60,
    h: 3600,
  };
  let seconds = 0;
  const parts = [...raw.matchAll(/([+-]?\d+(?:\.\d+)?)(ns|us|µs|ms|s|m|h)/g)];
  if (parts.map((p) => p[0]).join("") !== raw) return { amount: raw, unit: "h" };
  for (const p of parts) seconds += Number(p[1]) * factors[p[2]!]!;
  const unit =
    seconds && seconds % 86400 === 0
      ? "d"
      : seconds % 3600 === 0
        ? "h"
        : seconds % 60 === 0
          ? "m"
          : "s";
  return { amount: String(seconds / { d: 86400, h: 3600, m: 60, s: 1 }[unit]), unit };
}
export function formatDuration(amount: string, unit: DurationUnit): string {
  if (amount === "") return "";
  const seconds = Number(amount) * { d: 86400, h: 3600, m: 60, s: 1 }[unit];
  if (seconds % 3600 === 0) return `${seconds / 3600}h`;
  if (seconds % 60 === 0) return `${seconds / 60}m`;
  return `${Number(seconds.toPrecision(12))}s`;
}
