import {
  Button,
  SettingsSection,
  Spinner,
  Select,
  Tooltip,
  toast,
} from "@braedonsaunders/appkit-ui";
import { AlertTriangle, CheckCircle2 } from "lucide-react";
import { useState } from "react";
import { ErrorPanel } from "../../../components/feedback";
import { SettingsRow } from "../../../components/SettingsRow";
import { localized, t, tn, tx } from "../../../i18n";
import { apiFetch, errorMessage } from "../../../lib/api";
import { formatDateTime } from "../../../lib/format";
import { useAsync } from "../../../lib/hooks";

type PlexWatchStatus = {
  connected: boolean;
  plexId?: number;
  username?: string;
  direction: number;
  importInterval: number;
  lastImport: number;
  lastFullImport: number;
  lastSend: number;
  nextImport: number;
  problem: string;
  needsReconnect: boolean;
  pending: number;
  failed: number;
  restricted: boolean;
  pinCode?: string;
  pinUrl?: string;
  pinExpires?: string;
  accountMatch: boolean;
};

const SENDS = 1;
const READS = 2;

/** What the server's problem codes mean to the owner. */
const PROBLEMS = localized<Record<string, string>>(() => ({
  token: t("Plex no longer accepts this connection. Reconnect the account."),
  consent: t(
    "Watch sync is turned off in this viewer's Plex account, so Ether sends and reads nothing. To turn it on, sign in to Plex as this viewer, open Account settings and switch on “Sync My Watch State and Ratings”. Sync then starts on its own and waiting changes are kept.",
  ),
  rate_limited: t("Plex asked Ether to slow down. Sync resumes on its own."),
  network: t("Plex could not be reached. Ether will try again."),
}));

/** The Plex approval page, only when it is Plex's own HTTPS page. */
function approvalUrl(raw?: string): string {
  if (!raw) return "";
  try {
    const u = new URL(raw);
    return u.protocol === "https:" && u.hostname === "app.plex.tv" ? u.href : "";
  } catch {
    return "";
  }
}

/** One viewer's Plex account watch sync, on their Limits tab. */
export function PlexWatchSync({ viewerId, plexId }: { viewerId: number; plexId: number }) {
  const status = useAsync(
    (signal) => apiFetch<PlexWatchStatus>(`/viewers/${viewerId}/plex-watch`, { signal }),
    [viewerId],
  );
  const [busy, setBusy] = useState(false);

  const run = async (path: string, method: string, body?: unknown, done?: string) => {
    setBusy(true);
    try {
      const next = await apiFetch<PlexWatchStatus>(`/viewers/${viewerId}/plex-watch${path}`, {
        method,
        body: body === undefined ? undefined : JSON.stringify(body),
      });
      status.setData(next);
      if (done) toast.success(done);
    } catch (e) {
      toast.error(t("Plex watch sync was not updated"), { description: errorMessage(e) });
      void status.reload();
    } finally {
      setBusy(false);
    }
  };

  if (plexId <= 0) return null;
  if (status.error) {
    return <ErrorPanel error={status.error} onRetry={() => void status.reload()} />;
  }
  if (!status.data) {
    return (
      <div className="py-6 text-center">
        <Spinner size={18} label={t("Loading Plex watch sync")} />
      </div>
    );
  }

  const st = status.data;
  const reconnect = st.needsReconnect && st.accountMatch;
  const approval = approvalUrl(st.pinUrl);
  const date = (seconds: number) =>
    seconds ? formatDateTime(new Date(seconds * 1000)) : t("Never");
  const setDirection = (direction: number, interval: number) =>
    void run("", "PUT", { direction, interval });

  return (
    <SettingsSection title={t("Plex account watch sync")}>
      <SettingsRow
        title={
          <span className="inline-flex items-center gap-2">
            {t("Plex account")}
            <Tooltip
              content={t(
                "Keeps watched marks, resume positions and ratings in step with this viewer's own Plex account.",
              )}
            >
              <button
                type="button"
                aria-label={t("About Plex watch sync")}
                className="text-fg-muted"
              >
                ?
              </button>
            </Tooltip>
          </span>
        }
        description={
          st.connected
            ? st.username
              ? tx("Connected as {name}", { name: <strong>{st.username}</strong> })
              : t("Connected")
            : reconnect
              ? t("Reconnect required")
              : t("Not connected")
        }
        control={
          <div className="flex flex-wrap items-center gap-2">
            {!st.connected && !st.pinCode ? (
              <Button disabled={busy} onClick={() => void run("/pin", "POST")}>
                {reconnect ? t("Reconnect") : t("Connect")}
              </Button>
            ) : null}
            {st.connected || reconnect ? (
              <Button
                variant="outline"
                disabled={busy}
                onClick={() =>
                  void run("/disconnect", "POST", undefined, t("Plex account disconnected."))
                }
              >
                {t("Disconnect")}
              </Button>
            ) : null}
          </div>
        }
      />
      {st.pinCode ? (
        <SettingsRow
          title={t("Approval code")}
          description={t("Sign in to Plex as this viewer to approve Ether.")}
          control={
            <div className="flex flex-wrap items-center gap-2">
              {approval ? (
                <a
                  href={approval}
                  target="_blank"
                  rel="noopener noreferrer"
                  className="text-sm font-medium text-primary hover:underline"
                >
                  {t("Open Plex approval")}
                </a>
              ) : null}
              <Button disabled={busy} onClick={() => void run("/pin/finish", "POST")}>
                {t("Check connection")}
              </Button>
            </div>
          }
        />
      ) : null}
      {st.connected ? (
        <>
          <SettingsRow
            title={t("Sync")}
            control={
              <Select
                aria-label={t("Sync")}
                value={st.direction}
                disabled={busy}
                onChange={(e) => setDirection(Number(e.currentTarget.value), st.importInterval)}
              >
                <option value={0}>{t("Off")}</option>
                <option value={SENDS}>{t("Ether → Plex")}</option>
                <option value={READS}>{t("Plex → Ether")}</option>
                <option value={SENDS | READS}>{t("Both")}</option>
              </Select>
            }
          />
          {st.direction & READS ? (
            <SettingsRow
              title={t("Check Plex every")}
              control={
                <Select
                  aria-label={t("Check Plex every")}
                  value={st.importInterval}
                  disabled={busy}
                  onChange={(e) => setDirection(st.direction, Number(e.currentTarget.value))}
                >
                  {[5, 10, 30, 60].map((n) => (
                    <option value={n} key={n}>
                      {tn(n, "{count} minute", "{count} minutes")}
                    </option>
                  ))}
                </Select>
              }
            />
          ) : null}
        </>
      ) : null}
      {st.connected && st.direction ? (
        <SyncStatus st={st} date={date} />
      ) : reconnect ? (
        <SettingsRow
          title={t("Status")}
          description={
            <span className="inline-flex items-center gap-1.5">
              <AlertTriangle size={14} className="text-warning" aria-hidden />
              {PROBLEMS.token}
            </span>
          }
        />
      ) : null}
      {st.connected && st.direction ? (
        <div className="flex flex-wrap gap-2 px-5 py-4">
          {st.direction & READS ? (
            <Button
              variant="outline"
              disabled={busy}
              onClick={() => void run("/import", "POST", undefined, t("Checked Plex."))}
            >
              {t("Check Plex now")}
            </Button>
          ) : null}
          {st.direction & SENDS ? (
            <Tooltip
              content={t(
                "Queues every title this viewer has watched, started or rated in Ether. New changes are sent on their own.",
              )}
            >
              <span tabIndex={0}>
                <Button
                  variant="outline"
                  disabled={busy}
                  onClick={() =>
                    void run("/export", "POST", undefined, t("Ether's watched titles are queued."))
                  }
                >
                  {t("Send watched titles to Plex")}
                </Button>
              </span>
            </Tooltip>
          ) : null}
          {st.failed > 0 ? (
            <Button variant="outline" disabled={busy} onClick={() => void run("/retry", "POST")}>
              {tn(st.failed, "Retry {count} failed change", "Retry {count} failed changes")}
            </Button>
          ) : null}
        </div>
      ) : null}
    </SettingsSection>
  );
}

function SyncStatus({ st, date }: { st: PlexWatchStatus; date: (s: number) => string }) {
  const problem = st.problem ? (PROBLEMS[st.problem] ?? PROBLEMS.network) : "";
  return (
    <>
      <SettingsRow
        title={t("Status")}
        description={
          <span className="inline-flex items-center gap-1.5">
            {problem ? (
              <AlertTriangle size={14} className="text-warning" aria-hidden />
            ) : (
              <CheckCircle2 size={14} className="text-success" aria-hidden />
            )}
            {problem ||
              (st.pending > 0
                ? tn(
                    st.pending,
                    "{count} change waiting to send",
                    "{count} changes waiting to send",
                  )
                : t("Up to date"))}
          </span>
        }
      />
      {st.direction & SENDS ? (
        <SettingsRow title={t("Last sent to Plex")} control={<span>{date(st.lastSend)}</span>} />
      ) : null}
      {st.direction & READS ? (
        <SettingsRow
          title={
            <span className="inline-flex items-center gap-2">
              {t("Last checked Plex")}
              <Tooltip
                content={t(
                  "Ether reads part of the library on each check. Whole library last read: {when}.",
                  { when: date(st.lastFullImport) },
                )}
              >
                <button type="button" aria-label={t("About Plex checks")} className="text-fg-muted">
                  ?
                </button>
              </Tooltip>
            </span>
          }
          control={<span>{date(st.lastImport)}</span>}
        />
      ) : null}
    </>
  );
}
