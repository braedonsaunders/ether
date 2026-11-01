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
]);

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
 * Last step of the profile drawer: which kind of source leads when two
 * releases are equal after the profile's ranking ladder. Applies to all
 * profiles (resolver.sourcePreference).
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
        title={t("When two releases are equal")}
        description={
          <>
            {t("Start fastest picks whichever source has been quicker to start.")}{" "}
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
              ariaLabel={t("When two releases are equal")}
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
