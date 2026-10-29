import { Button, SettingsSection, toast } from "@braedonsaunders/appkit-ui";
import { ArrowRight } from "lucide-react";
import { useState } from "react";
import { useNavigate } from "react-router-dom";
import { ErrorPanel, LoadingRows } from "../../components/feedback";
import { SaveBar, useDraft } from "../../components/SaveBar";
import { SettingsRow } from "../../components/SettingsRow";
import { t } from "../../i18n";
import { apiFetch } from "../../lib/api";
import { useAsync } from "../../lib/hooks";
import { WatchlistDefaults } from "./sections/WatchlistDefaults";
import { normalizeRules, StreamRulesFields, type StreamRules } from "./shared";

/**
 * What every viewer gets unless their profile says otherwise: audio and
 * subtitle choices, watchlist alerts, and pointers to the server-wide limits.
 */
export function ViewerDefaults() {
  const navigate = useNavigate();
  const rules = useAsync(
    async (signal) =>
      normalizeRules(await apiFetch<StreamRules>("/stream-preferences/0", { signal })),
    [],
  );
  const { draft, setDraft, dirty, reset } = useDraft<StreamRules>(rules.data);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<unknown>(null);

  const save = async () => {
    if (!draft) return;
    setSaving(true);
    setError(null);
    try {
      await apiFetch("/stream-preferences/0", { method: "PUT", body: JSON.stringify(draft) });
      rules.setData(draft);
      toast.success(t("Defaults saved"), {
        description: t("Viewers without their own audio and subtitle choices use these."),
      });
    } catch (e) {
      setError(e);
    } finally {
      setSaving(false);
    }
  };

  return (
    <div className="max-w-3xl space-y-6">
      {rules.error ? (
        <ErrorPanel error={rules.error} onRetry={() => void rules.reload()} />
      ) : !draft ? (
        <LoadingRows rows={4} />
      ) : (
        <>
          <p className="text-sm text-fg-muted">
            {t(
              "Audio and subtitle choices for every viewer. Give one person their own on the Audio & subtitles tab of their profile.",
            )}
          </p>
          <StreamRulesFields value={draft} onChange={setDraft} disabled={saving} />
        </>
      )}

      <WatchlistDefaults />

      <SettingsSection title={t("Elsewhere")}>
        <SettingsRow
          title={t("Stream limit")}
          description={t(
            "The server-wide limit on simultaneous streams. Set a different one per viewer on their Access tab.",
          )}
          control={
            <Button variant="outline" size="sm" onClick={() => navigate("/server")}>
              {t("Settings › General")} <ArrowRight size={14} />
            </Button>
          }
        />
        <SettingsRow
          title={t("Skip intros and credits")}
          description={t("Marker sources and coverage.")}
          control={
            <Button variant="outline" size="sm" onClick={() => navigate("/playback/markers")}>
              {t("Playback › Markers")} <ArrowRight size={14} />
            </Button>
          }
        />
      </SettingsSection>

      <SaveBar
        dirty={dirty}
        saving={saving}
        error={error}
        onSave={save}
        onDiscard={() => {
          reset();
          setError(null);
        }}
      />
    </div>
  );
}
