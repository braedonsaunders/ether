import { Badge, Switch } from "@braedonsaunders/appkit-ui";
import { useEffect, useState } from "react";
import { t } from "../../i18n";

type Props = {
  load: () => Promise<{ enabled: boolean }>;
  save?: (enabled: boolean) => Promise<unknown>;
};

/** A viewer's opt-in; loading and failed writes never change the saved value. */
export function FavoriteEpisodeAlerts({ load, save }: Props) {
  const [enabled, setEnabled] = useState<boolean>();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState(false);

  useEffect(() => {
    let active = true;
    setEnabled(undefined);
    setError(false);
    void load()
      .then((value) => {
        if (active) setEnabled(value.enabled);
      })
      .catch(() => {
        if (active) setError(true);
      });
    return () => {
      active = false;
    };
  }, [load]);

  async function change() {
    if (enabled === undefined || busy || !save) return;
    setBusy(true);
    setError(false);
    try {
      await save(!enabled);
      setEnabled(!enabled);
    } catch {
      setError(true);
    } finally {
      setBusy(false);
    }
  }

  return (
    <section className="space-y-2">
      <h2 className="font-medium">{t("Alerts")}</h2>
      {save ? (
        <label className="flex items-center gap-2 text-sm">
          <Switch
            checked={enabled ?? false}
            disabled={enabled === undefined || busy}
            onChange={() => void change()}
          />
          {t("New episodes from favourite shows")}
        </label>
      ) : (
        // Read-only for the owner: this is the viewer's own opt-in, so show
        // it as status rather than a switch that can never move.
        <p className="flex items-center gap-2 text-sm">
          {t("New episodes from favourite shows")}
          {enabled !== undefined ? (
            <Badge variant={enabled ? "success" : "default"}>{enabled ? t("On") : t("Off")}</Badge>
          ) : null}
        </p>
      )}
      <p className="text-sm text-fg-muted">
        {t("Get alerts about new episodes of favourite shows.")}
      </p>
      {!save && (
        <p className="text-sm text-fg-muted">
          {t("The viewer can change this setting in their profile.")}
        </p>
      )}
      {error && <p role="alert">{t("The request could not be completed. Try again.")}</p>}
    </section>
  );
}
