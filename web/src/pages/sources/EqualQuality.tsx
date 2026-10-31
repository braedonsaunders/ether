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

const SOURCE_PREFERENCE = localized(() => [
  { value: "auto", label: t("Fastest (automatic)") },
  { value: "usenet", label: t("Usenet") },
  { value: "debrid", label: t("Debrid") },
]);

/** Measured startup per source, as the resolver sees it. */
function SourceStartupNote() {
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
 * Which kind of source leads when two releases are the same quality. It saves
 * on change, so it can sit beside other settings without a second save bar.
 * Quality is never traded for a faster start.
 */
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
    <SettingsSection title={t("Usenet or debrid")}>
      <SettingsRow
        title={t("When quality matches")}
        description={
          <>
            {t("Automatic starts whichever source has been faster.")} <SourceStartupNote />
          </>
        }
        control={
          <div className="w-52">
            <SearchSelect
              {...searchSelectLabels()}
              value={resolver.sourcePreference || "auto"}
              onChange={(v) => void choose(v)}
              options={SOURCE_PREFERENCE}
              ariaLabel={t("Which source first when quality matches")}
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
