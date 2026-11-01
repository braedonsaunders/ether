import { SearchSelect } from "@braedonsaunders/appkit-ui";
import { useMemo } from "react";
import { MoreOptions } from "../../components/ConfigSection";
import { Field } from "../../components/fields";
import { searchSelectLabels } from "../../i18n/appkit-labels";
import { loadLocale, LOCALES, rememberLocale, t, type Locale } from "../../i18n";
import { useLocale } from "../../i18n/react";
import { LANGUAGE_TAGS, REGION_CODES } from "../catalog/CatalogPickers";
import {
  ORIGINAL_AUDIO,
  baseLanguage,
  withConsoleLanguage,
  type HouseholdLanguage,
} from "./language";

function displayName(locale: string, type: "language" | "region", code: string) {
  try {
    return new Intl.DisplayNames([locale], { type }).of(code) ?? code;
  } catch {
    return code;
  }
}

function metadataLabel(locale: string, tag: string) {
  const [lang, region] = tag.split("-");
  const name = lang ? displayName(locale, "language", lang) : tag;
  if (!region) return name;
  const country = displayName(locale, "region", region);
  return country && country !== region ? `${name} (${country})` : name;
}

function withCurrent(options: { value: string; label: string }[], current: string, label?: string) {
  if (!current || options.some((option) => option.value === current)) return options;
  return [{ value: current, label: label ?? current }, ...options];
}

/** Searchable household language, with audio, subtitles and metadata split under More options. */
export function LanguageStep({
  choice,
  onChange,
}: {
  choice: HouseholdLanguage;
  onChange: (next: HouseholdLanguage) => void;
}) {
  const { locale, setLocale } = useLocale();
  const labels = searchSelectLabels();
  const languages = useMemo(
    () =>
      LOCALES.map((item) => ({ value: item.value, label: item.nativeLabel })).sort((a, b) =>
        a.label.localeCompare(b.label, locale),
      ),
    [locale],
  );
  const metadata = useMemo(
    () =>
      withCurrent(
        LANGUAGE_TAGS.map((tag) => ({ value: tag, label: metadataLabel(locale, tag) })).sort(
          (a, b) => a.label.localeCompare(b.label, locale),
        ),
        choice.metadata,
      ),
    [locale, choice.metadata],
  );
  const regions = useMemo(
    () =>
      withCurrent(
        REGION_CODES.map((code) => ({
          value: code,
          label: displayName(locale, "region", code),
        })).sort((a, b) => a.label.localeCompare(b.label, locale)),
        choice.region,
      ),
    [locale, choice.region],
  );
  const audioLanguages = useMemo(() => {
    const seen = new Set<string>();
    const options = [{ value: ORIGINAL_AUDIO, label: t("Original language") }];
    for (const tag of LANGUAGE_TAGS) {
      const code = baseLanguage(tag);
      if (!code || seen.has(code)) continue;
      seen.add(code);
      options.push({ value: code, label: displayName(locale, "language", code) });
    }
    options.sort((a, b) =>
      a.value === ORIGINAL_AUDIO
        ? -1
        : b.value === ORIGINAL_AUDIO
          ? 1
          : a.label.localeCompare(b.label, locale),
    );
    return withCurrent(options, choice.audio);
  }, [locale, choice.audio]);
  const subtitleLanguages = useMemo(() => {
    const seen = new Set<string>();
    const options: { value: string; label: string }[] = [];
    for (const tag of LANGUAGE_TAGS) {
      const code = baseLanguage(tag);
      if (!code || seen.has(code)) continue;
      seen.add(code);
      options.push({ value: code, label: displayName(locale, "language", code) });
    }
    options.sort((a, b) => a.label.localeCompare(b.label, locale));
    return withCurrent(options, choice.subtitle);
  }, [locale, choice.subtitle]);

  const pickConsole = (next: string) => {
    const localeNext = next as Locale;
    void (async () => {
      rememberLocale(localeNext);
      await loadLocale(localeNext);
      await setLocale(localeNext);
      onChange(withConsoleLanguage(choice, localeNext));
    })();
  };

  return (
    <div className="rounded-xl border border-border bg-surface [animation:ether-caption_0.45s_ease_both]">
      <div className="p-4">
        <Field label={t("Language")}>
          <SearchSelect
            value={choice.console}
            onChange={pickConsole}
            options={languages}
            ariaLabel={t("Language")}
            searchable
            {...labels}
          />
        </Field>
      </div>
      <MoreOptions label={t("More options")}>
        <div className="grid gap-4 px-4 pb-4 sm:grid-cols-2">
          <Field label={t("Metadata language")}>
            <SearchSelect
              value={choice.metadata}
              onChange={(metadata) => onChange({ ...choice, metadata })}
              options={metadata}
              ariaLabel={t("Metadata language")}
              searchable
              {...labels}
            />
          </Field>
          <Field label={t("Region")}>
            <SearchSelect
              value={choice.region}
              onChange={(region) => onChange({ ...choice, region })}
              options={regions}
              ariaLabel={t("Region")}
              searchable
              {...labels}
            />
          </Field>
          <Field label={t("Audio language")}>
            <SearchSelect
              value={choice.audio}
              onChange={(audio) => onChange({ ...choice, audio })}
              options={audioLanguages}
              ariaLabel={t("Audio language")}
              searchable
              {...labels}
            />
          </Field>
          <Field label={t("Subtitle language")}>
            <SearchSelect
              value={choice.subtitle}
              onChange={(subtitle) => onChange({ ...choice, subtitle })}
              options={subtitleLanguages}
              ariaLabel={t("Subtitle language")}
              searchable
              {...labels}
            />
          </Field>
        </div>
      </MoreOptions>
    </div>
  );
}
