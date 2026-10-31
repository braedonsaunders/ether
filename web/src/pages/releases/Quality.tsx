import {
  Badge,
  Button,
  EmptyState,
  SearchSelect,
  SettingsSection,
  Textarea,
  confirmDialog,
} from "@braedonsaunders/appkit-ui";
import { Download, Plus, SlidersHorizontal } from "lucide-react";
import { useMemo, useState } from "react";
import { Link } from "react-router-dom";
import { MoreOptions } from "../../components/ConfigSection";
import { ErrorPanel, LoadingRows } from "../../components/feedback";
import { ChipGroup, ListInput, NumberInput } from "../../components/fields";
import { Section } from "../../components/layout";
import { SaveBar, useDraft } from "../../components/SaveBar";
import { SettingsRow } from "../../components/SettingsRow";
import { localized, t, tn, tx } from "../../i18n";
import { api } from "../../lib/api";
import { qualityGateLabel } from "../../lib/releases";
import type { QualityProfile } from "../../lib/types";
import { parseScoring, scoringText, type ResolverPolicy } from "./resolverDraft";
import { QUALITY_AUDIO, QUALITY_CODECS, QUALITY_SOURCES } from "../settings/schema";
import {
  BoolRow,
  RecordDrawer,
  RecordItem,
  TextRow,
  recordErrors,
  type Errors,
} from "../sources/form";
import { searchSelectLabels } from "../../i18n/appkit-labels";
import { ReleasePreferences, releaseRuleConflict } from "./ReleasePreferences";
import { TrashImport } from "./TrashImport";
import { useQuality, type Quality } from "./useQuality";

function blankProfile(n: number): QualityProfile {
  return {
    name: t("Profile {n}", { n }),
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

/** Rules an imported (TRaSH) profile carries beyond the ones above. */
function ImportedRules({
  draft,
  patch,
}: {
  draft: QualityProfile;
  patch: (p: Partial<QualityProfile>) => void;
}) {
  const gates = draft.allowedQualities ?? [];
  const scored = Object.keys(draft.customFormatScores ?? {}).length;
  if (!gates.length && !scored && draft.minCustomFormatScore == null) return null;
  return (
    <SettingsSection
      title={t("Imported rules")}
      description={t("Carried over from a Radarr or Sonarr profile.")}
    >
      {gates.length ? (
        <SettingsRow
          stacked
          title={t("Allowed qualities")}
          description={t("Other source and resolution pairs are never played with this profile.")}
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
    </SettingsSection>
  );
}

/** Quality tab: the default profiles edited in place, and the profile list. */
export function QualityTab() {
  const { config, quality, commit, refresh } = useQuality();
  if (config.error) return <ErrorPanel error={config.error} onRetry={() => void config.reload()} />;
  if (!quality) return <LoadingRows rows={4} />;
  return (
    <>
      <QualityDefaults quality={quality} commit={commit} />
      <ProfileList
        quality={quality}
        resolver={config.data?.resolver ?? null}
        commit={commit}
        refresh={refresh}
      />
    </>
  );
}

type Commit = (next: Quality, message: string) => Promise<void>;

function QualityDefaults({ quality, commit }: { quality: Quality; commit: Commit }) {
  const saved = useMemo(
    () => ({ default: quality.default, remote: quality.remote }),
    [quality.default, quality.remote],
  );
  const { draft, setDraft, dirty, reset } = useDraft(saved);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<unknown>(null);
  const [errors, setErrors] = useState<Errors>({});
  const names = (quality.profiles ?? []).map((p) => ({ value: p.name, label: p.name }));
  if (!draft) return null;

  async function save() {
    if (!draft) return;
    setSaving(true);
    setError(null);
    setErrors({});
    try {
      await commit({ ...quality, ...draft }, t("Default quality saved"));
    } catch (e) {
      setError(e);
      setErrors(recordErrors(e, "quality."));
    } finally {
      setSaving(false);
    }
  }

  return (
    <>
      <SettingsSection
        title={t("Defaults")}
        description={t("The profile an app gets when no device rule matches it.")}
      >
        <SettingsRow
          title={t("On your network")}
          description={
            errors.default ? (
              <span className="text-danger">{errors.default}</span>
            ) : (
              t("Apps on the same network as this server.")
            )
          }
          control={
            <div className="w-48">
              <SearchSelect
                {...searchSelectLabels()}
                value={draft.default}
                onChange={(v) => setDraft({ ...draft, default: v })}
                options={names}
                ariaLabel={t("Default profile")}
                searchable={false}
              />
            </div>
          }
        />
        <SettingsRow
          title={t("Remote")}
          description={
            errors.remote ? (
              <span className="text-danger">{errors.remote}</span>
            ) : (
              t("Apps streaming from outside your network.")
            )
          }
          control={
            <div className="w-48">
              <SearchSelect
                {...searchSelectLabels()}
                value={draft.remote}
                onChange={(v) => setDraft({ ...draft, remote: v })}
                options={names}
                ariaLabel={t("Remote profile")}
                searchable={false}
              />
            </div>
          }
        />
      </SettingsSection>
      <SaveBar
        dirty={dirty}
        saving={saving}
        error={error}
        onSave={save}
        onDiscard={() => {
          reset();
          setError(null);
          setErrors({});
        }}
      />
    </>
  );
}

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
  const [scoring, setScoring] = useState<{ formats: string; scores: string } | null>(null);
  const [errors, setErrors] = useState<Errors>({});
  const [error, setError] = useState<unknown>(null);
  const [saving, setSaving] = useState(false);

  const open = (index: number, draft: QualityProfile) => {
    setErrors({});
    setError(null);
    setEditing({ index, draft: structuredClone(draft) });
    setScoring(scoringText(resolver, draft));
  };

  async function save() {
    if (!editing) return;
    const { index, draft: profileDraft } = editing;
    if (!scoring) return;
    let parsed: ReturnType<typeof parseScoring>;
    try {
      parsed = parseScoring(scoring.formats, scoring.scores);
    } catch (e) {
      setError(e);
      return;
    }
    const draft = { ...profileDraft, customFormatScores: parsed.customFormatScores };
    const old = profiles[index];
    const list =
      index < profiles.length
        ? profiles.map((p, j) => (j === index ? draft : p))
        : [...profiles, draft];
    // Keep defaults and client rules pointing at a renamed profile.
    const renamed = old && old.name !== draft.name ? old.name : null;
    const next: Quality = renamed
      ? {
          ...quality,
          profiles: list,
          default: quality.default === renamed ? draft.name : quality.default,
          remote: quality.remote === renamed ? draft.name : quality.remote,
          rules: rules.map((r) => (r.profile === renamed ? { ...r, profile: draft.name } : r)),
        }
      : { ...quality, profiles: list };
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
      const previous = JSON.stringify(resolver?.customFormats ?? []);
      if (previous !== JSON.stringify(parsed.customFormats)) {
        await api.putSection("resolver", {
          ...(resolver ?? {}),
          customFormats: parsed.customFormats,
          customFormatScores: resolver?.customFormatScores ?? {},
        });
        await refresh();
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
    // The saved name: the drawer's draft may hold an unsaved rename.
    const name = profiles[index]?.name ?? editing.draft.name;
    const rest = profiles.filter((_, j) => j !== index);
    const fallback = rest[0]?.name ?? "";
    const usedBy = rules.filter((r) => r.profile === name).length;
    const moves = [quality.default, quality.remote].includes(name);
    // Rules and defaults may not name a missing profile: removing one also
    // removes its client rules and moves the defaults to another profile.
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
          remote: quality.remote === name ? fallback : quality.remote,
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
        description={t("Resolution, HDR, source and language preferences used to rank releases.")}
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
                        <Badge variant="info">{t("On your network")}</Badge>
                      ) : null}
                      {quality.remote === p.name ? (
                        <Badge variant="info">{t("Remote")}</Badge>
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
        description={t("How releases are ranked for apps using this profile.")}
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
            <ReleasePreferences
              profile={draft}
              onChange={(releaseRules) => patch({ releaseRules })}
              error={errors.releaseRules}
            />
            <SettingsSection title={t("Picture")}>
              <TextRow
                label={t("Name")}
                value={draft.name}
                error={errors.name}
                onChange={(v) => patch({ name: v })}
              />
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
            </SettingsSection>
            <SettingsSection
              title={t("Preferences")}
              description={t("Pick in order of preference.")}
            >
              <SettingsRow stacked title={t("Sources")}>
                <PreferenceList
                  label={t("Sources")}
                  all={QUALITY_SOURCES}
                  value={draft.preferredSources}
                  onChange={(v) => patch({ preferredSources: v })}
                />
              </SettingsRow>
              <SettingsRow stacked title={t("Video codecs")}>
                <PreferenceList
                  label={t("Video codecs")}
                  all={QUALITY_CODECS}
                  value={draft.preferredCodecs}
                  onChange={(v) => patch({ preferredCodecs: v })}
                />
              </SettingsRow>
              <SettingsRow stacked title={t("Audio")}>
                <PreferenceList
                  label={t("Audio")}
                  all={QUALITY_AUDIO}
                  value={draft.preferredAudio}
                  onChange={(v) => patch({ preferredAudio: v })}
                />
              </SettingsRow>
              <SettingsRow
                stacked
                title={t("Languages")}
                description={t("ISO codes in preference order. Empty accepts any.")}
              >
                <ListInput
                  ariaLabel={t("Languages")}
                  value={draft.preferredLanguages}
                  onChange={(v) => patch({ preferredLanguages: v.length ? v.map(String) : null })}
                  placeholder="en, fr"
                />
              </SettingsRow>
            </SettingsSection>
            <SettingsSection
              title={t("File size")}
              description={t(
                "Megabytes per minute of runtime. This filters out fakes and bloated files. 0 means no limit.",
              )}
            >
              <div className="grid gap-6 px-4 py-3 sm:grid-cols-2">
                <fieldset className="min-w-0 space-y-2">
                  <legend className="text-sm font-medium text-fg">{t("Movies")}</legend>
                  {errors.minSizeMbPerMin || errors.maxSizeMbPerMin ? (
                    <p className="text-xs text-danger">
                      {errors.minSizeMbPerMin || errors.maxSizeMbPerMin}
                    </p>
                  ) : (
                    <p className="text-xs text-fg-muted">
                      {t("Lowest and highest size for a film.")}
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
                label={t("Prefer largest file")}
                value={draft.preferLargest ?? false}
                help={t("Ranks larger files higher within the allowed size range.")}
                onChange={(v) => patch({ preferLargest: v })}
              />
            </SettingsSection>
            <MoreOptions label={t("More options: Custom scoring")}>
              <SettingsSection
                title={t("Custom scoring")}
                description={t(
                  "Formats describe what to match. They are shared by every profile. The scores below apply only when this profile ranks a release.",
                )}
              >
                <p className="text-sm text-fg-muted">
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
                        s
                          ? { ...s, formats: e.target.value }
                          : { formats: e.target.value, scores: "{}" },
                      )
                    }
                  />
                </label>
                <label className="block space-y-2 px-4 pb-4 text-sm font-medium text-fg">
                  {t("Score overrides for this profile")}
                  <Textarea
                    className="min-h-28 w-full font-mono text-xs"
                    spellCheck={false}
                    value={scoring?.scores ?? "{}"}
                    onChange={(e) =>
                      setScoring((s) =>
                        s
                          ? { ...s, scores: e.target.value }
                          : { formats: "[]", scores: e.target.value },
                      )
                    }
                  />
                </label>
                <p className="px-4 pb-4 text-sm">
                  <Link to="/releases/history" className="text-primary underline">
                    {t("Inspect release scores and rejection reasons")}
                  </Link>
                </p>
              </SettingsSection>
            </MoreOptions>
            <ImportedRules draft={draft} patch={patch} />
          </>
        ) : null}
      </RecordDrawer>
    </>
  );
}
