import { t } from "../../i18n";
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
          <p>{t("Sends Server crashed if Ether exits unexpectedly. A normal stop does not.")}</p>
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
            {t("Ether pings this URL once a minute while it is up.")}
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
