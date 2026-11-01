import { cn } from "@braedonsaunders/appkit-ui";
import { Check, HardDrive, Leaf, Sparkles, Zap } from "lucide-react";
import { localized, t } from "../../i18n";
import type { EtherConfig } from "../../lib/types";

const GiB = 1024 ** 3;
const MiB = 1024 ** 2;

export type CachePresetChoice = {
  key: string;
  name: string;
  values: Partial<EtherConfig["cache"]>;
};

export const CACHE_PRESETS: {
  key: string;
  name: string;
  space: string;
  description: string;
  values: Partial<EtherConfig["cache"]>;
}[] = localized(() => [
  {
    key: "feather",
    name: t("Feather"),
    space: "256 MiB",
    description: t("Keeps only what you just played."),
    values: {
      preloadEnabled: false,
      preloadBudgetBytes: 0,
      mode: "demand",
      budgetBytes: 256 * MiB,
      retentionDays: 1,
      replayBoostHours: 0,
      detailBytes: 2 * MiB,
      detailWarmup: "on",
      disableSpeculation: false,
      prefetchNextEpisode: false,
      keepInProgressWarm: false,
      prefetchBytes: 16 * MiB,
    },
  },
  {
    key: "everyday",
    name: t("Everyday"),
    space: "32 GiB",
    description: t("The next episode starts quickly for a normal household."),
    values: {
      preloadEnabled: false,
      preloadBudgetBytes: 0,
      mode: "adaptive",
      budgetBytes: 32 * GiB,
      retentionDays: 7,
      replayBoostHours: 12,
      detailBytes: 8 * MiB,
      detailWarmup: "on",
      disableSpeculation: false,
      prefetchNextEpisode: true,
      keepInProgressWarm: false,
      prefetchBytes: 32 * MiB,
    },
  },
  {
    key: "encore",
    name: t("Encore"),
    space: "250 GiB",
    description: t("Favourite films stay ready to play again."),
    values: {
      preloadEnabled: false,
      preloadBudgetBytes: 0,
      mode: "adaptive",
      budgetBytes: 250 * GiB,
      retentionDays: 30,
      replayBoostHours: 48,
      detailBytes: 16 * MiB,
      detailWarmup: "on",
      disableSpeculation: false,
      prefetchNextEpisode: true,
      keepInProgressWarm: true,
      prefetchBytes: 64 * MiB,
    },
  },
  {
    key: "deep-shelf",
    name: t("Deep shelf"),
    space: "750 GiB",
    description: t("Holds more of what the household rewatches."),
    values: {
      preloadEnabled: false,
      preloadBudgetBytes: 0,
      mode: "adaptive",
      budgetBytes: 750 * GiB,
      retentionDays: 90,
      replayBoostHours: 72,
      detailBytes: 32 * MiB,
      detailWarmup: "on",
      disableSpeculation: false,
      prefetchNextEpisode: true,
      keepInProgressWarm: true,
      prefetchBytes: 64 * MiB,
    },
  },
  {
    key: "cinema-node",
    name: t("Cinema node"),
    space: "2 TiB",
    description: t("Complete movies and episodes."),
    values: {
      mode: "adaptive",
      budgetBytes: 2048 * GiB,
      retentionDays: 90,
      replayBoostHours: 72,
      detailBytes: 32 * MiB,
      detailWarmup: "on",
      disableSpeculation: false,
      prefetchNextEpisode: true,
      keepInProgressWarm: true,
      prefetchBytes: 64 * MiB,
      preloadEnabled: true,
      preloadBudgetBytes: 1536 * GiB,
      preloadMaxTitles: 400,
      preloadRateMiB: 20,
      preloadSource: "smart",
      preloadContent: "mixed",
      preloadHouseholdWeight: 65,
      preloadHouseholdScope: "home",
      preloadEpisodesAhead: 3,
      preloadHistoryDays: 90,
    },
  },
  {
    key: "edge-library",
    name: t("Edge library"),
    space: "20 TiB",
    description: t("Thousands of films."),
    values: {
      mode: "adaptive",
      budgetBytes: 20480 * GiB,
      retentionDays: 180,
      replayBoostHours: 96,
      detailBytes: 32 * MiB,
      detailWarmup: "on",
      disableSpeculation: false,
      prefetchNextEpisode: true,
      keepInProgressWarm: true,
      prefetchBytes: 64 * MiB,
      preloadEnabled: true,
      preloadBudgetBytes: 18432 * GiB,
      preloadMaxTitles: 2500,
      preloadRateMiB: 50,
      preloadSource: "smart",
      preloadContent: "mixed",
      preloadHouseholdWeight: 65,
      preloadHouseholdScope: "home",
      preloadEpisodesAhead: 3,
      preloadHistoryDays: 90,
    },
  },
]);

/** Everyday when the disk can hold it. A tight disk gets the largest preset that fits. */
export function presetForFreeBytes(freeBytes: number): (typeof CACHE_PRESETS)[number] | null {
  const everyday = CACHE_PRESETS.find((preset) => preset.key === "everyday");
  const everydayBudget = everyday?.values.budgetBytes ?? 0;
  if (everyday && everydayBudget > 0 && freeBytes * 0.8 >= everydayBudget) return everyday;
  const room = freeBytes * 0.8;
  let fit: (typeof CACHE_PRESETS)[number] | null = null;
  for (const preset of CACHE_PRESETS) {
    const budget = preset.values.budgetBytes ?? 0;
    if (budget > 0 && budget <= room) fit = preset;
  }
  return fit ?? CACHE_PRESETS.find((preset) => preset.key === "feather") ?? null;
}

export function cachePresetKey(cache: { mode?: string; budgetBytes?: number }): string | null {
  if (cache.mode === "off") return "pass-through";
  return (
    CACHE_PRESETS.find(
      (preset) =>
        preset.values.budgetBytes === cache.budgetBytes && preset.values.mode === cache.mode,
    )?.key ?? null
  );
}

export function CachePresetCards({
  selected,
  onSelect,
}: {
  selected: string | null;
  onSelect: (choice: CachePresetChoice) => void;
}) {
  return (
    <div className="space-y-2">
      <button
        type="button"
        aria-pressed={selected === "pass-through"}
        onClick={() =>
          onSelect({
            key: "pass-through",
            name: t("Pass-through"),
            values: {
              mode: "off",
              budgetBytes: 0,
              preloadEnabled: false,
              preloadBudgetBytes: 0,
              disableSpeculation: true,
            },
          })
        }
        className={cn(
          "flex w-full items-center gap-3 rounded-xl border p-3 text-left transition hover:border-primary/60",
          selected === "pass-through" ? "border-primary bg-primary/10" : "border-border bg-surface",
        )}
      >
        <Leaf size={18} className="text-primary" />
        <span className="flex-1">
          <span className="block text-sm font-semibold">{t("Pass-through")}</span>
          <span className="block text-xs text-fg-muted">
            {t("No persistent media. Stream and move on.")}
          </span>
        </span>
        <span className="text-xs text-primary">0 B</span>
      </button>
      <div className="grid grid-cols-2 gap-2">
        {CACHE_PRESETS.map((preset, i) => (
          <button
            key={preset.key}
            type="button"
            aria-pressed={selected === preset.key}
            onClick={() => onSelect(preset)}
            className={cn(
              "relative rounded-xl border p-3 text-left transition hover:border-primary/60 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring",
              selected === preset.key ? "border-primary bg-primary/10" : "border-border bg-surface",
            )}
          >
            <span className="flex items-center justify-between text-fg-muted">
              {i === 0 ? (
                <Leaf size={16} />
              ) : i === 1 ? (
                <Zap size={16} />
              ) : i === 2 ? (
                <Sparkles size={16} />
              ) : (
                <HardDrive size={16} />
              )}
              {selected === preset.key && <Check size={14} className="text-primary" />}
            </span>
            <span className="mt-3 block text-sm font-semibold">{preset.name}</span>
            <span className="block text-xs text-primary">{preset.space}</span>
            <span className="mt-2 block text-[11px] leading-relaxed text-fg-muted">
              {preset.description}
            </span>
          </button>
        ))}
      </div>
    </div>
  );
}
