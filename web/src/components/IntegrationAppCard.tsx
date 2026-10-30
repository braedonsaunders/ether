import { buttonVariants, cn, Switch } from "@braedonsaunders/appkit-ui";
import { ArrowRight } from "lucide-react";
import type { ReactNode } from "react";
import { Link } from "react-router-dom";
import { t } from "../i18n";

/**
 * Shared branded card for Integrations and Apps.
 *
 * The switch is rendered only when `onToggle` is provided. Omit it for a
 * core or source entry that has no enable flag. This component does not
 * read config, look up a logo from `identity.id`, or call the API. The
 * caller owns persistence, status, and the brand mark (`logo`).
 *
 * ```ts
 * export interface IntegrationAppIdentity {
 *   id: string;
 *   name: string;
 * }
 * export interface IntegrationAppCardProps {
 *   identity: IntegrationAppIdentity;
 *   logo?: ReactNode;
 *   description: string;
 *   status?: ReactNode;
 *   enabled?: boolean;
 *   onToggle?: (enabled: boolean) => void;
 *   configureHref?: string;
 *   configureLabel?: string;
 *   toggleLabel?: string;
 *   label?: string;
 *   disabled?: boolean;
 *   busy?: boolean;
 *   action?: ReactNode;
 * }
 * ```
 */
export interface IntegrationAppIdentity {
  /** Stable key. Not displayed. */
  id: string;
  /** Visible product name. */
  name: string;
}

export interface IntegrationAppCardProps {
  identity: IntegrationAppIdentity;
  /**
   * Caller-supplied brand mark. Never derived from `identity.id`.
   * Omit it only when no real mark exists; the card then shows a neutral
   * monogram of `identity.name`, not a drawn logo.
   */
  logo?: ReactNode;
  description: string;
  /** Caller-owned live status (a Badge, health, or text). Not interpreted. */
  status?: ReactNode;
  /**
   * Switch state. Pass it together with `onToggle`. Ignored when `onToggle`
   * is omitted.
   */
  enabled?: boolean;
  /**
   * When set, the card shows an on/off switch and calls this with the next
   * value. Omit it when the product has no enable semantic.
   */
  onToggle?: (enabled: boolean) => void;
  /** In-app route for the configure control, for example `/apps/plex`. */
  configureHref?: string;
  /** Visible configure label. Defaults to "Configure". */
  configureLabel?: string;
  /**
   * Accessible name of the switch. Defaults to "{app} enabled" with
   * `identity.name`.
   */
  toggleLabel?: string;
  /** Accessible name of the card. Defaults to `identity.name`. */
  label?: string;
  /** Disables the switch. The configure link stays usable. */
  disabled?: boolean;
  /** Disables the switch and marks the card aria-busy. */
  busy?: boolean;
  /** Extra caller-owned control, rendered beside Configure. */
  action?: ReactNode;
}

function monogram(name: string) {
  return name
    .trim()
    .split(/\s+/)
    .slice(0, 2)
    .map((part) => part[0] ?? "")
    .join("")
    .toUpperCase();
}

const configureClass = buttonVariants({ variant: "outline", size: "sm" });

/** One branded product card. Render inside a list item or a grid cell. */
export function IntegrationAppCard({
  identity,
  logo,
  description,
  status,
  enabled,
  onToggle,
  configureHref,
  configureLabel,
  toggleLabel,
  label,
  disabled,
  busy,
  action,
}: IntegrationAppCardProps) {
  const showSwitch = typeof onToggle === "function";
  const switchDisabled = disabled || busy;
  return (
    <article
      aria-label={label ?? identity.name}
      aria-busy={busy || undefined}
      data-app-id={identity.id}
      className="flex h-full flex-col rounded-xl border border-border bg-surface p-5"
    >
      <div className="flex items-start gap-3">
        <span className="flex size-11 shrink-0 items-center justify-center overflow-hidden rounded-lg bg-bg-subtle text-sm font-semibold text-fg-muted [&_img]:size-7 [&_img]:object-contain [&_svg]:size-7">
          {logo ?? monogram(identity.name)}
        </span>
        <div className="min-w-0 flex-1 space-y-1">
          <h3 className="font-semibold text-fg">{identity.name}</h3>
          {status ? <div className="flex flex-wrap gap-1">{status}</div> : null}
        </div>
        {showSwitch ? (
          <Switch
            className="mt-0.5 shrink-0"
            checked={enabled === true}
            disabled={switchDisabled}
            aria-label={toggleLabel ?? t("{app} enabled", { app: identity.name })}
            onChange={(event) => onToggle(event.currentTarget.checked)}
          />
        ) : null}
      </div>
      <p className="mt-3 flex-1 text-sm text-fg-muted">{description}</p>
      {configureHref || action ? (
        <div className="mt-4 flex flex-wrap items-center gap-2">
          {action}
          {configureHref ? (
            <Link to={configureHref} className={cn(configureClass)}>
              {configureLabel ?? t("Configure")} <ArrowRight size={14} />
            </Link>
          ) : null}
        </div>
      ) : null}
    </article>
  );
}
