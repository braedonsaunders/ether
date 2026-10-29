import { Badge, cn } from "@braedonsaunders/appkit-ui";
import { AlertTriangle, CheckCircle2, CircleSlash, HelpCircle, XCircle } from "lucide-react";
import { localized, t } from "../i18n";
import type { HealthLevel } from "../lib/health";

export const LEVEL_LABEL: Record<HealthLevel, string> = localized(() => ({
  ok: t("Healthy"),
  warn: t("Needs attention"),
  error: t("Problem"),
  off: t("Off"),
  unknown: t("Unknown"),
}));

const DOT: Record<HealthLevel, string> = {
  ok: "bg-success",
  warn: "bg-warning",
  error: "bg-danger",
  off: "bg-fg-subtle",
  unknown: "bg-border-strong",
};

export function StatusDot({
  level,
  pulse,
  className,
}: {
  level: HealthLevel;
  pulse?: boolean;
  className?: string;
}) {
  return (
    <span className={cn("relative inline-flex size-2.5 shrink-0", className)} aria-hidden>
      {pulse && (level === "warn" || level === "error") ? (
        <span
          className={cn(
            "absolute inset-0 animate-ping rounded-full opacity-60 motion-reduce:animate-none",
            DOT[level],
          )}
        />
      ) : null}
      <span className={cn("relative inline-flex size-2.5 rounded-full", DOT[level])} />
    </span>
  );
}

/** Status as a badge: icon + label, never color alone. */
export function LevelBadge({ level, label }: { level: HealthLevel; label?: string }) {
  const variant =
    level === "ok"
      ? "success"
      : level === "warn"
        ? "warning"
        : level === "error"
          ? "destructive"
          : "secondary";
  const Icon =
    level === "ok"
      ? CheckCircle2
      : level === "warn"
        ? AlertTriangle
        : level === "error"
          ? XCircle
          : level === "off"
            ? CircleSlash
            : HelpCircle;
  return (
    <Badge variant={variant}>
      <Icon size={12} aria-hidden /> {label ?? LEVEL_LABEL[level]}
    </Badge>
  );
}
