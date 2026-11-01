import { Button } from "@braedonsaunders/appkit-ui";
import { useCallback, useEffect, useState } from "react";
import { t } from "../../i18n";

export type NotInterestedEntry = {
  handle: string;
  unavailable: boolean;
  title?: string;
  year?: number;
};

type Props = {
  load: () => Promise<NotInterestedEntry[]>;
  restore: (handle: string) => Promise<unknown>;
};

/** Restore actions use opaque handles, including for entries no longer visible. */
export function NotInterested({ load, restore }: Props) {
  const [entries, setEntries] = useState<NotInterestedEntry[]>();
  const [error, setError] = useState(false);
  const [busy, setBusy] = useState<string>();
  const [attempt, setAttempt] = useState(0);
  useEffect(() => {
    let active = true;
    setEntries(undefined);
    setError(false);
    void load()
      .then((items) => {
        if (active) setEntries(items);
      })
      .catch(() => {
        if (active) setError(true);
      });
    return () => {
      active = false;
    };
  }, [load, attempt]);

  const show = useCallback(
    async (handle: string) => {
      if (busy) return;
      setBusy(handle);
      setError(false);
      try {
        await restore(handle);
        setEntries((items) => items?.filter((item) => item.handle !== handle));
      } catch {
        setError(true);
      } finally {
        setBusy(undefined);
      }
    },
    [busy, restore],
  );

  return (
    <section className="space-y-2">
      <h2 className="font-medium">{t("Not interested")}</h2>
      <p className="text-sm text-fg-muted">
        {t("Titles you have hidden from recommendations appear here.")}
      </p>
      {error && <p role="alert">{t("The request could not be completed. Try again.")}</p>}
      {!entries && !error && <p role="status">{t("Loading…")}</p>}
      {!entries && error && (
        <Button type="button" variant="outline" onClick={() => setAttempt((n) => n + 1)}>
          {t("Retry")}
        </Button>
      )}
      {entries?.length === 0 && (
        <p className="text-sm text-fg-muted">{t("No hidden recommendations.")}</p>
      )}
      {entries && entries.length > 0 && (
        <ul className="divide-y divide-border">
          {entries.map((entry) => {
            const label = entry.unavailable || !entry.title ? t("Unavailable title") : entry.title;
            return (
              <li
                key={entry.handle}
                className="flex flex-wrap items-center justify-between gap-3 py-2"
              >
                <span className="text-sm">
                  {label}
                  {!entry.unavailable && entry.year ? ` (${entry.year})` : ""}
                </span>
                <Button
                  type="button"
                  size="sm"
                  variant="outline"
                  disabled={busy !== undefined}
                  aria-label={t("Show {title} in recommendations", { title: label })}
                  onClick={() => void show(entry.handle)}
                >
                  {t("Show in recommendations")}
                </Button>
              </li>
            );
          })}
        </ul>
      )}
    </section>
  );
}
