import {
  Alert,
  AlertDescription,
  Button,
  Input,
  PagedTable,
  SearchSelect,
  SettingsSection,
  confirmDialog,
  toast,
} from "@braedonsaunders/appkit-ui";
import { Download, FileUp, Plus } from "lucide-react";
import { useCallback, useEffect, useState, type ReactNode } from "react";
import { ErrorPanel, LoadingRows } from "../../components/feedback";
import { useClustered } from "./Cluster";
import { NumberInput } from "../../components/fields";
import { Section } from "../../components/layout";
import { SaveBar, useDraft } from "../../components/SaveBar";
import { SettingsRow } from "../../components/SettingsRow";
import { getLocale, localized, t, tn, tx } from "../../i18n";
import { api, apiBlob, apiFetch, API_BASE, errorMessage, getToken } from "../../lib/api";
import { formatBytes } from "../../lib/format";
import type { EtherUser } from "../../lib/types";
import { pagedTableLabels, searchSelectLabels } from "../../i18n/appkit-labels";

type Plan = {
  id: string;
  imported: boolean;
  items: { title: string; key: number; views: number; rating: number; offsetMs: number }[];
  lists: { name: string; kind: string; keys: number[] }[];
  warnings: string[];
};
type Schedule = { intervalHours: number; retain: number; sharedDir?: string };
type Snapshot = {
  name: string;
  size: number;
  created: string;
  /** Clustered servers: the node that wrote (and, unless shared, holds) the bundle. */
  node?: string;
  nodeId?: string;
  /** In the shared backup directory every node reaches. */
  shared?: boolean;
  /** On another node's disk: download it from that node. */
  remote?: boolean;
};
type Backups = {
  snapshots: Snapshot[] | null;
  settings: Schedule;
  warning: string;
};

/** Where a clustered snapshot lives, for the Location column. */
function snapshotLocation(s: Snapshot): string {
  if (s.shared) return t("Shared folder");
  const node = s.node || s.nodeId || "";
  if (!node) return "";
  return s.remote ? node : t("{node} (this node)", { node });
}

const base = "/adoption";
const post = <T,>(path: string, body: unknown = {}) =>
  apiFetch<T>(base + path, { method: "POST", body: JSON.stringify(body) });

function saveBlob(blob: Blob, name: string) {
  const url = URL.createObjectURL(blob);
  const anchor = document.createElement("a");
  anchor.href = url;
  anchor.download = name;
  anchor.click();
  window.setTimeout(() => URL.revokeObjectURL(url), 1000);
}

async function download(path: string, name: string) {
  saveBlob(await apiBlob(API_BASE + base + path), name);
}

async function failure(res: Response): Promise<Error> {
  const data = (await res.json().catch(() => ({}))) as { error?: { message?: string } };
  return new Error(data.error?.message ?? t("Request failed ({status})", { status: res.status }));
}

async function upload(
  path: string,
  file: File,
  contentType: string,
  headers: Record<string, string> = {},
) {
  const res = await fetch(API_BASE + base + path, {
    method: "POST",
    body: file,
    headers: {
      ...headers,
      "X-Plex-Token": getToken(),
      "X-Ether-Request": "1",
      "Content-Type": contentType,
    },
  });
  if (!res.ok) throw await failure(res);
  return (await res.json()) as unknown;
}

/** Backups leave the server encrypted with a password of at least this length. */
export const MIN_BACKUP_PASSWORD = 12;
const ENCRYPTED_MAGIC = "ETHERBK1";

/** Downloads a snapshot encrypted with password (never stored). */
async function exportBackup(name: string, password: string) {
  const res = await fetch(API_BASE + base + "/backups/export/" + encodeURIComponent(name), {
    method: "POST",
    body: JSON.stringify({ password }),
    headers: {
      "X-Plex-Token": getToken(),
      "X-Ether-Request": "1",
      "Content-Type": "application/json",
    },
  });
  if (!res.ok) throw await failure(res);
  saveBlob(await res.blob(), name.replace(/\.tar\.gz$/, "") + ".etherbackup");
}

function readHead(file: File, n: number): Promise<string> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader();
    reader.onload = () =>
      resolve(String.fromCharCode(...new Uint8Array(reader.result as ArrayBuffer)));
    reader.onerror = () => reject(reader.error ?? new Error(t("Could not read the file")));
    reader.readAsArrayBuffer(file.slice(0, n));
  });
}

/** Header values are Latin-1 only, so the password travels as base64 of UTF-8. */
function base64Utf8(s: string): string {
  let bin = "";
  for (const b of new TextEncoder().encode(s)) bin += String.fromCharCode(b);
  return btoa(bin);
}

/**
 * Restores an uploaded bundle: an encrypted download with its password, or
 * (after an explicit confirmation) an unencrypted bundle from an older
 * version. Returns the staged folder, or null when the owner cancelled.
 */
async function restoreBackup(file: File, password: string): Promise<string | null> {
  const headers: Record<string, string> = {};
  if ((await readHead(file, ENCRYPTED_MAGIC.length)) === ENCRYPTED_MAGIC) {
    if (!password) throw new Error(t("Enter the backup password first."));
    headers["X-Ether-Backup-Password"] = base64Utf8(password);
  } else {
    const ok = await confirmDialog({
      title: t("Restore an unencrypted backup?"),
      message: t(
        "This bundle is from an older version and is not password protected. Restore it only if it came straight from your own server.",
      ),
      confirmLabel: t("Restore"),
    });
    if (!ok) return null;
    headers["X-Ether-Backup-Legacy"] = "plaintext";
  }
  const result = (await upload("/backups/restore", file, "application/octet-stream", headers)) as {
    path?: string;
  };
  return result.path ?? "";
}

/** Runs one action at a time, reporting failures as toasts. */
function useBusy() {
  const [busy, setBusy] = useState(false);
  const run = useCallback(async (fn: () => Promise<unknown>) => {
    setBusy(true);
    try {
      await fn();
      return true;
    } catch (e) {
      toast.error(errorMessage(e));
      return false;
    } finally {
      setBusy(false);
    }
  }, []);
  return { busy, run };
}

function useImportViewers() {
  const [users, setUsers] = useState<EtherUser[] | null>(null);
  useEffect(() => {
    let live = true;
    api.users().then(
      (us) => {
        if (live) setUsers(us);
      },
      (e) => toast.error(errorMessage(e)),
    );
    return () => {
      live = false;
    };
  }, []);
  return users;
}

/** A native file picker styled as a button. */
function FilePick({
  label,
  accept,
  disabled,
  onFile,
}: {
  label: string;
  accept: string;
  disabled?: boolean;
  onFile: (file: File) => void;
}) {
  return (
    <label
      className={`relative inline-flex h-8 cursor-pointer items-center gap-1.5 rounded-md border border-border bg-surface px-3 text-sm font-medium text-fg shadow-sm hover:bg-surface-hover focus-within:ring-2 focus-within:ring-ring/50 ${disabled ? "pointer-events-none opacity-50" : ""}`}
    >
      <FileUp size={14} /> {label}
      <input
        className="sr-only"
        type="file"
        accept={accept}
        aria-label={label}
        disabled={disabled}
        onChange={(e) => {
          const file = e.currentTarget.files?.[0];
          if (file) onFile(file);
          e.currentTarget.value = "";
        }}
      />
    </label>
  );
}

/**
 * Backups & import: the automatic backup schedule edited in place, snapshots
 * and restore on one tab; one-off imports from Plex, Trakt and Letterboxd on
 * the other. Both stay mounted so a half-finished import survives a tab switch.
 */
export function BackupsAndImport({ tab = "backups" }: { tab?: "backups" | "import" }) {
  const users = useImportViewers();
  const clustered = useClustered();
  const [data, setData] = useState<Backups | null>(null);
  const [loadError, setLoadError] = useState<unknown>(null);
  const [restored, setRestored] = useState("");
  // Held in memory only: never saved, and gone when the page closes.
  const [password, setPassword] = useState("");
  const passwordOK = password.length >= MIN_BACKUP_PASSWORD;
  const [saveError, setSaveError] = useState<unknown>(null);
  const [saving, setSaving] = useState(false);
  const { busy, run } = useBusy();
  const { draft, setDraft, dirty, reset } = useDraft<Schedule>(data?.settings);

  const load = useCallback(async () => {
    try {
      setData(await apiFetch<Backups>(base + "/backups/"));
      setLoadError(null);
    } catch (e) {
      setLoadError(e);
    }
  }, []);
  useEffect(() => {
    void load();
  }, [load]);

  const saveSchedule = async () => {
    if (!draft) return;
    setSaving(true);
    setSaveError(null);
    try {
      await post("/backups/settings", {
        intervalHours: draft.intervalHours,
        retain: draft.retain,
        ...(clustered ? { sharedDir: (draft.sharedDir ?? "").trim() } : {}),
      });
      toast.success(t("Settings saved"), { description: t("Automatic backups") });
      await load();
    } catch (e) {
      setSaveError(e);
    } finally {
      setSaving(false);
    }
  };

  return (
    <>
      <div hidden={tab !== "backups"} className="space-y-6">
        {loadError && !data ? (
          <ErrorPanel error={loadError} onRetry={() => void load()} />
        ) : (
          <>
            <SettingsSection
              title={t("Automatic backups")}
              description={t("Snapshots hold your settings and a consistent copy of the database.")}
            >
              {draft ? (
                <>
                  <SettingsRow
                    title={<label htmlFor="backup-interval">{t("Back up every")}</label>}
                    description={t("0 turns automatic backups off.")}
                    control={
                      <span className="flex items-center gap-2">
                        <NumberInput
                          id="backup-interval"
                          aria-label={t("Back up every")}
                          className="w-24"
                          min={0}
                          max={8760}
                          step={1}
                          value={draft.intervalHours}
                          onChange={(n) => setDraft({ ...draft, intervalHours: n })}
                        />
                        <span className="text-sm text-fg-muted">{t("hours")}</span>
                      </span>
                    }
                  />
                  <SettingsRow
                    title={<label htmlFor="backup-retain">{t("Snapshots to keep")}</label>}
                    description={t(
                      "The oldest snapshot is removed when a new one would exceed this.",
                    )}
                    control={
                      <NumberInput
                        id="backup-retain"
                        aria-label={t("Snapshots to keep")}
                        className="w-24"
                        min={1}
                        max={365}
                        step={1}
                        value={draft.retain}
                        onChange={(n) => setDraft({ ...draft, retain: n })}
                      />
                    }
                  />
                  {clustered ? (
                    <SettingsRow
                      stacked
                      title={<label htmlFor="backup-shared-dir">{t("Shared backup folder")}</label>}
                      description={t(
                        "A folder every node reaches at the same path, such as a network share. Leave empty to keep each backup on the node that made it.",
                      )}
                    >
                      <Input
                        id="backup-shared-dir"
                        placeholder="/mnt/backups/ether"
                        value={draft.sharedDir ?? ""}
                        onChange={(e) => setDraft({ ...draft, sharedDir: e.currentTarget.value })}
                      />
                    </SettingsRow>
                  ) : null}
                </>
              ) : (
                <div className="px-5 py-4">
                  <LoadingRows rows={2} />
                </div>
              )}
            </SettingsSection>

            <Section
              title={t("Snapshots")}
              description={t(
                "Snapshots hold your settings, database and the key that unlocks saved credentials. Ones kept on this server are readable only by Ether's account; downloads are encrypted with the password below.",
              )}
              actions={
                <Button
                  size="sm"
                  disabled={busy}
                  onClick={() => void run(() => post("/backups/").then(load))}
                >
                  <Plus size={14} /> {busy ? t("Working…") : t("Create snapshot")}
                </Button>
              }
            >
              {data?.warning ? (
                <Alert variant="warning" role="status">
                  <AlertDescription>{data.warning}</AlertDescription>
                </Alert>
              ) : null}
              <SettingsRow
                stacked
                title={<label htmlFor="backup-password">{t("Backup password")}</label>}
                description={t(
                  "Downloads are encrypted with it and restoring one asks for it again. At least {min} characters; Ether does not keep it.",
                  { min: MIN_BACKUP_PASSWORD },
                )}
              >
                <Input
                  id="backup-password"
                  type="password"
                  autoComplete="new-password"
                  spellCheck={false}
                  value={password}
                  onChange={(e) => setPassword(e.currentTarget.value)}
                />
              </SettingsRow>
              {data ? (
                <PagedTable
                  rows={data.snapshots ?? []}
                  rowKey={(s) => s.name}
                  pageSize={10}
                  empty={t("No snapshots yet. Create your first backup.")}
                  defaultSort={{ key: "created", dir: "desc" }}
                  labels={pagedTableLabels({ searchLabel: t("Search backups") })}
                  columns={[
                    {
                      key: "created",
                      header: t("Created"),
                      search: (s) => s.name,
                      sortValue: (s) => s.created,
                      cell: (s) => new Date(s.created).toLocaleString(getLocale()),
                    },
                    {
                      key: "size",
                      header: t("Size"),
                      align: "right",
                      sortValue: (s) => s.size,
                      cell: (s) => <span className="tabular-nums">{formatBytes(s.size)}</span>,
                    },
                    ...((data.snapshots ?? []).some((s) => snapshotLocation(s))
                      ? [
                          {
                            key: "location",
                            header: t("Location"),
                            search: snapshotLocation,
                            sortValue: snapshotLocation,
                            cell: (s: Snapshot) => (
                              <span className="text-fg-muted">{snapshotLocation(s) || "—"}</span>
                            ),
                          },
                        ]
                      : []),
                    {
                      key: "download",
                      header: "",
                      align: "right",
                      cell: (s) => (
                        <Button
                          variant="outline"
                          size="sm"
                          disabled={busy || s.remote || !passwordOK}
                          title={
                            s.remote
                              ? s.node
                                ? t("Stored on {node}: download it from that node's admin page", {
                                    node: s.node,
                                  })
                                : t(
                                    "Stored on another node: download it from that node's admin page",
                                  )
                              : !passwordOK
                                ? t("Enter a backup password to download")
                                : undefined
                          }
                          aria-label={t("Download {name}", { name: s.name })}
                          onClick={() => void run(() => exportBackup(s.name, password))}
                        >
                          <Download size={14} /> {t("Download")}
                        </Button>
                      ),
                    },
                  ]}
                />
              ) : (
                <LoadingRows rows={3} />
              )}
            </Section>

            <Section
              title={t("Restore a backup")}
              description={t(
                "Ether decrypts the download with the backup password, checks its checksums and database, then prepares it in a separate data folder. Nothing changes until you stop Ether and start it from that folder.",
              )}
            >
              <FilePick
                label={t("Choose backup bundle")}
                accept=".etherbackup,.tar.gz,.gz"
                disabled={busy}
                onFile={(file) =>
                  void run(async () => {
                    const path = await restoreBackup(file, password);
                    if (path === null) return;
                    setRestored(path);
                    toast.success(t("Restore validated and staged"));
                  })
                }
              />
              {restored ? (
                <Alert variant="success" role="status">
                  <AlertDescription>
                    {tx(
                      "Ready to restore. Stop Ether and start it with the data folder {folder}.",
                      {
                        folder: <code className="break-all">{restored}</code>,
                      },
                    )}
                  </AlertDescription>
                </Alert>
              ) : null}
            </Section>
          </>
        )}
      </div>
      <div hidden={tab !== "import"} className="space-y-6">
        <PlexImport users={users} />
        <CsvTools users={users} />
      </div>

      <SaveBar
        dirty={dirty}
        saving={saving}
        error={saveError}
        onSave={saveSchedule}
        onDiscard={() => {
          reset();
          setSaveError(null);
        }}
      />
    </>
  );
}

// ---- Imports ---------------------------------------------------------------------------

function ViewerPick({
  label,
  users,
  value,
  onChange,
}: {
  label: string;
  users: EtherUser[] | null;
  value: string;
  onChange: (v: string) => void;
}) {
  return (
    <div className="w-full sm:w-64">
      <SearchSelect
        {...searchSelectLabels()}
        ariaLabel={label}
        placeholder={users ? t("Choose a viewer") : t("Loading viewers…")}
        value={value}
        onChange={onChange}
        options={(users ?? []).map((u) => ({ value: String(u.id), label: u.title || u.username }))}
      />
    </div>
  );
}

/** Step two of every import: what will change, and the explicit import action. */
function ImportReview({
  id,
  plan,
  busy,
  onImport,
}: {
  id: string;
  plan: Plan;
  busy: boolean;
  onImport: () => void;
}) {
  return (
    <Section
      id={id}
      title={plan.imported ? t("Imported") : t("Review import")}
      description={`${tn(plan.items.length, "{count} watch record", "{count} watch records")} · ${tn(
        plan.lists.length,
        "{count} playlist or collection",
        "{count} playlists and collections",
      )}`}
      footer={
        <Button size="sm" disabled={busy || plan.imported} onClick={onImport}>
          {plan.imported ? t("Import complete") : t("Import reviewed records")}
        </Button>
      }
    >
      {plan.warnings.length > 0 ? (
        <ul className="max-h-52 list-disc space-y-1 overflow-auto pl-5 text-sm text-warning">
          {plan.warnings.map((w, i) => (
            <li key={i}>{w}</li>
          ))}
        </ul>
      ) : null}
      {plan.items.length ? (
        <ul className="max-h-64 space-y-0.5 overflow-auto text-sm">
          {plan.items.slice(0, 100).map((it, i) => (
            <li key={i}>
              {it.title} · {tn(it.views, "{count} watch", "{count} watches")} ·{" "}
              {t("rating {rating}", { rating: it.rating || "—" })}
              {it.offsetMs > 0
                ? ` · ${t("resume at {minutes} min", { minutes: Math.floor(it.offsetMs / 60000) })}`
                : ""}
            </li>
          ))}
        </ul>
      ) : (
        <p className="text-sm text-fg-muted">{t("Nothing matched.")}</p>
      )}
    </Section>
  );
}

function useImportPlan(reviewId: string) {
  const [plan, setPlan] = useState<Plan | null>(null);
  const { busy, run } = useBusy();
  useEffect(() => {
    if (plan)
      document.getElementById(reviewId)?.scrollIntoView?.({ block: "nearest", behavior: "smooth" });
  }, [plan, reviewId]);
  const review: ReactNode = plan ? (
    <ImportReview
      id={reviewId}
      plan={plan}
      busy={busy}
      onImport={() =>
        void run(async () => {
          setPlan(await post<Plan>("/migration/import", { id: plan.id }));
          toast.success(t("Import complete"));
        })
      }
    />
  ) : null;
  return { setPlan, busy, run, review };
}

/** The viewer imports apply to, defaulting to the first one. */
function useViewerChoice(users: EtherUser[] | null) {
  const [viewer, setViewer] = useState("");
  useEffect(() => {
    if (!viewer && users?.[0]) setViewer(String(users[0].id));
  }, [users, viewer]);
  return [viewer, setViewer] as const;
}

function PlexImport({ users }: { users: EtherUser[] | null }) {
  const [viewer, setViewer] = useViewerChoice(users);
  const [url, setURL] = useState("");
  const [token, setToken] = useState("");
  const { setPlan, busy, run, review } = useImportPlan("plex-import-review");
  const edit = (fn: (v: string) => void) => (v: string) => {
    fn(v);
    setPlan(null);
  };
  return (
    <>
      <SettingsSection
        title={t("Import from Plex")}
        description={t(
          "Bring one viewer's watch history, ratings and playlists across from another Plex server. That server is only read, and you review everything before it's imported.",
        )}
        footer={
          <Button
            size="sm"
            variant="outline"
            disabled={busy || !url || !token || !viewer}
            onClick={() =>
              void run(async () => {
                const p = await post<Plan>("/migration/preview", {
                  url,
                  token,
                  viewers: [
                    {
                      localUserId: Number(viewer),
                      name: users?.find((u) => String(u.id) === viewer)?.username,
                    },
                  ],
                });
                setPlan(p);
                setToken("");
              })
            }
          >
            {busy ? t("Working…") : t("Preview import")}
          </Button>
        }
      >
        <SettingsRow
          title={t("Ether viewer")}
          description={t(
            "Who the history belongs to here. To add friends, share your library with them in Plex first.",
          )}
          control={
            <ViewerPick
              label={t("Ether viewer")}
              users={users}
              value={viewer}
              onChange={edit(setViewer)}
            />
          }
        />
        <SettingsRow
          stacked
          title={<label htmlFor="plex-import-url">{t("Plex server URL")}</label>}
        >
          <Input
            id="plex-import-url"
            type="url"
            placeholder="http://plex-server:32400"
            value={url}
            onChange={(e) => edit(setURL)(e.currentTarget.value)}
          />
        </SettingsRow>
        <SettingsRow
          stacked
          title={<label htmlFor="plex-import-token">{t("Source viewer’s Plex token")}</label>}
          description={t(
            "That viewer's access token on the source server. It's used for the preview and not stored.",
          )}
        >
          <Input
            id="plex-import-token"
            type="password"
            autoComplete="off"
            value={token}
            onChange={(e) => edit(setToken)(e.currentTarget.value)}
          />
        </SettingsRow>
      </SettingsSection>
      {review}
    </>
  );
}

const CSV_SERVICES = [
  { value: "letterboxd", label: "Letterboxd" },
  { value: "trakt", label: "Trakt" },
];
const CSV_KINDS = localized(() => [
  { value: "history", label: t("History") },
  { value: "ratings", label: t("Ratings") },
  { value: "watchlist", label: t("Watchlist") },
]);

function CsvTools({ users }: { users: EtherUser[] | null }) {
  const [viewer, setViewer] = useViewerChoice(users);
  const [format, setFormat] = useState("letterboxd");
  const [kind, setKind] = useState("history");
  const { setPlan, busy, run, review } = useImportPlan("csv-import-review");
  const query = `viewer=${viewer}&format=${format}&kind=${kind}`;
  const edit = (fn: (v: string) => void) => (v: string) => {
    fn(v);
    setPlan(null);
  };
  return (
    <>
      <SettingsSection
        title={t("Trakt and Letterboxd files")}
        description={t(
          "Export or import a diary, ratings or watchlist as CSV. Letterboxd matches need the same title and year.",
        )}
        footer={
          <>
            <Button
              size="sm"
              variant="outline"
              disabled={busy || !viewer}
              onClick={() =>
                void run(() =>
                  download(`/migration/csv/export?${query}`, `ether-${format}-${kind}.csv`),
                )
              }
            >
              <Download size={14} /> {t("Export CSV")}
            </Button>
            <FilePick
              label={t("Preview a CSV import")}
              accept=".csv,text/csv"
              disabled={busy || !viewer}
              onFile={(file) =>
                void run(async () => {
                  setPlan(
                    (await upload(`/migration/csv/preview?${query}`, file, "text/csv")) as Plan,
                  );
                })
              }
            />
          </>
        }
      >
        <SettingsRow
          title={t("Viewer")}
          control={
            <ViewerPick
              label={t("CSV viewer")}
              users={users}
              value={viewer}
              onChange={edit(setViewer)}
            />
          }
        />
        <SettingsRow
          title={t("Service")}
          control={
            <div className="w-48">
              <SearchSelect
                {...searchSelectLabels()}
                ariaLabel={t("CSV service")}
                value={format}
                onChange={edit(setFormat)}
                options={CSV_SERVICES}
              />
            </div>
          }
        />
        <SettingsRow
          title={t("Contents")}
          control={
            <div className="w-48">
              <SearchSelect
                {...searchSelectLabels()}
                ariaLabel={t("CSV contents")}
                value={kind}
                onChange={edit(setKind)}
                options={CSV_KINDS}
              />
            </div>
          }
        />
      </SettingsSection>
      {review}
    </>
  );
}
