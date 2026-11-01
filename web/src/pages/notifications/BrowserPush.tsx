import { t } from "../../i18n";
import { Button, toast } from "@braedonsaunders/appkit-ui";
import { useState } from "react";
import { BellRing } from "lucide-react";
import { Section } from "../../components/layout";
import { apiFetch, errorMessage } from "../../lib/api";
import { blankChannel, type NotificationsData } from "./types";

export function BrowserPush({
  data,
  onChanged,
}: {
  data: NotificationsData;
  onChanged: () => Promise<void>;
}) {
  const [busy, setBusy] = useState(false);
  const [message, setMessage] = useState("");
  const supported =
    window.isSecureContext &&
    "Notification" in window &&
    "serviceWorker" in navigator &&
    "PushManager" in window;
  const blocked = "Notification" in window && Notification.permission === "denied";
  const connect = async () => {
    setBusy(true);
    setMessage("");
    let created: PushSubscription | null = null;
    let saved = false;
    try {
      const permission = await Notification.requestPermission();
      if (permission !== "granted")
        throw new Error(
          t("Allow notifications in this site’s browser settings, then connect again."),
        );
      const { publicKey } = await apiFetch<{ publicKey: string }>("/notifications/push/key");
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
      const hash = new Uint8Array(
        await crypto.subtle.digest("SHA-256", new TextEncoder().encode(subscription.endpoint)),
      );
      const id = `browser-${Array.from(hash)
        .map((b) => b.toString(16).padStart(2, "0"))
        .join("")}`;
      const previous = data.channels.find((c) => c.id === id);
      const channel = {
        ...(previous ?? blankChannel()),
        id,
        kind: "webpush",
        name:
          previous?.name ||
          (navigator.platform
            ? t("Browser · {platform}", { platform: navigator.platform })
            : t("Browser · this device")),
        enabled: true,
        subscription: subscription.toJSON(),
      };
      await apiFetch(`/notifications/${id}`, { method: "PUT", body: JSON.stringify(channel) });
      saved = true;
      setMessage(
        t(
          "This device is connected. Edit its Browser push channel to choose events, or send a test to verify delivery.",
        ),
      );
      toast.success(t("Device connected"));
      await onChanged();
    } catch (e) {
      if (created && !saved) await created.unsubscribe().catch(() => false);
      setMessage(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };
  return (
    <Section
      title={t("Browser push")}
      description={t("Receive alerts on this device even when the dashboard is closed.")}
    >
      <div className="flex flex-wrap items-start justify-between gap-4">
        <div className="flex max-w-2xl gap-3 text-sm text-fg-muted">
          <BellRing size={20} className="shrink-0" />
          <div className="space-y-2">
            <p>
              {!supported
                ? t(
                    "Browser push needs HTTPS (or localhost), a supported browser and notification permission. On iPhone or iPad, install Ether on your Home Screen and open it there.",
                  )
                : blocked
                  ? t(
                      "Notifications are blocked for this site. Allow them in your browser’s site settings, then reconnect.",
                    )
                  : t("Connect each phone, tablet or computer separately.")}
            </p>
            {message ? (
              <p role="status" className="text-fg">
                {message}
              </p>
            ) : null}
          </div>
        </div>
        <Button disabled={!supported || blocked || busy} onClick={() => void connect()}>
          {busy ? t("Connecting…") : t("Connect this device")}
        </Button>
      </div>
    </Section>
  );
}
