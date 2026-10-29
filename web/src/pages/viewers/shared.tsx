import { Input, Select, SettingsSection, Switch } from "@braedonsaunders/appkit-ui";
import { useEffect, useState } from "react";
import { SettingsRow } from "../../components/SettingsRow";
import { localized, t } from "../../i18n";

/** Split a comma-separated list, optionally lower-casing (language codes). */
export function parseList(text: string, lower = false): string[] {
  return text
    .split(",")
    .map((s) => (lower ? s.trim().toLowerCase() : s.trim()))
    .filter(Boolean);
}

/**
 * Comma-separated list edited as text. Unlike `ListInput` it reports every
 * keystroke, so the save bar appears while typing rather than after blur.
 */
export function ListText({
  value,
  onChange,
  lower = false,
  placeholder,
  ariaLabel,
  disabled,
}: {
  value: string[] | null | undefined;
  onChange: (v: string[]) => void;
  lower?: boolean;
  placeholder?: string;
  ariaLabel: string;
  disabled?: boolean;
}) {
  const list = value ?? [];
  const [text, setText] = useState(list.join(", "));
  const key = JSON.stringify(list);
  useEffect(() => {
    // Follow outside changes (discard, reset, presets) without fighting typing.
    if (JSON.stringify(parseList(text, lower)) !== key) setText(list.join(", "));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [key]);
  return (
    <Input
      aria-label={ariaLabel}
      value={text}
      disabled={disabled}
      placeholder={placeholder ?? t("Comma-separated")}
      onChange={(e) => {
        setText(e.target.value);
        onChange(parseList(e.target.value, lower));
      }}
    />
  );
}

// ---- Audio & subtitle rules (/stream-preferences/:user; user 0 is the default) ----

export type StreamRules = {
  audioLanguages: string[];
  subtitleLanguages: string[];
  preferOriginal: boolean;
  subtitleMode: string;
  subtitleStyle: string;
  loudness: boolean;
  nightMode: boolean;
  dialogueBoost: boolean;
};

export type ShowMemory = {
  showId: number;
  showTitle?: string;
  kind: string;
  choice: { language: string; forced: boolean; sdh: boolean; off: boolean };
};

const INITIAL_RULES: StreamRules = {
  audioLanguages: [],
  subtitleLanguages: [],
  preferOriginal: false,
  subtitleMode: "off",
  subtitleStyle: "normal",
  loudness: false,
  nightMode: false,
  dialogueBoost: false,
};

/** Fill gaps in a server response (older servers send null language lists). */
export function normalizeRules(r: Partial<StreamRules> | null | undefined): StreamRules {
  return {
    ...INITIAL_RULES,
    ...r,
    audioLanguages: r?.audioLanguages ?? [],
    subtitleLanguages: r?.subtitleLanguages ?? [],
  };
}

export function sameRules(a: StreamRules, b: StreamRules): boolean {
  const keys = Object.keys(INITIAL_RULES) as (keyof StreamRules)[];
  return keys.every((k) => JSON.stringify(a[k]) === JSON.stringify(b[k]));
}

const SUBTITLE_MODES = localized(() => [
  { value: "off", label: t("Off") },
  { value: "forced-only", label: t("Forced tracks only") },
  { value: "foreign-only", label: t("When the audio is in another language") },
  { value: "always", label: t("Always") },
]);
const SUBTITLE_STYLES = localized(() => [
  { value: "normal", label: t("Normal") },
  { value: "sdh", label: t("SDH / hearing impaired") },
  { value: "forced", label: t("Forced") },
]);

/** The audio and subtitle choices, shared by the defaults and a viewer's override. */
export function StreamRulesFields({
  value,
  onChange,
  disabled,
}: {
  value: StreamRules;
  onChange: (next: StreamRules) => void;
  disabled?: boolean;
}) {
  const set = <K extends keyof StreamRules>(k: K, v: StreamRules[K]) =>
    onChange({ ...value, [k]: v });
  return (
    <>
      <SettingsSection
        title={t("Languages")}
        description={t("Plex track changes are remembered for later episodes of the same show.")}
      >
        <SettingsRow
          title={t("Audio languages")}
          description={t("Language codes in preference order, e.g. en, ja.")}
          control={
            <div className="w-56">
              <ListText
                ariaLabel={t("Audio languages")}
                lower
                placeholder="en, ja, fr"
                value={value.audioLanguages}
                onChange={(v) => set("audioLanguages", v)}
                disabled={disabled}
              />
            </div>
          }
        />
        <SettingsRow
          title={t("Prefer the original language")}
          description={t("Play a title's original language before dubs.")}
          control={
            <Switch
              aria-label={t("Prefer the original language")}
              checked={value.preferOriginal}
              disabled={disabled}
              onChange={(e) => set("preferOriginal", e.target.checked)}
            />
          }
        />
        <SettingsRow
          title={t("Subtitle languages")}
          description={t("Language codes in preference order, e.g. en, fr.")}
          control={
            <div className="w-56">
              <ListText
                ariaLabel={t("Subtitle languages")}
                lower
                placeholder="en, fr"
                value={value.subtitleLanguages}
                onChange={(v) => set("subtitleLanguages", v)}
                disabled={disabled}
              />
            </div>
          }
        />
        <SettingsRow
          title={t("Show subtitles")}
          control={
            <div className="w-64">
              <Select
                aria-label={t("Show subtitles")}
                value={value.subtitleMode}
                disabled={disabled}
                onChange={(e) => set("subtitleMode", e.target.value)}
              >
                {SUBTITLE_MODES.map((o) => (
                  <option key={o.value} value={o.value}>
                    {o.label}
                  </option>
                ))}
              </Select>
            </div>
          }
        />
        <SettingsRow
          title={t("Subtitle style")}
          control={
            <div className="w-64">
              <Select
                aria-label={t("Subtitle style")}
                value={value.subtitleStyle}
                disabled={disabled}
                onChange={(e) => set("subtitleStyle", e.target.value)}
              >
                {SUBTITLE_STYLES.map((o) => (
                  <option key={o.value} value={o.value}>
                    {o.label}
                  </option>
                ))}
              </Select>
            </div>
          }
        />
      </SettingsSection>
      <SettingsSection
        title={t("Audio processing")}
        description={t(
          "Needs server audio transcoding. Dialogue boost applies to sources with six or more channels.",
        )}
      >
        {(
          [
            ["loudness", t("Normalize loudness")],
            ["nightMode", t("Night mode compression")],
            ["dialogueBoost", t("Boost centre-channel dialogue")],
          ] as const
        ).map(([key, label]) => (
          <SettingsRow
            key={key}
            title={label}
            control={
              <Switch
                aria-label={label}
                checked={value[key]}
                disabled={disabled}
                onChange={(e) => set(key, e.target.checked)}
              />
            }
          />
        ))}
      </SettingsSection>
    </>
  );
}

// ---- Safety policy (/safety/policies/:user) ----------------------------------------

export type SafetyPolicy = {
  maxContentRating: string;
  blockedGenres: string[];
  blockedKeywords: string[];
  remoteDisabled: boolean;
  maxBitrateKbps: number;
  streamLimit: number;
  allowedHours: number[];
  timezone: string;
};

export function emptyPolicy(): SafetyPolicy {
  return {
    maxContentRating: "",
    blockedGenres: [],
    blockedKeywords: [],
    remoteDisabled: false,
    maxBitrateKbps: 0,
    streamLimit: 0,
    allowedHours: [],
    timezone: Intl.DateTimeFormat().resolvedOptions().timeZone,
  };
}

export function normalizePolicy(p: Partial<SafetyPolicy> | null | undefined): SafetyPolicy {
  const base = emptyPolicy();
  return {
    ...base,
    ...p,
    blockedGenres: p?.blockedGenres ?? [],
    blockedKeywords: p?.blockedKeywords ?? [],
    allowedHours: [...(p?.allowedHours ?? [])].sort((a, b) => a - b),
    timezone: p?.timezone ?? base.timezone,
  };
}

export const KID_PRESETS = localized(
  () =>
    [
      { value: "preschool", label: t("Young children") },
      { value: "family", label: t("Family") },
      { value: "teens", label: t("Teens") },
    ] as const,
);
