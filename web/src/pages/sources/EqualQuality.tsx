import { SearchSelect, SettingsSection } from "@braedonsaunders/appkit-ui";
import { ErrorPanel } from "../../components/feedback";
import { SettingsRow } from "../../components/SettingsRow";
import { localized, t, tn } from "../../i18n";
import { searchSelectLabels } from "../../i18n/appkit-labels";
import { api } from "../../lib/api";
import { formatNanos } from "../../lib/format";
import { useAsync } from "../../lib/hooks";

export const SOURCE_PREFERENCE = localized(() => [
  { value: "auto", label: t("Start fastest") },
  { value: "usenet", label: t("Usenet first") },
  { value: "debrid", label: t("Debrid first") },
  { value: "usenet-always", label: t("Always Usenet") },
  { value: "debrid-always", label: t("Always cached debrid") },
]);

/** The select value for resolver.sourcePreference + sourceAlwaysFirst. */
export function sourceOrderValue(r?: {
  sourcePreference?: string;
  sourceAlwaysFirst?: boolean;
}): string {
  const pref = r?.sourcePreference || "auto";
  return r?.sourceAlwaysFirst && (pref === "usenet" || pref === "debrid") ? `${pref}-always` : pref;
}

/** The resolver fields a select value stands for. */
export function sourceOrderFields(value: string): {
  sourcePreference: string;
  sourceAlwaysFirst: boolean;
} {
  const always = value.endsWith("-always");
  return {
    sourcePreference: always ? value.slice(0, -"-always".length) : value,
    sourceAlwaysFirst: always,
  };
}

/** Measured startup per source, as the resolver sees it. */
export function SourceStartupNote() {
  const startup = useAsync(() => api.sourceStartup().catch(() => []), []);
  const rows = startup.data ?? [];
  if (rows.length === 0) return <>{t("No plays measured yet.")}</>;
  return (
    <>
      {t("Measured so far: {list}.", {
        list: rows
          .map((r) =>
            tn(
              r.samples,
              "{source} {duration} ({count} play)",
              "{source} {duration} ({count} plays)",
              {
                source: r.protocol === "torrent" ? t("debrid") : "Usenet",
                duration: formatNanos(r.average),
              },
            ),
          )
          .join(", "),
      })}
    </>
  );
}

/**
 * Last step of the profile drawer: which kind of source leads. The first
 * three choices only decide between releases that are equal after the
 * profile's ranking ladder; the "Always" choices put that source first
 * whatever the other has (resolver.sourcePreference + sourceAlwaysFirst).
 * Applies to all profiles.
 */
export function TieBreakerSection({
  value,
  saving,
  error,
  onChange,
}: {
  value: string;
  saving?: boolean;
  error?: unknown;
  onChange: (sourcePreference: string) => void;
}) {
  return (
    <SettingsSection title={t("Tie-breaker")} description={t("Applies to all profiles.")}>
      <SettingsRow
        title={t("Which source goes first")}
        description={
          <>
            {t("Start fastest picks whichever source has been quicker to start.")}{" "}
            {t(
              "Usenet first and Debrid first only decide between equal releases. The Always choices use that source even when the other has a better release, and fall back to the other when it has nothing that plays.",
            )}{" "}
            <SourceStartupNote />
          </>
        }
        control={
          <div className="w-52">
            <SearchSelect
              {...searchSelectLabels()}
              value={value || "auto"}
              onChange={onChange}
              options={SOURCE_PREFERENCE}
              ariaLabel={t("Which source goes first")}
              searchable={false}
              disabled={saving}
            />
          </div>
        }
      />
      {error ? <ErrorPanel error={error} /> : null}
    </SettingsSection>
  );
}
