import {
  Badge,
  Button,
  SettingsSection,
  Spinner,
  Select,
  Tooltip,
  toast,
} from "@braedonsaunders/appkit-ui";
import { useState } from "react";
import { ErrorPanel } from "../../../components/feedback";
import { SettingsRow } from "../../../components/SettingsRow";
import { t, tn } from "../../../i18n";
import { formatDateTime } from "../../../lib/format";
import { apiFetch } from "../../../lib/api";
import { useAsync } from "../../../lib/hooks";

type PlexWatchStatus = {
  connected: boolean;
  plexId?: number;
  username?: string;
  direction: number;
  importInterval: number;
  lastImport: number;
  lastSend: number;
  nextImport: number;
  problem: string;
  eligibility: string;
  revoked: boolean;
  pending: number;
  dead: number;
  held: number;
  heldLegacy: number;
  inFlight: boolean;
  skipped: number;
  restricted: boolean;
  pinCode?: string;
  pinUrl?: string;
  pinExpires?: string;
  accountMatch: boolean;
};

/** Per-viewer Plex account watch state, on Limits & preferences. */
export function PlexWatchSync({ viewerId, plexId }: { viewerId: number; plexId: number }) {
  const status = useAsync(
    (signal) => apiFetch<PlexWatchStatus>(`/viewers/${viewerId}/plex-watch`, { signal }),
    [viewerId],
  );
  const [busy, setBusy] = useState(false);
  const [unmatched, setUnmatched] = useState<string[] | null>(null);
  const [hasMore, setHasMore] = useState(false);
  const loadUnmatched = async (after = "") => {
    setBusy(true);
    try {
      const items = await apiFetch<string[]>(
        `/viewers/${viewerId}/plex-watch/unmatched?after=${encodeURIComponent(after)}`,
      );
      setUnmatched((old) => (after ? [...(old ?? []), ...items] : items));
      setHasMore(items.length === 100);
    } catch {
      toast.error(t("Plex watch state was not updated"));
    } finally {
      setBusy(false);
    }
  };

  const run = async (path: string, method: string, body?: unknown) => {
    setBusy(true);
    try {
      const next = await apiFetch<PlexWatchStatus>(`/viewers/${viewerId}/plex-watch${path}`, {
        method,
        body: body === undefined ? undefined : JSON.stringify(body),
      });
      status.setData(next);
    } catch {
      toast.error(t("Plex watch state was not updated"));
      void status.reload();
    } finally {
      setBusy(false);
    }
  };

  if (plexId <= 0) return null;
  if (status.error)
    return (
      <ErrorPanel
        error={new Error(t("Plex watch state was not updated"))}
        onRetry={() => void status.reload()}
      />
    );
  if (!status.data) {
    return (
      <div className="py-6 text-center">
        <Spinner size={18} label={t("Loading Plex watch state")} />
      </div>
    );
  }

  const st = status.data;
  // Plex accepts account watch sync only from its own servers. The section
  // stays visible and honest, and a stored account can still be removed.
  if (st.problem === "unsupported") {
    const account = st.connected || (st.accountMatch && !!st.username);
    return (
      <SettingsSection
        title={t("Plex watch sync")}
        description={t("Sync watched marks with other Plex Media Servers you run alongside Ether.")}
      >
        <p className="px-5 py-4 text-sm">
          <a href="/ether/services/plexservers" className="text-primary hover:underline">
            {t("Manage Plex watch sync")}
          </a>
        </p>
        {account ? (
          <SettingsRow
            title={t("Plex account watch state")}
            description={<Badge variant="secondary">{t("Unavailable")}</Badge>}
            control={
              <div className="flex items-center gap-2">
                {st.username ? <span>{st.username}</span> : null}
                <Button disabled={busy} onClick={() => void run("/disconnect", "POST")}>
                  {t("Disconnect")}
                </Button>
              </div>
            }
          />
        ) : null}
      </SettingsSection>
    );
  }
  const date = (seconds: number) =>
    seconds ? formatDateTime(new Date(seconds * 1000)) : t("Never");
  const problem =
    st.problem === "consent"
      ? t("Turn on Sync My Watch State and Ratings in Plex")
      : st.problem === "token"
        ? t("Reconnect your Plex account")
        : st.problem === "credential"
          ? t("This credential cannot read Plex watch state")
          : st.problem
            ? t("Plex watch state could not be synchronized")
            : "";
  return (
    <SettingsSection>
      <h2 className="flex items-center gap-2 px-5 py-4 text-sm font-semibold">
        {t("Plex account watch state")}
        <Tooltip
          content={t(
            "Connect this viewer's Plex account to sync watched marks and ratings. Uploading existing Ether state is a separate action.",
          )}
        >
          <button type="button" aria-label={t("About Plex watch state")} className="text-fg-muted">
            ?
          </button>
        </Tooltip>
      </h2>
      <SettingsRow
        title={t("Connection")}
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
        title={t("Sync direction")}
        control={
          <Select
            aria-label={t("Sync direction")}
            value={st.direction}
            disabled={busy || !st.connected}
            onChange={(e) =>
              void run("", "PUT", {
                direction: Number(e.currentTarget.value),
                interval: st.importInterval,
              })
            }
          >
            <option value={0}>{t("Off")}</option>
            <option value={1}>{t("Ether → Plex")}</option>
            <option value={2}>{t("Plex → Ether")}</option>
            <option value={3}>{t("Both")}</option>
          </Select>
        }
      />
      {(st.direction & 2) !== 0 ? (
        <SettingsRow
          title={t("Import interval")}
          control={
            <Select
              aria-label={t("Import interval")}
              value={st.importInterval}
              disabled={busy || !st.connected}
              onChange={(e) =>
                void run("", "PUT", {
                  direction: st.direction,
                  interval: Number(e.currentTarget.value),
                })
              }
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
      <p className="px-5 py-2 text-sm text-fg-muted">
        {t("Plex syncs watched and unwatched state and ratings, but not resume positions.")}
      </p>
      <SettingsRow
        title={t("Last successful import")}
        control={<span>{date(st.lastImport)}</span>}
      />
      <SettingsRow title={t("Last successful send")} control={<span>{date(st.lastSend)}</span>} />
      {(st.direction & 2) !== 0 ? (
        <SettingsRow
          title={t("Next scheduled import")}
          control={<span>{date(st.nextImport)}</span>}
        />
      ) : null}
      {problem ? (
        <SettingsRow title={t("Current problem")} control={<span>{problem}</span>} />
      ) : null}
      <SettingsRow
        title={tn(st.skipped, "{count} unmatched title", "{count} unmatched titles")}
        control={
          st.skipped > 0 ? (
            <Button disabled={busy} onClick={() => void loadUnmatched()}>
              {t("View unmatched titles")}
            </Button>
          ) : null
        }
      />
      {unmatched ? (
        <div className="px-5 py-2">
          <ul>
            {unmatched.map((guid) => (
              <li key={guid}>
                <code>{guid}</code>
              </li>
            ))}
          </ul>
          {hasMore ? (
            <Button
              disabled={busy}
              onClick={() => void loadUnmatched(unmatched[unmatched.length - 1])}
            >
              {t("Load more")}
            </Button>
          ) : null}
        </div>
      ) : null}
      <div className="flex flex-wrap gap-2 px-5 py-4">
        {(
          [
            ["/import", t("Import now")],
            ["/export", t("Export current state")],
          ] as const
        ).map(([path, label]) => {
          const disabled =
            busy || !st.connected || (path === "/export" && (st.direction & 1) === 0);
          const button = (
            <Button
              variant={disabled ? "outline" : "default"}
              disabled={disabled}
              onClick={() => void run(path, "POST")}
            >
              {label}
            </Button>
          );
          return disabled ? (
            <Tooltip
              key={path}
              content={
                !st.connected
                  ? t("Connect the Plex account first")
                  : (st.direction & 1) === 0 && path === "/export"
                    ? t("Choose a direction that sends to Plex first")
                    : t("Please wait")
              }
            >
              <span tabIndex={0}>{button}</span>
            </Tooltip>
          ) : (
            <span key={path}>{button}</span>
          );
        })}
        {/* Recovery actions appear only while something is stuck. Legacy holds
            have no Plex id, so only a manual Resolve can release them. */}
        {st.held > 0 ? (
          <Tooltip
            content={
              st.heldLegacy > 0
                ? t("Older changes are held until they are checked against Plex.")
                : t("Unconfirmed changes are held until they are checked against Plex.")
            }
          >
            <span tabIndex={0}>
              <Button
                variant="outline"
                disabled={busy || !st.connected}
                onClick={() => void run("/resolve", "POST")}
              >
                {tn(st.held, "Resolve {count} held change", "Resolve {count} held changes")}
              </Button>
            </span>
          </Tooltip>
        ) : null}
        {st.pending > 0 || st.dead > 0 || st.skipped > 0 ? (
          <Button
            variant="outline"
            disabled={busy || !st.connected}
            onClick={() => void run("/retry", "POST")}
          >
            {t("Retry failed items")}
          </Button>
        ) : null}
      </div>
    </SettingsSection>
  );
}
