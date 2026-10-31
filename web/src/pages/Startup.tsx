import {
  Alert,
  AlertDescription,
  Button,
  Card,
  Spinner,
  ThemeToggle,
  confirmDialog,
  cn,
} from "@braedonsaunders/appkit-ui";
import { Archive, FileUp, LifeBuoy, LogIn, RotateCcw } from "lucide-react";
import { useEffect, useRef, useState } from "react";
import { EtherMark } from "../components/brand";
import { MutationError } from "../components/feedback";
import { PasswordInput } from "../components/fields";
import { ServerProgress, StartFailure, useServerReconnect } from "../components/ServerProgress";
import { t, tx } from "../i18n";
import { LanguagePicker } from "../i18n/react";
import { ApiError } from "../lib/api";
import { formatBytes, formatDateTime, relativeTime } from "../lib/format";
import { isEncryptedBackup, recoveryApi, type LocalBackup } from "../lib/ops";
import { phaseText } from "../lib/startup-text";

const SIGN_IN_TIMEOUT_MS = 10 * 60 * 1000;

/**
 * The page a browser sees while Ether starts: waiting for its database,
 * updating it, restoring a backup, or a start that failed (with what to do,
 * and on a standalone server a restore from here). It hands over to the
 * console as soon as the server is ready.
 */
export default function Startup({ onReady }: { onReady: () => void }) {
  const state = useServerReconnect({ baseline: null });
  const status = state.status;
  useEffect(() => {
    if (state.step !== "done") return;
    // A beat on "ready" so the hand-over reads as a transition, not a flash.
    const id = window.setTimeout(onReady, 600);
    return () => window.clearTimeout(id);
  }, [state.step, onReady]);

  const failed = state.step === "failed";
  const headline = !status
    ? t("Connecting to Ether…")
    : state.step === "done"
      ? t("Ether is ready")
      : failed
        ? t("Ether couldn't start")
        : phaseText(status).title;
  return (
    <main className="relative min-h-screen bg-bg-subtle">
      <div className="absolute right-4 top-4 flex items-center gap-1">
        <ThemeToggle
          collapsed
          labels={{
            label: t("Theme"),
            light: t("Light"),
            dark: t("Dark"),
            system: t("System"),
            current: (mode) =>
              t("Theme: {mode}", {
                mode: mode === "light" ? t("Light") : mode === "dark" ? t("Dark") : t("System"),
              }),
          }}
        />
        <LanguagePicker side="bottom" className="h-8 px-2 text-xs" />
      </div>
      <div className="mx-auto flex min-h-screen w-full max-w-lg flex-col justify-center px-4 py-16">
        <div className="reveal mb-6 flex flex-col items-center text-center">
          <EtherMark size={48} motion={failed || state.step === "done" ? "static" : "loop"} />
          <h1 className="mt-5 text-xl font-semibold tracking-tight text-fg" aria-live="polite">
            {headline}
          </h1>
          <p className="mt-1 text-sm text-fg-muted">
            {failed
              ? t("Here's what happened and how to fix it.")
              : t("The console opens by itself as soon as Ether is ready.")}
          </p>
        </div>
        {failed && status ? (
          <StartFailure status={status} />
        ) : (
          <Card className="reveal p-5 shadow-lg sm:p-6">
            {status ? (
              <ServerProgress kind="startup" state={state} />
            ) : (
              <div className="flex items-center gap-3 text-sm text-fg-muted" role="status">
                <Spinner size={16} label={t("Connecting to Ether…")} />
                {state.step === "starting"
                  ? t("Waiting for Ether to answer…")
                  : t("Connecting to Ether…")}
              </div>
            )}
          </Card>
        )}
        {failed && status?.failure?.recoverable ? <Recovery /> : null}
        {status?.version ? (
          <p className="mt-6 text-center text-xs text-fg-subtle">
            {t("Ether {version}", { version: status.version })}
          </p>
        ) : null}
      </div>
    </main>
  );
}

/**
 * Restore from the startup page: the console can't open, so the backups
 * kept on this server (or a downloaded one) are restored from here. The
 * owner signs in with Plex unless the browser is on the server itself or
 * setup isn't finished.
 */
function Recovery() {
  const [access, setAccess] = useState<{ granted: boolean; plex: boolean } | null>(null);
  const [token, setToken] = useState("");
  const [backups, setBackups] = useState<LocalBackup[] | null>(null);
  const [error, setError] = useState<unknown>(null);
  const [busy, setBusy] = useState(false);
  const [signingIn, setSigningIn] = useState<{ authUrl: string } | null>(null);
  const [file, setFile] = useState<File | null>(null);
  const [password, setPassword] = useState("");
  const [started, setStarted] = useState(false);
  const cancelled = useRef(false);

  useEffect(() => {
    cancelled.current = false;
    recoveryApi.access().then(setAccess, setError);
    return () => {
      cancelled.current = true;
    };
  }, []);
  const allowed = Boolean(access?.granted || token);
  useEffect(() => {
    if (!allowed) return;
    recoveryApi.backups(token).then(setBackups, setError);
  }, [allowed, token]);

  const signIn = async () => {
    setError(null);
    setBusy(true);
    const popup = window.open("", "plex-sign-in", "width=560,height=720");
    try {
      const pin = await recoveryApi.signInStart();
      setSigningIn({ authUrl: pin.authUrl });
      if (popup) popup.location.href = pin.authUrl;
      const deadline = Date.now() + SIGN_IN_TIMEOUT_MS;
      while (!cancelled.current && Date.now() < deadline) {
        await new Promise((r) => setTimeout(r, 2000));
        const res = await recoveryApi.signInPoll(pin.handle);
        if (res.done && res.token) {
          setToken(res.token);
          return;
        }
      }
      if (!cancelled.current) setError(new Error(t("Sign-in timed out. Try again.")));
    } catch (e) {
      setError(
        e instanceof ApiError && e.code === "not_owner"
          ? new Error(
              t(
                "That Plex account doesn't own this server. Sign in with the account that set Ether up.",
              ),
            )
          : e,
      );
    } finally {
      popup?.close();
      setSigningIn(null);
      setBusy(false);
    }
  };

  const restoreSnapshot = async (b: LocalBackup) => {
    const ok = await confirmDialog({
      title: t("Restore this backup?"),
      message: t(
        "Ether puts the backup from {date} in place and starts again. The current data is kept in a rollback folder.",
        { date: formatDateTime(b.created) },
      ),
      confirmLabel: t("Restore and start"),
    });
    if (!ok) return;
    setBusy(true);
    setError(null);
    try {
      await recoveryApi.restoreSnapshot(b.name, token);
      setStarted(true);
    } catch (e) {
      setError(e);
    } finally {
      setBusy(false);
    }
  };

  const restoreUpload = async () => {
    if (!file) return;
    setBusy(true);
    setError(null);
    try {
      if (!(await isEncryptedBackup(file)))
        throw new Error(
          t(
            "Choose a backup downloaded from Ether (an .etherbackup file). Older unencrypted bundles can only be restored from the running console.",
          ),
        );
      await recoveryApi.restoreUpload(file, password, token);
      setPassword("");
      setStarted(true);
    } catch (e) {
      setError(e);
    } finally {
      setBusy(false);
    }
  };

  if (started)
    return (
      <Alert variant="info" role="status" className="mt-4">
        <AlertDescription>
          {t("Restoring your backup. Ether starts again by itself; this page follows along.")}
        </AlertDescription>
      </Alert>
    );

  return (
    <section
      aria-labelledby="recovery-title"
      className="reveal mt-4 overflow-hidden rounded-xl border border-border bg-surface shadow-sm"
    >
      <div className="flex items-start gap-3 border-b border-border px-5 py-4">
        <span
          aria-hidden
          className="grid size-9 shrink-0 place-items-center rounded-lg bg-primary-subtle text-primary"
        >
          <LifeBuoy size={18} />
        </span>
        <div className="min-w-0">
          <h2 id="recovery-title" className="text-sm font-semibold text-fg">
            {t("Restore from a backup")}
          </h2>
          <p className="mt-0.5 text-sm text-fg-muted">
            {t(
              "Put a working backup in place and start again. The current data is kept in a rollback folder, so nothing is lost.",
            )}
          </p>
        </div>
      </div>
      <div className="space-y-4 px-5 py-4">
        <MutationError error={error} />
        {!access ? (
          <div className="flex items-center gap-2 text-sm text-fg-muted" role="status">
            <Spinner size={14} label={t("Loading")} /> {t("Loading…")}
          </div>
        ) : !allowed ? (
          access.plex ? (
            <div className="space-y-3">
              <p className="text-sm text-fg-muted">
                {t("Sign in with the Plex account that owns this server to restore.")}
              </p>
              <Button disabled={busy} onClick={() => void signIn()}>
                <LogIn size={15} />
                {signingIn ? t("Waiting for Plex…") : t("Sign in with Plex")}
              </Button>
              {signingIn ? (
                <p className="text-xs text-fg-muted">
                  {tx("Finish signing in in the Plex window. No window? {link}.", {
                    link: (
                      <a
                        className="font-medium text-primary underline-offset-4 hover:underline"
                        href={signingIn.authUrl}
                        target="_blank"
                        rel="noreferrer"
                      >
                        {t("Open the Plex sign-in page")}
                      </a>
                    ),
                  })}
                </p>
              ) : null}
            </div>
          ) : (
            <p className="text-sm text-fg-muted">
              {tx("To restore, open {address} in a browser on the computer that runs Ether.", {
                address: (
                  <code className="text-fg">{`http://localhost:${window.location.port || "80"}/ether/`}</code>
                ),
              })}
            </p>
          )
        ) : (
          <>
            {backups === null ? (
              <div className="flex items-center gap-2 text-sm text-fg-muted" role="status">
                <Spinner size={14} label={t("Loading")} /> {t("Loading backups…")}
              </div>
            ) : backups.length ? (
              <ul
                aria-label={t("Backups on this server")}
                className="divide-y divide-border-subtle overflow-hidden rounded-lg border border-border"
              >
                {backups.slice(0, 6).map((b, i) => (
                  <li key={b.name} className="flex items-center gap-3 px-3 py-2.5">
                    <Archive size={16} className="shrink-0 text-fg-subtle" aria-hidden />
                    <div className="min-w-0 flex-1">
                      <p className="text-sm font-medium text-fg">{formatDateTime(b.created)}</p>
                      <p className="text-xs text-fg-muted">
                        {i === 0 ? (
                          <span className="font-medium text-success">{t("Newest")} · </span>
                        ) : null}
                        {relativeTime(b.created)} · {formatBytes(b.size)}
                      </p>
                    </div>
                    <Button
                      size="sm"
                      variant={i === 0 ? "default" : "outline"}
                      disabled={busy}
                      aria-label={t("Restore the backup from {date}", {
                        date: formatDateTime(b.created),
                      })}
                      onClick={() => void restoreSnapshot(b)}
                    >
                      <RotateCcw size={14} /> {t("Restore")}
                    </Button>
                  </li>
                ))}
              </ul>
            ) : (
              <p className="text-sm text-fg-muted">
                {t("No backups are kept on this server. Upload one you downloaded earlier.")}
              </p>
            )}
            <details className="group rounded-lg border border-border px-3 py-2.5">
              <summary className="cursor-pointer select-none text-sm font-medium text-fg">
                {t("Upload a downloaded backup")}
              </summary>
              <form
                className="mt-3 space-y-3"
                onSubmit={(e) => {
                  e.preventDefault();
                  void restoreUpload();
                }}
              >
                <label
                  className={cn(
                    "relative flex cursor-pointer items-center gap-2 rounded-md border border-dashed border-border-strong bg-bg-subtle px-3 py-3 text-sm text-fg-muted hover:bg-surface-hover focus-within:ring-2 focus-within:ring-ring/50",
                  )}
                >
                  <FileUp size={16} aria-hidden />
                  <span className="truncate">{file ? file.name : t("Choose backup file")}</span>
                  <input
                    type="file"
                    accept=".etherbackup"
                    className="sr-only"
                    aria-label={t("Choose backup file")}
                    onChange={(e) => setFile(e.currentTarget.files?.[0] ?? null)}
                  />
                </label>
                <div className="space-y-1.5">
                  <label htmlFor="recovery-password" className="text-sm font-medium text-fg">
                    {t("Backup password")}
                  </label>
                  <PasswordInput
                    id="recovery-password"
                    autoComplete="off"
                    value={password}
                    onChange={(e) => setPassword(e.currentTarget.value)}
                  />
                </div>
                <Button type="submit" disabled={busy || !file || !password}>
                  {busy ? t("Checking the backup…") : t("Restore and start")}
                </Button>
              </form>
            </details>
          </>
        )}
      </div>
    </section>
  );
}

/** Whether a failed bootstrap means the server is starting (or restarting). */
export function isStarting(error: unknown): boolean {
  return (
    error instanceof ApiError &&
    (error.status === 0 || (error.status === 503 && error.code === "starting"))
  );
}
