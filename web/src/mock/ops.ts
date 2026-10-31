/**
 * Fake restart, guided restore and startup status for the mock backend. A
 * restart or restore plays a timeline (stopping → unreachable → the new
 * start's phases → ready) so the console's reconnect flow runs for real;
 * `startup` pins the server in a starting or failed state instead.
 */
import type { RestoreReport, StartupPhase, StartupStatus } from "../lib/ops";

type Segment = {
  /** Length in paces; Infinity holds the phase. */
  paces: number;
  phase: StartupPhase | "down";
  /** The segment belongs to a new start (a different `started`). */
  fresh?: boolean;
  steps?: number;
  failure?: StartupStatus["failure"];
  restoring?: string;
};

export interface OpsOptions {
  /**
   * Pin the server in a startup state: a phase ("waiting_database",
   * "migrating", "restoring", …) or "failed:<code>".
   */
  startup?: string;
  /** Milliseconds per timeline step (default 1500). */
  pace?: number;
  /** Viewers watching (for the restart and restore drawers). */
  viewers?: number;
  /** Seed a restore report ("restored", "failed") or a scheduled restore ("pending"). */
  restore?: string;
  clustered?: () => boolean;
  backups: () => { name: string; size: number; created: string }[];
}

/** The fetch makes no answer at all: the process is between lives. */
export const UNREACHABLE = Symbol("unreachable");

type Result = { status: number; body: unknown } | typeof UNREACHABLE;

const SESSIONS = [
  {
    ID: "s1",
    Key: "movie:1",
    TitleName: "The Long Harbor (2021)",
    Username: "Sam Rivera",
    Client: { product: "Plex for Apple TV", deviceName: "Living Room" },
  },
  {
    ID: "s2",
    Key: "show:2",
    TitleName: "Northern Lights · S2 E4",
    Username: "Alex Chen",
    Client: { product: "Plex for iOS", deviceName: "Alex's iPhone" },
  },
];

export function createOpsMock(opts: OpsOptions) {
  const pace = opts.pace ?? 1500;
  // The running process's start, and the next one's while a timeline plays.
  let started = new Date(Date.now() - 6 * 3600_000).toISOString();
  let next = started;
  let viewers = opts.viewers ?? 0;
  let timeline: { at: number; segments: Segment[]; then?: () => void } | null = null;
  let waiting: { reason: string; requested: string } | null = null;
  let waitTimer = 0;
  let report: RestoreReport | null = null;
  let pending: { staged: string; kind: string; source: string; requested: string } | null = null;
  const staged = new Map<
    string,
    { id: string; kind: string; source: string; created: string; size: number; path: string }
  >();

  if (opts.restore === "restored" || opts.restore === "failed") {
    const backup = opts.backups()[1] ?? opts.backups()[0];
    report = {
      status: opts.restore,
      kind: "snapshot",
      source: backup?.name ?? "ether-20261030T030000.000000000Z.tar.gz",
      backupCreated: backup?.created,
      restoredAt: new Date(Date.now() - 4 * 60_000).toISOString(),
      ...(opts.restore === "restored"
        ? {
            rollback: "restore-20261031T101500.000Z",
            rollbackPath: "/config/rollbacks/restore-20261031T101500.000Z",
            files: ["config.json", ".credentials-key", "ether.db", "ether.db-wal"],
            integrity: { ok: true, checked: new Date(Date.now() - 4 * 60_000).toISOString() },
          }
        : { error: "install ether.db: no space left on device" }),
    };
  }
  if (opts.restore === "pending")
    pending = {
      staged: "restored-1",
      kind: "snapshot",
      source: opts.backups()[0]?.name ?? "",
      requested: new Date().toISOString(),
    };

  if (opts.startup) {
    const [phase, code] = opts.startup.split(":") as [StartupPhase, string | undefined];
    const failure =
      phase === "failed"
        ? {
            code: (code ?? "unknown") as NonNullable<StartupStatus["failure"]>["code"],
            detail:
              code === "database_unreachable"
                ? "dial tcp 10.0.0.12:5432: connect: connection refused"
                : code === "database_corrupt"
                  ? "database: migration catalog/14: database disk image is malformed (11)"
                  : "migration playback/9: no such column: resume_ms",
            recoverable: code !== "database_unreachable" && code !== "database_rejected",
          }
        : undefined;
    timeline = {
      at: Date.now(),
      segments: [
        {
          paces: Infinity,
          phase,
          fresh: true,
          steps: phase === "migrating" ? 14 : undefined,
          failure,
          restoring: phase === "restoring" ? "ether-20261030T030000.000000000Z.tar.gz" : undefined,
        },
      ],
    };
    started = next = new Date(Date.now() - 12_000).toISOString();
  }

  /** Where the timeline is now. */
  function current(): { seg: Segment; offset: number; startedAt: number } | null {
    if (!timeline) return null;
    let t = timeline.at;
    for (const seg of timeline.segments) {
      const end = t + seg.paces * pace;
      if (Date.now() < end) return { seg, offset: Date.now() - t, startedAt: t };
      t = end;
    }
    const done = timeline.then;
    timeline = null;
    started = next;
    done?.();
    return null;
  }

  function play(segments: Segment[], then?: () => void) {
    timeline = { at: Date.now(), segments, then };
    let t = Date.now();
    for (const seg of segments) {
      if (seg.fresh) {
        next = new Date(t).toISOString();
        break;
      }
      t += seg.paces * pace;
    }
  }

  function restartTimeline(restore?: { source: string; created?: string; kind: string }) {
    play(
      [
        { paces: 1, phase: "stopping" },
        { paces: 1, phase: "down" },
        ...(restore
          ? ([
              { paces: 1.5, phase: "restoring", fresh: true, restoring: restore.source },
              { paces: 1.2, phase: "checking_integrity", fresh: true },
            ] as Segment[])
          : ([{ paces: 0.6, phase: "starting", fresh: true }] as Segment[])),
        { paces: 1.5, phase: "migrating", fresh: true, steps: 3 },
        { paces: 0.8, phase: "starting_services", fresh: true },
      ],
      () => {
        if (restore) {
          pending = null;
          report = {
            status: "restored",
            kind: restore.kind as RestoreReport["kind"],
            source: restore.source,
            backupCreated: restore.created,
            restoredAt: new Date().toISOString(),
            rollback: "restore-20261031T101500.000Z",
            rollbackPath: "/config/rollbacks/restore-20261031T101500.000Z",
            files: ["config.json", ".credentials-key", "ether.db"],
            integrity: { ok: true, checked: new Date().toISOString() },
          };
        }
      },
    );
  }

  function schedule(whenIdle: boolean, run: () => void) {
    if (!whenIdle || viewers === 0) {
      run();
      return;
    }
    waiting = { reason: "restart", requested: new Date().toISOString() };
    window.clearTimeout(waitTimer);
    waitTimer = window.setTimeout(() => {
      viewers = 0;
      waiting = null;
      run();
    }, 4 * pace);
  }

  function status(): StartupStatus | typeof UNREACHABLE {
    const now = current();
    const base = {
      started: now?.seg.fresh ? next : started,
      since: new Date().toISOString(),
      elapsedSeconds: Math.round((Date.now() - Date.parse(now?.seg.fresh ? next : started)) / 1000),
      version: "0.3.0",
      message: "",
    };
    if (!now) return { ...base, phase: "ready", ready: true };
    const { seg, offset } = now;
    if (seg.phase === "down") return UNREACHABLE;
    // Before the fresh start began, the old process is the one answering.
    const st: StartupStatus = {
      ...base,
      phase: seg.phase,
      ready: false,
      database: { kind: "sqlite" },
    };
    if (seg.steps) {
      const step = Number.isFinite(seg.paces)
        ? Math.min(seg.steps, 1 + Math.floor((offset / (seg.paces * pace)) * seg.steps))
        : Math.min(seg.steps, 1 + Math.floor(offset / pace) * 2);
      st.step = step;
      st.steps = seg.steps;
    }
    if (seg.phase === "waiting_database") {
      st.database = { kind: "postgres", host: "db.home.lan:5432" };
      st.attempt = 4 + Math.floor(offset / 8000);
      st.retryAt = new Date(Date.now() + 8000 - (offset % 8000)).toISOString();
      st.failure = {
        code: "database_unreachable",
        detail: "dial tcp 10.0.0.12:5432: connect: connection refused",
      };
    }
    if (seg.failure) {
      st.failure = seg.failure;
      st.attempt = 2;
      st.retryAt = new Date(Date.now() + 45_000).toISOString();
      if (seg.failure.code === "database_unreachable")
        st.database = { kind: "postgres", host: "db.home.lan:5432" };
    }
    if (seg.restoring) st.restoring = seg.restoring;
    return st;
  }

  function restartInfo() {
    return {
      method: "supervisor",
      manager: "systemd",
      active: viewers,
      sessions: SESSIONS.slice(0, viewers),
      waiting,
      clustered: opts.clustered?.() ?? false,
    };
  }

  /** Handles ops routes; for other routes says whether the server answers. */
  function handle(method: string, seg: string[], body: unknown, headers: Headers): Result | null {
    const path = "/" + seg.join("/");
    const st = status();
    if (path === "/startup-status")
      return st === UNREACHABLE ? UNREACHABLE : { status: 200, body: st };
    if (st === UNREACHABLE) return UNREACHABLE;
    const ready = st.phase === "ready";
    if (seg[0] === "startup" && seg[1] === "recovery")
      return st.phase === "failed" ? recovery(method, seg.slice(2), headers) : null;
    if (!ready)
      return {
        status: 503,
        body: { error: { code: "starting", message: "Ether is not ready yet", phase: st.phase } },
      };

    if (path === "/server/restart" || path === "/server/restart/") {
      if (method === "GET") return { status: 200, body: restartInfo() };
      if (method === "POST") {
        const whenIdle = Boolean((body as { whenIdle?: boolean } | null)?.whenIdle);
        schedule(whenIdle, () => restartTimeline());
        return { status: 202, body: restartInfo() };
      }
      if (method === "DELETE") {
        const cancelled = waiting !== null || pending !== null;
        window.clearTimeout(waitTimer);
        waiting = null;
        pending = null;
        return { status: 200, body: { cancelled } };
      }
    }
    if (seg[0] === "adoption" && seg[1] === "backups" && seg[2] === "restore")
      return restoreRoute(method, seg.slice(3), body, headers);
    return null;
  }

  function restoreRoute(method: string, rest: string[], body: unknown, headers: Headers): Result {
    const clustered = opts.clustered?.() ?? false;
    if (method === "GET" && rest.length === 0)
      return { status: 200, body: { oneClick: !clustered, report, pending } };
    if (method === "POST" && rest[0] === "snapshot" && rest[1]) {
      const name = decodeURIComponent(rest[1]);
      const snap = opts.backups().find((b) => b.name === name);
      if (!snap)
        return { status: 404, body: { error: { code: "not_found", message: "no such backup" } } };
      const id = `restored-${Date.now()}`;
      const s = {
        id,
        kind: "snapshot",
        source: name,
        created: snap.created,
        size: snap.size * 3.1,
        path: `/config/${id}`,
      };
      staged.set(id, s);
      return { status: 200, body: s };
    }
    if (method === "POST" && rest.length === 0) {
      const pw = headers.get("X-Ether-Backup-Password") ?? "";
      if (pw && atob(pw) === "wrong password")
        return {
          status: 400,
          body: {
            error: { code: "bad_password", message: "wrong password or not an Ether backup" },
          },
        };
      const id = `restored-${Date.now()}`;
      const s = {
        id,
        kind: "upload",
        source:
          decodeURIComponent(headers.get("X-Ether-Backup-Name") ?? "") ||
          "ether-backup.etherbackup",
        created: new Date(Date.now() - 3 * 86400_000).toISOString(),
        size: 48_600_000,
        logical: clustered,
        path: `/config/${id}`,
      };
      staged.set(id, s);
      return { status: 200, body: s };
    }
    if (method === "DELETE" && rest[0] === "staged") {
      staged.delete(rest[1] ?? "");
      return { status: 200, body: { ok: true } };
    }
    if (method === "POST" && (rest[0] === "apply" || rest[0] === "undo")) {
      if (clustered)
        return {
          status: 409,
          body: {
            error: {
              code: "guided_restore",
              message: "a cluster node restores through the guided cluster restore",
            },
          },
        };
      const whenIdle = Boolean((body as { whenIdle?: boolean } | null)?.whenIdle);
      const s = rest[0] === "apply" ? staged.get(rest[1] ?? "") : null;
      const source =
        rest[0] === "undo"
          ? { source: report?.source ?? "", kind: "undo" }
          : { source: s?.source ?? "", created: s?.created, kind: s?.kind ?? "snapshot" };
      pending = {
        staged: rest[1] ?? "undo",
        kind: source.kind,
        source: source.source,
        requested: new Date().toISOString(),
      };
      schedule(whenIdle, () => restartTimeline(source));
      return { status: 202, body: { ok: true } };
    }
    if (method === "DELETE" && rest[0] === "report") {
      report = null;
      return { status: 200, body: { ok: true } };
    }
    return { status: 404, body: { error: { code: "not_found", message: "no such endpoint" } } };
  }

  let signedIn = false;
  function recovery(method: string, rest: string[], headers: Headers): Result {
    const route = `${method} /${rest.join("/")}`;
    if (route === "GET /access")
      return { status: 200, body: { granted: false, plex: true, available: true } };
    if (route === "POST /pin")
      return {
        status: 200,
        body: { handle: "h1", code: "WXYZ", authUrl: "https://app.plex.tv/auth#?code=WXYZ" },
      };
    if (route === "GET /pin/h1") {
      signedIn = true;
      return { status: 200, body: { done: true, token: "ether-recovery-mock" } };
    }
    if (!signedIn && !headers.get("X-Ether-Recovery-Token"))
      return { status: 401, body: { error: { code: "unauthorized", message: "sign in" } } };
    if (route === "GET /backups") return { status: 200, body: { snapshots: opts.backups() } };
    if (method === "POST" && rest[0] === "restore") {
      const name =
        rest[1] === "snapshot" ? decodeURIComponent(rest[2] ?? "") : "upload.etherbackup";
      play([
        { paces: 1.5, phase: "restoring", fresh: true, restoring: name },
        { paces: 1.2, phase: "checking_integrity", fresh: true },
        { paces: 0.8, phase: "starting_services", fresh: true },
      ]);
      return { status: 202, body: { id: "restored-x", source: name } };
    }
    return { status: 404, body: { error: { code: "not_found", message: "no such endpoint" } } };
  }

  return { handle };
}
