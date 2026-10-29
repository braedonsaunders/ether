import {
  Alert,
  AlertDescription,
  AlertTitle,
  Button,
  Skeleton,
  Spinner,
} from "@braedonsaunders/appkit-ui";
import { RefreshCw } from "lucide-react";
import type { ReactNode } from "react";
import { t } from "../i18n";
import { ApiError, errorMessage } from "../lib/api";

export function LoadingRows({ rows = 3, className }: { rows?: number; className?: string }) {
  return (
    <div className={className} role="status" aria-label={t("Loading")}>
      <div className="space-y-3">
        {Array.from({ length: rows }, (_, i) => (
          <Skeleton key={i} className="h-14 w-full rounded-lg" />
        ))}
      </div>
    </div>
  );
}

export function LoadingInline({ label = t("Loading…") }: { label?: string }) {
  return (
    <span role="status" className="inline-flex items-center gap-2 text-sm text-fg-muted">
      <Spinner size={14} label={label} /> {label}
    </span>
  );
}

export function PosterSkeletons({
  count = 12,
  className = "grid grid-cols-[repeat(auto-fill,minmax(7.5rem,1fr))] gap-3",
}: {
  count?: number;
  className?: string;
}) {
  return (
    <div className={className} role="status" aria-label={t("Loading titles")}>
      {Array.from({ length: count }, (_, i) => (
        <Skeleton key={i} className="aspect-[2/3] w-full rounded-md" />
      ))}
    </div>
  );
}

/** Error panel for a failed load, with an optional retry. */
export function ErrorPanel({
  error,
  onRetry,
  className,
}: {
  error: unknown;
  onRetry?: () => void;
  className?: string;
}) {
  if (!error) return null;
  return (
    <Alert variant="destructive" className={className} role="alert">
      <AlertTitle>
        {error instanceof ApiError && error.status === 0
          ? t("Can't reach Ether")
          : t("Something went wrong")}
      </AlertTitle>
      <AlertDescription>
        <span className="block">{errorMessage(error)}</span>
        {onRetry ? (
          <Button variant="outline" size="sm" className="mt-3" onClick={onRetry}>
            <RefreshCw size={14} /> {t("Try again")}
          </Button>
        ) : null}
      </AlertDescription>
    </Alert>
  );
}

/** Inline banner for mutation errors, with validation field messages listed. */
export function MutationError({ error }: { error: unknown }) {
  if (!error) return null;
  const fields = error instanceof ApiError ? (error.fields ?? []) : [];
  return (
    <Alert variant="destructive" role="alert">
      <AlertTitle>
        {fields.length ? t("Please fix the highlighted fields") : t("Couldn't save")}
      </AlertTitle>
      <AlertDescription>
        {fields.length ? (
          <ul className="mt-1 list-disc space-y-0.5 pl-5">
            {fields.map((f) => (
              <li key={f.field}>
                <code className="text-xs">{f.field}</code> {f.message}
              </li>
            ))}
          </ul>
        ) : (
          errorMessage(error)
        )}
      </AlertDescription>
    </Alert>
  );
}

export function SectionHeading({
  title,
  description,
  actions,
  id,
}: {
  title: string;
  description?: ReactNode;
  actions?: ReactNode;
  id?: string;
}) {
  return (
    <div className="mb-3 flex flex-wrap items-end justify-between gap-2">
      <div className="min-w-0">
        <h2 id={id} className="text-sm font-semibold text-fg">
          {title}
        </h2>
        {description ? <p className="mt-0.5 text-sm text-fg-muted">{description}</p> : null}
      </div>
      {actions ? <div className="flex shrink-0 items-center gap-2">{actions}</div> : null}
    </div>
  );
}
