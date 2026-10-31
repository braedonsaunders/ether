import { toast } from "@braedonsaunders/appkit-ui";
import { useCallback } from "react";
import { useApp } from "../../app-context";
import { t } from "../../i18n";
import { api } from "../../lib/api";
import { useAsync } from "../../lib/hooks";
import type { EtherConfig, QualityProfile } from "../../lib/types";

export type ResolverPolicy = EtherConfig["resolver"];

/** The resolver section, saved whole so a partial edit does not drop other fields. */
export function useResolver() {
  const { refreshDashboard } = useApp();
  const config = useAsync(() => api.getConfig(), []);
  const resolver = config.data?.resolver ?? null;
  const save = useCallback(
    async (patch: Partial<ResolverPolicy>, message: string) => {
      if (!resolver) return;
      await api.putSection("resolver", {
        ...resolver,
        customFormats: resolver.customFormats ?? [],
        customFormatScores: resolver.customFormatScores ?? {},
        ...patch,
      });
      toast.success(message);
      await config.reload();
      void refreshDashboard();
    },
    [config, refreshDashboard, resolver],
  );
  return { config, resolver, save };
}

/** Parse the two JSON editors; throws a readable error when either is invalid. */
export function parseScoring(formats: string, scores: string) {
  let definitions: unknown;
  let overrides: unknown;
  try {
    definitions = JSON.parse(formats);
  } catch {
    throw Error(t("Custom format definitions aren't valid JSON."));
  }
  try {
    overrides = JSON.parse(scores);
  } catch {
    throw Error(t("Score overrides aren't valid JSON."));
  }
  if (!Array.isArray(definitions)) throw Error(t("Definitions must be a JSON array."));
  if (
    !overrides ||
    Array.isArray(overrides) ||
    typeof overrides !== "object" ||
    Object.values(overrides).some((v) => typeof v !== "number" || !Number.isInteger(v))
  )
    throw Error(t("Scores must map format IDs to whole numbers."));
  return {
    customFormats: definitions,
    customFormatScores: overrides as Record<string, number>,
  };
}

/** What the profile drawer shows: shared definitions, and this profile's scores. */
export function scoringText(resolver: ResolverPolicy | null, profile: QualityProfile) {
  const own = profile.customFormatScores;
  const scores = own && Object.keys(own).length > 0 ? own : (resolver?.customFormatScores ?? {});
  return {
    formats: JSON.stringify(resolver?.customFormats ?? [], null, 2),
    scores: JSON.stringify(scores, null, 2),
  };
}
