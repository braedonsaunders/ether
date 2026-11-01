import { SettingsSection, Switch } from "@braedonsaunders/appkit-ui";
import { useMemo, useState } from "react";
import { Link } from "react-router-dom";
import { useApp } from "../../app-context";
import { secretConfigured } from "../../components/IntegrationAccount";
import { ErrorPanel, LoadingRows } from "../../components/feedback";
import { SaveBar, useDraft } from "../../components/SaveBar";
import { SettingsRow } from "../../components/SettingsRow";
import { t, tx } from "../../i18n";
import { api } from "../../lib/api";
import { useAsync } from "../../lib/hooks";

type Draft = { theIntroDb: boolean; skipIntro: boolean; skipCredits: boolean };

/** Intro and credits skip buttons, plus the named places those times come from. */
export function SkipButtons({ hidden = false }: { hidden?: boolean }) {
  const { refreshDashboard } = useApp();
  const config = useAsync(() => api.getConfig(), []);
  const cfg = config.data;
  const saved = useMemo<Draft | null>(() => {
    if (!cfg) return null;
    const markers = cfg.markers;
    return {
      theIntroDb: Boolean(markers?.theIntroDb),
      skipIntro: markers?.skipIntro !== false,
      skipCredits: markers?.skipCredits !== false,
    };
  }, [cfg]);
  const { draft, setDraft, dirty, reset } = useDraft(saved);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<unknown>(null);
  const apiKey = cfg?.markers?.apiKey;

  async function submit() {
    if (!draft) return;
    setSaving(true);
    setError(null);
    try {
      await api.putSection("markers", draft);
      await config.reload();
      void refreshDashboard();
    } catch (e) {
      setError(e);
    } finally {
      setSaving(false);
    }
  }

  const saveBar = (
    <SaveBar
      dirty={dirty}
      saving={saving}
      error={error}
      onSave={() => void submit()}
      onDiscard={() => {
        reset();
        setError(null);
      }}
    />
  );
  if (hidden) return saveBar;
  if (config.error) return <ErrorPanel error={config.error} onRetry={() => void config.reload()} />;
  if (!draft) return <LoadingRows rows={3} />;

  return (
    <>
      <SettingsSection
        title={t("Skip buttons")}
        description={t("Each switch turns that button on or off. The app still has to show it.")}
      >
        <SettingsRow
          title={t("Skip intro")}
          description={t("Shows a button to skip the opening and a recap.")}
          control={
            <Switch
              checked={draft.skipIntro}
              aria-label={t("Skip intro")}
              onChange={(e) => setDraft({ ...draft, skipIntro: e.currentTarget.checked })}
            />
          }
        />
        <SettingsRow
          title={t("Skip credits")}
          description={t("Shows a button to skip the credits.")}
          control={
            <Switch
              checked={draft.skipCredits}
              aria-label={t("Skip credits")}
              onChange={(e) => setDraft({ ...draft, skipCredits: e.currentTarget.checked })}
            />
          }
        />
      </SettingsSection>
      <SettingsSection title={t("Where the times come from")}>
        <SettingsRow
          title={t("Intro and credit times from IntroDB.")}
          description={
            <>
              {t("Ether asks IntroDB once per episode and remembers the answer.")}{" "}
              {secretConfigured(apiKey)
                ? null
                : tx("Add an IntroDB key in {link}.", {
                    link: (
                      <Link to="/services/introdb" className="text-primary underline">
                        {t("Integrations")}
                      </Link>
                    ),
                  })}
            </>
          }
          control={
            <Switch
              checked={draft.theIntroDb}
              aria-label={t("Look up IntroDB")}
              onChange={(e) => setDraft({ ...draft, theIntroDb: e.currentTarget.checked })}
            />
          }
        />
        <SettingsRow
          title={t("Chapter titles")}
          description={t(
            "A chapter named intro, opening, credits, or ending supplies the skip range.",
          )}
        />
        <SettingsRow
          title={t("Subtitle dialogue")}
          description={t(
            "Dialogue in a downloaded subtitle keeps a scene in the credits from being skipped.",
          )}
        />
      </SettingsSection>
      {saveBar}
    </>
  );
}
