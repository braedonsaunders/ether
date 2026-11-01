import { PageContainer, PageHeader, SubtabNav, cn } from "@braedonsaunders/appkit-ui";
import type { ReactNode } from "react";
import { useNavigate, useParams } from "react-router-dom";
import { t } from "../i18n";

export interface PageTab {
  key: string;
  label: string;
  count?: number;
}

/**
 * The active tab of a domain page, read from the `:tab` route segment so every
 * tab is deep-linkable (search, health links, notifications all land on it).
 */
export function useRouteTab<K extends string>(tabs: readonly { key: K }[], fallback?: K): K {
  const { tab } = useParams();
  const match = tabs.find((x) => x.key === tab);
  return (match?.key ?? fallback ?? tabs[0]!.key) as K;
}

/**
 * One layout for every multi-section page: header, optional live status strip,
 * URL-backed tabs, then the active tab's content edited in place.
 */
export function DomainPage({
  title,
  description,
  actions,
  status,
  base,
  tabs,
  active,
  children,
  className,
}: {
  title: string;
  description?: string;
  actions?: ReactNode;
  /** Live state shown above the tabs (health, budgets, now playing). */
  status?: ReactNode;
  /** Route prefix; tabs navigate to `${base}/${key}`. */
  base: string;
  tabs?: readonly PageTab[];
  active?: string;
  children: ReactNode;
  className?: string;
}) {
  const navigate = useNavigate();
  return (
    <PageContainer className={cn("space-y-6", className)}>
      <PageHeader title={title} description={description} actions={actions} />
      {status}
      {tabs && tabs.length > 1 ? (
        <div className="border-b border-border">
          <SubtabNav
            ariaLabel={t("{page} sections", { page: title })}
            active={active ?? tabs[0]!.key}
            onSelect={(key) => navigate(`${base}/${key}`)}
            tabs={tabs.map((x) => ({ key: x.key, label: x.label, count: x.count }))}
          />
        </div>
      ) : null}
      <div className="space-y-6 pb-4">{children}</div>
    </PageContainer>
  );
}

/**
 * A titled block of in-place content. Use AppKit `SettingsSection` +
 * `SettingsRow` for label/control rows; use this for free-form content (lists,
 * previews, tools) that still needs the same card chrome.
 */
export function Section({
  title,
  description,
  descriptionClassName,
  actions,
  footer,
  children,
  className,
  bodyClassName,
  id,
}: {
  title?: ReactNode;
  description?: ReactNode;
  descriptionClassName?: string;
  actions?: ReactNode;
  footer?: ReactNode;
  children: ReactNode;
  className?: string;
  bodyClassName?: string;
  id?: string;
}) {
  return (
    <section
      id={id}
      aria-label={typeof title === "string" ? title : undefined}
      className={cn("overflow-hidden rounded-xl border border-border bg-surface", className)}
    >
      {title || description || actions ? (
        <div className="flex flex-wrap items-start justify-between gap-3 border-b border-border px-5 py-4">
          <div className="min-w-0">
            {title ? <h2 className="text-sm font-semibold text-fg">{title}</h2> : null}
            {description ? (
              <p
                className={cn("mt-0.5 text-sm text-fg-muted", descriptionClassName ?? "max-w-2xl")}
              >
                {description}
              </p>
            ) : null}
          </div>
          {actions ? (
            <div className="flex shrink-0 flex-wrap items-center gap-2">{actions}</div>
          ) : null}
        </div>
      ) : null}
      <div className={bodyClassName ?? "space-y-4 px-5 py-4"}>{children}</div>
      {footer ? (
        <div className="flex flex-wrap items-center justify-end gap-2 border-t border-border bg-bg-subtle px-5 py-3">
          {footer}
        </div>
      ) : null}
    </section>
  );
}
