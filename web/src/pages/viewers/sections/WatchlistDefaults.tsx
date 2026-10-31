import { SettingsSection, Spinner, Switch, toast } from "@braedonsaunders/appkit-ui";
import { useState } from "react";
import { ErrorPanel } from "../../../components/feedback";
import { NumberInput } from "../../../components/fields";
import { SaveBar, useDraft } from "../../../components/SaveBar";
import { SettingsRow } from "../../../components/SettingsRow";
import { localized, t } from "../../../i18n";
import { apiFetch } from "../../../lib/api";
import { useAsync } from "../../../lib/hooks";

/** `/discovery/settings`: watchlist alerts plus the language-model features. */
export type DiscoverySettings = {
  enabled: boolean;
  plex: boolean;
  trakt: boolean;
  notify: boolean;
  prewarm: boolean;
  checkHours: number;
  naturalSearch: boolean;
  recaps: boolean;
  subtitleTranslate?: boolean;
  speechToText?: boolean;
  overviewTranslate?: boolean;
};

/**
 * Load and save the shared discovery settings object. Each caller edits only
 * its own fields; saving sends the whole object back, as the API expects.
 */
export function useDiscoverySettings() {
  const doc = useAsync(() => apiFetch<DiscoverySettings>("/discovery/settings"), []);
  const { draft, setDraft, dirty, reset } = useDraft(doc.data);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<unknown>(null);
  const save = async (toastTitle: string) => {
    if (!draft) return;
    setSaving(true);
    setError(null);
    try {
      const next = await apiFetch<DiscoverySettings>("/discovery/settings", {
        method: "PUT",
        body: JSON.stringify(draft),
      });
      doc.setData(next);
      toast.success(toastTitle);
    } catch (e) {
      setError(e);
    } finally {
      setSaving(false);
    }
  };
  const discard = () => {
    reset();
    setError(null);
  };
  return { doc, draft, setDraft, dirty, saving, error, save, discard };
}

const SOURCES = localized(
  () =>
    [
      {
        key: "plex",
        label: t("Plex watchlists"),
        hint: t(
          "You and your Plex Home members. A friend's server access doesn't include their Plex watchlist.",
        ),
      },
      {
        key: "trakt",
        label: t("Trakt watchlists"),
        hint: t("Anyone who has connected Trakt, including friends."),
      },
      {
        key: "notify",
        label: t("Tell me too"),
        hint: t("Also send availability alerts to your notification channels."),
      },
      {
        key: "prewarm",
        label: t("Warm up new arrivals"),
        hint: t(
          "Fetch the opening minutes as soon as a title becomes available, so it starts quickly.",
        ),
      },
    ] as const,
);

/** Server-wide watchlist alerts, edited in place (Viewers › Defaults). */
export function WatchlistDefaults() {
  const s = useDiscoverySettings();
  if (s.doc.error) return <ErrorPanel error={s.doc.error} onRetry={() => void s.doc.reload()} />;
  if (!s.draft)
    return (
      <div className="py-10 text-center">
        <Spinner size={20} label={t("Loading watchlist settings")} />
      </div>
    );
  const draft = s.draft;
  return (
    <>
      <SettingsSection
        title={t("Watchlist alerts")}
        description={t(
          "When a title on a viewer's watchlist can be streamed, it appears on their Plex Home. A release date alone isn't enough: Ether waits for a healthy release.",
        )}
      >
        <SettingsRow
          title={<label htmlFor="watchlist-enabled">{t("Watchlist monitoring")}</label>}
          description={t("Check viewers' watchlists for titles that have become available.")}
          control={
            <Switch
              id="watchlist-enabled"
              aria-label={t("Watchlist monitoring")}
              checked={draft.enabled}
              onChange={(e) => s.setDraft({ ...draft, enabled: e.currentTarget.checked })}
            />
          }
        />
        {SOURCES.map((o) => (
          <SettingsRow
            key={o.key}
            title={<label htmlFor={`watchlist-${o.key}`}>{o.label}</label>}
            description={o.hint}
            control={
              <Switch
                id={`watchlist-${o.key}`}
                aria-label={o.label}
                checked={draft[o.key]}
                disabled={!draft.enabled}
                onChange={(e) => s.setDraft({ ...draft, [o.key]: e.currentTarget.checked })}
              />
            }
          />
        ))}
        <SettingsRow
          title={<label htmlFor="watchlist-hours">{t("Check every")}</label>}
          description={t("Minimum time between checks of the same watchlist.")}
          control={
            <span className="flex items-center gap-2">
              <NumberInput
                id="watchlist-hours"
                aria-label={t("Check every")}
                className="w-24"
                min={1}
                max={168}
                disabled={!draft.enabled}
                value={draft.checkHours}
                onChange={(n) => s.setDraft({ ...draft, checkHours: n })}
              />
              <span className="text-sm text-fg-muted">{t("hours")}</span>
            </span>
          }
        />
      </SettingsSection>
      <SaveBar
        dirty={s.dirty}
        saving={s.saving}
        error={s.error}
        onSave={() => s.save(t("Watchlist settings saved"))}
        onDiscard={s.discard}
      />
    </>
  );
}
