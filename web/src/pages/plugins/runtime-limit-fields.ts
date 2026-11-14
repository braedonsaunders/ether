import { getLocale, t } from "../../i18n";
import type { RuntimeLimits } from "./types";

export function deadlineHours(value?: string): number {
  return [...(value ?? "").matchAll(/([\d.]+)(h|m|s)/g)].reduce(
    (sum, m) => sum + Number(m[1]) * (m[2] === "h" ? 1 : m[2] === "m" ? 1 / 60 : 1 / 3600),
    0,
  );
}
export function runtimeLimitFields() {
  return [
    { key: "memoryBytes", label: t("Memory (MiB)"), scale: 1048576, fallback: 2048, max: 4096 },
    { key: "deadline", label: t("Time (hours)"), scale: 1, fallback: 12, max: 8760 },
    { key: "unitBudget", label: t("Work units"), scale: 1, fallback: 0 },
    { key: "fetchBytes", label: t("Download (GiB)"), scale: 1073741824, fallback: 8 },
    {
      key: "archiveMemberBytes",
      label: t("File inside an archive (GiB)"),
      scale: 1073741824,
      fallback: 8,
    },
    {
      key: "archiveExpandedBytes",
      label: t("Archive total (GiB)"),
      scale: 1073741824,
      fallback: 32,
    },
    {
      key: "archiveMembers",
      label: t("Files per archive"),
      scale: 1,
      fallback: 65535,
      max: 1000000,
    },
  ] satisfies {
    key: keyof RuntimeLimits;
    label: string;
    scale: number;
    fallback: number;
    max?: number;
  }[];
}
export function limitNumber(key: keyof RuntimeLimits, limits?: Partial<RuntimeLimits>): number {
  return key === "deadline" ? deadlineHours(limits?.deadline) : Number(limits?.[key] ?? 0);
}
export function formatLimit(key: keyof RuntimeLimits, value: number): string {
  const n = new Intl.NumberFormat(getLocale(), { maximumFractionDigits: 4 }).format(value);
  if (key === "deadline") return t("{value} hours", { value: n });
  if (key === "memoryBytes") return t("{value} MiB", { value: n });
  if (key === "fetchBytes" || key === "archiveMemberBytes" || key === "archiveExpandedBytes")
    return t("{value} GiB", { value: n });
  if (key === "unitBudget" && (!value || value >= 2 ** 53)) return t("No limit");
  return n;
}
