import { SearchSelect, SettingsSection } from "@braedonsaunders/appkit-ui";
import { useState } from "react";
import { ErrorPanel, LoadingRows } from "../../components/feedback";
import { SettingsRow } from "../../components/SettingsRow";
import { localized, t, tn } from "../../i18n";
import { searchSelectLabels } from "../../i18n/appkit-labels";
import { api } from "../../lib/api";
import { formatNanos } from "../../lib/format";
import { useAsync } from "../../lib/hooks";
import { useResolver } from "../releases/resolverDraft";

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

export function EqualQuality() {
  const { config, resolver, save } = useResolver();
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<unknown>(null);

  if (config.error) return <ErrorPanel error={config.error} onRetry={() => void config.reload()} />;
  if (!resolver) return <LoadingRows rows={1} />;

  async function choose(sourcePreference: string) {
    setSaving(true);
    setError(null);
    try {
      await save({ sourcePreference }, t("Source order saved"));
    } catch (e) {
      setError(e);
    } finally {
      setSaving(false);
    }
  }

  return (
    <TieBreakerSection
      value={resolver.sourcePreference || "auto"}
      saving={saving}
      error={error}
      onChange={(v) => void choose(v)}
    />
  );
}
