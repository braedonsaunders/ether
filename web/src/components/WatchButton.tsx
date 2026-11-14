import { useEffect, useState } from "react";
import { Tv } from "lucide-react";
import { apiFetch } from "../lib/api";
import { t } from "../i18n";

export function WatchButton({ app }: { app?: "vela" } = {}) {
  const endpoint = app ? `/web-player?app=${app}` : "/web-player";
  const [available, setAvailable] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState(false);
  useEffect(() => {
    let active = true;
    const check = () =>
      apiFetch<{ available: boolean }>(endpoint)
        .then((r) => {
          if (active) setAvailable(r.available);
        })
        .catch(() => {
          if (active) setAvailable(false);
        });
    void check();
    window.addEventListener("focus", check);
    return () => {
      active = false;
      window.removeEventListener("focus", check);
    };
  }, [endpoint]);
  if (!available) return null;
  return (
    <button
      type="button"
      disabled={busy}
      className="inline-flex h-9 items-center gap-2 rounded-md px-3 text-sm font-medium hover:bg-bg-muted focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/50 disabled:opacity-50"
      title={error ? t("Could not open the web player. Try again.") : t("Open the web player")}
      aria-label={t("Open the web player")}
      onClick={async () => {
        const tab = window.open("about:blank", "_blank");
        if (!tab) {
          setError(true);
          return;
        }
        tab.opener = null;
        setBusy(true);
        setError(false);
        try {
          const result = await apiFetch<{ url: string }>(endpoint, {
            method: "POST",
            body: "{}",
          });
          const target = new URL(result.url);
          const args = new URLSearchParams(target.hash.slice(1));
          args.set("console", window.location.origin + "/ether/");
          target.hash = args.toString();
          tab.location.replace(target.href);
        } catch {
          tab.close();
          setError(true);
        } finally {
          setBusy(false);
        }
      }}
    >
      <Tv size={16} aria-hidden />
      {t("Watch")}
    </button>
  );
}
