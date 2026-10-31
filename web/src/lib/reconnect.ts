import type { StartupStatus } from "./ops";

/**
 * Following a server through a restart (or a cold start) from its public
 * startup status. The old process answers until it stops, then nothing
 * answers, then the new process reports its own start (a different
 * `started`) until it is ready. A restart can be so quick that the gap is
 * never seen: a new `started` alone proves the server came back.
 */
export type ReconnectStep = "waiting" | "stopping" | "restoring" | "starting" | "done" | "failed";

export interface ReconnectState {
  step: ReconnectStep;
  /** The running server's `started` before the restart; null follows any start. */
  baseline: string | null;
  /** A new start has reported in. */
  restarted: boolean;
  /** The new start's latest status. */
  status: StartupStatus | null;
  /** A restore phase was seen (so its step stays ticked once passed). */
  sawRestore: boolean;
  /** When following began, and whether it is taking longer than expected. */
  since: number;
  slow: boolean;
}

export type ReconnectEvent =
  | { type: "reset"; baseline: string | null; waitForIdle: boolean; at: number }
  | { type: "status"; status: StartupStatus }
  | { type: "unreachable" }
  | { type: "tick"; at: number };

/** After this long without being ready, the console says it is taking a while. */
export const SLOW_AFTER_MS = 120_000;

export function reconnectInit(
  baseline: string | null,
  waitForIdle: boolean,
  at: number,
): ReconnectState {
  return {
    step: waitForIdle ? "waiting" : baseline === null ? "starting" : "stopping",
    baseline,
    restarted: false,
    status: null,
    sawRestore: false,
    since: at,
    slow: false,
  };
}

const FINISHED: ReconnectStep[] = ["done", "failed"];

export function reconnect(state: ReconnectState, event: ReconnectEvent): ReconnectState {
  switch (event.type) {
    case "reset":
      return reconnectInit(event.baseline, event.waitForIdle, event.at);
    case "tick":
      return {
        ...state,
        slow: !FINISHED.includes(state.step) && event.at - state.since > SLOW_AFTER_MS,
      };
    case "unreachable":
      if (FINISHED.includes(state.step) && state.step !== "failed") return state;
      // Down after the new start reported in: it is still coming up.
      return { ...state, step: state.restarted ? "starting" : "stopping" };
    case "status": {
      const s = event.status;
      const fresh = state.baseline === null || s.started !== state.baseline;
      if (!fresh) {
        if (s.phase === "stopping") return { ...state, step: "stopping" };
        // The old server still runs: waiting for viewers, or about to stop.
        return { ...state, step: state.step === "waiting" ? "waiting" : "stopping" };
      }
      const sawRestore = state.sawRestore || s.phase === "restoring";
      const step: ReconnectStep =
        s.phase === "ready"
          ? "done"
          : s.phase === "failed"
            ? "failed"
            : s.phase === "restoring"
              ? "restoring"
              : s.phase === "stopping"
                ? "stopping"
                : "starting";
      return {
        ...state,
        step,
        restarted: true,
        status: s,
        sawRestore,
        slow: step === "done" ? false : state.slow,
      };
    }
  }
}

/** The steps a flow shows, in order. */
export function reconnectSteps(
  kind: "restart" | "restore" | "startup",
  state: ReconnectState,
): ReconnectStep[] {
  switch (kind) {
    case "restart":
      return ["stopping", "starting", "done"];
    case "restore":
      return ["stopping", "restoring", "starting", "done"];
    case "startup":
      return state.sawRestore || state.step === "restoring"
        ? ["restoring", "starting", "done"]
        : ["starting", "done"];
  }
}

const ORDER: ReconnectStep[] = ["waiting", "stopping", "restoring", "starting", "done"];

/** Where a step stands relative to the current one. */
export function stepState(
  step: ReconnectStep,
  state: ReconnectState,
): "complete" | "current" | "upcoming" | "failed" {
  if (state.step === "failed") {
    // A failure belongs to the step that was running: the new start's.
    if (step === "done") return "upcoming";
    if (step === "starting") return "failed";
    return "complete";
  }
  if (state.step === "done") return "complete";
  const at = ORDER.indexOf(state.step);
  const i = ORDER.indexOf(step);
  if (i < at) return "complete";
  return i === at ? "current" : "upcoming";
}
