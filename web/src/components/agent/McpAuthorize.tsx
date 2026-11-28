import { Button, Spinner } from "@braedonsaunders/appkit-ui";
import { useEffect, useState } from "react";
import { useSearchParams } from "react-router-dom";
import { t } from "../../i18n";
import { apiFetch } from "../../lib/api";
import { ErrorPanel } from "../feedback";

type AuthorizationDetails = {
  name: string;
  scope: string;
  redirectHost?: string;
  redirectLocal?: boolean;
};

export function McpAuthorize() {
  const [params] = useSearchParams();
  const request = params.get("request") ?? "";
  const [details, setDetails] = useState<AuthorizationDetails | null>(null);
  const [error, setError] = useState<unknown>(null);
  const [busy, setBusy] = useState(false);
  useEffect(() => {
    let active = true;
    apiFetch<AuthorizationDetails>(
      `/agent/mcp/authorization?request=${encodeURIComponent(request)}`,
    )
      .then((value) => {
        if (active) setDetails(value);
      })
      .catch((err: unknown) => {
        if (active) setError(err);
      });
    return () => {
      active = false;
    };
  }, [request]);
  const decide = async (approve: boolean) => {
    setBusy(true);
    setError(null);
    try {
      const result = await apiFetch<{ redirect: string }>("/agent/mcp/authorization", {
        method: "POST",
        body: JSON.stringify({ request, approve }),
      });
      window.location.assign(result.redirect);
    } catch (err) {
      setError(err);
      setBusy(false);
    }
  };
  return (
    <div className="mx-auto max-w-xl space-y-5 p-6">
      <h1 className="text-2xl font-semibold text-fg">{t("Connect an AI app")}</h1>
      {error ? <ErrorPanel error={error} /> : null}
      {details ? (
        <div className="space-y-4 rounded-xl border border-border bg-bg-subtle p-5">
          <p className="text-sm text-fg">
            {t("{name} wants to connect to Ether.", { name: details.name })}
          </p>
          <p className="text-sm text-fg-muted">
            {details.scope === "read" ? t("Access: read") : t("Access: full")}
          </p>
          {details.redirectHost ? (
            <div
              className={
                details.redirectLocal
                  ? "rounded-lg border border-border bg-bg px-3 py-2 text-xs text-fg-muted"
                  : "rounded-lg border border-amber-500/40 bg-amber-500/10 px-3 py-2 text-xs text-fg"
              }
            >
              <div>
                {t("Redirects to: {host}", { host: details.redirectHost })}
              </div>
              {!details.redirectLocal ? (
                <div className="mt-1 text-fg-muted">
                  {t("External host: make sure you recognize this address before approving.")}
                </div>
              ) : null}
            </div>
          ) : null}
          <p className="text-sm text-fg-muted">
            {t("Changes to your server still need your approval.")}
          </p>
          <div className="flex gap-3">
            <Button disabled={busy} onClick={() => void decide(true)}>
              {t("Approve")}
            </Button>
            <Button variant="outline" disabled={busy} onClick={() => void decide(false)}>
              {t("Deny")}
            </Button>
          </div>
        </div>
      ) : !error ? (
        <Spinner size={20} label={t("Loading request")} />
      ) : null}
    </div>
  );
}
