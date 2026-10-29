import { t, tx } from "../../i18n";
import { Badge, Button, Switch, toast } from "@braedonsaunders/appkit-ui";
import { useId, useState } from "react";
import { Link } from "react-router-dom";
import { Section } from "../../components/layout";
import { Field, SecretInput } from "../../components/fields";
import { ErrorPanel, LoadingRows } from "../../components/feedback";
import { apiFetch, errorMessage } from "../../lib/api";
import { useAsync } from "../../lib/hooks";

type Heartbeat = { enabled: boolean; endpoint: string };
export function Protection({ active }: { active?: boolean }) {
  const id = useId();
  const settings = useAsync(() => apiFetch<Heartbeat>("/notifications/heartbeat"), []);
  const [draft, setDraft] = useState<Heartbeat | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const value = draft ?? settings.data;
  const save = async (test = false) => {
    if (!value) return;
    setBusy(true);
    setError("");
    try {
      await apiFetch("/notifications/heartbeat", { method: "PUT", body: JSON.stringify(value) });
      if (test) await apiFetch("/notifications/heartbeat/test", { method: "POST", body: "{}" });
      toast.success(test ? t("Heartbeat accepted") : t("Heartbeat settings saved"));
      setDraft(null);
      await settings.reload();
    } catch (e) {
      setError(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };
  return (
    <>
      <Section
        title={t("Crash watcher")}
        description={t("Send an alert after the server process dies.")}
      >
        <div className="space-y-3 text-sm text-fg-muted">
          <Badge variant={active ? "success" : "secondary"}>
            {active ? t("Watcher running on this node") : t("Watcher unavailable on this node")}
          </Badge>
          <p>
            {tx(
              "The Ether executable starts an independent watcher. If Ether exits unexpectedly, including a fatal panic or process fault, the watcher sends directly to enabled channels with {event} selected. It keeps a private copy of those destinations in memory, so delivery does not depend on the failed process or its database. Digest delays and delivery caps do not delay crash alerts.",
              { event: <strong>{t("Server crashed")}</strong> },
            )}
          </p>
          <p>
            {t(
              "A normal stop or restart does not trigger a crash alert. Crash-watcher deliveries happen outside Delivery history. Delivery is best effort: the watcher must stay alive and have network access. A supervisor that kills the entire process group can stop it too.",
            )}
          </p>
          <Link to="/notifications/channels" className="font-medium text-fg underline">
            {t("Choose crash alert destinations")}
          </Link>
        </div>
      </Section>
      <Section
        title={t("Whole-server outage detection")}
        description={t("Let an independent service alert you when Ether cannot send anything.")}
      >
        <div className="space-y-4">
          <p className="text-sm text-fg-muted">
            {t(
              "Power failure, a dead machine or lost internet cannot reliably send a last-second message. Configure a dead-man heartbeat in an external monitoring service. Ether posts to its secret HTTPS ping URL once a minute, after its local HTTP health check succeeds. Set the monitor to expect a one-minute interval with at least three minutes of grace, and configure its own email or phone alerts. In a cluster, this shared heartbeat continues while any node remains responsive; monitor each node externally to detect individual failures.",
            )}
          </p>
          {settings.error && !value ? (
            <ErrorPanel error={settings.error} onRetry={() => void settings.reload()} />
          ) : !value ? (
            <LoadingRows rows={2} />
          ) : (
            <>
              <label className="flex items-center justify-between gap-3 text-sm font-medium text-fg">
                {t("Send uptime heartbeat")}
                <Switch
                  aria-label={t("Send uptime heartbeat")}
                  checked={value.enabled}
                  onChange={(e) => setDraft({ ...value, enabled: e.target.checked })}
                />
              </label>
              <Field
                label={t("Secret heartbeat URL")}
                htmlFor={`${id}-heartbeat`}
                hint={t("Kept private. Leave [configured] unchanged to retain the saved URL.")}
              >
                <SecretInput
                  id={`${id}-heartbeat`}
                  value={value.endpoint}
                  onChange={(endpoint) => setDraft({ ...value, endpoint })}
                />
              </Field>
              {error ? (
                <p role="alert" className="text-sm text-danger">
                  {error}
                </p>
              ) : null}
              <div className="flex flex-wrap gap-2">
                <Button disabled={busy} onClick={() => void save()}>
                  {t("Save heartbeat")}
                </Button>
                <Button
                  variant="outline"
                  disabled={busy || !value.endpoint}
                  onClick={() => void save(true)}
                >
                  {t("Save and send test ping")}
                </Button>
              </div>
            </>
          )}
        </div>
      </Section>
    </>
  );
}
