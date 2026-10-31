import { SearchSelect } from "@braedonsaunders/appkit-ui";
import { useMemo } from "react";
import { SettingsRow } from "../../components/SettingsRow";
import { searchSelectLabels } from "../../i18n/appkit-labels";
import { t } from "../../i18n";
import { useLocale } from "../../i18n/react";

const LANGUAGE_TAGS = [
  "en-US",
  "en-GB",
  "fr-FR",
  "fr-CA",
  "de-DE",
  "es-ES",
  "es-MX",
  "pt-BR",
  "pt-PT",
  "it-IT",
  "nl-NL",
  "sv-SE",
  "da-DK",
  "nb-NO",
  "fi-FI",
  "pl-PL",
  "cs-CZ",
  "sk-SK",
  "hu-HU",
  "ro-RO",
  "bg-BG",
  "hr-HR",
  "sl-SI",
  "et-EE",
  "lv-LV",
  "lt-LT",
  "ru-RU",
  "uk-UA",
  "tr-TR",
  "el-GR",
  "he-IL",
  "ar-SA",
  "fa-IR",
  "hi-IN",
  "bn-BD",
  "ta-IN",
  "te-IN",
  "ur-PK",
  "th-TH",
  "vi-VN",
  "id-ID",
  "ms-MY",
  "tl-PH",
  "zh-CN",
  "zh-TW",
  "zh-HK",
  "ja-JP",
  "ko-KR",
  "is-IS",
  "ga-IE",
  "ca-ES",
  "eu-ES",
  "af-ZA",
  "sw-KE",
];

const REGION_CODES =
  "AD AE AF AG AI AL AM AO AR AT AU AW AZ BA BB BD BE BF BG BH BI BJ BN BO BR BS BT BW BY BZ CA CD CF CG CH CI CL CM CN CO CR CV CY CZ DE DJ DK DM DO DZ EC EE EG ER ES ET FI FJ FR GA GB GD GE GH GM GN GR GT GW GY HK HN HR HT HU ID IE IL IN IQ IR IS IT JM JO JP KE KG KH KR KW KZ LA LB LC LI LK LR LS LT LU LV LY MA MC MD ME MG MK ML MM MN MR MT MU MV MW MX MY MZ NA NE NG NI NL NO NP NZ OM PA PE PG PH PK PL PT PY QA RO RS RU RW SA SB SC SD SE SG SI SK SL SM SN SO SR SS ST SV SY SZ TD TG TH TJ TL TM TN TR TT TW TZ UA UG US UY UZ VA VE VN VU YE ZA ZM ZW".split(
    " ",
  );

/** Go's time.Duration.String for the four refresh choices. */
const REFRESH = [
  { match: ["6h0m0s", "6h"], value: "6h0m0s", label: () => t("Every 6 hours") },
  { match: ["12h0m0s", "12h"], value: "12h0m0s", label: () => t("Every 12 hours") },
  { match: ["24h0m0s", "24h"], value: "24h0m0s", label: () => t("Daily") },
  { match: ["168h0m0s", "168h"], value: "168h0m0s", label: () => t("Weekly") },
];

export function isCatalogPicker(key: string) {
  return key === "language" || key === "region" || key === "refreshInterval";
}

function displayName(locale: string, type: "language" | "region", code: string) {
  try {
    return new Intl.DisplayNames([locale], { type }).of(code) ?? code;
  } catch {
    return code;
  }
}

function languageName(locale: string, tag: string) {
  const [lang, region] = tag.split("-");
  const name = lang ? displayName(locale, "language", lang) : tag;
  if (!region) return name;
  const country = displayName(locale, "region", region);
  return country && country !== region ? `${name} (${country})` : name;
}

function withCurrent(options: { value: string; label: string }[], current: string) {
  if (!current || options.some((o) => o.value === current)) return options;
  return [{ value: current, label: current }, ...options];
}

export function CatalogPickers({
  language,
  region,
  refreshInterval,
  onChange,
}: {
  language: unknown;
  region: unknown;
  refreshInterval: unknown;
  onChange: (key: "language" | "region" | "refreshInterval", value: string) => void;
}) {
  const { locale } = useLocale();
  const lang = String(language ?? "");
  const regionCode = String(region ?? "");
  const refresh = String(refreshInterval ?? "");
  const languages = useMemo(
    () =>
      withCurrent(
        LANGUAGE_TAGS.map((tag) => ({ value: tag, label: languageName(locale, tag) })).sort((a, b) =>
          a.label.localeCompare(b.label, locale),
        ),
        lang,
      ),
    [locale, lang],
  );
  const regions = useMemo(
    () =>
      withCurrent(
        REGION_CODES.map((code) => ({ value: code, label: displayName(locale, "region", code) })).sort(
          (a, b) => a.label.localeCompare(b.label, locale),
        ),
        regionCode,
      ),
    [locale, regionCode],
  );
  const refreshValue = REFRESH.find((row) => row.match.includes(refresh))?.value ?? refresh;
  const refreshOptions = REFRESH.map((row) => ({ value: row.value, label: row.label() }));
  if (refresh && !refreshOptions.some((o) => o.value === refreshValue))
    refreshOptions.unshift({ value: refresh, label: refresh });
  return (
    <>
      <SettingsRow
        title={t("Language")}
        control={
          <div className="w-56">
            <SearchSelect
              value={lang}
              onChange={(v) => onChange("language", v)}
              options={languages}
              ariaLabel={t("Language")}
              searchable
              {...searchSelectLabels()}
            />
          </div>
        }
      />
      <SettingsRow
        title={t("Region")}
        control={
          <div className="w-56">
            <SearchSelect
              value={regionCode}
              onChange={(v) => onChange("region", v)}
              options={regions}
              ariaLabel={t("Region")}
              searchable
              {...searchSelectLabels()}
            />
          </div>
        }
      />
      <SettingsRow
        title={t("Refresh every")}
        control={
          <div className="w-48">
            <SearchSelect
              value={refreshValue}
              onChange={(v) => onChange("refreshInterval", v)}
              options={refreshOptions}
              ariaLabel={t("Refresh every")}
              searchable={false}
              {...searchSelectLabels()}
            />
          </div>
        }
      />
    </>
  );
}
