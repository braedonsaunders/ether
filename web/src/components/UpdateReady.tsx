import { Button } from "@braedonsaunders/appkit-ui";
import { useEffect, useRef, useState } from "react";
import { t } from "../i18n";

const POLL_MS = 20_000;

/**
 * A new server build used to reload the page as soon as it was noticed.
 * That wiped a code the owner was typing. The page stays put and this banner
 * waits until they choose to reload — including while a field is focused or
 * a form is dirty.
 */
export function UpdateReady() {
  const [ready, setReady] = useState(false);
  const version = useRef<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    const tick = async () => {
      try {
        const res = await fetch("/ether/api/bootstrap");
        if (!res.ok || cancelled) return;
        const boot = (await res.json()) as { version?: string };
        const next = boot.version ?? "";
        if (!next || cancelled) return;
        if (version.current == null) {
          version.current = next;
          return;
        }
        if (next !== version.current) setReady(true);
      } catch {
        // The server is restarting. Leave the page alone until it answers.
      }
    };
    void tick();
    const id = window.setInterval(() => {
      if (document.visibilityState !== "hidden") void tick();
    }, POLL_MS);
    const onPreload = (event: Event) => {
      event.preventDefault();
      setReady(true);
    };
    window.addEventListener("vite:preloadError", onPreload);
    return () => {
      cancelled = true;
      window.clearInterval(id);
      window.removeEventListener("vite:preloadError", onPreload);
    };
  }, []);

  if (!ready) return null;
  return (
    <div
      role="status"
      className="fixed inset-x-0 top-0 z-50 flex flex-wrap items-center justify-center gap-3 border-b border-border bg-elevated px-4 py-2 text-sm text-fg shadow-md"
    >
      <p>{t("Ether was updated — reload when ready")}</p>
      <Button type="button" size="sm" onClick={() => window.location.reload()}>
        {t("Reload")}
      </Button>
    </div>
  );
}
