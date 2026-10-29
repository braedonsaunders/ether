import { Button } from "@braedonsaunders/appkit-ui";
import { useCallback, useEffect, useState } from "react";
import { t } from "../../i18n";

type Props = {
  getKey: () => Promise<{ publicKey: string }>;
  status: (endpoint: string) => Promise<{ registered: boolean }>;
  subscribe: (subscription: PushSubscriptionJSON) => Promise<unknown>;
  remove: (endpoint: string) => Promise<unknown>;
};

/** Viewer-owned push registration; it never removes another user's subscription. */
export function ViewerPush({ getKey, status, subscribe, remove }: Props) {
  const supported =
    window.isSecureContext &&
    "Notification" in window &&
    "serviceWorker" in navigator &&
    "PushManager" in window;
  const [registered, setRegistered] = useState(false);
  const [loading, setLoading] = useState(supported);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState(false);

  const existing = useCallback(async () => {
    const registration = await navigator.serviceWorker.getRegistration("/ether/");
    return (await registration?.pushManager.getSubscription()) ?? null;
  }, []);
  useEffect(() => {
    if (!supported) return;
    let active = true;
    setLoading(true);
    setRegistered(false);
    setError(false);
    void existing()
      .then(async (subscription) => {
        const registered = subscription ? (await status(subscription.endpoint)).registered : false;
        if (active) setRegistered(registered);
      })
      .catch(() => {
        if (active) setError(true);
      })
      .finally(() => {
        if (active) setLoading(false);
      });
    return () => {
      active = false;
    };
  }, [existing, status, supported]);

  async function change() {
    setBusy(true);
    setError(false);
    let created: PushSubscription | null = null;
    try {
      if (registered) {
        const subscription = await existing();
        if (!subscription) throw new Error("missing subscription");
        await remove(subscription.endpoint);
        // The browser subscription can also belong to another viewer or the owner.
        setRegistered(false);
      } else {
        if ((await Notification.requestPermission()) !== "granted")
          throw new Error("notification permission denied");
        const { publicKey } = await getKey();
        await navigator.serviceWorker.register("/ether/service-worker.js", { scope: "/ether/" });
        const registration = await navigator.serviceWorker.ready;
        let subscription = await registration.pushManager.getSubscription();
        if (!subscription) {
          const key = Uint8Array.from(atob(publicKey.replace(/-/g, "+").replace(/_/g, "/")), (c) =>
            c.charCodeAt(0),
          );
          subscription = await registration.pushManager.subscribe({
            userVisibleOnly: true,
            applicationServerKey: key,
          });
          created = subscription;
        }
        await subscribe(subscription.toJSON());
        setRegistered(true);
      }
    } catch {
      if (created) await created.unsubscribe().catch(() => false);
      setError(true);
    } finally {
      setBusy(false);
    }
  }

  return (
    <section className="space-y-2">
      <h2 className="font-medium">{t("Browser push")}</h2>
      <p className="text-sm text-fg-muted">
        {supported
          ? t(
              "Connect each phone, tablet or computer separately. Your browser’s push service delivers the alerts; operating-system permissions and Focus settings control how they appear.",
            )
          : t(
              "Browser push needs HTTPS (or localhost), a supported browser and notification permission. On iPhone or iPad, install Ether on your Home Screen and open it there.",
            )}
      </p>
      <Button
        variant="outline"
        type="button"
        disabled={!supported || loading || busy}
        onClick={() => void change()}
      >
        {registered ? t("Disconnect this device") : t("Connect this device")}
      </Button>
      {error && <p role="alert">{t("The request could not be completed. Try again.")}</p>}
    </section>
  );
}
