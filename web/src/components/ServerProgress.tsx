import { Alert, AlertDescription, AlertTitle, Progress, cn } from "@braedonsaunders/appkit-ui";
import { Check, X } from "lucide-react";
import { useEffect, useReducer, useState, type ReactNode } from "react";
import { t } from "../i18n";
import { startupStatus, type StartupStatus } from "../lib/ops";
import {
  reconnect,
  reconnectInit,
  reconnectSteps,
  stepState,
  type ReconnectState,
  type ReconnectStep,
} from "../lib/reconnect";
import { clock, failureText, phaseText, stepLabel } from "../lib/startup-text";

const POLL_MS = 1000;

/**
 * Follows the server through a restart (baseline: the running server's
 * `started`) or a cold start (baseline null) by polling its public startup
 * status. Stops polling once the server is ready.
 */
export function useServerReconnect({
  baseline,
  waitForIdle = false,
  enabled = true,
}: {
  baseline: string | null;
  waitForIdle?: boolean;
  enabled?: boolean;
}): ReconnectState {
  const [state, dispatch] = useReducer(reconnect, undefined, () =>
    reconnectInit(baseline, waitForIdle, Date.now()),
  );
  const finished = state.step === "done";
  // Following starts afresh whenever what it follows changes.
  useEffect(() => {
    if (enabled) dispatch({ type: "reset", baseline, waitForIdle, at: Date.now() });
  }, [enabled, baseline, waitForIdle]);
  useEffect(() => {
    if (!enabled || finished) return;
    let live = true;
    let timer = 0;
    const poll = async () => {
      const ctl = new AbortController();
      const abort = window.setTimeout(() => ctl.abort(), 4000);
      try {
        const status = await startupStatus(ctl.signal);
        if (live) dispatch({ type: "status", status });
      } catch {
        if (live) dispatch({ type: "unreachable" });
      } finally {
        window.clearTimeout(abort);
        if (live) {
          dispatch({ type: "tick", at: Date.now() });
          timer = window.setTimeout(() => void poll(), POLL_MS);
        }
      }
    };
    void poll();
    return () => {
      live = false;
      window.clearTimeout(timer);
    };
  }, [enabled, finished]);
  return state;
}

/** Re-renders every second, for elapsed clocks and countdowns. */
export function useNow(active = true): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    if (!active) return;
    const id = window.setInterval(() => setNow(Date.now()), 1000);
    return () => window.clearInterval(id);
  }, [active]);
  return now;
}

/** One line under the active step: what the server is doing right now. */
function liveDetail(step: ReconnectStep, status: StartupStatus | null, now: number): string {
  if (step === "waiting") return t("Ether restarts as soon as nobody is watching.");
  if (step === "stopping") return t("Finishing requests in progress, then stopping…");
  if (!status) return t("Waiting for Ether to come back…");
  return phaseText(status, now).detail;
}

/**
 * The progress of a restart, a restore or a start: a stepper whose active
 * step says what the server is doing, a progress bar while the database
 * updates, the elapsed time, and the failure (with what to do) if the start
 * fails. Shared by the restart and restore drawers and the startup page.
 */
export function ServerProgress({
  kind,
  state,
  done,
  className,
}: {
  kind: "restart" | "restore" | "startup";
  state: ReconnectState;
  /** Shown above the steps once the server is back. */
  done?: ReactNode;
  className?: string;
}) {
  const now = useNow(state.step !== "done");
  const steps = reconnectSteps(kind, state);
  const status = state.status;
  // A cold start is timed by the server (it began before this page loaded).
  const elapsed = kind === "startup" && status ? status.elapsedSeconds : (now - state.since) / 1000;
  const migrating =
    status?.phase === "migrating" && status.steps ? (status.step ?? 0) / status.steps : null;
  const label =
    kind === "restore"
      ? t("Restore progress")
      : kind === "restart"
        ? t("Restart progress")
        : t("Startup progress");
  // A cold start that failed has no steps worth showing: only what to do.
  if (kind === "startup" && state.step === "failed" && status)
    return (
      <div className={className}>
        <StartFailure status={status} />
      </div>
    );
  return (
    <div className={cn("space-y-5", className)}>
      {state.step === "done" && done ? done : null}
      <ol aria-label={label} className="relative">
        {(state.step === "waiting" ? (["waiting", ...steps] as ReconnectStep[]) : steps).map(
          (step, i, all) => {
            const st = stepState(step, state);
            const last = i === all.length - 1;
            return (
              <li
                key={step}
                aria-current={st === "current" ? "step" : undefined}
                className="relative flex gap-3 pb-5 last:pb-0"
              >
                {!last ? (
                  <span
                    aria-hidden
                    className={cn(
                      "absolute left-3 top-7 -bottom-0 w-px -translate-x-1/2 transition-colors duration-500",
                      st === "complete" ? "bg-success/60" : "bg-border",
                    )}
                  />
                ) : null}
                <StepIcon state={st} />
                <div className="min-w-0 flex-1 pt-0.5">
                  <p
                    className={cn(
                      "text-sm font-medium transition-colors",
                      st === "upcoming" ? "text-fg-subtle" : "text-fg",
                    )}
                  >
                    {step === "done" && kind === "restore"
                      ? t("Restored")
                      : step === "done" && kind === "startup"
                        ? t("Ready")
                        : step === "done"
                          ? t("Back online")
                          : stepLabel(step)}
                    <span className="sr-only">
                      {" "}
                      {st === "complete"
                        ? t("(complete)")
                        : st === "current"
                          ? t("(in progress)")
                          : st === "failed"
                            ? t("(failed)")
                            : t("(not started)")}
                    </span>
                  </p>
                  {st === "current" ? (
                    <p className="mt-0.5 text-sm text-fg-muted" role="status" aria-live="polite">
                      {step === "starting" && status && kind !== "startup"
                        ? phaseText(status, now).title
                        : liveDetail(step, status, now)}
                    </p>
                  ) : null}
                  {st === "current" && step === "starting" && status?.phase ? (
                    <div className="mt-2.5 space-y-1.5">
                      <Progress
                        value={migrating !== null ? Math.round(migrating * 100) : undefined}
                        aria-label={phaseText(status, now).title}
                        className="h-1.5"
                      />
                      {kind !== "startup" ? (
                        <p className="text-xs tabular-nums text-fg-subtle">
                          {phaseText(status, now).detail}
                        </p>
                      ) : null}
                      {status.phase === "waiting_database" ? (
                        <WaitingForDatabase status={status} />
                      ) : null}
                    </div>
                  ) : null}
                </div>
              </li>
            );
          },
        )}
      </ol>
      {state.step !== "done" && state.step !== "failed" ? (
        <p className="flex flex-wrap items-center justify-between gap-2 border-t border-border pt-3 text-xs text-fg-subtle">
          <span>{t("This page reconnects by itself — no need to refresh.")}</span>
          <span className="tabular-nums" aria-label={t("Elapsed time")}>
            {clock(elapsed)}
          </span>
        </p>
      ) : null}
      {state.slow && state.step !== "failed" && state.step !== "done" ? (
        <Alert variant="info" role="status">
          <AlertTitle>{t("This is taking longer than usual")}</AlertTitle>
          <AlertDescription>
            {state.step === "stopping"
              ? t(
                  "Ether hasn't come back yet. If it runs without a service manager or container restart policy, start it again yourself; this page picks it up as soon as it answers.",
                )
              : t(
                  "Large databases can take several minutes to update or check. Ether is still working; this page keeps following it.",
                )}
          </AlertDescription>
        </Alert>
      ) : null}
      {state.step === "failed" && status ? <StartFailure status={status} /> : null}
    </div>
  );
}

/** After a few failed attempts, what to check, and the last error. */
function WaitingForDatabase({ status }: { status: StartupStatus }) {
  if ((status.attempt ?? 0) < 3 && !status.failure?.detail) return null;
  return (
    <div className="space-y-1.5 pt-1 text-xs text-fg-muted">
      {(status.attempt ?? 0) >= 3 ? (
        <p>
          {t(
            "Check that PostgreSQL is running and reachable from this machine, and that ETHER_DATABASE_URL is correct.",
          )}
        </p>
      ) : null}
      {status.failure?.detail ? (
        <details>
          <summary className="cursor-pointer select-none font-medium text-fg-muted">
            {t("Last error")}
          </summary>
          <code className="mt-1 block whitespace-pre-wrap break-words text-fg-subtle">
            {status.failure.detail}
          </code>
        </details>
      ) : null}
    </div>
  );
}

function StepIcon({ state }: { state: ReturnType<typeof stepState> }) {
  return (
    <span
      aria-hidden
      className={cn(
        "relative z-10 grid size-6 shrink-0 place-items-center rounded-full border transition-all duration-300",
        state === "complete" && "border-success bg-success text-success-fg",
        state === "current" && "border-primary bg-surface text-primary",
        state === "upcoming" && "border-border bg-surface text-fg-subtle",
        state === "failed" && "border-danger bg-danger text-danger-fg",
      )}
    >
      {state === "complete" ? (
        <Check size={14} strokeWidth={3} />
      ) : state === "failed" ? (
        <X size={14} strokeWidth={3} />
      ) : state === "current" ? (
        <>
          <span className="absolute inset-0 animate-ping rounded-full bg-primary/25 motion-reduce:animate-none" />
          <span className="size-2 rounded-full bg-primary" />
        </>
      ) : (
        <span className="size-1.5 rounded-full bg-border-strong" />
      )}
    </span>
  );
}

/** The moment the server is back: an animated check, a title and a line. */
export function DoneHero({ title, children }: { title: string; children?: ReactNode }) {
  return (
    <div role="status" className="flex flex-col items-center px-2 pb-1 pt-2 text-center">
      <svg viewBox="0 0 160 160" className="ether-approve-pop size-20 text-success" aria-hidden>
        <circle cx="80" cy="80" r="68" className="ether-approve-glow fill-success/15" />
        <path
          d="M46 84 L72 110 L116 54"
          className="ether-approve-check fill-none stroke-current"
          strokeWidth="10"
          strokeLinecap="round"
          strokeLinejoin="round"
        />
      </svg>
      <h3 className="mt-3 text-lg font-semibold tracking-tight text-fg">{title}</h3>
      {children ? <div className="mt-1 max-w-sm text-sm text-fg-muted">{children}</div> : null}
    </div>
  );
}

/** A failed start: what happened, what to do, and the technical detail. */
export function StartFailure({ status }: { status: StartupStatus }) {
  const now = useNow();
  const { title, body } = failureText(status);
  const retry = status.retryAt
    ? Math.max(0, Math.ceil((Date.parse(status.retryAt) - now) / 1000))
    : 0;
  return (
    <Alert variant="destructive" role="alert">
      <AlertTitle>{title}</AlertTitle>
      <AlertDescription>
        <span className="block">{body}</span>
        {retry > 0 ? (
          <span className="mt-2 block text-xs opacity-90">
            {t("Ether tries again by itself in {time}.", { time: clock(retry) })}
          </span>
        ) : null}
        {status.failure?.detail ? (
          <details className="mt-2 text-xs">
            <summary className="cursor-pointer select-none font-medium">
              {t("Technical details")}
            </summary>
            <code className="mt-1 block whitespace-pre-wrap break-words opacity-90">
              {status.failure.detail}
            </code>
          </details>
        ) : null}
      </AlertDescription>
    </Alert>
  );
}
