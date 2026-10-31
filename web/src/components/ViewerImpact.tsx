import { Avatar, cn } from "@braedonsaunders/appkit-ui";
import { CheckCircle2, Clock, Zap } from "lucide-react";
import { useId, useMemo, type ReactNode } from "react";
import { t, tn } from "../i18n";
import { useLocale } from "../i18n/react";
import { normalizeSessions } from "../lib/health";

/**
 * Who a restart would interrupt, and whether to wait for them. Shown before
 * every restart (on its own, or to finish a restore).
 */
export function ViewerImpact({
  active,
  sessions,
  waitForIdle,
  onWaitForIdle,
}: {
  /** Everything a restart interrupts: streams, transcodes, live TV. */
  active: number;
  sessions: unknown;
  waitForIdle: boolean;
  onWaitForIdle: (v: boolean) => void;
}) {
  const id = useId();
  const { locale } = useLocale();
  // Untitled sessions and unknown devices are worded in the active language.
  const streams = useMemo(() => normalizeSessions(sessions), [sessions, locale]);
  if (active === 0)
    return (
      <p className="flex items-center gap-2 rounded-lg border border-border bg-bg-subtle px-3 py-2.5 text-sm text-fg-muted">
        <CheckCircle2 size={16} className="shrink-0 text-success" aria-hidden />
        {t("Nobody is watching right now, so no one is interrupted.")}
      </p>
    );
  const others = Math.max(0, active - streams.length);
  return (
    <fieldset className="space-y-3">
      <legend className="mb-2 text-sm font-medium text-fg">
        {tn(active, "{count} stream is playing", "{count} streams are playing")}
      </legend>
      {streams.length ? (
        <ul className="divide-y divide-border-subtle overflow-hidden rounded-lg border border-border">
          {streams.map((s) => (
            <li key={s.id || s.key + s.user} className="flex items-center gap-3 px-3 py-2">
              <Avatar name={s.user} size={28} />
              <div className="min-w-0 flex-1">
                <p className="truncate text-sm font-medium text-fg">{s.title}</p>
                <p className="truncate text-xs text-fg-muted">
                  {t("{viewer} on {device}", { viewer: s.user, device: s.device })}
                </p>
              </div>
            </li>
          ))}
          {others > 0 ? (
            <li className="px-3 py-2 text-xs text-fg-muted">
              {tn(
                others,
                "and {count} more stream (transcoding or live TV)",
                "and {count} more streams (transcoding or live TV)",
              )}
            </li>
          ) : null}
        </ul>
      ) : null}
      <div
        role="radiogroup"
        aria-label={t("When to restart")}
        className="grid gap-2 sm:grid-cols-2"
      >
        <ChoiceCard
          name={id}
          checked={waitForIdle}
          onSelect={() => onWaitForIdle(true)}
          icon={<Clock size={16} aria-hidden />}
          title={t("Wait until nobody is watching")}
          description={t("Ether restarts by itself when the last stream ends.")}
        />
        <ChoiceCard
          name={id}
          checked={!waitForIdle}
          onSelect={() => onWaitForIdle(false)}
          icon={<Zap size={16} aria-hidden />}
          title={t("Restart now")}
          description={tn(
            active,
            "The stream stops. It can be started again in about a minute.",
            "These streams stop. They can be started again in about a minute.",
          )}
          tone="warning"
        />
      </div>
    </fieldset>
  );
}

function ChoiceCard({
  name,
  checked,
  onSelect,
  icon,
  title,
  description,
  tone,
}: {
  name: string;
  checked: boolean;
  onSelect: () => void;
  icon: ReactNode;
  title: string;
  description: string;
  tone?: "warning";
}) {
  return (
    <label
      className={cn(
        "relative flex cursor-pointer gap-3 rounded-lg border p-3 text-left transition-colors focus-within:ring-2 focus-within:ring-ring/50",
        checked
          ? tone === "warning"
            ? "border-warning bg-warning-subtle"
            : "border-primary bg-primary-subtle"
          : "border-border bg-surface hover:bg-surface-hover",
      )}
    >
      <input type="radio" name={name} checked={checked} onChange={onSelect} className="sr-only" />
      <span
        className={cn(
          "mt-0.5 shrink-0",
          checked ? (tone === "warning" ? "text-warning" : "text-primary") : "text-fg-subtle",
        )}
      >
        {icon}
      </span>
      <span className="min-w-0">
        <span className="block text-sm font-medium text-fg">{title}</span>
        <span className="mt-0.5 block text-xs text-fg-muted">{description}</span>
      </span>
    </label>
  );
}
