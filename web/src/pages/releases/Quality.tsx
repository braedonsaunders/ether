import {
  Badge,
  Button,
  EmptyState,
  SearchSelect,
  SettingsSection,
  Switch,
  Textarea,
  toast,
  confirmDialog,
} from "@braedonsaunders/appkit-ui";
import { ChevronDown, Download, Plus, SlidersHorizontal } from "lucide-react";
import { useState } from "react";
import { Link } from "react-router-dom";
import { MoreOptions } from "../../components/ConfigSection";
import { ErrorPanel, LoadingRows } from "../../components/feedback";
import { ChipGroup, ListInput, NumberInput } from "../../components/fields";
import { Section } from "../../components/layout";
import { SettingsRow } from "../../components/SettingsRow";
import { DragHandle, SortableList, type DragHandleProps } from "../../components/Sortable";
import { localized, t, tn, tx } from "../../i18n";
import { searchSelectLabels } from "../../i18n/appkit-labels";
import { api } from "../../lib/api";
import { qualityGateLabel } from "../../lib/releases";
import type { QualityProfile, ReleaseRule } from "../../lib/types";
import { QUALITY_AUDIO, QUALITY_CODECS, QUALITY_SOURCES } from "../settings/schema";
import { sourceOrderFields, sourceOrderValue, TieBreakerSection } from "../sources/EqualQuality";
import {
  BoolRow,
  OrderButtons,
  RecordDrawer,
  RecordItem,
  TextRow,
  recordErrors,
  swap,
  type Errors,
} from "../sources/form";
import {
  DEFAULT_BUILTIN_RANKING,
  FilterRulesEditor,
  RuleEditorCard,
  RulePreview,
  insertPreferRuleInRanking,
  normalizeRanking,
  releaseRuleConflict,
  syncRulesWithRanking,
} from "./ReleasePreferences";
import { parseScoring, scoringText, type ResolverPolicy } from "./resolverDraft";
import { TrashImport } from "./TrashImport";
import { useQuality, type Quality } from "./useQuality";

function blankProfile(n: number): QualityProfile {
  return {
    name: t("Profile {n}", { n }),
    ranking: [...DEFAULT_BUILTIN_RANKING],
    maxHeight: 1080,
    minHeight: 720,
    allowHdr: false,
    allowDolbyVision: false,
    preferredSources: ["webdl", "bluray"],
    preferredCodecs: ["h264", "hevc"],
    preferredAudio: ["eac3", "ac3", "aac"],
    minSizeMbPerMin: 5,
    maxSizeMbPerMin: 150,
    episodeMinSizeMbPerMin: 0,
    episodeMaxSizeMbPerMin: 0,
    preferLargest: false,
    preferredLanguages: null,
    allowCam: false,
  };
}

const HEIGHTS = localized(() => [
  { value: "0", label: t("Any") },
  { value: "480", label: "480p" },
  { value: "720", label: "720p" },
  { value: "1080", label: "1080p" },
  { value: "2160", label: t("4K (2160p)") },
]);

const heightLabel = (h: number) => HEIGHTS.find((x) => x.value === String(h))?.label ?? `${h}p`;

export function profileSummary(p: QualityProfile) {
  const range =
    p.minHeight === p.maxHeight
      ? heightLabel(p.maxHeight)
      : t("{min} to {max}", { min: heightLabel(p.minHeight), max: heightLabel(p.maxHeight) });
  return [
    range,
    p.allowDolbyVision ? t("HDR and Dolby Vision") : p.allowHdr ? t("HDR allowed") : t("SDR only"),
    p.preferredSources?.length
      ? t("prefers {sources}", { sources: p.preferredSources.join(", ") })
      : "",
  ]
    .filter(Boolean)
    .join(" · ");
}

/** Ordered multi-pick: chips toggle membership; the order picked is the preference order. */
function PreferenceList({
  label,
  all,
  value,
  onChange,
}: {
  label: string;
  all: string[];
  value: string[] | null;
  onChange: (v: string[]) => void;
}) {
  const v = value ?? [];
  return (
    <div className="space-y-1.5">
      <ChipGroup
        ariaLabel={label}
        size="sm"
        value={v}
        onChange={(next) =>
          onChange(next.filter((x) => v.includes(x)).concat(next.filter((x) => !v.includes(x))))
        }
        options={[...all, ...v.filter((x) => !all.includes(x))].map((x) => ({
          value: x,
          label: x,
        }))}
      />
      <p className="text-xs text-fg-muted">
        {v.length ? t("Preference: {order}", { order: v.join(" › ") }) : t("No preference")}
      </p>
    </div>
  );
}

export function hasImportedRules(draft: QualityProfile, resolver: ResolverPolicy | null): boolean {
  const gates = draft.allowedQualities ?? [];
  const ownScores = Object.keys(draft.customFormatScores ?? {}).length;
  const sharedFormats = (resolver?.customFormats ?? []).length;
  const sharedScores = Object.keys(resolver?.customFormatScores ?? {}).length;
  return (
    gates.length > 0 ||
    draft.minCustomFormatScore != null ||
    ownScores > 0 ||
    sharedFormats > 0 ||
    sharedScores > 0
  );
}

/** Section 4: shown only when the profile carries imported TRaSH / Radarr / Sonarr rules or custom formats. */
function ImportedSection({
  draft,
  resolver,
  scoring,
  setScoring,
  patch,
}: {
  draft: QualityProfile;
  resolver: ResolverPolicy | null;
  scoring: { formats: string; scores: string } | null;
  setScoring: React.Dispatch<React.SetStateAction<{ formats: string; scores: string } | null>>;
  patch: (p: Partial<QualityProfile>) => void;
}) {
  if (!hasImportedRules(draft, resolver)) return null;
  const gates = draft.allowedQualities ?? [];
  const scored = Object.keys(draft.customFormatScores ?? {}).length;
  return (
    <SettingsSection
      title={t("Imported from TRaSH / Radarr / Sonarr")}
      description={t("Carried over from a Radarr or Sonarr profile.")}
    >
      {gates.length ? (
        <SettingsRow
          stacked
          title={t("Allowed qualities")}
          description={t(
            "Other release type and resolution pairs are never played with this profile.",
          )}
        >
          <span className="flex flex-wrap items-center gap-1.5">
            {gates.map((q) => (
              <Badge key={q} variant="outline">
                {qualityGateLabel(q)}
              </Badge>
            ))}
            <Button variant="ghost" size="sm" onClick={() => patch({ allowedQualities: null })}>
              {t("Allow all")}
            </Button>
          </span>
        </SettingsRow>
      ) : null}
      <SettingsRow
        title={t("Minimum format score")}
        description={
          scored
            ? tn(
                scored,
                "Releases whose custom formats score lower aren't played. This profile sets its own scores for {count} format.",
                "Releases whose custom formats score lower aren't played. This profile sets its own scores for {count} formats.",
              )
            : t("Releases whose custom formats score lower aren't played.")
        }
        control={
          <NumberInput
            aria-label={t("Minimum format score")}
            value={draft.minCustomFormatScore ?? 0}
            className="w-24"
            onChange={(n) => patch({ minCustomFormatScore: n })}
          />
        }
      />
      <MoreOptions label={t("More options: Custom scoring")}>
        <div className="space-y-4 py-3">
          <p className="px-4 text-sm text-fg-muted">
            {tx("Scores are whole numbers, for example {example}.", {
              example: <code>{'{ "x265-hd": -500 }'}</code>,
            })}
          </p>
          <label className="block space-y-2 px-4 text-sm font-medium text-fg">
            {t("Custom format definitions")}
            <Textarea
              className="min-h-40 w-full font-mono text-xs"
              spellCheck={false}
              value={scoring?.formats ?? "[]"}
              onChange={(e) =>
                setScoring((s) =>
                  s ? { ...s, formats: e.target.value } : { formats: e.target.value, scores: "{}" },
                )
              }
            />
          </label>
          <label className="block space-y-2 px-4 pb-2 text-sm font-medium text-fg">
            {t("Score overrides for this profile")}
            <Textarea
              className="min-h-28 w-full font-mono text-xs"
              spellCheck={false}
              value={scoring?.scores ?? "{}"}
              onChange={(e) =>
                setScoring((s) =>
                  s ? { ...s, scores: e.target.value } : { formats: "[]", scores: e.target.value },
                )
              }
            />
          </label>
          <p className="px-4 pb-2 text-sm">
            <Link to="/releases/history" className="text-primary underline">
              {t("Inspect release scores and rejection reasons")}
            </Link>
          </p>
        </div>
      </MoreOptions>
    </SettingsSection>
  );
}

/** Section 2: ONE numbered, draggable list representing the profile's ranking ladder. */
function WhatMattersMostSection({
  draft,
  patch,
  rankingError,
}: {
  draft: QualityProfile;
  patch: (p: Partial<QualityProfile>) => void;
  rankingError?: string;
}) {
  const [expanded, setExpanded] = useState<string | null>(null);
  const ladder = normalizeRanking(draft);
  const rules = draft.releaseRules ?? [];

  function updateRanking(nextRanking: string[]) {
    patch({
      ranking: nextRanking,
      releaseRules: syncRulesWithRanking(rules, nextRanking),
    });
  }

  function updatePreferRule(id: string, rulePatch: Partial<ReleaseRule>) {
    const nextRules = rules.map((r) => (r.id === id ? { ...r, ...rulePatch } : r));
    let nextRanking = ladder;
    if (rulePatch.action === "require" || rulePatch.action === "avoid") {
      nextRanking = ladder.filter((s) => s !== `rule:${id}`);
    } else if (rulePatch.action === "prefer" && !ladder.includes(`rule:${id}`)) {
      nextRanking = insertPreferRuleInRanking(ladder, id);
    }
    patch({
      ranking: nextRanking,
      releaseRules: syncRulesWithRanking(nextRules, nextRanking),
    });
  }

  function removePreferRule(id: string) {
    const nextRules = rules.filter((r) => r.id !== id);
    const nextRanking = ladder.filter((s) => s !== `rule:${id}`);
    patch({
      ranking: nextRanking,
      releaseRules: nextRules,
    });
  }

  function renderBuiltinStep(step: string, handle: DragHandleProps, index: number) {
    const fixed = step === "connection" || step === "verified";
    const stepMeta: Record<string, { title: string; summary: string; expandable?: boolean }> = {
      language: {
        title: t("Audio language"),
        summary: draft.preferredLanguages?.length
          ? draft.preferredLanguages.join(" › ")
          : t("Any language"),
        expandable: true,
      },
      connection: {
        title: t("Connection fit"),
        summary: t("Fits the viewer's bandwidth"),
      },
      resolution: {
        title: t("Resolution"),
        summary:
          draft.minHeight === draft.maxHeight
            ? heightLabel(draft.maxHeight)
            : t("{min} to {max}, highest first", {
                min: heightLabel(draft.minHeight),
                max: heightLabel(draft.maxHeight),
              }),
      },
      releaseType: {
        title: t("Release type"),
        summary: draft.preferredSources?.length
          ? draft.preferredSources.join(" › ")
          : t("No preference"),
        expandable: true,
      },
      hdr: {
        title: t("HDR"),
        summary: draft.allowDolbyVision
          ? t("Dolby Vision › HDR10 › SDR")
          : draft.allowHdr
            ? t("HDR10 › SDR")
            : t("SDR only"),
      },
      verified: {
        title: t("Played fine recently"),
        summary: t("Verified streams rank ahead of untested ones"),
      },
      videoCodec: {
        title: t("Video codec"),
        summary: draft.preferredCodecs?.length
          ? draft.preferredCodecs.join(" › ")
          : t("No preference"),
        expandable: true,
      },
      audioFormat: {
        title: t("Audio format"),
        summary: draft.preferredAudio?.length
          ? draft.preferredAudio.join(" › ")
          : t("No preference"),
        expandable: true,
      },
      size: {
        title: t("Larger file"),
        summary: draft.preferLargest
          ? t("Ranks larger files higher within the allowed size range")
          : t("Off"),
      },
    };
    const meta = stepMeta[step] ?? { title: step, summary: "" };
    const isExpanded = expanded === step;

    return (
      <div
        data-ranking-step={step}
        className={`space-y-3 px-3 py-3 ${fixed ? "bg-bg-subtle/40 opacity-70" : ""}`}
      >
        <div className="flex items-center gap-2">
          {fixed ? (
            <span className="size-8 shrink-0" aria-hidden />
          ) : (
            <DragHandle handle={handle} label={t("Move {name}", { name: meta.title })} />
          )}
          <span className="w-5 text-xs tabular-nums text-fg-muted">{index + 1}</span>
          <div className="min-w-0 flex-1">
            <p className="text-sm font-medium text-fg">{meta.title}</p>
            <p className="truncate text-xs text-fg-muted">{meta.summary}</p>
          </div>
          {fixed ? <Badge variant="secondary">{t("Automatic")}</Badge> : null}
          {step === "size" ? (
            <Switch
              aria-label={t("Larger file")}
              checked={draft.preferLargest ?? false}
              onChange={(e) => patch({ preferLargest: e.currentTarget.checked })}
            />
          ) : null}
          {meta.expandable ? (
            <Button
              variant="ghost"
              size="sm"
              aria-expanded={isExpanded}
              aria-label={t("Configure {name}", { name: meta.title })}
              onClick={() => setExpanded(isExpanded ? null : step)}
            >
              {t("Values")}
              <ChevronDown size={13} />
            </Button>
          ) : null}
          {!fixed ? (
            <OrderButtons
              label={meta.title}
              i={index}
              n={ladder.length}
              onMove={(d) => updateRanking(swap(ladder, index, index + d))}
            />
          ) : null}
        </div>
        {isExpanded && step === "language" ? (
          <div className="space-y-1.5 border-t border-border pt-3">
            <ListInput
              ariaLabel={t("Languages")}
              value={draft.preferredLanguages}
              onChange={(v) => patch({ preferredLanguages: v.length ? v.map(String) : null })}
              placeholder="en, fr"
            />
            <p className="text-xs text-fg-muted">
              {t("ISO codes in preference order. Empty accepts any.")}
            </p>
          </div>
        ) : null}
        {isExpanded && step === "releaseType" ? (
          <div className="border-t border-border pt-3">
            <PreferenceList
              label={t("Release type")}
              all={QUALITY_SOURCES}
              value={draft.preferredSources}
              onChange={(v) => patch({ preferredSources: v })}
            />
          </div>
        ) : null}
        {isExpanded && step === "videoCodec" ? (
          <div className="border-t border-border pt-3">
            <PreferenceList
              label={t("Video codec")}
              all={QUALITY_CODECS}
              value={draft.preferredCodecs}
              onChange={(v) => patch({ preferredCodecs: v })}
            />
          </div>
        ) : null}
        {isExpanded && step === "audioFormat" ? (
          <div className="border-t border-border pt-3">
            <PreferenceList
              label={t("Audio format")}
              all={QUALITY_AUDIO}
              value={draft.preferredAudio}
              onChange={(v) => patch({ preferredAudio: v })}
            />
          </div>
        ) : null}
      </div>
    );
  }

  return (
    <SettingsSection
      title={t("What matters most")}
      description={t("Checked from top to bottom. Drag or use the arrows to change the order.")}
    >
      <div className="space-y-3 px-5 py-4">
        {rankingError ? (
          <p role="alert" className="text-sm text-danger">
            {rankingError}
          </p>
        ) : null}
        <SortableList
          items={ladder}
          getId={(step) => step}
          onReorder={(next) => updateRanking(next)}
          className="divide-y divide-border overflow-hidden rounded-lg border border-border"
          renderItem={(step, handle, index) => {
            if (step.startsWith("rule:")) {
              const id = step.slice(5);
              const r = rules.find((x) => x.id === id);
              if (!r) return null;
              return (
                <div data-ranking-step={step}>
                  <RuleEditorCard
                    rule={r}
                    handle={handle}
                    stepNumber={index + 1}
                    canMoveUp={index > 0}
                    canMoveDown={index < ladder.length - 1}
                    onMove={(d) => updateRanking(swap(ladder, index, index + d))}
                    onChange={(rulePatch) => updatePreferRule(r.id, rulePatch)}
                    onRemove={() => removePreferRule(r.id)}
                  />
                </div>
              );
            }
            return renderBuiltinStep(step, handle, index);
          }}
        />
      </div>
      <RulePreview profile={draft} />
    </SettingsSection>
  );
}

/** Profiles tab: only the profile cards plus "Add profile" and "Import from TRaSH Guides". */
export function QualityTab() {
  const { config, quality, commit, refresh } = useQuality();
  if (config.error) return <ErrorPanel error={config.error} onRetry={() => void config.reload()} />;
  if (!quality) return <LoadingRows rows={4} />;
  return (
    <ProfileList
      quality={quality}
      resolver={config.data?.resolver ?? null}
      commit={commit}
      refresh={refresh}
    />
  );
}

type Commit = (next: Quality, message: string) => Promise<void>;

function ProfileList({
  quality,
  resolver,
  commit,
  refresh,
}: {
  quality: Quality;
  resolver: ResolverPolicy | null;
  commit: Commit;
  refresh: () => Promise<void>;
}) {
  const profiles = quality.profiles ?? [];
  const [importing, setImporting] = useState(false);
  const rules = quality.rules ?? [];
  const [editing, setEditing] = useState<{ index: number; draft: QualityProfile } | null>(null);
  const [defaultFor, setDefaultFor] = useState<{ home: boolean; away: boolean }>({
    home: false,
    away: false,
  });
  const [sourcePref, setSourcePref] = useState<string>("auto");
  const [savingTieBreaker, setSavingTieBreaker] = useState(false);
  const [tieBreakerError, setTieBreakerError] = useState<unknown>(null);
  const [scoring, setScoring] = useState<{ formats: string; scores: string } | null>(null);
  const [errors, setErrors] = useState<Errors>({});
  const [error, setError] = useState<unknown>(null);
  const [saving, setSaving] = useState(false);

  const open = (index: number, draft: QualityProfile) => {
    setErrors({});
    setError(null);
    setTieBreakerError(null);
    const cloned = structuredClone(draft);
    cloned.ranking = normalizeRanking(cloned);
    setEditing({ index, draft: cloned });
    const isExisting = index < profiles.length;
    setDefaultFor({
      home: isExisting ? quality.default === draft.name : profiles.length === 0,
      away: isExisting && quality.remote === draft.name,
    });
    setSourcePref(sourceOrderValue(resolver ?? undefined));
    setScoring(scoringText(resolver, draft));
  };

  async function chooseSourcePreference(nextPref: string) {
    setSourcePref(nextPref);
    if (!resolver) return;
    setSavingTieBreaker(true);
    setTieBreakerError(null);
    try {
      await api.putSection("resolver", {
        ...resolver,
        customFormats: resolver.customFormats ?? [],
        customFormatScores: resolver.customFormatScores ?? {},
        ...sourceOrderFields(nextPref),
      });
      toast.success(t("Source order saved"));
      await refresh();
    } catch (e) {
      setTieBreakerError(e);
    } finally {
      setSavingTieBreaker(false);
    }
  }

  async function save() {
    if (!editing) return;
    const { index, draft: profileDraft } = editing;
    let draft: QualityProfile = {
      ...profileDraft,
      ranking: normalizeRanking(profileDraft),
    };
    let parsed: ReturnType<typeof parseScoring> | null = null;
    if (scoring && hasImportedRules(profileDraft, resolver)) {
      try {
        parsed = parseScoring(scoring.formats, scoring.scores);
      } catch (e) {
        setError(e);
        return;
      }
      draft = { ...draft, customFormatScores: parsed.customFormatScores };
    }
    const old = profiles[index];
    const list =
      index < profiles.length
        ? profiles.map((p, j) => (j === index ? draft : p))
        : [...profiles, draft];
    const renamed = old && old.name !== draft.name ? old.name : null;
    const fallbackOther = list.find((_, j) => j !== index)?.name ?? draft.name;

    let nextDefault = quality.default === renamed ? draft.name : quality.default;
    if (defaultFor.home) {
      nextDefault = draft.name;
    } else if (old && quality.default === old.name) {
      nextDefault = fallbackOther;
    }

    // Away is optional: with no Away profile, people away from home get
    // the At home one.
    let nextRemote = quality.remote === renamed ? draft.name : quality.remote;
    if (defaultFor.away) {
      nextRemote = draft.name;
    } else if (old && quality.remote === old.name) {
      nextRemote = "";
    }

    const next: Quality = {
      ...quality,
      profiles: list,
      default: nextDefault || draft.name,
      remote: nextRemote ?? "",
      rules: renamed
        ? rules.map((r) => (r.profile === renamed ? { ...r, profile: draft.name } : r))
        : rules,
    };
    const conflict = releaseRuleConflict(draft.releaseRules ?? []);
    if (conflict) {
      setErrors({ releaseRules: conflict });
      return;
    }
    setSaving(true);
    setError(null);
    setErrors({});
    try {
      await commit(next, index < profiles.length ? t("Profile saved") : t("Profile added"));
      if (parsed) {
        const previous = JSON.stringify(resolver?.customFormats ?? []);
        if (previous !== JSON.stringify(parsed.customFormats)) {
          await api.putSection("resolver", {
            ...(resolver ?? {}),
            ...sourceOrderFields(sourcePref),
            customFormats: parsed.customFormats,
            customFormatScores: resolver?.customFormatScores ?? {},
          });
          await refresh();
        }
      }
      setEditing(null);
      setScoring(null);
    } catch (e) {
      setError(e);
      setErrors(recordErrors(e, `quality.profiles[${index}].`));
    } finally {
      setSaving(false);
    }
  }

  async function remove() {
    if (!editing) return;
    const { index } = editing;
    const name = profiles[index]?.name ?? editing.draft.name;
    const rest = profiles.filter((_, j) => j !== index);
    const fallback = rest[0]?.name ?? "";
    const usedBy = rules.filter((r) => r.profile === name).length;
    const moves = quality.default === name;
    const consequences = [
      name ? t("{name} will be removed.", { name }) : t("This profile will be removed."),
      usedBy
        ? tn(
            usedBy,
            "The device rule using it will be removed.",
            "{count} device rules using it will be removed.",
          )
        : "",
      moves && fallback
        ? t("Clients that used it as a default will use {profile}.", { profile: fallback })
        : "",
    ]
      .filter(Boolean)
      .join(" ");
    const ok = await confirmDialog({
      title: t("Remove profile?"),
      message: consequences,
      confirmLabel: t("Remove"),
      tone: "danger",
    });
    if (!ok) return;
    setSaving(true);
    setError(null);
    try {
      await commit(
        {
          ...quality,
          profiles: rest,
          rules: rules.filter((r) => r.profile !== name),
          default: quality.default === name ? fallback : quality.default,
          remote: quality.remote === name ? "" : quality.remote,
        },
        t("Profile removed"),
      );
      setEditing(null);
    } catch (e) {
      setError(e);
    } finally {
      setSaving(false);
    }
  }

  const draft = editing?.draft;
  const isNew = editing ? editing.index >= profiles.length : false;
  const patch = (p: Partial<QualityProfile>) =>
    setEditing((e) => (e ? { ...e, draft: { ...e.draft, ...p } } : e));

  return (
    <>
      <Section
        title={t("Profiles")}
        description={t("Resolution, HDR, release type and language rules used to rank releases.")}
        actions={
          <>
            <Button size="sm" variant="outline" onClick={() => setImporting(true)}>
              <Download size={14} /> {t("Import from TRaSH Guides")}
            </Button>
            <Button
              size="sm"
              onClick={() => open(profiles.length, blankProfile(profiles.length + 1))}
            >
              <Plus size={14} /> {t("Add profile")}
            </Button>
          </>
        }
        bodyClassName=""
      >
        {profiles.length === 0 ? (
          <EmptyState
            icon={<SlidersHorizontal />}
            title={t("No profiles")}
            description={t("Add a profile so Ether knows which releases to prefer.")}
          />
        ) : (
          <ul className="divide-y divide-border">
            {profiles.map((p, i) => {
              const usedBy = rules.filter((r) => r.profile === p.name).length;
              return (
                <RecordItem
                  key={`${p.name}-${i}`}
                  title={p.name || t("Profile {n}", { n: i + 1 })}
                  description={profileSummary(p)}
                  meta={
                    <>
                      {quality.default === p.name ? (
                        <Badge variant="info">{t("Default at home")}</Badge>
                      ) : null}
                      {quality.remote === p.name ? (
                        <Badge variant="info">{t("Default away")}</Badge>
                      ) : null}
                      {usedBy ? (
                        <Badge variant="secondary">
                          {tn(usedBy, "{count} rule", "{count} rules")}
                        </Badge>
                      ) : null}
                    </>
                  }
                  editLabel={t("Edit {name}", { name: p.name || t("profile {n}", { n: i + 1 }) })}
                  onEdit={() => open(i, p)}
                />
              );
            })}
          </ul>
        )}
      </Section>

      <TrashImport open={importing} onClose={() => setImporting(false)} onImported={refresh} />

      <RecordDrawer
        open={editing !== null}
        onClose={() => !saving && setEditing(null)}
        title={isNew ? t("New profile") : draft?.name || t("Profile")}
        description={t("How releases are filtered and ranked for apps using this profile.")}
        onSave={() => void save()}
        saving={saving}
        saveLabel={t("Save profile")}
        onRemove={isNew ? undefined : () => void remove()}
        removeLabel={t("Remove profile")}
        removeDisabled={profiles.length <= 1}
        error={error}
      >
        {draft ? (
          <>
            <SettingsSection title={t("Profile")}>
              <TextRow
                label={t("Name")}
                value={draft.name}
                error={errors.name}
                onChange={(v) => patch({ name: v })}
              />
              <SettingsRow
                title={t("Default for")}
                description={t(
                  "Used when no device rule matches. With no profile for Away, people away from home get the At home profile.",
                )}
                control={
                  <ChipGroup
                    ariaLabel={t("Default for")}
                    size="sm"
                    options={[
                      { value: "home", label: t("At home") },
                      { value: "away", label: t("Away") },
                    ]}
                    value={[
                      ...(defaultFor.home ? ["home" as const] : []),
                      ...(defaultFor.away ? ["away" as const] : []),
                    ]}
                    onChange={(next) =>
                      setDefaultFor({
                        home: next.includes("home"),
                        away: next.includes("away"),
                      })
                    }
                  />
                }
              />
            </SettingsSection>

            <SettingsSection
              title={t("Never play")}
              description={t("Releases outside these limits are skipped.")}
            >
              <SettingsRow
                title={t("Resolution")}
                description={
                  errors.minHeight || errors.maxHeight ? (
                    <span className="text-danger">{errors.minHeight || errors.maxHeight}</span>
                  ) : (
                    t("Lowest and highest acceptable.")
                  )
                }
                control={
                  <span className="flex items-center gap-2">
                    <span className="w-32">
                      <SearchSelect
                        {...searchSelectLabels()}
                        value={String(draft.minHeight)}
                        onChange={(v) => patch({ minHeight: Number(v) })}
                        options={HEIGHTS}
                        ariaLabel={t("Minimum resolution")}
                        searchable={false}
                      />
                    </span>
                    <span className="text-sm text-fg-muted">{t("to")}</span>
                    <span className="w-32">
                      <SearchSelect
                        {...searchSelectLabels()}
                        value={String(draft.maxHeight)}
                        onChange={(v) => patch({ maxHeight: Number(v) })}
                        options={HEIGHTS}
                        ariaLabel={t("Maximum resolution")}
                        searchable={false}
                      />
                    </span>
                  </span>
                }
              />
              <div className="grid gap-6 border-t border-border px-4 py-3 sm:grid-cols-2">
                <fieldset className="min-w-0 space-y-2">
                  <legend className="text-sm font-medium text-fg">{t("Movies")}</legend>
                  {errors.minSizeMbPerMin || errors.maxSizeMbPerMin ? (
                    <p className="text-xs text-danger">
                      {errors.minSizeMbPerMin || errors.maxSizeMbPerMin}
                    </p>
                  ) : (
                    <p className="text-xs text-fg-muted">
                      {t("Lowest and highest MB per minute for a film. 0 means no limit.")}
                    </p>
                  )}
                  <span className="flex flex-wrap items-center gap-2">
                    <NumberInput
                      aria-label={t("Minimum MB per minute")}
                      value={draft.minSizeMbPerMin}
                      min={0}
                      className="w-20"
                      onChange={(n) => patch({ minSizeMbPerMin: n })}
                    />
                    <span className="text-sm text-fg-muted">{t("to")}</span>
                    <NumberInput
                      aria-label={t("Maximum MB per minute")}
                      value={draft.maxSizeMbPerMin}
                      min={0}
                      className="w-20"
                      onChange={(n) => patch({ maxSizeMbPerMin: n })}
                    />
                    <span className="text-sm text-fg-muted">MB</span>
                  </span>
                </fieldset>
                <fieldset className="min-w-0 space-y-2">
                  <legend className="text-sm font-medium text-fg">{t("TV episodes")}</legend>
                  {errors.episodeMinSizeMbPerMin || errors.episodeMaxSizeMbPerMin ? (
                    <p className="text-xs text-danger">
                      {errors.episodeMinSizeMbPerMin || errors.episodeMaxSizeMbPerMin}
                    </p>
                  ) : (
                    <p className="text-xs text-fg-muted">
                      {t("Optional override for TV episodes. 0 uses the movie limits.")}
                    </p>
                  )}
                  <span className="flex flex-wrap items-center gap-2">
                    <NumberInput
                      aria-label={t("Episode minimum MB per minute")}
                      value={draft.episodeMinSizeMbPerMin ?? 0}
                      min={0}
                      className="w-20"
                      onChange={(n) => patch({ episodeMinSizeMbPerMin: n })}
                    />
                    <span className="text-sm text-fg-muted">{t("to")}</span>
                    <NumberInput
                      aria-label={t("Episode maximum MB per minute")}
                      value={draft.episodeMaxSizeMbPerMin ?? 0}
                      min={0}
                      className="w-20"
                      onChange={(n) => patch({ episodeMaxSizeMbPerMin: n })}
                    />
                    <span className="text-sm text-fg-muted">MB</span>
                  </span>
                </fieldset>
              </div>
              <BoolRow
                label={t("Allow HDR")}
                value={draft.allowHdr}
                help={t("Only used when the app supports it.")}
                onChange={(v) => patch({ allowHdr: v })}
              />
              <BoolRow
                label={t("Allow Dolby Vision")}
                value={draft.allowDolbyVision}
                onChange={(v) => patch({ allowDolbyVision: v })}
              />
              <BoolRow
                label={t("Allow CAM releases")}
                value={draft.allowCam}
                onChange={(v) => patch({ allowCam: v })}
              />
              <FilterRulesEditor
                profile={draft}
                onChange={(next) => patch(next)}
                error={errors.releaseRules}
              />
            </SettingsSection>

            <WhatMattersMostSection draft={draft} patch={patch} rankingError={errors.ranking} />

            <TieBreakerSection
              value={sourcePref}
              saving={savingTieBreaker}
              error={tieBreakerError}
              onChange={(v) => void chooseSourcePreference(v)}
            />

            <ImportedSection
              draft={draft}
              resolver={resolver}
              scoring={scoring}
              setScoring={setScoring}
              patch={patch}
            />
          </>
        ) : null}
      </RecordDrawer>
    </>
  );
}
