import { Button, Skeleton, cn, toast } from "@braedonsaunders/appkit-ui";
import {
  Boxes,
  Container,
  MonitorCog,
  RefreshCw,
  RotateCw,
  Server,
  SquareTerminal,
} from "lucide-react";
import { useCallback, useEffect, useState, type ReactNode } from "react";
import { t } from "../i18n";
import { errorMessage } from "../lib/api";
import { restartApi, startupStatus, type RestartInfo } from "../lib/ops";
import { Drawer } from "./Drawer";
import { ErrorPanel, MutationError } from "./feedback";
import type { ReconnectState } from "../lib/reconnect";
import { clock } from "../lib/startup-text";
import { DoneHero, ServerProgress, useServerReconnect } from "./ServerProgress";
import { ViewerImpact } from "./ViewerImpact";

/** How this install brings Ether back, in the owner's terms. */
export function restartHow(info: Pick<RestartInfo, "manager" | "method">): {
  icon: ReactNode;
  text: string;
} {
  switch (info.manager) {
    case "systemd":
      return {
        icon: <Server size={16} />,
        text: t(
          "Ether restarts itself in place, so systemd keeps running it without its restart delay.",
        ),
      };
    case "launchd":
      return {
        icon: <MonitorCog size={16} />,
        text: t("Ether restarts itself in place; macOS keeps running it."),
      };
    case "windows-service":
      return {
        icon: <MonitorCog size={16} />,
        text: t(
          "Windows starts the Ether service again after 5 seconds, or up to a minute if it has restarted several times today.",
        ),
      };
    case "container":
      return {
        icon: <Container size={16} />,
        text: t(
          "Ether restarts inside its container. The container keeps running, whatever its restart policy.",
        ),
      };
    case "terminal":
      return {
        icon: <SquareTerminal size={16} />,
        text:
          info.method === "spawn"
            ? t("Ether starts again as a new process in the same console window.")
            : t("Ether restarts in the same terminal window."),
      };
    default:
      return {
        icon: <Server size={16} />,
        text: t("Ether restarts itself in place; your service manager keeps running it."),
      };
  }
}

/** A row of what will happen, with an icon. */
export function Fact({ icon, children }: { icon: ReactNode; children: ReactNode }) {
  return (
    <li className="flex gap-3">
      <span className="mt-0.5 shrink-0 text-fg-subtle" aria-hidden>
        {icon}
      </span>
      <span className="min-w-0 text-sm text-fg-muted">{children}</span>
    </li>
  );
}

/** Loads what a restart needs: how it happens, who it interrupts, and the
 * running server's start time to recognise the new one by. */
export function useRestartPlan(open: boolean) {
  const [info, setInfo] = useState<RestartInfo | null>(null);
  const [baseline, setBaseline] = useState<string | null>(null);
  const [error, setError] = useState<unknown>(null);
  const load = useCallback(async () => {
    setError(null);
    try {
      const [i, s] = await Promise.all([restartApi.status(), startupStatus()]);
      setInfo(i);
      setBaseline(s.started);
    } catch (e) {
      setError(e);
    }
  }, []);
  useEffect(() => {
    if (open) void load();
  }, [open, load]);
  return { info, baseline, error, reload: load };
}

export function PlanSkeleton() {
  return (
    <div className="space-y-4" role="status" aria-label={t("Loading")}>
      <Skeleton className="h-5 w-3/4" />
      <Skeleton className="h-16 w-full rounded-lg" />
      <Skeleton className="h-24 w-full rounded-lg" />
    </div>
  );
}

/**
 * Restart Ether from the console: what happens and who is watching first,
 * then the restart followed live until the server is back.
 */
export function RestartDrawer({
  open,
  onClose,
  context,
}: {
  open: boolean;
  onClose: () => void;
  /** Why a restart is suggested here, e.g. a saved cache folder. */
  context?: string;
}) {
  const plan = useRestartPlan(open);
  const [waitForIdle, setWaitForIdle] = useState(true);
  const [started, setStarted] = useState<{ waitForIdle: boolean } | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>(null);

  useEffect(() => {
    if (!open) {
      setStarted(null);
      setError(null);
      setWaitForIdle(true);
    }
  }, [open]);

  const follow = useServerReconnect({
    baseline: plan.baseline,
    waitForIdle: started?.waitForIdle ?? false,
    enabled: started !== null,
  });
  const active = plan.info?.active ?? 0;
  const restart = async () => {
    setBusy(true);
    setError(null);
    const wait = waitForIdle && active > 0;
    try {
      await restartApi.restart(wait);
      setStarted({ waitForIdle: wait });
    } catch (e) {
      // The reply can be lost as the server stops: follow it anyway.
      if (e instanceof Error && "status" in e && (e as { status: number }).status === 0)
        setStarted({ waitForIdle: wait });
      else setError(e);
    } finally {
      setBusy(false);
    }
  };

  return (
    <Drawer
      open={open}
      onClose={() => {
        if (busy) return;
        if (!started) onClose();
        else if (follow.step === "waiting") {
          // The held restart carries on without this page.
          toast.info(t("Ether restarts as soon as nobody is watching."));
          onClose();
        }
      }}
      title={t("Restart Ether")}
      description={
        started ? undefined : t("Stop Ether and start it again, without leaving this page.")
      }
      size="md"
      footer={
        started ? (
          following(follow) ? undefined : (
            <FollowFooter state={follow} onCancelled={() => setStarted(null)} />
          )
        ) : (
          <div className="flex w-full flex-wrap items-center justify-end gap-2">
            <Button variant="outline" disabled={busy} onClick={onClose}>
              {t("Cancel")}
            </Button>
            <Button
              variant={active > 0 && !waitForIdle ? "destructive" : "default"}
              disabled={busy || !plan.info}
              onClick={() => void restart()}
            >
              <RotateCw size={15} className={cn(busy && "animate-spin")} />
              {busy
                ? t("Restarting…")
                : active > 0 && waitForIdle
                  ? t("Restart when idle")
                  : t("Restart Ether")}
            </Button>
          </div>
        )
      }
    >
      {started ? (
        <ServerProgress
          kind="restart"
          state={follow}
          done={
            <DoneHero title={t("Ether is back online")}>
              {t("Restarted in {time}. Everything is running again.", {
                time: clock((Date.now() - follow.since) / 1000),
              })}
            </DoneHero>
          }
        />
      ) : plan.error ? (
        <ErrorPanel error={plan.error} onRetry={() => void plan.reload()} />
      ) : !plan.info ? (
        <PlanSkeleton />
      ) : (
        <div className="space-y-6">
          {context ? (
            <p className="rounded-lg border border-info/30 bg-info-subtle px-3 py-2.5 text-sm text-fg">
              {context}
            </p>
          ) : null}
          <section aria-labelledby="restart-what" className="space-y-3">
            <h3 id="restart-what" className="text-sm font-medium text-fg">
              {t("What happens")}
            </h3>
            <ul className="space-y-2.5">
              <Fact icon={<RefreshCw size={16} />}>
                {t(
                  "Ether stops, then starts again. This usually takes less than a minute, and this page reconnects by itself.",
                )}
              </Fact>
              <Fact icon={restartHow(plan.info).icon}>{restartHow(plan.info).text}</Fact>
              {plan.info.clustered ? (
                <Fact icon={<Boxes size={16} />}>
                  {t("Other nodes keep serving your viewers while this node restarts.")}
                </Fact>
              ) : null}
            </ul>
          </section>
          <ViewerImpact
            active={active}
            sessions={plan.info.sessions}
            waitForIdle={waitForIdle}
            onWaitForIdle={setWaitForIdle}
          />
          <MutationError error={error} />
        </div>
      )}
    </Drawer>
  );
}

/** Following a restart with nothing to do but wait: the drawer has no footer. */
export function following(state: ReconnectState): boolean {
  return state.step !== "waiting" && state.step !== "done" && state.step !== "failed";
}

/**
 * The footer while following a restart: it can be called off while waiting
 * for viewers; once the server is back the page reloads into it.
 */
export function FollowFooter({
  state,
  onCancelled,
  doneLabel = t("Continue"),
  onDone = () => window.location.reload(),
}: {
  state: ReconnectState;
  onCancelled: () => void;
  doneLabel?: string;
  onDone?: () => void;
}) {
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  if (state.step === "waiting")
    return (
      <div className="flex w-full flex-wrap items-center justify-between gap-2">
        <span className="text-xs text-danger" role="alert">
          {error}
        </span>
        <Button
          variant="outline"
          disabled={busy}
          onClick={() => {
            setBusy(true);
            void restartApi
              .cancel()
              .then(onCancelled, (e) => setError(errorMessage(e)))
              .finally(() => setBusy(false));
          }}
        >
          {t("Cancel restart")}
        </Button>
      </div>
    );
  const finished = state.step === "done" || state.step === "failed";
  return (
    <div className="flex w-full justify-end">
      <Button
        disabled={!finished}
        onClick={state.step === "failed" ? () => window.location.reload() : onDone}
      >
        {state.step === "failed" ? t("Reload page") : doneLabel}
      </Button>
    </div>
  );
}

/** A button that opens the restart drawer. */
export function RestartButton({
  context,
  label = t("Restart Ether"),
  size = "sm",
  variant = "outline",
}: {
  context?: string;
  label?: string;
  size?: "sm" | "md";
  variant?: "outline" | "default";
}) {
  const [open, setOpen] = useState(false);
  return (
    <>
      <Button size={size} variant={variant} onClick={() => setOpen(true)}>
        <RotateCw size={14} /> {label}
      </Button>
      <RestartDrawer open={open} onClose={() => setOpen(false)} context={context} />
    </>
  );
}
