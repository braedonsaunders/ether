import { SearchSelect, SettingsSection } from "@braedonsaunders/appkit-ui";
import { useMemo, useState } from "react";
import { ErrorPanel, LoadingRows } from "../../components/feedback";
import { SaveBar, useDraft } from "../../components/SaveBar";
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
 * Which kind of source leads when two releases are the same quality.
 * Dragging cards still chooses which account of that kind is asked first.
 * Quality is never traded for a faster start.
 */
export function EqualQuality() {
  const { config, resolver, save } = useResolver();
  const saved = useMemo(
    () => (resolver ? { sourcePreference: resolver.sourcePreference || "auto" } : null),
    [resolver],
  );
  const { draft, setDraft, dirty, reset } = useDraft(saved);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<unknown>(null);

  if (config.error) return <ErrorPanel error={config.error} onRetry={() => void config.reload()} />;
  if (!resolver || !draft) return <LoadingRows rows={1} />;

  async function submit() {
    if (!draft) return;
    setSaving(true);
    setError(null);
    try {
      await save(draft, t("Source order saved"));
    } catch (e) {
      setError(e);
    } finally {
      setSaving(false);
    }
  }

  return (
    <>
      <SettingsSection
        title={t("Which source first")}
        description={t(
          "Dragging a card chooses which account of that kind Ether asks first. This chooses Usenet or debrid when two releases are the same quality. Ether never picks a worse picture just to start sooner.",
        )}
      >
        <SettingsRow
          title={t("When quality matches")}
          description={
            <>
              {t(
                "Automatic puts the source that has been starting faster first, and cached debrid first for popular titles until both sources have measurements. The other source stays a fallback.",
              )}{" "}
              <SourceStartupNote />
            </>
          }
          control={
            <div className="w-52">
              <SearchSelect
                {...searchSelectLabels()}
                value={draft.sourcePreference}
                onChange={(v) => setDraft({ ...draft, sourcePreference: v })}
                options={SOURCE_PREFERENCE}
                ariaLabel={t("Which source first when quality matches")}
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
        onSave={submit}
        onDiscard={() => {
          reset();
          setError(null);
        }}
      />
    </>
  );
}
