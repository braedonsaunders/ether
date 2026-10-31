import { negotiateLocale, type AppLocale } from "@braedonsaunders/appkit-i18n";
import { rememberedLocale } from "../../i18n";
import { LANGUAGE_TAGS, REGION_CODES } from "../catalog/CatalogPickers";
import type { StreamRules } from "../viewers/shared";

/** Audio choice that ranks the title's original language first. */
export const ORIGINAL_AUDIO = "original";

/**
 * One household language choice. The simple picker sets console, metadata,
 * audio and subtitles together. Region follows the browser and stays put
 * when the language changes.
 */
export interface HouseholdLanguage {
  /** Owner console locale. */
  console: AppLocale;
  /** Catalogue metadata language (Catalog › General), e.g. en-US. */
  metadata: string;
  /** Catalogue region. Editable, and not rewritten by the language picker. */
  region: string;
  /** ISO 639 audio code, or ORIGINAL_AUDIO. */
  audio: string;
  /** ISO 639 subtitle code. Subtitle mode is left alone. */
  subtitle: string;
}

/** The browser's language list, or English when the runtime has no navigator. */
export function browserLanguageList(): string[] {
  if (typeof navigator === "undefined") return ["en"];
  const list = navigator.languages?.length ? [...navigator.languages] : [navigator.language];
  return list.filter(Boolean);
}

export function baseLanguage(tag: string): string {
  return tag.trim().split("-")[0]?.toLowerCase() ?? "";
}

function regionSubtag(tag: string): string {
  for (const part of tag.split("-").slice(1)) {
    if (/^[A-Za-z]{2}$/.test(part)) return part.toUpperCase();
  }
  return "";
}

const TIME_ZONE_REGION: Record<string, string> = {
  "America/Toronto": "CA",
  "America/Vancouver": "CA",
  "America/Edmonton": "CA",
  "America/Winnipeg": "CA",
  "America/Halifax": "CA",
  "America/St_Johns": "CA",
  "America/Montreal": "CA",
  "Europe/London": "GB",
  "Europe/Dublin": "IE",
  "Europe/Paris": "FR",
  "Europe/Berlin": "DE",
  "Europe/Madrid": "ES",
  "Europe/Rome": "IT",
  "Europe/Amsterdam": "NL",
  "Europe/Brussels": "BE",
  "Europe/Stockholm": "SE",
  "Europe/Oslo": "NO",
  "Europe/Copenhagen": "DK",
  "Europe/Helsinki": "FI",
  "Europe/Warsaw": "PL",
  "Europe/Prague": "CZ",
  "Europe/Lisbon": "PT",
  "Europe/Vienna": "AT",
  "Europe/Zurich": "CH",
  "Australia/Sydney": "AU",
  "Australia/Melbourne": "AU",
  "Pacific/Auckland": "NZ",
  "Asia/Tokyo": "JP",
  "Asia/Seoul": "KR",
  "Asia/Shanghai": "CN",
  "Asia/Hong_Kong": "HK",
  "Asia/Taipei": "TW",
  "America/Sao_Paulo": "BR",
  "America/Mexico_City": "MX",
};

/** Region for a time zone the catalogue knows, or empty. */
export function regionFromTimeZone(timeZone: string): string {
  const region = TIME_ZONE_REGION[timeZone] ?? "";
  return region && REGION_CODES.includes(region) ? region : "";
}

/** First region subtag in the browser's language list that the catalogue knows. */
export function regionFromLocales(languages: readonly string[], fallback = "US"): string {
  for (const tag of languages) {
    const region = regionSubtag(tag);
    if (region && REGION_CODES.includes(region)) return region;
  }
  return fallback;
}

/** Metadata tag for a console locale, preferring the household's region when that tag exists. */
export function metadataFor(locale: string, region: string): string {
  const lang = baseLanguage(locale);
  const regional = LANGUAGE_TAGS.find(
    (tag) => tag.toLowerCase() === `${lang}-${region}`.toLowerCase(),
  );
  if (regional) return regional;
  if ((LANGUAGE_TAGS as readonly string[]).includes(locale)) return locale;
  const any = LANGUAGE_TAGS.find((tag) => baseLanguage(tag) === lang);
  if (any) return any;
  return region ? `${lang}-${region}` : locale;
}

/** Browser languages, before the owner has saved a console language. */
export function freshHouseholdLanguage(browserLanguages: readonly string[]): HouseholdLanguage {
  const list = browserLanguages.map((tag) => tag.trim()).filter(Boolean);
  const consoleLocale = negotiateLocale(list.length ? list : ["en"]);
  const explicit = list.some((tag) => regionSubtag(tag));
  const zone =
    !explicit && typeof Intl !== "undefined"
      ? regionFromTimeZone(Intl.DateTimeFormat().resolvedOptions().timeZone)
      : "";
  const region = zone || regionFromLocales(list);
  const audio = baseLanguage(consoleLocale);
  return {
    console: consoleLocale,
    metadata: metadataFor(consoleLocale, region),
    region,
    audio,
    subtitle: audio,
  };
}

/** Saved catalogue and household defaults, for a server that already chose. */
export function savedHouseholdLanguage(input: {
  remembered: AppLocale | null;
  browserLanguages?: readonly string[];
  catalogLanguage: string;
  catalogRegion: string;
  audioLanguages: readonly string[];
  subtitleLanguages: readonly string[];
  preferOriginal: boolean;
}): HouseholdLanguage {
  const metadata = input.catalogLanguage.trim() || "en-US";
  const region = input.catalogRegion.trim() || regionSubtag(metadata) || "US";
  const browser = input.browserLanguages?.map((tag) => tag.trim()).filter(Boolean) ?? [];
  const consoleLocale =
    input.remembered ??
    negotiateLocale(browser.length ? browser : [metadata, baseLanguage(metadata)]);
  const audio = input.preferOriginal
    ? ORIGINAL_AUDIO
    : baseLanguage(input.audioLanguages[0] || metadata);
  const subtitle = baseLanguage(input.subtitleLanguages[0] || metadata);
  return { console: consoleLocale, metadata, region, audio, subtitle };
}

/**
 * A first-time setup with no saved console language and no household audio
 * or subtitle list takes the browser. Anything already stored is shown as-is.
 */
export function initialHouseholdLanguage(input: {
  setupComplete: boolean;
  remembered: AppLocale | null;
  browserLanguages: readonly string[];
  catalogLanguage: string;
  catalogRegion: string;
  audioLanguages: readonly string[];
  subtitleLanguages: readonly string[];
  preferOriginal: boolean;
}): HouseholdLanguage {
  const untouched =
    !input.setupComplete &&
    input.remembered == null &&
    input.audioLanguages.length === 0 &&
    input.subtitleLanguages.length === 0 &&
    !input.preferOriginal;
  if (untouched) return freshHouseholdLanguage(input.browserLanguages);
  return savedHouseholdLanguage({ ...input, browserLanguages: input.browserLanguages });
}

/** The simple picker: one language sets console, metadata, audio and subtitles. Region stays. */
export function withConsoleLanguage(
  choice: HouseholdLanguage,
  locale: AppLocale,
): HouseholdLanguage {
  const audio = baseLanguage(locale);
  return {
    ...choice,
    console: locale,
    metadata: metadataFor(locale, choice.region),
    audio,
    subtitle: audio,
  };
}

/** First-run suggestion, or the languages this server already stores. */
export function householdLanguageFor(input: {
  setupComplete: boolean;
  catalogLanguage: string;
  catalogRegion: string;
  audioLanguages: readonly string[];
  subtitleLanguages: readonly string[];
  preferOriginal: boolean;
  browserLanguages?: readonly string[];
}): HouseholdLanguage {
  return initialHouseholdLanguage({
    ...input,
    remembered: rememberedLocale(),
    browserLanguages: input.browserLanguages ?? browserLanguageList(),
  });
}

export function sameHouseholdLanguage(a: HouseholdLanguage, b: HouseholdLanguage): boolean {
  return (
    a.console === b.console &&
    a.metadata === b.metadata &&
    a.region === b.region &&
    a.audio === b.audio &&
    a.subtitle === b.subtitle
  );
}

/**
 * Household defaults the playback fallback reads: user 0 audioLanguages,
 * then subtitleLanguages. Subtitle mode is copied through unchanged.
 * Original-language audio sets preferOriginal and leaves any secondary
 * audio list that was already stored with that flag.
 */
export function projectHouseholdRules<T extends StreamRules>(
  existing: T,
  choice: HouseholdLanguage,
): T {
  const preferOriginal = choice.audio === ORIGINAL_AUDIO;
  const audio = baseLanguage(choice.audio);
  const subtitle = baseLanguage(choice.subtitle);
  return {
    ...existing,
    preferOriginal,
    audioLanguages: preferOriginal
      ? existing.preferOriginal
        ? existing.audioLanguages
        : []
      : audio.length >= 2
        ? [audio]
        : existing.audioLanguages,
    subtitleLanguages: subtitle.length >= 2 ? [subtitle] : existing.subtitleLanguages,
  };
}
