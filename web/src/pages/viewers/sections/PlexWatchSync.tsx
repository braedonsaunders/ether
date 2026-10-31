import { Button, SettingsSection, Spinner, Switch, toast } from "@braedonsaunders/appkit-ui";
import { useState } from "react";
import { ErrorPanel } from "../../../components/feedback";
import { SettingsRow } from "../../../components/SettingsRow";
import { t } from "../../../i18n";
import { apiFetch, errorMessage } from "../../../lib/api";
import { useAsync } from "../../../lib/hooks";

type PlexWatchStatus = {
  connected: boolean;
  plexId?: number;
  username?: string;
  enabled: boolean;
  eligibility: string;
  revoked: boolean;
  pending: number;
  dead: number;
  inFlight: boolean;
  skipped: number;
  restricted: boolean;
  pinCode?: string;
  pinUrl?: string;
  pinExpires?: string;
  accountMatch: boolean;
};

function eligibilityCopy(value: string) {
  switch (value) {
    case "allowed":
      return t("Plex allows watch-state sync for this account.");
    case "refused":
      return t("Plex has not allowed watch-state sync for this account.");
    case "unauthorized":
      return t("Plex rejected the saved token. Connect again.");
    default:
      return t("Ether has not confirmed whether Plex allows watch-state sync for this account.");
  }
}

/** Per-viewer Plex account watch state, on Limits & preferences. */
export function PlexWatchSync({ viewerId, plexId }: { viewerId: number; plexId: number }) {
  const status = useAsync(
    (signal) => apiFetch<PlexWatchStatus>(`/viewers/${viewerId}/plex-watch`, { signal }),
    [viewerId],
  );
  const [busy, setBusy] = useState(false);

  const run = async (path: string, method: string, body?: unknown) => {
    setBusy(true);
    try {
      const next = await apiFetch<PlexWatchStatus>(`/viewers/${viewerId}/plex-watch${path}`, {
        method,
        body: body === undefined ? undefined : JSON.stringify(body),
      });
      status.setData(next);
    } catch (e) {
      toast.error(t("Plex watch state was not updated"), { description: errorMessage(e) });
    } finally {
      setBusy(false);
    }
  };

  if (plexId <= 0) {
    return (
      <SettingsSection
        title={t("Plex account watch state")}
        description={t("This viewer has no Plex account id, so account sync cannot be connected.")}
      >
        {null}
      </SettingsSection>
    );
  }
  if (status.error) return <ErrorPanel error={status.error} onRetry={() => void status.reload()} />;
  if (!status.data) {
    return (
      <div className="py-6 text-center">
        <Spinner size={18} label={t("Loading Plex watch state")} />
      </div>
    );
  }

  const st = status.data;
  return (
    <SettingsSection
      title={t("Plex account watch state")}
      description={t(
        "Optional. Sends this viewer's future watched and unwatched marks, and whole-star ratings, to their own Plex account. Playback progress stays on this server.",
      )}
    >
      <p className="text-sm text-muted">
        {t(
          "Ether sends only watched or unwatched marks and ratings of 1, 2, 3, 4, or 5 whole stars. Clears, fractions, and other values stay here. Episodes are sent only when an exact Plex episode id is already known. A successful send does not create a Discover Activity card. Ether does not turn on Plex's watch-state setting. Ether does not use server or shared-account tokens for this.",
        )}
      </p>
      <p className="text-sm text-muted">
        {t(
          "This is not continuous two-way sync. Ether delivers future changes in the background only while sync is on. Import runs only when you use the button.",
        )}
      </p>
      <SettingsRow
        title={t("Connection")}
        description={
          st.connected
            ? t(
                "This viewer's own Plex account is connected. Sync sends future changes only until you ask for an export.",
              )
            : t(
                "The person who approves the code must be this viewer. Connecting a different Plex account, including the owner, is rejected.",
              )
        }
        control={
          st.connected ? (
            <Button disabled={busy} onClick={() => void run("/disconnect", "POST")}>
              {t("Disconnect")}
            </Button>
          ) : (
            <Button disabled={busy} onClick={() => void run("/pin", "POST")}>
              {t("Connect this viewer's Plex account")}
            </Button>
          )
        }
      />
      {st.pinCode ? (
        <SettingsRow
          title={t("Approval code")}
          description={t(
            "Approve this code while signed in as this viewer. Then check approval. The code is not a password Ether stores in the page after it expires.",
          )}
          control={
            <div className="flex items-center gap-2">
              <code>{st.pinCode}</code>
              <Button disabled={busy} onClick={() => void run("/pin/finish", "POST")}>
                {t("Check approval")}
              </Button>
            </div>
          }
        />
      ) : null}
      {st.pinUrl ? (
        <SettingsRow
          title={t("Approve on plex.tv")}
          control={
            <a href={st.pinUrl} target="_blank" rel="noreferrer">
              {t("Open the approval page")}
            </a>
          }
        />
      ) : null}
      <SettingsRow
        title={t("Send future changes")}
        description={t(
          "Off by default. Turning this on does not upload existing history. Playback continues if Plex is unreachable.",
        )}
        control={
          <Switch
            aria-label={t("Send future changes")}
            checked={st.enabled}
            disabled={busy || !st.connected}
            onChange={(e) => void run("", "PUT", { enabled: e.currentTarget.checked })}
          />
        }
      />
      <SettingsRow
        title={t("Plex eligibility")}
        description={t(
          "Allowed means Plex answered that this account may sync. Refused leaves sync off. Unchecked means Ether has not confirmed it.",
        )}
      />
      <p className="text-sm">
        {st.revoked
          ? t("Plex rejected the saved token. Connect again.")
          : eligibilityCopy(st.eligibility)}
      </p>
      {st.inFlight ? (
        <p className="text-sm">
          {t(
            "A send is still in progress. Turning sync off stops queued changes. An update already being sent may still finish.",
          )}
        </p>
      ) : null}
      {st.skipped > 0 ? (
        <p className="text-sm">
          {t(
            "Some titles from Plex could not be matched yet. Retry checks them again. A database error does not skip ahead in the import.",
          )}
        </p>
      ) : null}
      {(st.pending > 0 || st.dead > 0 || st.skipped > 0) && (
        <SettingsRow
          title={t("Delivery")}
          description={t("Failed items stay queued until you retry them or disconnect.")}
          control={
            <Button disabled={busy || !st.connected} onClick={() => void run("/retry", "POST")}>
              {t("Retry failed items")}
            </Button>
          }
        />
      )}
      <details>
        <summary>{t("Import or export history")}</summary>
        <p className="text-sm text-muted">
          {t(
            "Import copies Plex's current watched marks and whole-star ratings onto this viewer when they are newer. Export queues this viewer's current watched marks and whole-star ratings. Neither sends playback progress, and neither runs until you use these buttons.",
          )}
        </p>
        <div className="flex gap-2">
          <Button disabled={busy || !st.connected} onClick={() => void run("/import", "POST")}>
            {t("Import from Plex")}
          </Button>
          <Button
            disabled={busy || !st.connected || !st.enabled}
            onClick={() => void run("/export", "POST")}
          >
            {t("Export current state")}
          </Button>
        </div>
      </details>
    </SettingsSection>
  );
}
