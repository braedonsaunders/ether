import { t } from "../i18n";
import { API_BASE, ApiError, apiFetch, getToken, SESSION_HEADER } from "./api";

// ---- Startup status ---------------------------------------------------------

/** What a starting (or stopping) server is doing; see internal/startup. */
export type StartupPhase =
  | "starting"
  | "restoring"
  | "waiting_database"
  | "waiting_cluster"
  | "backing_up"
  | "migrating"
  | "importing"
  | "checking_integrity"
  | "starting_services"
  | "ready"
  | "stopping"
  | "failed";

export type StartupFailureCode =
  | "database_unreachable"
  | "database_rejected"
  | "database_corrupt"
  | "migration_failed"
  | "credential_key"
  | "restore_failed"
  | "config"
  | "unknown";

export interface StartupStatus {
  phase: StartupPhase;
  ready: boolean;
  message: string;
  step?: number;
  steps?: number;
  /** When this start began (a new process or an in-process restart). */
  started: string;
  since: string;
  elapsedSeconds: number;
  attempt?: number;
  retryAt?: string;
  database?: { kind: "sqlite" | "postgres"; host?: string };
  failure?: { code: StartupFailureCode; detail?: string; recoverable?: boolean };
  restoring?: string;
  version?: string;
}

/** The public, read-only startup status. Network failures throw ApiError(0). */
export async function startupStatus(signal?: AbortSignal): Promise<StartupStatus> {
  let res: Response;
  try {
    res = await fetch(API_BASE + "/startup-status", { cache: "no-store", signal });
  } catch (e) {
    if (e instanceof DOMException && e.name === "AbortError") throw e;
    throw new ApiError(0, "network", t("Can't reach Ether"));
  }
  if (!res.ok) throw new ApiError(res.status, "http", t("Can't reach Ether"));
  return (await res.json()) as StartupStatus;
}

// ---- Restart ---------------------------------------------------------------

export type RestartManager =
  "systemd" | "launchd" | "windows-service" | "service" | "container" | "terminal";

export interface RestartInfo {
  method: "supervisor" | "in-place" | "spawn";
  manager: RestartManager;
  /** Playback sessions, transcodes and live TV viewers a restart interrupts. */
  active: number;
  /** Playback sessions (normalizeSessions reads them). */
  sessions: unknown;
  waiting?: { reason: string; requested: string } | null;
  clustered: boolean;
}

export const restartApi = {
  status: () => apiFetch<RestartInfo>("/server/restart/"),
  restart: (whenIdle: boolean) =>
    apiFetch<RestartInfo>("/server/restart/", {
      method: "POST",
      body: JSON.stringify({ whenIdle }),
    }),
  cancel: () => apiFetch<{ cancelled: boolean }>("/server/restart/", { method: "DELETE" }),
};

// ---- Guided restore ----------------------------------------------------------

/** A validated backup waiting in a staging folder. */
export interface StagedBackup {
  id: string;
  kind: "snapshot" | "upload" | "undo";
  source: string;
  created: string;
  /** A cluster's dump: imported into this server's own database. */
  logical?: boolean;
  size: number;
  /** The staging folder (shown for a cluster's guided restore). */
  path: string;
}

export interface RestoreReport {
  status: "restored" | "failed";
  kind?: "snapshot" | "upload" | "undo";
  source: string;
  backupCreated?: string;
  restoredAt: string;
  rollback?: string;
  rollbackPath?: string;
  files?: string[];
  logical?: boolean;
  imported?: boolean;
  integrity?: { ok: boolean; findings?: string[]; checked: string };
  error?: string;
}

export interface RestoreState {
  /** False on a cluster node: its shared database is restored by a guided procedure. */
  oneClick: boolean;
  report: RestoreReport | null;
  pending?: { staged: string; kind: string; source: string; requested: string } | null;
}

const BACKUPS = "/adoption/backups";

/** Header values are Latin-1 only, so the password travels as base64 of UTF-8. */
export function base64Utf8(s: string): string {
  let bin = "";
  for (const b of new TextEncoder().encode(s)) bin += String.fromCharCode(b);
  return btoa(bin);
}

/** The first bytes of every encrypted Ether backup. */
export const ENCRYPTED_MAGIC = "ETHERBK1";

export function readHead(file: Blob, n: number): Promise<string> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader();
    reader.onload = () =>
      resolve(String.fromCharCode(...new Uint8Array(reader.result as ArrayBuffer)));
    reader.onerror = () => reject(reader.error ?? new Error(t("Could not read the file")));
    reader.readAsArrayBuffer(file.slice(0, n));
  });
}

export async function isEncryptedBackup(file: Blob): Promise<boolean> {
  return (await readHead(file, ENCRYPTED_MAGIC.length)) === ENCRYPTED_MAGIC;
}

async function failure(res: Response): Promise<ApiError> {
  const data = (await res.json().catch(() => ({}))) as {
    error?: { code?: string; message?: string };
  };
  return new ApiError(
    res.status,
    data.error?.code ?? "http",
    data.error?.message ?? t("Request failed ({status})", { status: res.status }),
  );
}

/** Uploads a backup file as a raw body (the only non-JSON body the API accepts). */
async function uploadBackup<T>(
  url: string,
  file: File,
  headers: Record<string, string>,
  signal?: AbortSignal,
): Promise<T> {
  let res: Response;
  try {
    res = await fetch(url, {
      method: "POST",
      body: file,
      signal,
      headers: {
        ...headers,
        "X-Ether-Request": "1",
        "X-Ether-Backup-Name": encodeURIComponent(file.name).slice(0, 200),
        "Content-Type": "application/octet-stream",
      },
    });
  } catch (e) {
    if (e instanceof DOMException && e.name === "AbortError") throw e;
    throw new ApiError(0, "network", t("Can't reach Ether"));
  }
  if (!res.ok) throw await failure(res);
  return (await res.json()) as T;
}

export const restoreApi = {
  state: () => apiFetch<RestoreState>(`${BACKUPS}/restore`),
  stageSnapshot: (name: string) =>
    apiFetch<StagedBackup>(`${BACKUPS}/restore/snapshot/${encodeURIComponent(name)}`, {
      method: "POST",
    }),
  /**
   * Validates an uploaded backup into a staging folder: an encrypted download
   * with its password, or (legacy) an unencrypted bundle from an older version.
   */
  stageUpload: (file: File, password: string, legacy = false) =>
    uploadBackup<StagedBackup>(API_BASE + `${BACKUPS}/restore`, file, {
      [SESSION_HEADER]: getToken(),
      ...(legacy
        ? { "X-Ether-Backup-Legacy": "plaintext" }
        : { "X-Ether-Backup-Password": base64Utf8(password) }),
    }),
  apply: (id: string, whenIdle: boolean) =>
    apiFetch<{ ok: boolean }>(`${BACKUPS}/restore/apply/${encodeURIComponent(id)}`, {
      method: "POST",
      body: JSON.stringify({ whenIdle }),
    }),
  discard: (id: string) =>
    apiFetch<{ ok: boolean }>(`${BACKUPS}/restore/staged/${encodeURIComponent(id)}`, {
      method: "DELETE",
    }),
  undo: (whenIdle: boolean) =>
    apiFetch<{ ok: boolean }>(`${BACKUPS}/restore/undo`, {
      method: "POST",
      body: JSON.stringify({ whenIdle }),
    }),
  dismiss: () => apiFetch<{ ok: boolean }>(`${BACKUPS}/restore/report`, { method: "DELETE" }),
};

// ---- Recovery from a failed start --------------------------------------------

const RECOVERY = API_BASE + "/startup/recovery";
const RECOVERY_TOKEN = "X-Ether-Recovery-Token";

async function recoveryFetch<T>(path: string, token: string, init: RequestInit = {}): Promise<T> {
  const headers = new Headers(init.headers);
  if (token) headers.set(RECOVERY_TOKEN, token);
  if ((init.method ?? "GET") !== "GET") headers.set("X-Ether-Request", "1");
  let res: Response;
  try {
    res = await fetch(RECOVERY + path, { ...init, headers, cache: "no-store" });
  } catch {
    throw new ApiError(0, "network", t("Can't reach Ether"));
  }
  if (!res.ok) throw await failure(res);
  return (await res.json()) as T;
}

export interface LocalBackup {
  name: string;
  size: number;
  created: string;
}

/** Recovery tokens live in memory only: a failed start's sign-in, never a console session. */
export const recoveryApi = {
  /** Whether this browser may restore now, and whether Plex sign-in can let it. */
  access: () =>
    recoveryFetch<{ granted: boolean; plex: boolean; available: boolean }>("/access", ""),
  signInStart: () =>
    recoveryFetch<{ handle: string; code: string; authUrl: string }>("/pin", "", {
      method: "POST",
    }),
  signInPoll: (handle: string) =>
    recoveryFetch<{ done: boolean; token?: string }>(`/pin/${encodeURIComponent(handle)}`, ""),
  backups: (token: string) =>
    recoveryFetch<{ snapshots: LocalBackup[] }>("/backups", token).then((r) => r.snapshots ?? []),
  restoreSnapshot: (name: string, token: string) =>
    recoveryFetch<StagedBackup>(`/restore/snapshot/${encodeURIComponent(name)}`, token, {
      method: "POST",
    }),
  restoreUpload: (file: File, password: string, token: string) =>
    uploadBackup<StagedBackup>(RECOVERY + "/restore", file, {
      ...(token ? { [RECOVERY_TOKEN]: token } : {}),
      "X-Ether-Backup-Password": base64Utf8(password),
    }),
};
