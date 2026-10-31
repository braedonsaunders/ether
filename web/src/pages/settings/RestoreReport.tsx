import { Alert, AlertDescription, AlertTitle, Button, cn, toast } from "@braedonsaunders/appkit-ui";
import { CheckCircle2, Clock, RotateCcw, ShieldAlert, XCircle } from "lucide-react";
import { useState, type ReactNode } from "react";
import { t } from "../../i18n";
import { errorMessage } from "../../lib/api";
import { formatDateTime, relativeTime } from "../../lib/format";
import { restartApi, restoreApi, type RestoreReport } from "../../lib/ops";

function Fact({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="min-w-0">
      <dt className="text-xs font-medium uppercase tracking-wide text-fg-subtle">{label}</dt>
      <dd className="mt-1 text-sm text-fg">{children}</dd>
    </div>
  );
}

/**
 * The last restore's report, kept on the Backups page until dismissed: what
 * was restored, from when, the integrity check, and where the replaced data
 * is kept, with undo.
 */
export function RestoreReportPanel({
  report,
  onUndo,
  onDismissed,
}: {
  report: RestoreReport;
  onUndo: () => void;
  onDismissed: () => void;
}) {
  const [busy, setBusy] = useState(false);
  const ok = report.status === "restored";
  const damaged = ok && report.integrity && !report.integrity.ok;
  const dismiss = async () => {
    setBusy(true);
    try {
      await restoreApi.dismiss();
      onDismissed();
    } catch (e) {
      toast.error(t("Couldn't dismiss the report"), { description: errorMessage(e) });
    } finally {
      setBusy(false);
    }
  };
  const title = !ok
    ? report.rolledBack
      ? t("The backup couldn't start")
      : t("The restore didn't complete")
    : report.kind === "undo"
      ? t("Restore undone")
      : t("Backup restored");
  return (
    <section
      aria-label={title}
      className={cn(
        "overflow-hidden rounded-xl border bg-surface",
        !ok ? "border-danger/40" : damaged ? "border-warning/50" : "border-success/40",
      )}
    >
      <div className="flex flex-col gap-3 px-5 py-4 sm:flex-row sm:items-start sm:gap-4">
        <span
          aria-hidden
          className={cn(
            "grid size-10 shrink-0 place-items-center rounded-full",
            !ok
              ? "bg-danger-subtle text-danger"
              : damaged
                ? "bg-warning-subtle text-warning"
                : "bg-success-subtle text-success",
          )}
        >
          {!ok ? (
            <XCircle size={20} />
          ) : damaged ? (
            <ShieldAlert size={20} />
          ) : (
            <CheckCircle2 size={20} />
          )}
        </span>
        <div className="min-w-0 sm:flex-1">
          <h2 className="text-sm font-semibold text-fg">{title}</h2>
          <p className="mt-0.5 text-sm text-fg-muted">
            {!ok
              ? report.rolledBack
                ? t("Ether put your previous data back automatically, so nothing was lost.")
                : t("Your server kept its own data; nothing was replaced.")
              : report.kind === "undo"
                ? t("Your server is back on the data it had before the restore.")
                : t("Ether restarted on the backup {when}.", {
                    when: relativeTime(new Date(report.restoredAt)),
                  })}
          </p>
        </div>
        <div className="flex flex-wrap gap-2 sm:shrink-0">
          {ok && report.rollback ? (
            <Button size="sm" variant="outline" disabled={busy} onClick={onUndo}>
              <RotateCcw size={14} /> {t("Undo restore")}
            </Button>
          ) : null}
          <Button size="sm" variant="ghost" disabled={busy} onClick={() => void dismiss()}>
            {t("Dismiss")}
          </Button>
        </div>
      </div>
      <dl className="grid gap-4 border-t border-border bg-bg-subtle px-5 py-4 sm:grid-cols-2">
        {report.source ? (
          <Fact label={report.kind === "undo" ? t("Undid the restore of") : t("Restored from")}>
            <span className="block truncate" title={report.source}>
              {report.source}
            </span>
          </Fact>
        ) : null}
        {report.backupCreated && report.kind !== "undo" ? (
          <Fact label={t("Backup taken")}>{formatDateTime(report.backupCreated)}</Fact>
        ) : null}
        {!ok ? (
          <Fact label={t("What happened")}>
            <span className="break-words">{report.error || t("Unknown error")}</span>
          </Fact>
        ) : (
          <Fact label={t("Database check")}>
            {report.integrity ? (
              report.integrity.ok ? (
                <span className="text-success">{t("No problems found")}</span>
              ) : (
                <span className="text-warning">
                  {t("Problems found — restore an earlier backup")}
                </span>
              )
            ) : report.logical ? (
              report.imported ? (
                t("Imported from a cluster backup")
              ) : (
                t("Import pending")
              )
            ) : (
              <span className="text-fg-muted">{t("Not checked")}</span>
            )}
          </Fact>
        )}
        {ok && report.rollbackPath ? (
          <Fact label={t("Your previous data")}>
            <code className="block truncate text-xs text-fg-muted" title={report.rollbackPath}>
              {report.rollbackPath}
            </code>
            <span className="mt-1 block text-xs text-fg-subtle">
              {t("Kept until three newer restores replace it.")}
            </span>
          </Fact>
        ) : null}
      </dl>
      {damaged && report.integrity?.findings?.length ? (
        <details className="border-t border-border px-5 py-3 text-xs text-fg-muted">
          <summary className="cursor-pointer select-none font-medium text-fg">
            {t("Technical details")}
          </summary>
          <ul className="mt-2 space-y-1 font-mono">
            {report.integrity.findings.map((f, i) => (
              <li key={i} className="break-words">
                {f}
              </li>
            ))}
          </ul>
        </details>
      ) : null}
    </section>
  );
}

/** A restore waiting until nobody is watching; it can be called off. */
export function PendingRestore({ onCancelled }: { onCancelled: () => void }) {
  const [busy, setBusy] = useState(false);
  return (
    <Alert variant="info" role="status">
      <AlertTitle className="flex items-center gap-2">
        <Clock size={15} aria-hidden /> {t("A restore is scheduled")}
      </AlertTitle>
      <AlertDescription>
        <span className="block">
          {t("Ether restores the backup and restarts as soon as nobody is watching.")}
        </span>
        <Button
          size="sm"
          variant="outline"
          className="mt-3"
          disabled={busy}
          onClick={() => {
            setBusy(true);
            void restartApi
              .cancel()
              .then(
                () => {
                  toast.success(t("Scheduled restore cancelled"));
                  onCancelled();
                },
                (e) =>
                  toast.error(t("Couldn't cancel the restore"), { description: errorMessage(e) }),
              )
              .finally(() => setBusy(false));
          }}
        >
          {t("Cancel the restore")}
        </Button>
      </AlertDescription>
    </Alert>
  );
}
