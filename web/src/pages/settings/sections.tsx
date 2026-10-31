import { Button, SettingsSection, toast } from "@braedonsaunders/appkit-ui";
import { useEffect, useState } from "react";
import { ErrorPanel, LoadingRows } from "../../components/feedback";
import { LevelBadge } from "../../components/status";
import { SettingsRow } from "../../components/SettingsRow";
import { localized, t, tn } from "../../i18n";
import { api } from "../../lib/api";
import { isPlainObject, relativeTime } from "../../lib/format";
import { plexDirectHealth, remoteHealth } from "../../lib/health";
import { useAsync } from "../../lib/hooks";
import type { UpdateStatus } from "../../lib/types";
import { SecureAddresses } from "./SecureAddresses";

/**
 * Settings helpers shared with the Settings page. Usenet providers and indexers
 * now live in pages/sources, quality in pages/releases, cache and transcoding
 * status in pages/playback.
 */

export type Errors = Record<string, string>;
export type RunTest = (
  key: string,
  fn: () => Promise<{ ok: boolean; error?: string; durationMs: number }>,
) => void;

// ---- Status cards -----------------------------------------------------------------------

/** Remote-access and plex.direct health for the Plex app page. Account sign-in lives on that page, not here. */
export function PlexStatus({ remote }: { remote: unknown }) {
  const r = remoteHealth(remote);
  const c = plexDirectHealth(remote);
  const conns =
    isPlainObject(remote) && Array.isArray(remote.publishedUris)
      ? remote.publishedUris.filter((uri): uri is string => typeof uri === "string")
      : [];
  return (
    <SettingsSection title={t("Status")}>
      <SettingsRow
        title={t("Remote access")}
        description={r.details.join(" · ") || r.summary}
        control={<LevelBadge level={r.level} label={r.summary} />}
      />
      <SettingsRow
        title={t("plex.direct certificate")}
        description={c.details.join(" · ") || c.summary}
        control={<LevelBadge level={c.level} label={c.statusLabel ?? c.summary} />}
      />
      <SecureAddresses remote={remote} />
      {conns.length ? (
        <SettingsRow
          stacked
          title={t("Published addresses")}
          description={t(
            "These are registration addresses. Plex may provide different HTTPS addresses to apps.",
          )}
        >
          <ul className="space-y-1 text-xs">
            {conns.map((uri, i) => (
              <li key={i} className="flex items-center gap-2">
                <code className="truncate text-fg-muted">{uri}</code>
              </li>
            ))}
          </ul>
        </SettingsRow>
      ) : null}
    </SettingsSection>
  );
}

// ---- Updates -----------------------------------------------------------------------------

const UPDATE_STATE: Record<UpdateStatus["state"], string> = localized(() => ({
  idle: t("Idle"),
  downloading: t("Downloading"),
  staged: t("Downloaded"),
  "waiting-for-window": t("Waiting for maintenance window"),
  "waiting-for-idle": t("Waiting until nobody is watching"),
  applying: t("Installing — Ether will restart"),
  "rolled-back": t("Rolled back"),
}));

export function UpdatesStatus() {
  const status = useAsync(() => api.updateStatus(), []);
  const [busy, setBusy] = useState(false);
  const s = status.data;
  useEffect(() => {
    if (!s || !(s.requested || s.state === "downloading" || s.state === "applying")) return;
    const timer = setInterval(() => void status.reload(), 5000);
    return () => clearInterval(timer);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [s?.requested, s?.state]);
  if (status.error) return <ErrorPanel error={status.error} onRetry={() => void status.reload()} />;
  if (!s) return <LoadingRows rows={2} />;

  const updateNow = async (force: boolean) => {
    setBusy(true);
    try {
      await api.updateNow(force);
      toast.success(
        force ? t("Installing update now") : t("Update will install once nobody is watching"),
      );
      await status.reload();
    } catch (e) {
      toast.error(t("Couldn't start the update"), { description: (e as Error).message });
    } finally {
      setBusy(false);
    }
  };
  const canApply = s.capability === "self-apply" && s.state !== "applying";
  let availableText = t("Ether is up to date");
  if (s.available)
    availableText = t("Version {version} — {state}", {
      version: s.available.version,
      state: UPDATE_STATE[s.state],
    });
  else if (s.state === "rolled-back" && s.failedVersion)
    availableText = t("Version {version} was rolled back and will be skipped", {
      version: s.failedVersion,
    });
  else if (!s.enabled) availableText = s.disabledReason || t("Updates are disabled in this build");
  else if (s.lastError) availableText = s.lastError;
  else if (!s.lastCheck) availableText = t("Not checked yet");

  return (
    <SettingsSection title={t("Status")}>
      <SettingsRow
        title={t("Installed")}
        description={
          s.confirming
            ? t("Version {version} — confirming the update", { version: s.current })
            : t("Version {version}", { version: s.current })
        }
        control={
          s.state === "rolled-back" ? (
            <LevelBadge level="warn" label={t("Rolled back")} />
          ) : (
            <LevelBadge
              level={s.lastError ? "warn" : "ok"}
              label={s.lastError ? t("Issue") : t("OK")}
            />
          )
        }
      />
      <SettingsRow
        title={t("Available")}
        description={
          <>
            {availableText}
            {s.available?.notesUrl ? (
              <>
                {" · "}
                <a
                  className="underline"
                  href={s.available.notesUrl}
                  target="_blank"
                  rel="noreferrer"
                >
                  {t("Release notes")}
                </a>
              </>
            ) : null}
            {s.capability === "notify-only" && s.capabilityReason ? (
              <span className="block">
                {t("Install manually: {reason}.", { reason: s.capabilityReason })}
              </span>
            ) : null}
          </>
        }
        control={
          s.available && canApply ? (
            s.requested && s.state === "waiting-for-idle" ? (
              <Button
                size="sm"
                variant="outline"
                disabled={busy}
                onClick={() => void updateNow(true)}
              >
                {t("Install now anyway")}
              </Button>
            ) : (
              <Button
                size="sm"
                disabled={busy || s.requested}
                onClick={() => void updateNow(false)}
              >
                {t("Update now")}
              </Button>
            )
          ) : null
        }
      />
      <SettingsRow
        title={t("Maintenance window")}
        description={`${
          s.window.open
            ? t("{start}–{end} local time (open now)", { start: s.window.start, end: s.window.end })
            : t("{start}–{end} local time", { start: s.window.start, end: s.window.end })
        } · ${tn(s.activeSessions, "{count} active stream", "{count} active streams")}`}
      />
      <SettingsRow
        title={t("Last check")}
        description={
          s.lastError
            ? s.lastError
            : s.lastCheck
              ? relativeTime(s.lastCheck)
              : s.enabled
                ? t("Not checked yet")
                : t("Never")
        }
      />
    </SettingsSection>
  );
}
