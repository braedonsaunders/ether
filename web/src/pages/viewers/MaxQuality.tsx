import { SettingsSection } from "@braedonsaunders/appkit-ui";
import { useState } from "react";
import { Segmented } from "../../components/fields";
import { ErrorPanel, LoadingRows } from "../../components/feedback";
import { SettingsRow } from "../../components/SettingsRow";
import { t } from "../../i18n";
import { errorMessage } from "../../lib/api";
import type { QualityProfile, QualityRule } from "../../lib/types";
import { useQuality } from "../releases/useQuality";

const LEVELS = [
  { value: "2160", label: "4K" },
  { value: "1080", label: "1080p" },
  { value: "720", label: "720p" },
] as const;

/** The rule that is only about this viewer (no device, platform or app). */
export function viewerQualityRule(rules: QualityRule[], userId: number): QualityRule | undefined {
  return rules.find((r) => r.userId === userId && !r.product && !r.platform && !r.protocol);
}

/** Quality levels that exist as profiles, highest first. */
export function qualityLevels(profiles: QualityProfile[]) {
  return LEVELS.filter((level) => profiles.some((p) => p.maxHeight === Number(level.value)));
}

/**
 * The highest quality this viewer may stream, as a per-viewer quality rule.
 * "Auto" removes the rule, so the usual defaults apply. Saving is immediate:
 * it writes the whole quality section, like Device rules does.
 */
export function MaxQuality({ viewerId }: { viewerId: number }) {
  const { config, quality, commit } = useQuality();
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState("");

  if (config.error) return <ErrorPanel error={config.error} onRetry={() => void config.reload()} />;
  if (!quality) return <LoadingRows rows={1} />;

  const profiles = quality.profiles ?? [];
  const rules = quality.rules ?? [];
  const levels = qualityLevels(profiles);
  const current = viewerQualityRule(rules, viewerId);
  const currentProfile = profiles.find((p) => p.name === current?.profile);
  const value =
    currentProfile && levels.some((l) => Number(l.value) === currentProfile.maxHeight)
      ? String(currentProfile.maxHeight)
      : "auto";

  async function change(next: string) {
    if (!quality || saving) return;
    const rest = rules.filter((r) => viewerQualityRule([r], viewerId) !== r);
    let nextRules = rest;
    if (next !== "auto") {
      const profile = profiles.find((p) => p.maxHeight === Number(next));
      if (!profile) return;
      // First, so this viewer's cap wins over device rules and the Away profile.
      nextRules = [{ userId: viewerId, profile: profile.name }, ...rest];
    }
    setSaving(true);
    setError("");
    try {
      await commit({ ...quality, rules: nextRules }, t("Max quality saved"));
    } catch (e) {
      setError(errorMessage(e));
    } finally {
      setSaving(false);
    }
  }

  return (
    <SettingsSection title={t("Streaming quality")}>
      <SettingsRow
        title={t("Max quality")}
        description={
          levels.length
            ? t("The highest quality this viewer can stream. Auto uses the usual defaults.")
            : t("No quality profiles are set up yet, so there is nothing to choose.")
        }
        control={
          <Segmented
            ariaLabel={t("Max quality")}
            value={value}
            onChange={(v) => void change(v)}
            options={[{ value: "auto", label: t("Auto") }, ...levels]}
          />
        }
      />
      {error ? (
        <p role="alert" className="px-5 py-2 text-sm text-danger">
          {error}
        </p>
      ) : null}
    </SettingsSection>
  );
}
