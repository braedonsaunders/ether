import {
  Alert,
  AlertDescription,
  AlertTitle,
  Badge,
  Button,
  EmptyState,
  SearchSelect,
  SettingsSection,
  Switch,
  Textarea,
  confirmDialog,
  toast,
} from "@braedonsaunders/appkit-ui";
import {
  Check,
  ChevronDown,
  Download,
  Film,
  Plus,
  RefreshCw,
  SlidersHorizontal,
  Trash2,
  Tv,
} from "lucide-react";
import { useState } from "react";
import { Link } from "react-router-dom";
import { MoreOptions } from "../../components/ConfigSection";
import { ErrorPanel, LoadingRows } from "../../components/feedback";
import { ChipGroup, ListInput, NumberInput, Segmented } from "../../components/fields";
import { Section } from "../../components/layout";
import { SaveBar, useDraft } from "../../components/SaveBar";
import { SettingsRow } from "../../components/SettingsRow";
import { DragHandle, SortableList, type DragHandleProps } from "../../components/Sortable";
import { localized, t, tn, tx } from "../../i18n";
import { searchSelectLabels } from "../../i18n/appkit-labels";
import { api } from "../../lib/api";
import { isPluginDescriptor, textOf } from "../../lib/extend";
import { relativeTime } from "../../lib/format";
import {
  MEDIA_MOVIE,
  MEDIA_TV,
  appForMedia,
  mediaApplies,
  mediaLabel,
  mediaTypeForApp,
  profileMediaType,
  qualityGateLabel,
  releaseConsole,
  type ProfileMedia,
  type QualitySyncReport,
} from "../../lib/releases";
import type { CustomFormatDefinition, QualityProfile, ReleaseRule } from "../../lib/types";
import { parseDuration } from "../settings/duration";
import { QUALITY_AUDIO, QUALITY_CODECS, QUALITY_SOURCES } from "../settings/schema";
import { SourceOrderSection } from "../sources/EqualQuality";
import { AwaySection } from "./AwaySection";
import { RemoteBitrateSection } from "./RemoteBitrateSection";
import { ConfigSection } from "../../components/ConfigSection";
import { PLAY_VERSIONS } from "../settings/schema";
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
  useRankingRegistry,
  descriptorLabel,
  descriptorOwnerName,
  insertStepAtDefaultPosition,
  PluginBadge,
  FilterRulesEditor,
  RuleEditorCard,
  RulePreview,
  insertPreferRuleInRanking,
  normalizeRanking,
  releaseRuleConflict,
  syncRulesWithRanking,
} from "./ReleasePreferences";
import {
  mergeCustomFormats,
  parseScoring,
  sameFormats,
  scoringText,
  type ParsedScoring,
  type ResolverPolicy,
} from "./resolverDraft";
import { TrashImport } from "./TrashImport";
import { useQuality, type Quality } from "./useQuality";

function blankProfile(n: number, media: ProfileMedia): QualityProfile {
  return {
    name: t("Profile {n}", { n }),
    mediaType: media,
    ranking: [],
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

function profileSummary(p: QualityProfile) {
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

const EVALUATION_OPTIONS = localized(() => [
  { value: "servarr" as const, label: t("Servarr") },
  { value: "ether" as const, label: t("Ether") },
]);

/** The mode a profile runs in: its choice, else Servarr for imported profiles that carry a quality order. */
function evaluationMode(p: QualityProfile): "servarr" | "ether" {
  if (p.evaluationMode === "servarr" || p.evaluationMode === "ether") return p.evaluationMode;
  return p.import && (p.qualityOrder?.length ?? 0) > 0 ? "servarr" : "ether";
}

const APPLIES_OPTIONS = localized(() => [
  { value: "" as const, label: t("Both") },
  { value: "movie" as const, label: t("Movies") },
  { value: "tv" as const, label: t("Shows") },
]);

/** Movies / Shows / Movies & shows badge for a profile or rule scope. */
export function MediaBadge({ media }: { media?: string }) {
  return <Badge variant={media ? "info" : "secondary"}>{mediaLabel(media)}</Badge>;
}

/**
 * The page-level scope choice. Two large, filled tabs make the selected
 * media obvious before the dependent profile/default/Away sections.
 */
function MediaScopeToggle({
  media,
  onChange,
}: {
  media: ProfileMedia;
  onChange: (media: ProfileMedia) => void;
}) {
  const options = [
    { value: MEDIA_MOVIE as ProfileMedia, label: t("Movies"), Icon: Film },
    { value: MEDIA_TV as ProfileMedia, label: t("Shows"), Icon: Tv },
  ];
  return (
    <div
      role="radiogroup"
      aria-label={t("Media")}
      className="grid w-full max-w-sm grid-cols-2 gap-1 rounded-xl border border-border bg-bg-subtle p-1"
    >
      {options.map(({ value, label, Icon }, i) => {
        const on = value === media;
        return (
          <button
            key={value}
            type="button"
            role="radio"
            aria-checked={on}
            aria-label={label}
            tabIndex={on ? 0 : -1}
            onClick={() => onChange(value)}
            onKeyDown={(e) => {
              if (e.key !== "ArrowRight" && e.key !== "ArrowLeft") return;
              e.preventDefault();
              const next =
                options[(i + (e.key === "ArrowRight" ? 1 : options.length - 1)) % options.length];
              if (!next) return;
              onChange(next.value);
              (
                e.currentTarget.parentElement?.children[options.indexOf(next)] as
                  HTMLElement | undefined
              )?.focus();
            }}
            className={[
              "flex h-12 items-center justify-center gap-2 rounded-lg px-4 text-base font-semibold transition-colors",
              "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/50",
              on
                ? "bg-primary text-primary-fg shadow-sm shadow-primary/30"
                : "text-fg-muted hover:bg-surface hover:text-fg",
            ].join(" ")}
          >
            <Icon size={18} aria-hidden />
            {label}
            {on ? <Check size={16} aria-hidden className="ml-0.5" /> : null}
          </button>
        );
      })}
    </div>
  );
}

/** The stored default for a media, falling back to the legacy field only when it fits. */
function mediaDefault(quality: Quality, media: ProfileMedia, away: boolean): string {
  const scoped = away
    ? media === MEDIA_MOVIE
      ? quality.remoteMovie
      : quality.remoteTv
    : media === MEDIA_MOVIE
      ? quality.defaultMovie
      : quality.defaultTv;
  if (scoped) return scoped;
  const legacy = (away ? quality.remote : quality.default) ?? "";
  if (!legacy) return "";
  const profile = (quality.profiles ?? []).find((p) => p.name === legacy);
  return !profile || mediaApplies(profile.mediaType, media) ? legacy : "";
}

function defaultField(
  media: string,
  away: boolean,
): "default" | "defaultMovie" | "defaultTv" | "remote" | "remoteMovie" | "remoteTv" {
  if (media === MEDIA_MOVIE) return away ? "remoteMovie" : "defaultMovie";
  if (media === MEDIA_TV) return away ? "remoteTv" : "defaultTv";
  return away ? "remote" : "default";
}

/** First profile (optionally other than `exclude`) that applies to media. */
function mediaFallback(profiles: QualityProfile[], media: string, exclude?: string): string {
  return profiles.find((p) => p.name !== exclude && mediaApplies(p.mediaType, media))?.name ?? "";
}

const DEFAULT_FIELDS = [
  "default",
  "defaultMovie",
  "defaultTv",
  "remote",
  "remoteMovie",
  "remoteTv",
] as const;

function withRenamedDefaults(quality: Quality, from: string, to: string): Quality {
  const next: Quality = { ...quality };
  for (const field of DEFAULT_FIELDS) {
    if (next[field] === from) next[field] = to;
  }
  return next;
}

/**
 * Keep every stored default pointing at a profile that exists and applies to
 * its media. A media default that outlives its profile falls back to the
 * first compatible profile (or none).
 */
function settleDefaults(next: Quality): Quality {
  const profiles = next.profiles ?? [];
  const exists = (name: string, media: string) =>
    profiles.some((p) => p.name === name && mediaApplies(p.mediaType, media));
  for (const media of [MEDIA_MOVIE, MEDIA_TV] as const) {
    const home = media === MEDIA_MOVIE ? "defaultMovie" : "defaultTv";
    const away = media === MEDIA_MOVIE ? "remoteMovie" : "remoteTv";
    if (next[home] && !exists(next[home]!, media)) next[home] = mediaFallback(profiles, media);
    if (next[away] && !exists(next[away]!, media)) next[away] = "";
    // The legacy default must not decide for a media it cannot serve.
    if (!next[home]) {
      const legacy = profiles.find((p) => p.name === next.default);
      if (legacy && !mediaApplies(legacy.mediaType, media)) {
        next[home] = mediaFallback(profiles, media);
      }
    }
  }
  return next;
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

/** Anchor of the profile drawer's custom scoring section. */
const CUSTOM_SCORES_ID = "profile-custom-scores";

function hasImportedRules(draft: QualityProfile, resolver: ResolverPolicy | null): boolean {
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
  revealed,
}: {
  draft: QualityProfile;
  resolver: ResolverPolicy | null;
  scoring: { formats: string; scores: string } | null;
  setScoring: React.Dispatch<React.SetStateAction<{ formats: string; scores: string } | null>>;
  patch: (p: Partial<QualityProfile>) => void;
  /** The Custom score step's "Edit scores" asked for the scoring editor. */
  revealed: boolean;
}) {
  const imported = hasImportedRules(draft, resolver);
  if (!imported && !revealed) return null;
  const gates = draft.allowedQualities ?? [];
  const scored = Object.keys(draft.customFormatScores ?? {}).length;
  return (
    <SettingsSection
      title={imported ? t("Imported profile") : t("Custom scores")}
      description={
        imported
          ? t("Carried over from a guide or Starr profile.")
          : t("Give custom formats points. Higher totals rank first at the Custom score step.")
      }
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
      <MoreOptions label={t("More options: Custom scoring")} reveal={revealed}>
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
            <Link to="/quality/history" className="text-primary underline">
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
  hasScores,
  onEditScores,
}: {
  draft: QualityProfile;
  patch: (p: Partial<QualityProfile>) => void;
  rankingError?: string;
  /** The profile or the shared custom formats carry scores. */
  hasScores: boolean;
  /** Shows and scrolls to the profile's custom scoring editor. */
  onEditScores: () => void;
}) {
  const { steps, fields } = useRankingRegistry();
  const [expanded, setExpanded] = useState<string | null>(null);
  const ladder = normalizeRanking(draft, steps);
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

  const [stepToAdd, setStepToAdd] = useState("");
  const isPluginStep = (step: string) => {
    const desc = steps.find((d) => d.id === step);
    return desc ? isPluginDescriptor(desc) : step.startsWith("plugin:");
  };
  const addable = steps.filter((d) => isPluginDescriptor(d) && !ladder.includes(d.id));
  const chosen = addable.some((d) => d.id === stepToAdd) ? stepToAdd : (addable[0]?.id ?? "");

  /** A ranking step a plugin contributes: its badge, its own editor, removal. */
  function renderPluginStep(step: string, handle: DragHandleProps, index: number) {
    const desc = steps.find((d) => d.id === step);
    const meta = desc?.meta;
    const title = desc ? textOf(desc.label) || step : step;
    const editor = typeof meta?.editor === "object" ? meta.editor : null;
    const editorKind =
      editor?.kind ?? editor?.editor ?? (typeof meta?.editor === "string" ? meta.editor : "none");
    const choices = (
      editor?.options ??
      editor?.choices ??
      meta?.options ??
      meta?.choices ??
      []
    ).map((o) =>
      typeof o === "string"
        ? { value: o, label: o }
        : { value: o.value, label: textOf(o.label) || o.value },
    );
    const config = draft.rankingConfig?.[step] ?? {};
    const setConfig = (next: Record<string, unknown>) =>
      patch({ rankingConfig: { ...draft.rankingConfig, [step]: next } });
    const expandable = ["ordered_list", "choice", "range", "toggle"].includes(editorKind);
    const isExpanded = expanded === step;
    const num = (v: unknown) => (typeof v === "number" ? v : 0);
    return (
      <div data-ranking-step={step} className="space-y-3 px-3 py-3">
        <div className="flex items-center gap-2">
          <DragHandle handle={handle} label={t("Move {name}", { name: title })} />
          <span className="w-5 text-xs tabular-nums text-fg-muted">{index + 1}</span>
          <div className="min-w-0 flex-1">
            <p className="flex flex-wrap items-center gap-2 text-sm font-medium text-fg">
              {title}
              {desc ? <PluginBadge owner={descriptorOwnerName(desc)} /> : null}
            </p>
            {desc ? null : (
              <p className="truncate text-xs text-fg-muted">
                {t("This plugin is off or removed. The step is skipped.")}
              </p>
            )}
          </div>
          {expandable && desc ? (
            <Button
              variant="ghost"
              size="sm"
              aria-expanded={isExpanded}
              aria-label={t("Configure {name}", { name: title })}
              onClick={() => setExpanded(isExpanded ? null : step)}
            >
              {t("Values")}
              <ChevronDown size={13} />
            </Button>
          ) : null}
          <OrderButtons
            label={title}
            i={index}
            n={ladder.length}
            onMove={(d) => updateRanking(swap(ladder, index, index + d))}
          />
          <Button
            variant="ghost"
            size="icon"
            aria-label={t("Remove step")}
            onClick={() => updateRanking(ladder.filter((s) => s !== step))}
          >
            <Trash2 size={14} />
          </Button>
        </div>
        {isExpanded && desc ? (
          <div className="space-y-3 border-t border-border pt-3">
            {editorKind === "toggle" ? (
              <label className="flex items-center gap-2 text-sm">
                <Switch
                  aria-label={title}
                  checked={config.value === true}
                  onChange={(e) => setConfig({ value: e.currentTarget.checked })}
                />
                {t("Enabled")}
              </label>
            ) : null}
            {editorKind === "choice" ? (
              <SearchSelect
                {...searchSelectLabels()}
                ariaLabel={title}
                value={typeof config.value === "string" ? config.value : ""}
                options={choices}
                onChange={(value) => setConfig({ value })}
              />
            ) : null}
            {editorKind === "ordered_list" ? (
              <PreferenceList
                label={title}
                all={choices.map((c) => c.value)}
                value={
                  Array.isArray(config.value)
                    ? config.value.filter((v): v is string => typeof v === "string")
                    : []
                }
                onChange={(value) => setConfig({ value })}
              />
            ) : null}
            {editorKind === "range" ? (
              <div className="grid grid-cols-2 gap-3">
                <label className="space-y-1 text-sm">
                  {t("Minimum")}
                  <NumberInput
                    aria-label={t("Minimum")}
                    value={num(config.min)}
                    onChange={(min) => setConfig({ min, max: Math.max(min, num(config.max)) })}
                  />
                </label>
                <label className="space-y-1 text-sm">
                  {t("Maximum")}
                  <NumberInput
                    aria-label={t("Maximum")}
                    value={num(config.max)}
                    onChange={(max) => setConfig({ min: Math.min(max, num(config.min)), max })}
                  />
                </label>
              </div>
            ) : null}
          </div>
        ) : null}
      </div>
    );
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
      customScore: {
        title: t("Custom score"),
        summary: hasScores
          ? t("Higher total score first, including your custom format scores")
          : t("Higher total score first. No custom format scores yet."),
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
          {step === "customScore" ? (
            <Button variant="ghost" size="sm" onClick={onEditScores}>
              {t("Edit scores")}
            </Button>
          ) : null}
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
                    fields={fields}
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
            return isPluginStep(step)
              ? renderPluginStep(step, handle, index)
              : renderBuiltinStep(step, handle, index);
          }}
        />
        {addable.length > 0 ? (
          <div className="flex flex-col gap-2 sm:flex-row sm:items-center">
            <div className="min-w-0 sm:flex-1">
              <SearchSelect
                {...searchSelectLabels()}
                ariaLabel={t("Step to add")}
                value={chosen}
                options={addable.map((d) => ({ value: d.id, label: descriptorLabel(d) }))}
                onChange={setStepToAdd}
              />
            </div>
            <Button
              size="sm"
              disabled={!chosen}
              onClick={() => updateRanking(insertStepAtDefaultPosition(ladder, steps, chosen))}
            >
              <Plus size={14} />
              {t("Add a step")}
            </Button>
          </div>
        ) : null}
      </div>
      <RulePreview profile={draft} />
    </SettingsSection>
  );
}

const SYNC_INTERVALS = localized(() => [
  { value: "6", label: t("Every 6 hours") },
  { value: "12", label: t("Every 12 hours") },
  { value: "24", label: t("Every 24 hours") },
  { value: "72", label: t("Every 3 days") },
  { value: "168", label: t("Every 7 days") },
]);

/** Hours in a Go duration string, defaulting to 24. */
function syncHours(raw: string | undefined): string {
  if (!raw) return "24";
  const { amount, unit } = parseDuration(raw);
  const n = Number(amount);
  if (!Number.isFinite(n)) return "24";
  const hours = unit === "d" ? n * 24 : unit === "h" ? n : n / 60;
  return hours > 0 ? String(hours) : "24";
}

/** Automatic update of imported profiles and their format definitions. */
function SyncSection({ quality, onSaved }: { quality: Quality; onSaved: () => Promise<void> }) {
  const sync = quality.sync;
  const hasRun = Boolean(sync?.lastRun) && !sync!.lastRun!.startsWith("0001-");
  const { draft, setDraft, dirty, reset } = useDraft({
    enabled: sync?.enabled ?? false,
    hours: syncHours(sync?.interval),
  });
  const [saving, setSaving] = useState(false);
  const [syncing, setSyncing] = useState(false);
  const [error, setError] = useState<unknown>(null);
  const [report, setReport] = useState<QualitySyncReport | null>(null);
  if (!draft) return null;
  const hours = draft.hours;
  const options = SYNC_INTERVALS.some((o) => o.value === hours)
    ? SYNC_INTERVALS
    : [...SYNC_INTERVALS, { value: hours, label: t("Every {hours} hours", { hours }) }];

  async function save() {
    if (!draft) return;
    setSaving(true);
    setError(null);
    try {
      // The admin endpoint patches the quality section, so omitted run
      // state stays whatever the server recorded while this form was open.
      await api.putSection("quality", {
        sync: { enabled: draft.enabled, interval: `${draft.hours}h` },
      });
      toast.success(t("Sync settings saved"));
      await onSaved();
    } catch (e) {
      setError(e);
    } finally {
      setSaving(false);
    }
  }

  async function syncNow() {
    setSyncing(true);
    setError(null);
    try {
      setReport(await releaseConsole.syncQuality());
      toast.success(t("Guide sync finished"));
      await onSaved();
    } catch (e) {
      setError(e);
    } finally {
      setSyncing(false);
    }
  }

  return (
    <Section
      title={t("Keep imported profiles in sync")}
      description={t(
        "Update profiles imported from a guide when its quality definition or custom format scores change.",
      )}
      actions={
        <Button variant="outline" size="sm" disabled={syncing} onClick={() => void syncNow()}>
          <RefreshCw size={14} className={syncing ? "animate-spin" : undefined} />
          {syncing ? t("Syncing…") : t("Sync now")}
        </Button>
      }
    >
      <SettingsSection>
        <SettingsRow
          title={t("Keep imported profiles in sync")}
          description={t("Only profiles imported from TRaSH Guides or Profilarr are updated.")}
          control={
            <Switch
              aria-label={t("Keep imported profiles in sync")}
              checked={draft.enabled}
              onChange={(e) => setDraft({ ...draft, enabled: e.currentTarget.checked })}
            />
          }
        />
        <SettingsRow
          title={t("Check for changes")}
          description={t("How often Ether fetches the guide's current profile and formats.")}
          control={
            <div className="w-52">
              <SearchSelect
                {...searchSelectLabels()}
                ariaLabel={t("Check for changes")}
                value={hours}
                onChange={(value) => setDraft({ ...draft, hours: value })}
                options={options}
                searchable={false}
              />
            </div>
          }
        />
        <SettingsRow
          title={t("Last sync")}
          description={
            sync?.lastError ? (
              <span className="text-danger">{sync.lastError}</span>
            ) : (
              t("The last time the guide sync ran.")
            )
          }
          control={
            <span className="text-sm text-fg">
              {hasRun ? t("Ran {when}", { when: relativeTime(sync!.lastRun!) }) : t("Never run")}
            </span>
          }
        />
      </SettingsSection>
      {error ? <ErrorPanel error={error} /> : null}
      {report ? (
        <Alert variant="success" role="status">
          <AlertTitle>{t("Guide sync finished")}</AlertTitle>
          <AlertDescription>
            <ul className="list-disc space-y-1 pl-5">
              <li>
                {tn(report.profilesUpdated, "{count} profile updated", "{count} profiles updated")}
              </li>
              <li>{tn(report.formatsAdded, "{count} format added", "{count} formats added")}</li>
              <li>
                {tn(report.formatsUpdated, "{count} format updated", "{count} formats updated")}
              </li>
              <li>
                {tn(report.formatsRemoved, "{count} format removed", "{count} formats removed")}
              </li>
              {(report.warnings ?? []).map((w) => (
                <li key={w}>{w}</li>
              ))}
            </ul>
          </AlertDescription>
        </Alert>
      ) : null}
      <SaveBar
        dirty={dirty}
        saving={saving}
        error={error}
        onSave={() => void save()}
        onDiscard={() => {
          reset();
          setError(null);
        }}
      />
    </Section>
  );
}

export function QualityTab() {
  const { config, quality, commit, refresh } = useQuality();
  const [media, setMedia] = useState<ProfileMedia>(MEDIA_MOVIE);
  if (config.error) return <ErrorPanel error={config.error} onRetry={() => void config.reload()} />;
  if (!quality) return <LoadingRows rows={4} />;
  return (
    <>
      <ProfileList
        quality={quality}
        resolver={config.data?.resolver ?? null}
        commit={commit}
        refresh={refresh}
        media={media}
        onMediaChange={setMedia}
      />
      <AwaySection quality={quality} media={media} onSaved={refresh} />
      <div className="pt-1">
        <div className="flex items-center gap-3">
          <div className="h-px flex-1 bg-border" />
          <span className="text-[11px] font-semibold uppercase tracking-[0.14em] text-fg-muted">
            {t("Shared for movies and shows")}
          </span>
          <div className="h-px flex-1 bg-border" />
        </div>
      </div>
      <SyncSection quality={quality} onSaved={refresh} />
      <RemoteBitrateSection />
      <SourceOrderSection />
      <ConfigSection
        configKey="resolver"
        endpoint="resolver"
        groups={PLAY_VERSIONS}
        label={t("Version menu")}
      />
    </>
  );
}

type Commit = (next: Quality, message: string) => Promise<void>;

function ProfileList({
  quality,
  resolver,
  commit,
  refresh,
  media,
  onMediaChange,
}: {
  quality: Quality;
  resolver: ResolverPolicy | null;
  commit: Commit;
  refresh: () => Promise<void>;
  media: ProfileMedia;
  onMediaChange: (media: ProfileMedia) => void;
}) {
  const { steps, fields } = useRankingRegistry();
  const profiles = quality.profiles ?? [];
  const visible = profiles
    .map((profile, index) => ({ profile, index }))
    .filter(({ profile }) => mediaApplies(profile.mediaType, media));
  const homeDefault = mediaDefault(quality, media, false);
  const awayDefault = mediaDefault(quality, media, true);
  const [importing, setImporting] = useState(false);
  const rules = quality.rules ?? [];
  const [editing, setEditing] = useState<{ index: number; draft: QualityProfile } | null>(null);
  const [defaultFor, setDefaultFor] = useState<{ home: boolean; away: boolean }>({
    home: false,
    away: false,
  });
  const [scoring, setScoring] = useState<{ formats: string; scores: string } | null>(null);
  const [scoringShown, setScoringShown] = useState(false);
  /** The definitions editor text when the drawer opened; unchanged means no PUT. */
  const [formatBaseline, setFormatBaseline] = useState("");
  /** The shared definitions as they were when the drawer opened (for merging). */
  const [formatSnapshot, setFormatSnapshot] = useState<CustomFormatDefinition[]>([]);
  const [errors, setErrors] = useState<Errors>({});
  const [error, setError] = useState<unknown>(null);
  const [saving, setSaving] = useState(false);

  const open = (index: number, draft: QualityProfile) => {
    setErrors({});
    setError(null);
    const cloned = structuredClone(draft);
    cloned.ranking = normalizeRanking(cloned, steps);
    setEditing({ index, draft: cloned });
    const isExisting = index < profiles.length;
    const scope = profileMediaType(cloned);
    const home = defaultField(scope, false);
    const away = defaultField(scope, true);
    setDefaultFor({
      home: isExisting ? quality[home] === cloned.name : profiles.length === 0,
      away: isExisting && quality[away] === cloned.name,
    });
    const text = scoringText(resolver, draft);
    setScoring(text);
    setFormatBaseline(text.formats);
    setScoringShown(false);
    setFormatSnapshot(structuredClone(resolver?.customFormats ?? []));
  };

  function editScores() {
    setScoringShown(true);
    // The section renders on this state change; scroll once it exists.
    requestAnimationFrame(() =>
      document
        .getElementById(CUSTOM_SCORES_ID)
        ?.scrollIntoView({ behavior: "smooth", block: "start" }),
    );
  }

  async function save() {
    if (!editing) return;
    const { index, draft: profileDraft } = editing;
    let draft: QualityProfile = {
      ...profileDraft,
      ranking: normalizeRanking(profileDraft, steps),
    };
    let parsed: ParsedScoring | null = null;
    if (scoring && (scoringShown || hasImportedRules(profileDraft, resolver))) {
      try {
        parsed = parseScoring(scoring.formats, scoring.scores);
      } catch (e) {
        setError(e);
        return;
      }
      draft = { ...draft, customFormatScores: parsed.customFormatScores };
    }
    const old = profiles[index];
    const oldScope = old ? profileMediaType(old) : null;
    const scope = profileMediaType(draft);
    const list =
      index < profiles.length
        ? profiles.map((p, j) => (j === index ? draft : p))
        : [...profiles, draft];
    const renamed = old && old.name !== draft.name ? old.name : null;
    const otherProfile = list.find((_, j) => j !== index)?.name ?? draft.name;

    let next: Quality = {
      ...quality,
      profiles: list,
      rules: renamed
        ? rules.map((r) => (r.profile === renamed ? { ...r, profile: draft.name } : r))
        : rules,
    };
    if (renamed) next = withRenamedDefaults(next, renamed, draft.name);

    // The checkboxes belong to the profile's media (legacy fields for Both).
    const homeField = defaultField(scope, false);
    const awayField = defaultField(scope, true);
    if (defaultFor.home) {
      next[homeField] = draft.name;
    } else if (old && quality[homeField] === old.name) {
      next[homeField] = scope === "" ? otherProfile : mediaFallback(list, scope, draft.name);
    }
    // Away is optional: with no Away profile, people away from home get
    // the At home one.
    if (defaultFor.away) {
      next[awayField] = draft.name;
    } else if (old && quality[awayField] === old.name) {
      next[awayField] = "";
    }
    // Moving a profile to another media must not leave its old default behind.
    if (old && oldScope !== scope) {
      const oldHome = defaultField(oldScope!, false);
      const oldAway = defaultField(oldScope!, true);
      if (oldHome !== homeField && quality[oldHome] === old.name) {
        next[oldHome] = oldScope === "" ? otherProfile : mediaFallback(list, oldScope!, draft.name);
      }
      if (oldAway !== awayField && quality[oldAway] === old.name) next[oldAway] = "";
    }
    if (!next.default) next.default = draft.name;
    next = settleDefaults(next);

    const conflict = releaseRuleConflict(draft.releaseRules ?? [], fields);
    if (conflict) {
      setErrors({ releaseRules: conflict });
      return;
    }
    setSaving(true);
    setError(null);
    setErrors({});
    try {
      await commit(next, index < profiles.length ? t("Profile saved") : t("Profile added"));
      // Only touch the shared definitions when this drawer actually edited
      // them; otherwise a stale snapshot must not be sent back.
      const formatsChanged = scoring !== null && scoring.formats !== formatBaseline;
      if (parsed && formatsChanged) {
        const latestConfig = await api.getConfig();
        const latest = latestConfig.resolver.customFormats ?? [];
        const merged = mergeCustomFormats({
          original: formatSnapshot,
          edited: parsed.customFormats,
          latest,
          app: appForMedia(scope),
        });
        if (!sameFormats(merged, latest)) {
          await api.putSection("resolver", { customFormats: merged });
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
    const moves =
      quality.default === name || quality.defaultMovie === name || quality.defaultTv === name;
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
      let next: Quality = {
        ...quality,
        profiles: rest,
        rules: rules.filter((r) => r.profile !== name),
        default: quality.default === name ? fallback : quality.default,
      };
      for (const field of DEFAULT_FIELDS) {
        if (field === "default" || next[field] !== name) continue;
        if (field === "defaultMovie") next.defaultMovie = mediaFallback(rest, MEDIA_MOVIE);
        else if (field === "defaultTv") next.defaultTv = mediaFallback(rest, MEDIA_TV);
        else next[field] = "";
      }
      next = settleDefaults(next);
      await commit(next, t("Profile removed"));
      setEditing(null);
    } catch (e) {
      setError(e);
    } finally {
      setSaving(false);
    }
  }

  const draft = editing?.draft;
  const draftScope = draft ? profileMediaType(draft) : "";
  const isNew = editing ? editing.index >= profiles.length : false;
  const patch = (p: Partial<QualityProfile>) =>
    setEditing((e) => (e ? { ...e, draft: { ...e.draft, ...p } } : e));

  /** Change "Applies to" and point the default checkboxes at the new media's fields. */
  function changeApplies(media: "" | ProfileMedia) {
    const existing = editing ? profiles[editing.index] : undefined;
    patch({ mediaType: media || undefined });
    if (existing) {
      const home = defaultField(media, false);
      const away = defaultField(media, true);
      setDefaultFor({
        home: quality[home] === existing.name,
        away: quality[away] === existing.name,
      });
    }
  }

  return (
    <>
      <div className="rounded-xl border border-primary/30 bg-primary/5 px-5 py-4">
        <div className="flex flex-wrap items-center justify-between gap-4">
          <div className="min-w-0">
            <p className="text-[11px] font-semibold uppercase tracking-[0.14em] text-primary">
              {t("Profile scope")}
            </p>
            <div className="mt-3 flex flex-wrap items-center gap-4">
              <MediaScopeToggle media={media} onChange={onMediaChange} />
              <p className="max-w-md text-sm text-fg-muted">
                {t(
                  "Profiles, At home and Away defaults, and Away settings below apply to {media}.",
                  { media: mediaLabel(media) },
                )}
              </p>
            </div>
          </div>
          <Button
            size="sm"
            onClick={() => open(profiles.length, blankProfile(profiles.length + 1, media))}
          >
            <Plus size={14} /> {t("Add profile")}
          </Button>
        </div>
      </div>

      <Section
        title={t("Profiles")}
        description={t("Resolution, HDR, release type and language rules used to rank releases.")}
        bodyClassName=""
      >
        <div className="flex flex-wrap items-center gap-x-6 gap-y-2 border-b border-border bg-bg-subtle/40 px-5 py-3 text-sm">
          <span className="flex items-center gap-2">
            <span className="text-xs font-medium text-fg-muted">{t("At home default")}</span>
            <Badge variant="info">{homeDefault || t("First compatible profile")}</Badge>
          </span>
          <span className="flex items-center gap-2">
            <span className="text-xs font-medium text-fg-muted">{t("Away default")}</span>
            <Badge variant="info">{awayDefault || t("Same as at home")}</Badge>
          </span>
          <span className="text-xs text-fg-muted">{t("Set these from a profile’s editor.")}</span>
        </div>
        {visible.length === 0 ? (
          <EmptyState
            icon={<SlidersHorizontal />}
            title={
              profiles.length
                ? media === MEDIA_MOVIE
                  ? t("No movie profiles")
                  : t("No show profiles")
                : t("No profiles")
            }
            description={
              profiles.length
                ? t("Switch to {media} to see its profiles.", {
                    media: mediaLabel(media === MEDIA_MOVIE ? MEDIA_TV : MEDIA_MOVIE),
                  })
                : t("Add a profile so Ether knows which releases to prefer.")
            }
          />
        ) : (
          <ul className="divide-y divide-border">
            {visible.map(({ profile: p, index: i }) => {
              const usedBy = rules.filter((r) => r.profile === p.name).length;
              return (
                <RecordItem
                  key={`${p.name}-${i}`}
                  title={p.name || t("Profile {n}", { n: i + 1 })}
                  description={profileSummary(p)}
                  meta={
                    <>
                      <MediaBadge media={profileMediaType(p)} />
                      {homeDefault === p.name ? (
                        <Badge variant="info">{t("Default at home")}</Badge>
                      ) : null}
                      {awayDefault === p.name ? (
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
            {isNew ? (
              <Button
                variant="outline"
                onClick={() => {
                  setEditing(null);
                  setImporting(true);
                }}
              >
                <Download size={16} />
                {t("Import a quality profile")}
              </Button>
            ) : null}
            <SettingsSection title={t("Profile")}>
              <TextRow
                label={t("Name")}
                value={draft.name}
                error={errors.name}
                onChange={(v) => patch({ name: v })}
              />
              <SettingsRow
                title={t("Applies to")}
                description={
                  draft.import
                    ? t("Set by the {app} profile it was imported from.", {
                        app: draft.import.app === "sonarr" ? "Sonarr" : "Radarr",
                      })
                    : t("Which titles this profile can rank.")
                }
                control={
                  draft.import ? (
                    <span className="flex flex-wrap items-center gap-1.5">
                      <Badge variant="secondary">
                        {mediaLabel(
                          draft.mediaType ||
                            mediaTypeForApp(draft.import.app === "sonarr" ? "sonarr" : "radarr"),
                        )}
                      </Badge>
                      <Badge variant="outline">
                        {draft.import.source === "dictionarry"
                          ? "Profilarr (Dictionarry)"
                          : "TRaSH Guides"}
                      </Badge>
                    </span>
                  ) : (
                    <Segmented
                      ariaLabel={t("Applies to")}
                      size="sm"
                      value={profileMediaType(draft) as "" | ProfileMedia}
                      options={APPLIES_OPTIONS}
                      onChange={changeApplies}
                    />
                  )
                }
              />
              <SettingsRow
                title={t("Evaluation mode")}
                description={
                  evaluationMode(draft) === "servarr"
                    ? t(
                        "Scores and orders releases like Radarr and Sonarr: the profile's quality order first, then custom format score alone. Formats the profile doesn't list score 0, and a release below the minimum format score isn't played.",
                      )
                    : t(
                        "Ether's own scoring: formats the profile doesn't list can keep their shared score, and quality points, size, age and your ranking steps all shape the order.",
                      )
                }
                control={
                  <Segmented
                    ariaLabel={t("Evaluation mode")}
                    size="sm"
                    value={evaluationMode(draft)}
                    options={EVALUATION_OPTIONS}
                    onChange={(mode) => patch({ evaluationMode: mode })}
                  />
                }
              />
              <SettingsRow
                title={t("Default for")}
                description={
                  draftScope === ""
                    ? t(
                        "Used when no device rule matches, for both movies and shows. With no profile for Away, people away from home get the At home profile.",
                      )
                    : t(
                        "Used when no device rule matches for {media}. With no profile for Away, people away from home get the At home profile.",
                        { media: mediaLabel(draftScope) },
                      )
                }
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
              description={
                draft.fallbackBelowMin
                  ? t(
                      "When no release meets this profile, allow lower resolutions, including qualities excluded by an imported profile. Other limits still apply.",
                    )
                  : t("Releases outside these limits are skipped.")
              }
            >
              <SettingsRow
                title={t("Resolution")}
                description={
                  errors.minHeight || errors.maxHeight ? (
                    <span className="text-danger">{errors.minHeight || errors.maxHeight}</span>
                  ) : draft.fallbackBelowMin ? (
                    t(
                      "When no release meets this profile, allow lower resolutions, including qualities excluded by an imported profile. Other limits still apply.",
                    )
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
              <SettingsRow
                title={t("Fall back below minimum resolution")}
                description={t(
                  "When no release meets this profile, allow lower resolutions, including qualities excluded by an imported profile. Other limits still apply.",
                )}
                control={
                  <Switch
                    aria-label={t("Fall back below minimum resolution")}
                    checked={draft.fallbackBelowMin ?? false}
                    onChange={(e) => patch({ fallbackBelowMin: e.currentTarget.checked })}
                  />
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
                label={t("Allow CAM and screener releases")}
                value={draft.allowCam}
                onChange={(v) => patch({ allowCam: v })}
              />
              <FilterRulesEditor
                profile={draft}
                onChange={(next) => patch(next)}
                error={errors.releaseRules}
              />
            </SettingsSection>

            <WhatMattersMostSection
              draft={draft}
              patch={patch}
              rankingError={errors.ranking}
              hasScores={
                Object.keys(draft.customFormatScores ?? {}).length > 0 ||
                (resolver?.customFormats ?? []).length > 0
              }
              onEditScores={editScores}
            />

            <div id={CUSTOM_SCORES_ID} className="scroll-mt-4">
              <ImportedSection
                draft={draft}
                resolver={resolver}
                scoring={scoring}
                setScoring={setScoring}
                patch={patch}
                revealed={scoringShown}
              />
            </div>
          </>
        ) : null}
      </RecordDrawer>
    </>
  );
}
