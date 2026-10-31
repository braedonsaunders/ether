import { t } from "../i18n";
import type { ReconnectStep } from "./reconnect";
import type { StartupFailureCode, StartupStatus } from "./ops";

/** Seconds until an ISO time, never negative. */
export function secondsUntil(iso: string | undefined, now = Date.now()): number {
  if (!iso) return 0;
  return Math.max(0, Math.ceil((new Date(iso).getTime() - now) / 1000));
}

/** 75 → "1:15"; durations past an hour keep counting minutes. */
export function clock(seconds: number): string {
  const s = Math.max(0, Math.floor(seconds));
  return `${Math.floor(s / 60)}:${String(s % 60).padStart(2, "0")}`;
}

/** The step labels of the restart, restore and startup flows. */
export function stepLabel(step: ReconnectStep): string {
  switch (step) {
    case "waiting":
      return t("Waiting for viewers");
    case "stopping":
      return t("Stopping");
    case "restoring":
      return t("Restoring");
    case "starting":
      return t("Starting");
    case "done":
      return t("Done");
    case "failed":
      return t("Couldn't start");
  }
}

function databaseName(s: StartupStatus): string {
  return s.database?.host || t("the database server");
}

/** A starting server's phase as a headline and one line of detail. */
export function phaseText(s: StartupStatus, now = Date.now()): { title: string; detail: string } {
  switch (s.phase) {
    case "starting":
      return { title: t("Starting Ether"), detail: t("Reading your settings…") };
    case "restoring":
      return {
        title: t("Restoring your backup"),
        detail: s.restoring
          ? t("Putting {backup} in place. Your current data is kept so you can undo this.", {
              backup: s.restoring,
            })
          : t("Putting the backup in place. Your current data is kept so you can undo this."),
      };
    case "waiting_database": {
      const wait = secondsUntil(s.retryAt, now);
      return {
        title: t("Waiting for the database"),
        detail:
          wait > 0
            ? t("Can't reach {host} yet. Trying again in {seconds}s (attempt {attempt}).", {
                host: databaseName(s),
                seconds: wait,
                attempt: s.attempt ?? 1,
              })
            : t("Can't reach {host} yet. Trying again now…", { host: databaseName(s) }),
      };
    }
    case "waiting_cluster":
      return {
        title: t("Waiting for the other nodes"),
        detail: t(
          "Older nodes in the cluster must update before this one updates the shared database.",
        ),
      };
    case "backing_up":
      return {
        title: t("Saving a safety copy"),
        detail: t("Copying the database before updating it. Large databases take a little longer."),
      };
    case "migrating":
      return {
        title: t("Updating the database"),
        detail:
          s.step && s.steps
            ? t("Applying update {step} of {steps}…", { step: s.step, steps: s.steps })
            : t("Applying updates…"),
      };
    case "importing":
      return {
        title: t("Importing your data"),
        detail: t("Copying the backup's data into this server's database…"),
      };
    case "checking_integrity":
      return {
        title: t("Checking the restored database"),
        detail: t("Reading every page of the database to make sure it's intact…"),
      };
    case "starting_services":
      return { title: t("Starting services"), detail: t("Almost ready…") };
    case "stopping":
      return { title: t("Stopping Ether"), detail: t("Finishing requests in progress…") };
    case "ready":
      return { title: t("Ether is ready"), detail: "" };
    case "failed":
      return { title: t("Ether couldn't start"), detail: failureText(s).body };
  }
}

/** What went wrong in a failed start, and what to do about it. */
export function failureText(s: StartupStatus): { title: string; body: string } {
  const host = databaseName(s);
  const code: StartupFailureCode = s.failure?.code ?? "unknown";
  switch (code) {
    case "database_unreachable":
      return {
        title: t("Can't reach the database"),
        body: t(
          "Ether can't connect to {host}. Check that PostgreSQL is running and reachable from this machine, and that ETHER_DATABASE_URL is correct.",
          { host },
        ),
      };
    case "database_rejected":
      return {
        title: t("The database refused the connection"),
        body: t(
          "{host} turned Ether away. Check the user name, password and database name in ETHER_DATABASE_URL.",
          { host },
        ),
      };
    case "database_corrupt":
      return {
        title: t("The database is damaged"),
        body: t(
          "Ether's database file can't be read. Restore a backup to get your server running again.",
        ),
      };
    case "migration_failed":
      return {
        title: t("The database couldn't be updated"),
        body: t(
          "This version of Ether couldn't update its database. Restore a backup, or reinstall the version you had before.",
        ),
      };
    case "credential_key":
      return {
        title: t("The settings key doesn't match"),
        body: t(
          "The key that unlocks saved passwords doesn't match this database. Restore a complete backup, which keeps the two together.",
        ),
      };
    case "restore_failed":
      return {
        title: t("The restore couldn't finish"),
        body: t(
          "Ether couldn't put your original files back automatically. They are safe in the rollback folder named below.",
        ),
      };
    case "config":
      return {
        title: t("The settings can't be read"),
        body: t(
          "Ether's settings file is damaged or invalid. Restore a backup, or fix config.json in the data folder.",
        ),
      };
    default:
      return {
        title: t("Something went wrong while starting"),
        body: t("Ether keeps trying by itself. The details below can help if it keeps failing."),
      };
  }
}
