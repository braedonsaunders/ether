import { Switch } from "@braedonsaunders/appkit-ui";
import { useEffect, useState } from "react";
import { t } from "../../i18n";

type Props = {
  load: () => Promise<{ enabled: boolean }>;
  save: (enabled: boolean) => Promise<unknown>;
};
export function FavoritesMirror({ load, save }: Props) {
  const [enabled, setEnabled] = useState<boolean>();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState(false);
  useEffect(() => {
    let active = true;
    void load()
      .then((v) => {
        if (active) setEnabled(v.enabled);
      })
      .catch(() => {
        if (active) setError(true);
      });
    return () => {
      active = false;
    };
  }, [load]);
  async function change() {
    if (enabled === undefined) return;
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
      <h2 className="font-medium">{t("Favorites in Plex")}</h2>
      <label className="flex items-center gap-2 text-sm">
        <Switch
          checked={enabled ?? false}
          disabled={enabled === undefined || busy}
          onChange={() => void change()}
        />
        {t("Show my favourites as a playlist in Plex.")}
      </label>
      {enabled === false && (
        <p className="text-sm text-fg-muted">{t("Your favourites are kept when this is off.")}</p>
      )}
      {error && <p role="alert">{t("The request could not be completed. Try again.")}</p>}
    </section>
  );
}
