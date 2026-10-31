import {
  Alert,
  AlertDescription,
  AlertTitle,
  Badge,
  Button,
  Spinner,
  cn,
} from "@braedonsaunders/appkit-ui";
import {
  Archive,
  Boxes,
  Copy,
  Database,
  History,
  KeyRound,
  RotateCcw,
  ShieldCheck,
} from "lucide-react";
import { useEffect, useRef, useState, type ReactNode } from "react";
import { Drawer } from "../../components/Drawer";
import { MutationError } from "../../components/feedback";
import { PasswordInput } from "../../components/fields";
import {
  Fact,
  FollowFooter,
  following,
  PlanSkeleton,
  restartHow,
  useRestartPlan,
} from "../../components/RestartDrawer";
import { DoneHero, ServerProgress, useServerReconnect } from "../../components/ServerProgress";
import { ViewerImpact } from "../../components/ViewerImpact";
import { t, tx } from "../../i18n";
import { ApiError, errorMessage } from "../../lib/api";
import { formatBytes, formatDateTime, relativeTime } from "../../lib/format";
import {
  isEncryptedBackup,
  restoreApi,
  type RestoreReport,
  type StagedBackup,
} from "../../lib/ops";

/** What the owner chose to restore. */
export type RestoreSource =
  | { kind: "snapshot"; name: string }
  | { kind: "upload"; file: File; password?: string }
  | { kind: "undo"; report: RestoreReport };

type Stage =
  | { name: "password" }
  | { name: "legacy" }
  | { name: "checking" }
  | { name: "confirm"; staged: StagedBackup | null }
  | { name: "guided"; staged: StagedBackup }
  | { name: "failed"; error: unknown }
  | { name: "restoring"; waitForIdle: boolean };

/** The backup about to be restored, as a card. */
function BackupCard({
  title,
  created,
  size,
  logical,
  icon = <Archive size={18} />,
}: {
  title: string;
  created?: string;
  size?: number;
  logical?: boolean;
  icon?: ReactNode;
}) {
  return (
    <div className="flex items-start gap-3 rounded-xl border border-border bg-bg-subtle p-4">
      <span
        className="grid size-10 shrink-0 place-items-center rounded-lg bg-primary-subtle text-primary"
        aria-hidden
      >
        {icon}
      </span>
      <div className="min-w-0 flex-1">
        <p className="truncate text-sm font-medium text-fg" title={title}>
          {title}
        </p>
        {created ? (
          <p className="mt-0.5 text-sm text-fg-muted">
            {t("Taken {date} · {ago}", {
              date: formatDateTime(created),
              ago: relativeTime(new Date(created)),
            })}
          </p>
        ) : null}
        <div className="mt-2 flex flex-wrap gap-1.5">
          {size ? <Badge variant="secondary">{formatBytes(size)}</Badge> : null}
          {logical ? <Badge variant="info">{t("From a cluster")}</Badge> : null}
        </div>
      </div>
    </div>
  );
}

/**
 * The guided restore: check the backup, say exactly what will happen, then
 * restart into it and follow the server back, ending with the result.
 * Opened from a snapshot, an uploaded file, or the last restore's undo.
 */
export function RestoreDrawer({
  source: requested,
  oneClick,
  onClose,
}: {
  source: RestoreSource | null;
  /** False on a cluster node: the drawer explains the guided procedure. */
  oneClick: boolean;
  onClose: () => void;
}) {
  const open = requested !== null;
  // Keep showing the last backup while the drawer slides out.
  const last = useRef(requested);
  if (requested) last.current = requested;
  const source = requested ?? last.current;
  const plan = useRestartPlan(open);
  const [stage, setStage] = useState<Stage>({ name: "checking" });
  const [password, setPassword] = useState("");
  const [waitForIdle, setWaitForIdle] = useState(true);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>(null);
  const staged = useRef<StagedBackup | null>(null);
  const follow = useServerReconnect({
    baseline: plan.baseline,
    waitForIdle: stage.name === "restoring" && stage.waitForIdle,
    enabled: stage.name === "restoring",
  });

  const check = async (src: RestoreSource, pw: string, legacy = false) => {
    setStage({ name: "checking" });
    setError(null);
    try {
      const st =
        src.kind === "snapshot"
          ? await restoreApi.stageSnapshot(src.name)
          : src.kind === "upload"
            ? await restoreApi.stageUpload(src.file, pw, legacy)
            : null;
      staged.current = st;
      setPassword("");
      setStage(st && !oneClick ? { name: "guided", staged: st } : { name: "confirm", staged: st });
    } catch (e) {
      if (src.kind === "upload" && e instanceof ApiError && e.code === "bad_password") {
        setStage({ name: "password" });
        setError(e);
        return;
      }
      setStage({ name: "failed", error: e });
    }
  };

  useEffect(() => {
    if (!source || !requested) return;
    staged.current = null;
    setError(null);
    setWaitForIdle(true);
    setPassword(source.kind === "upload" ? (source.password ?? "") : "");
    if (source.kind === "undo") {
      setStage({ name: "confirm", staged: null });
      return;
    }
    if (source.kind === "snapshot") {
      void check(source, "");
      return;
    }
    let live = true;
    void isEncryptedBackup(source.file).then(
      (encrypted) => {
        if (!live) return;
        if (!encrypted) setStage({ name: "legacy" });
        else if (source.password) void check(source, source.password);
        else setStage({ name: "password" });
      },
      (e) => live && setStage({ name: "failed", error: e }),
    );
    return () => {
      live = false;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [requested]);

  /** Closing before the restore starts discards the prepared copy. */
  const close = () => {
    if (busy) return;
    // A restore waiting for viewers carries on without this page (the
    // Backups page shows it is scheduled); once it runs, the drawer stays.
    if (stage.name === "restoring") {
      if (follow.step === "waiting") onClose();
      return;
    }
    const st = staged.current;
    staged.current = null;
    if (st && stage.name !== "guided") void restoreApi.discard(st.id).catch(() => undefined);
    onClose();
  };

  const active = plan.info?.active ?? 0;
  const start = async () => {
    setBusy(true);
    setError(null);
    const wait = waitForIdle && active > 0;
    try {
      if (source?.kind === "undo") await restoreApi.undo(wait);
      else if (staged.current) await restoreApi.apply(staged.current.id, wait);
      staged.current = null;
      setStage({ name: "restoring", waitForIdle: wait });
    } catch (e) {
      if (e instanceof ApiError && e.status === 0) {
        staged.current = null;
        setStage({ name: "restoring", waitForIdle: wait });
      } else setError(e);
    } finally {
      setBusy(false);
    }
  };

  if (!source) return null;

  const undo = source.kind === "undo";
  const title = undo ? t("Undo the restore") : t("Restore a backup");
  const created =
    stage.name === "confirm" || stage.name === "guided"
      ? (stage.staged?.created ?? (undo ? source.report.restoredAt : undefined))
      : undefined;

  let body: ReactNode;
  let footer: ReactNode = null;
  switch (stage.name) {
    case "password":
      body = (
        <form
          id="restore-unlock"
          className="space-y-5"
          onSubmit={(e) => {
            e.preventDefault();
            if (password) void check(source, password);
          }}
        >
          <BackupCard
            title={source.kind === "upload" ? source.file.name : ""}
            size={source.kind === "upload" ? source.file.size : undefined}
            icon={<KeyRound size={18} />}
          />
          <div className="space-y-1.5">
            <label htmlFor="restore-password" className="text-sm font-medium text-fg">
              {t("Backup password")}
            </label>
            <PasswordInput
              id="restore-password"
              autoComplete="off"
              autoFocus
              spellCheck={false}
              value={password}
              aria-invalid={error ? true : undefined}
              aria-describedby="restore-password-help"
              onChange={(e) => setPassword(e.currentTarget.value)}
            />
            <p id="restore-password-help" className="text-xs text-fg-muted">
              {t("The password chosen when this backup was downloaded. Ether doesn't keep it.")}
            </p>
          </div>
          <MutationError error={error} />
        </form>
      );
      footer = (
        <div className="flex w-full justify-end gap-2">
          <Button variant="outline" onClick={close}>
            {t("Cancel")}
          </Button>
          <Button type="submit" form="restore-unlock" disabled={!password}>
            {t("Check backup")}
          </Button>
        </div>
      );
      break;
    case "legacy":
      body = (
        <div className="space-y-5">
          <BackupCard
            title={source.kind === "upload" ? source.file.name : ""}
            size={source.kind === "upload" ? source.file.size : undefined}
          />
          <Alert variant="warning">
            <AlertTitle>{t("This backup isn't password protected")}</AlertTitle>
            <AlertDescription>
              {t(
                "It's from an older version of Ether. Restore it only if it came straight from your own server.",
              )}
            </AlertDescription>
          </Alert>
        </div>
      );
      footer = (
        <div className="flex w-full justify-end gap-2">
          <Button variant="outline" onClick={close}>
            {t("Cancel")}
          </Button>
          <Button onClick={() => void check(source, "", true)}>{t("Check backup")}</Button>
        </div>
      );
      break;
    case "checking":
      body = (
        <div className="grid min-h-64 place-items-center text-center" role="status">
          <div className="space-y-3">
            <Spinner size={24} label={t("Checking the backup")} />
            <p className="text-sm font-medium text-fg">{t("Checking the backup…")}</p>
            <p className="mx-auto max-w-xs text-sm text-fg-muted">
              {t(
                "Ether unlocks it, verifies every file's checksum and checks the database before anything changes.",
              )}
            </p>
          </div>
        </div>
      );
      footer = (
        <div className="flex w-full justify-end">
          <Button variant="outline" disabled>
            {t("Cancel")}
          </Button>
        </div>
      );
      break;
    case "failed":
      body = (
        <Alert variant="destructive" role="alert">
          <AlertTitle>{t("This backup can't be restored")}</AlertTitle>
          <AlertDescription>
            <span className="block">{errorMessage(stage.error)}</span>
            <span className="mt-2 block">{t("Nothing on your server was changed.")}</span>
          </AlertDescription>
        </Alert>
      );
      footer = (
        <div className="flex w-full justify-end">
          <Button onClick={close}>{t("Close")}</Button>
        </div>
      );
      break;
    case "guided":
      body = <GuidedRestore staged={stage.staged} />;
      footer = (
        <div className="flex w-full justify-end">
          <Button onClick={close}>{t("Done")}</Button>
        </div>
      );
      break;
    case "confirm":
      body = plan.error ? (
        <MutationError error={plan.error} />
      ) : !plan.info ? (
        <PlanSkeleton />
      ) : (
        <div className="space-y-6">
          {undo ? (
            <BackupCard
              title={t("Your data from before the restore")}
              created={source.report.restoredAt}
              icon={<RotateCcw size={18} />}
            />
          ) : (
            <BackupCard
              title={stage.staged?.source || t("Uploaded backup")}
              created={stage.staged?.created}
              size={stage.staged?.size}
              logical={stage.staged?.logical}
            />
          )}
          <section aria-labelledby="restore-what" className="space-y-3">
            <h3 id="restore-what" className="text-sm font-medium text-fg">
              {t("What happens")}
            </h3>
            <ul className="space-y-2.5">
              <Fact icon={<ShieldCheck size={16} />}>
                {undo
                  ? t(
                      "The restored data is kept in a rollback folder too, so you can change your mind again.",
                    )
                  : t(
                      "Your current settings, database and keys move to a rollback folder first, so you can undo this restore.",
                    )}
              </Fact>
              {created ? (
                <Fact icon={<History size={16} />}>
                  {undo
                    ? t("Changes made since the restore on {date} are replaced.", {
                        date: formatDateTime(created),
                      })
                    : t(
                        "Changes made after {date} — watch history, settings and new viewers — are replaced by the backup.",
                        { date: formatDateTime(created) },
                      )}
                </Fact>
              ) : null}
              {stage.staged?.logical ? (
                <Fact icon={<Database size={16} />}>
                  {t(
                    "This backup came from a cluster. Its data is imported into this server's own database, and this server stays standalone.",
                  )}
                </Fact>
              ) : null}
              <Fact icon={restartHow(plan.info).icon}>
                {t(
                  "Ether restarts to finish. It usually takes about a minute, and this page follows along.",
                )}{" "}
                {restartHow(plan.info).text}
              </Fact>
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
      );
      footer = (
        <div className="flex w-full flex-wrap justify-end gap-2">
          <Button variant="outline" disabled={busy} onClick={close}>
            {t("Cancel")}
          </Button>
          <Button
            variant={active > 0 && !waitForIdle ? "destructive" : "default"}
            disabled={busy || !plan.info}
            onClick={() => void start()}
          >
            <RotateCcw size={15} className={cn(busy && "animate-spin")} />
            {busy
              ? t("Starting…")
              : active > 0 && waitForIdle
                ? undo
                  ? t("Undo when idle")
                  : t("Restore when idle")
                : undo
                  ? t("Undo and restart")
                  : t("Restore and restart")}
          </Button>
        </div>
      );
      break;
    case "restoring":
      body = <ServerProgress kind="restore" state={follow} done={<RestoreOutcome />} />;
      footer = following(follow) ? undefined : (
        <FollowFooter state={follow} onCancelled={onClose} doneLabel={t("Back to Ether")} />
      );
      break;
  }

  return (
    <Drawer
      open={open}
      onClose={close}
      title={title}
      description={
        stage.name === "restoring"
          ? undefined
          : undo
            ? t("Put back the data this server had before the last restore.")
            : t("Check a backup, see what changes, then restore it in one step.")
      }
      size="md"
      footer={footer}
    >
      {body}
    </Drawer>
  );
}

/** Once the server is back: the restore report, or why it can't be read yet. */
function RestoreOutcome() {
  const [report, setReport] = useState<RestoreReport | null>(null);
  const [signedOut, setSignedOut] = useState(false);
  useEffect(() => {
    let live = true;
    restoreApi.state().then(
      (s) => live && setReport(s.report),
      (e) => live && setSignedOut(e instanceof ApiError && (e.status === 401 || e.status === 403)),
    );
    return () => {
      live = false;
    };
  }, []);
  if (signedOut)
    return (
      <DoneHero title={t("Backup restored")}>
        {t(
          "The restored backup has its own sign-ins, so sign in again to continue. The restore report is on the Backups page.",
        )}
      </DoneHero>
    );
  if (!report) return <DoneHero title={t("Ether is back online")} />;
  return <ReportSummary report={report} />;
}

/** The restore report's key facts, as the restore's last word. */
export function ReportSummary({ report }: { report: RestoreReport }) {
  if (report.status !== "restored")
    return (
      <Alert variant="destructive" role="alert">
        <AlertTitle>{t("The restore didn't complete")}</AlertTitle>
        <AlertDescription>
          <span className="block">{t("Your server kept its own data; nothing was replaced.")}</span>
          {report.error ? <span className="mt-1 block text-xs">{report.error}</span> : null}
        </AlertDescription>
      </Alert>
    );
  const integrity = report.integrity;
  return (
    <div className="space-y-4">
      <DoneHero title={report.kind === "undo" ? t("Restore undone") : t("Backup restored")}>
        {report.kind === "undo"
          ? t("Your server is back on the data it had before the restore.")
          : report.backupCreated
            ? t("Your server now runs on the backup taken {date}.", {
                date: formatDateTime(report.backupCreated),
              })
            : t("Your server now runs on the restored backup.")}
      </DoneHero>
      {integrity && !integrity.ok ? (
        <Alert variant="warning" role="alert">
          <AlertDescription>
            {t("The restored database has problems. Restore an earlier backup.")}
          </AlertDescription>
        </Alert>
      ) : integrity || report.logical ? (
        <p className="flex items-center justify-center gap-1.5 text-sm text-success">
          <ShieldCheck size={15} aria-hidden />
          {integrity
            ? t("Database checked — no problems found.")
            : report.imported
              ? t("The cluster's data was imported.")
              : t("The cluster's data is being imported.")}
        </p>
      ) : null}
    </div>
  );
}

/** A cluster node's restore: a shared database is replaced deliberately, cluster-wide. */
function GuidedRestore({ staged }: { staged: StagedBackup }) {
  const [copied, setCopied] = useState(false);
  return (
    <div className="space-y-5">
      <BackupCard
        title={staged.source || t("Uploaded backup")}
        created={staged.created}
        size={staged.size}
        logical={staged.logical}
      />
      <Alert variant="info">
        <AlertTitle>{t("This server is part of a cluster")}</AlertTitle>
        <AlertDescription>
          {t(
            "Every node shares one PostgreSQL database, so a restore replaces it for the whole cluster. To keep the cluster safe, Ether doesn't do this from one node. The backup is checked and ready; follow these steps.",
          )}
        </AlertDescription>
      </Alert>
      <ol className="list-decimal space-y-3 pl-5 text-sm text-fg-muted marker:text-fg-subtle">
        <li>{t("Stop Ether on every node in the cluster.")}</li>
        <li>{t("Create a new, empty PostgreSQL database.")}</li>
        <li>
          {tx(
            "Start this node with ETHER_DATABASE_URL pointing to the empty database and ETHER_DATA_DIR set to {folder}. Ether imports the backup on its first start.",
            { folder: <code className="break-all text-fg">{staged.path}</code> },
          )}
        </li>
        <li>{t("Point the other nodes at the new database and start them.")}</li>
      </ol>
      <Button
        variant="outline"
        size="sm"
        onClick={() =>
          void navigator.clipboard?.writeText(staged.path).then(() => {
            setCopied(true);
            window.setTimeout(() => setCopied(false), 2000);
          })
        }
      >
        <Copy size={14} /> {copied ? t("Copied") : t("Copy folder path")}
      </Button>
      <p className="flex gap-2 text-xs text-fg-subtle">
        <Boxes size={14} className="mt-px shrink-0" aria-hidden />
        {t("Unused prepared backups are removed automatically after a day.")}
      </p>
    </div>
  );
}
