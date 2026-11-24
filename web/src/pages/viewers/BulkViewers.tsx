import { Button, confirmDialog, toast } from "@braedonsaunders/appkit-ui";
import { useEffect, useMemo, useRef, useState } from "react";
import { CopyButton } from "../../components/CopyButton";
import { Drawer } from "../../components/Drawer";
import { viewerName } from "../../components/pickers";
import { t } from "../../i18n";
import { api, apiFetch, errorMessage } from "../../lib/api";
import type { Viewer } from "../../lib/contract";
import type { EtherUser } from "../../lib/types";
import {
  PlexMoveResults,
  loadPlexMembers,
  movePlexViewers,
  type PlexMembers,
  type PlexMoveResult,
} from "../apps/plex-moves";
import type { PlexServersStatus } from "../apps/PlexServers";

// ---- settings one viewer can share with others ---------------------------------

type Category = {
  key: string;
  label: () => string;
} & (
  | { kind: "user"; fields: (keyof EtherUser)[] }
  | { kind: "safety"; fields: string[] }
  | { kind: "path"; path: string }
  | { kind: "catalog" }
);

/** Every per-viewer setting group the bulk tools copy, export and import. */
export const SETTING_CATEGORIES: Category[] = [
  { key: "streamLimit", kind: "user", fields: ["streamLimit"], label: () => t("Stream limit") },
  { key: "rating", kind: "user", fields: ["maxContentRating"], label: () => t("Rating limit") },
  { key: "libraries", kind: "user", fields: ["allowedLibraries"], label: () => t("Libraries") },
  {
    key: "parental",
    kind: "safety",
    fields: ["blockedGenres", "blockedKeywords", "bedtime", "allowedHours", "timezone"],
    label: () => t("Parental controls (blocked genres and keywords, bedtime, viewing hours)"),
  },
  {
    key: "quality",
    kind: "safety",
    fields: ["remoteDisabled", "maxBitrateKbps", "videoTranscodingDisabled"],
    label: () => t("Away from home and quality (remote access, bitrate cap, video conversion)"),
  },
  {
    key: "downloads",
    kind: "safety",
    fields: [
      "allowDownloads",
      "downloadTranscodingDisabled",
      "downloadLimitUnit",
      "downloadLimitAmount",
      "downloadLimitPeriod",
      "downloadMaxHeight",
    ],
    label: () => t("Downloads (allowed, limit, highest quality)"),
  },
  {
    key: "audio",
    kind: "path",
    path: "/stream-preferences",
    label: () => t("Audio and subtitle choices"),
  },
  { key: "livetv", kind: "path", path: "/livetv/policies", label: () => t("Live TV access") },
  { key: "catalog", kind: "catalog", label: () => t("Catalog rules") },
];

type Obj = Record<string, unknown>;
/** A viewer's settings by category key, as exported. */
export type ViewerSettings = Record<string, unknown>;

async function readSettings(id: number, user: EtherUser | undefined, keys: Set<string>) {
  const out: ViewerSettings = {};
  let safety: Obj | null = null;
  for (const c of SETTING_CATEGORIES) {
    if (!keys.has(c.key)) continue;
    if (c.kind === "user") {
      if (!user) continue;
      out[c.key] = Object.fromEntries(c.fields.map((f) => [f, user[f]]));
    } else if (c.kind === "safety") {
      safety ??= await apiFetch<Obj>(`/safety/policies/${id}`);
      out[c.key] = Object.fromEntries(c.fields.map((f) => [f, safety?.[f]]));
    } else if (c.kind === "path") {
      out[c.key] = await apiFetch<unknown>(`${c.path}/${id}`);
    } else {
      const res = await apiFetch<{ policy: unknown }>(`/viewers/${id}/catalog-rules`);
      out[c.key] = res.policy;
    }
  }
  return out;
}

async function writeSettings(id: number, settings: ViewerSettings, keys: Set<string>) {
  const userPatch: Obj = {};
  let safetyPatch: Obj | null = null;
  for (const c of SETTING_CATEGORIES) {
    if (!keys.has(c.key) || settings[c.key] === undefined) continue;
    const value = settings[c.key];
    if (c.kind === "user") Object.assign(userPatch, value as Obj);
    else if (c.kind === "safety") safetyPatch = { ...(safetyPatch ?? {}), ...(value as Obj) };
    else if (c.kind === "path")
      await apiFetch(`${c.path}/${id}`, { method: "PUT", body: JSON.stringify(value) });
    else
      await apiFetch(`/viewers/${id}/catalog-rules`, {
        method: "PUT",
        body: JSON.stringify(value),
      });
  }
  if (Object.keys(userPatch).length) {
    if ("allowedLibraries" in userPatch && userPatch.allowedLibraries == null)
      userPatch.allowedLibraries = [];
    await api.updateUser(id, userPatch as Partial<EtherUser>);
  }
  if (safetyPatch) {
    const current = await apiFetch<Obj>(`/safety/policies/${id}`);
    await apiFetch(`/safety/policies/${id}`, {
      method: "PUT",
      body: JSON.stringify({ ...current, ...safetyPatch }),
    });
  }
}

// ---- backup file -----------------------------------------------------------------

type BackupViewer = {
  plexId: number;
  username: string;
  title: string;
  email?: string;
  local: boolean;
  plexServers: string[];
  settings: ViewerSettings;
};
type Backup = { kind: "ether-viewers"; version: 1; exportedAt: string; viewers: BackupViewer[] };

function download(name: string, text: string, type: string) {
  const url = URL.createObjectURL(new Blob([text], { type }));
  const a = document.createElement("a");
  a.href = url;
  a.download = name;
  document.body.appendChild(a);
  a.click();
  a.remove();
  window.setTimeout(() => URL.revokeObjectURL(url), 1000);
}

function allKeys() {
  return new Set(SETTING_CATEGORIES.map((c) => c.key));
}

// ---- UI ----------------------------------------------------------------------

type LinkRow = { name: string; url: string; note?: string };

function CheckRow({
  checked,
  onChange,
  label,
  hint,
  disabled,
}: {
  checked: boolean;
  onChange: (next: boolean) => void;
  label: string;
  hint?: string;
  disabled?: boolean;
}) {
  return (
    <label className={`flex items-start gap-2 py-1 text-sm ${disabled ? "opacity-50" : ""}`}>
      <input
        type="checkbox"
        className="mt-0.5"
        checked={checked}
        disabled={disabled}
        onChange={(e) => onChange(e.target.checked)}
      />
      <span>
        <span className="text-fg">{label}</span>
        {hint ? <span className="block text-xs text-fg-muted">{hint}</span> : null}
      </span>
    </label>
  );
}

function LinkTable({ rows, title }: { rows: LinkRow[]; title: string }) {
  if (!rows.length) return null;
  const csv = ["name,link", ...rows.map((r) => `"${r.name.replace(/"/g, '""')}",${r.url}`)].join(
    "\n",
  );
  return (
    <div className="space-y-2">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <p className="text-sm font-medium text-fg">{title}</p>
        <span className="flex gap-2">
          <CopyButton
            value={rows.map((r) => `${r.name}: ${r.url}`).join("\n")}
            label={t("Copy all")}
          />
          <Button
            type="button"
            size="sm"
            variant="outline"
            onClick={() => download("ether-invite-links.csv", csv, "text/csv")}
          >
            {t("Download CSV")}
          </Button>
        </span>
      </div>
      <ul className="max-h-64 divide-y divide-border overflow-auto rounded-lg border border-border">
        {rows.map((r) => (
          <li key={r.name + r.url} className="px-3 py-2 text-sm">
            <span className="font-medium text-fg">{r.name}</span>
            {r.note ? <span className="text-fg-muted"> · {r.note}</span> : null}
            <span className="block break-all text-xs text-fg-muted">{r.url}</span>
          </li>
        ))}
      </ul>
    </div>
  );
}

/**
 * Bulk actions on the selected viewers: copy setting groups from a template
 * viewer, give them a local sign-in (Jellyfin, Emby, Silo) with invite
 * links, move them between Plex servers, pause, resume, sign out, export.
 */
export function BulkViewersDrawer({
  open,
  onClose,
  selected,
  allViewers,
  users,
  onDone,
}: {
  open: boolean;
  onClose: () => void;
  selected: Viewer[];
  allViewers: Viewer[];
  users: EtherUser[];
  onDone: () => void;
}) {
  const byId = useMemo(() => new Map(users.map((u) => [u.id, u])), [users]);
  const [template, setTemplate] = useState<number | null>(null);
  const [keys, setKeys] = useState<Set<string>>(new Set());
  const [local, setLocal] = useState(false);
  const [move, setMove] = useState(false);
  const [moveTo, setMoveTo] = useState("auto");
  const [removeOld, setRemoveOld] = useState(true);
  const [pause, setPause] = useState<"" | "pause" | "resume">("");
  const [signOut, setSignOut] = useState(false);
  const [servers, setServers] = useState<PlexServersStatus | null>(null);
  const [busy, setBusy] = useState(false);
  const [progress, setProgress] = useState("");
  const [errors, setErrors] = useState<string[]>([]);
  const [links, setLinks] = useState<LinkRow[]>([]);
  const [moved, setMoved] = useState<PlexMoveResult[]>([]);
  const mounted = useRef(true);
  useEffect(() => {
    mounted.current = true;
    return () => {
      mounted.current = false;
    };
  }, []);
  useEffect(() => {
    if (!open) return;
    setErrors([]);
    setLinks([]);
    setMoved([]);
    setProgress("");
    apiFetch<PlexServersStatus>("/plex/servers")
      .then((s) => mounted.current && setServers(s))
      .catch(() => mounted.current && setServers(null));
  }, [open]);

  const targets = selected.filter((v) => !v.isOwner);
  const plexTargets = targets.filter((v) => v.plexId > 0);
  const templates = allViewers.filter((v) => !selected.some((s) => s.id === v.id) || v.isOwner);
  const nothing = (template == null || keys.size === 0) && !local && !move && !pause && !signOut;

  function toggleKey(key: string, on: boolean) {
    setKeys((old) => {
      const next = new Set(old);
      if (on) next.add(key);
      else next.delete(key);
      return next;
    });
  }

  async function run() {
    const ok = await confirmDialog({
      title: t("Apply to {count} viewers?", { count: targets.length }),
      message: t("The checked actions run for every selected viewer, one after another."),
      confirmLabel: t("Apply"),
    });
    if (!ok) return;
    setBusy(true);
    setErrors([]);
    setLinks([]);
    setMoved([]);
    const errs: string[] = [];
    const newLinks: LinkRow[] = [];
    try {
      let settings: ViewerSettings | null = null;
      if (template != null && keys.size) {
        setProgress(t("Reading the template's settings…"));
        settings = await readSettings(template, byId.get(template), keys);
      }
      for (const [i, v] of targets.entries()) {
        if (!mounted.current) return;
        const name = viewerName(v);
        setProgress(t("{n} of {total}: {name}", { n: i + 1, total: targets.length, name }));
        try {
          if (settings) await writeSettings(v.id, settings, keys);
          if (local) {
            const res = await apiFetch<{ url: string }>(`/users/${v.id}/invite`, {
              method: "POST",
              body: JSON.stringify({}),
            });
            newLinks.push({ name, url: new URL(res.url, window.location.origin).href });
          }
          if (pause)
            await apiFetch(`/users/${v.id}/${pause}`, { method: "POST", body: JSON.stringify({}) });
          if (signOut)
            await apiFetch(`/users/${v.id}/signout`, { method: "POST", body: JSON.stringify({}) });
        } catch (e) {
          errs.push(`${name}: ${errorMessage(e)}`);
        }
      }
      if (move && plexTargets.length) {
        setProgress(t("Moving {count} Plex viewers…", { count: plexTargets.length }));
        try {
          const results = await movePlexViewers(
            plexTargets.map((v) => v.id),
            moveTo,
            removeOld,
          );
          if (mounted.current) setMoved(results);
        } catch (e) {
          errs.push(errorMessage(e));
        }
      }
      if (!mounted.current) return;
      setLinks(newLinks);
      setErrors(errs);
      setProgress("");
      toast.success(
        errs.length
          ? t("Done with {count} problems", { count: errs.length })
          : t("Done for {count} viewers", { count: targets.length }),
      );
      onDone();
    } catch (e) {
      if (mounted.current) setErrors([...errs, errorMessage(e)]);
    } finally {
      if (mounted.current) setBusy(false);
    }
  }

  const activeServers = (servers?.servers ?? []).filter((s) => s.state === "active");

  return (
    <Drawer
      open={open}
      onClose={onClose}
      title={t("Bulk actions for {count} viewers", { count: targets.length })}
      size="lg"
    >
      <div className="space-y-6">
        <section className="space-y-2">
          <h3 className="text-sm font-semibold text-fg">{t("Copy settings from a viewer")}</h3>
          <select
            aria-label={t("Template viewer")}
            className="w-full rounded-md border border-border bg-surface px-2 py-1.5 text-sm"
            value={template ?? ""}
            onChange={(e) => setTemplate(e.target.value ? Number(e.target.value) : null)}
          >
            <option value="">{t("Don't copy settings")}</option>
            {templates.map((v) => (
              <option key={v.id} value={v.id}>
                {v.isOwner ? t("You") : viewerName(v)}
              </option>
            ))}
          </select>
          <div className="flex gap-3 text-xs">
            <button
              type="button"
              className="text-primary hover:underline"
              onClick={() => setKeys(allKeys())}
            >
              {t("Check all")}
            </button>
            <button
              type="button"
              className="text-primary hover:underline"
              onClick={() => setKeys(new Set())}
            >
              {t("Clear")}
            </button>
          </div>
          {SETTING_CATEGORIES.map((c) => (
            <CheckRow
              key={c.key}
              checked={keys.has(c.key)}
              disabled={template == null}
              onChange={(on) => toggleKey(c.key, on)}
              label={c.label()}
            />
          ))}
          <p className="text-xs text-fg-muted">
            {t("Sign-ins, watch history, watchlists and taste are never copied.")}
          </p>
        </section>

        <section className="space-y-1">
          <h3 className="text-sm font-semibold text-fg">{t("Sign-in and Plex")}</h3>
          <CheckRow
            checked={local}
            onChange={setLocal}
            label={t("Give them a local sign-in (Jellyfin, Emby, Silo and the web app)")}
            hint={t(
              "Makes an invite link per viewer to set a username and password. It is the same viewer: watch history, watchlist and settings come along. Use it before or after a Plex ban.",
            )}
          />
          <CheckRow
            checked={move}
            onChange={setMove}
            disabled={!plexTargets.length || !servers}
            label={t("Move to another Plex server ({count} Plex viewers)", {
              count: plexTargets.length,
            })}
            hint={t("Each one gets a new Plex invite to accept; watch history stays.")}
          />
          {move ? (
            <div className="space-y-1 pl-6">
              <select
                aria-label={t("Move to")}
                className="w-full rounded-md border border-border bg-surface px-2 py-1.5 text-sm"
                value={moveTo}
                onChange={(e) => setMoveTo(e.target.value)}
              >
                <option value="auto">{t("The first Plex server with room")}</option>
                {activeServers.map((s) => (
                  <option key={s.id} value={s.id} disabled={s.blocked || s.retired}>
                    {s.name}
                    {s.blocked
                      ? ` (${t("blocked")})`
                      : s.retired
                        ? ` (${t("not taking viewers")})`
                        : ""}
                  </option>
                ))}
              </select>
              <CheckRow
                checked={removeOld}
                onChange={setRemoveOld}
                label={t("Take them off their current Plex server once they accept")}
                hint={t("Their old server keeps working until they accept the new invite.")}
              />
            </div>
          ) : null}
        </section>

        <section className="space-y-1">
          <h3 className="text-sm font-semibold text-fg">{t("Access")}</h3>
          <CheckRow
            checked={pause === "pause"}
            onChange={(on) => setPause(on ? "pause" : "")}
            label={t("Pause access")}
          />
          <CheckRow
            checked={pause === "resume"}
            onChange={(on) => setPause(on ? "resume" : "")}
            label={t("Resume access")}
          />
          <CheckRow checked={signOut} onChange={setSignOut} label={t("Sign out everywhere")} />
        </section>

        {progress ? <p className="text-sm text-fg-muted">{progress}</p> : null}
        {errors.length ? (
          <ul role="alert" className="space-y-1 text-sm text-danger">
            {errors.map((e) => (
              <li key={e}>{e}</li>
            ))}
          </ul>
        ) : null}
        <LinkTable rows={links} title={t("Local sign-in invite links")} />
        <PlexMoveResults results={moved} />

        <div className="flex justify-end gap-2">
          <Button type="button" variant="ghost" onClick={onClose}>
            {t("Close")}
          </Button>
          <Button
            type="button"
            disabled={busy || nothing || !targets.length}
            onClick={() => void run()}
          >
            {t("Apply")}
          </Button>
        </div>
      </div>
    </Drawer>
  );
}

/** Export the given viewers (identity, Plex servers, every setting group). */
export async function exportViewers(viewers: Viewer[], users: EtherUser[]) {
  const byId = new Map(users.map((u) => [u.id, u]));
  const members: PlexMembers = await loadPlexMembers();
  const out: BackupViewer[] = [];
  for (const v of viewers) {
    if (v.isOwner) continue;
    const u = byId.get(v.id);
    out.push({
      plexId: v.plexId,
      username: v.username,
      title: v.title,
      email: u?.email,
      local: v.plexId === 0,
      plexServers: (members[String(v.id)] ?? []).map((s) => s.name),
      settings: await readSettings(v.id, u, allKeys()).catch(() => ({})),
    });
  }
  const backup: Backup = {
    kind: "ether-viewers",
    version: 1,
    exportedAt: new Date().toISOString(),
    viewers: out,
  };
  const day = new Date().toISOString().slice(0, 10);
  download(`ether-viewers-${day}.json`, JSON.stringify(backup, null, 2), "application/json");
  return out.length;
}

/**
 * Import a viewers backup: settings go to the matching viewers (by Plex id,
 * then username); viewers not on this Ether can be invited on the first Plex
 * server with room, or made local viewers with an invite link.
 */
export function ImportViewersDrawer({
  open,
  onClose,
  viewers,
  onDone,
}: {
  open: boolean;
  onClose: () => void;
  viewers: Viewer[];
  onDone: () => void;
}) {
  const [backup, setBackup] = useState<Backup | null>(null);
  const [fileError, setFileError] = useState("");
  const [keys, setKeys] = useState<Set<string>>(allKeys());
  const [invitePlex, setInvitePlex] = useState(true);
  const [createLocal, setCreateLocal] = useState(false);
  const [busy, setBusy] = useState(false);
  const [progress, setProgress] = useState("");
  const [errors, setErrors] = useState<string[]>([]);
  const [links, setLinks] = useState<LinkRow[]>([]);

  function match(b: BackupViewer) {
    if (b.plexId > 0) {
      const byPlex = viewers.find((v) => v.plexId === b.plexId);
      if (byPlex) return byPlex;
    }
    const name = b.username.toLowerCase();
    return viewers.find((v) => !v.isOwner && name && v.username.toLowerCase() === name);
  }

  async function readFile(file: File) {
    setFileError("");
    setBackup(null);
    try {
      const parsed = JSON.parse(await file.text()) as Backup;
      if (parsed?.kind !== "ether-viewers" || !Array.isArray(parsed.viewers))
        throw new Error(t("This is not an Ether viewers backup."));
      setBackup(parsed);
    } catch (e) {
      setFileError(errorMessage(e));
    }
  }

  async function run() {
    if (!backup) return;
    setBusy(true);
    setErrors([]);
    setLinks([]);
    const errs: string[] = [];
    const newLinks: LinkRow[] = [];
    for (const [i, b] of backup.viewers.entries()) {
      const name = b.title || b.username;
      setProgress(t("{n} of {total}: {name}", { n: i + 1, total: backup.viewers.length, name }));
      try {
        const existing = match(b);
        if (existing) {
          await writeSettings(existing.id, b.settings ?? {}, keys);
        } else if (b.plexId > 0 && invitePlex) {
          const res = await apiFetch<PlexMoveResult>("/plex/servers/invite", {
            method: "POST",
            body: JSON.stringify({ plexId: b.plexId, username: b.username, email: b.email }),
          });
          newLinks.push({
            name,
            url: res.inviteUrl || res.acceptUrl || "",
            note: t("Plex invite on {name}; import again after they accept to apply settings", {
              name: res.toName || res.to,
            }),
          });
        } else if (createLocal) {
          const res = await apiFetch<{ id: number; url: string }>("/users/invite", {
            method: "POST",
            body: JSON.stringify({ display: name }),
          });
          await writeSettings(res.id, b.settings ?? {}, keys);
          newLinks.push({
            name,
            url: new URL(res.url, window.location.origin).href,
            note: t("local sign-in"),
          });
        } else {
          errs.push(t("{name}: not on this server (skipped)", { name }));
        }
      } catch (e) {
        errs.push(`${name}: ${errorMessage(e)}`);
      }
    }
    setLinks(newLinks);
    setErrors(errs);
    setProgress("");
    setBusy(false);
    toast.success(t("Import finished"));
    onDone();
  }

  const found = backup ? backup.viewers.filter((b) => match(b)).length : 0;
  return (
    <Drawer open={open} onClose={onClose} title={t("Import viewers backup")} size="lg">
      <div className="space-y-5">
        <input
          type="file"
          accept="application/json,.json"
          aria-label={t("Backup file")}
          onChange={(e) => {
            const file = e.target.files?.[0];
            if (file) void readFile(file);
          }}
        />
        {fileError ? (
          <p role="alert" className="text-sm text-danger">
            {fileError}
          </p>
        ) : null}
        {backup ? (
          <p className="text-sm text-fg-muted">
            {t("{total} viewers in the file, {found} already on this server.", {
              total: backup.viewers.length,
              found,
            })}
          </p>
        ) : null}
        <section className="space-y-1">
          <h3 className="text-sm font-semibold text-fg">{t("Settings to apply")}</h3>
          {SETTING_CATEGORIES.map((c) => (
            <CheckRow
              key={c.key}
              checked={keys.has(c.key)}
              onChange={(on) =>
                setKeys((old) => {
                  const next = new Set(old);
                  if (on) next.add(c.key);
                  else next.delete(c.key);
                  return next;
                })
              }
              label={c.label()}
            />
          ))}
        </section>
        <section className="space-y-1">
          <h3 className="text-sm font-semibold text-fg">{t("Viewers not on this server")}</h3>
          <CheckRow
            checked={invitePlex}
            onChange={setInvitePlex}
            label={t("Invite Plex viewers on the first Plex server with room")}
          />
          <CheckRow
            checked={createLocal}
            onChange={setCreateLocal}
            label={t("Create the rest as local viewers with an invite link")}
            hint={t("Also used for Plex viewers when the Plex invite is unchecked.")}
          />
        </section>
        {progress ? <p className="text-sm text-fg-muted">{progress}</p> : null}
        {errors.length ? (
          <ul role="alert" className="space-y-1 text-sm text-danger">
            {errors.map((e) => (
              <li key={e}>{e}</li>
            ))}
          </ul>
        ) : null}
        <LinkTable rows={links} title={t("Invite links")} />
        <div className="flex justify-end gap-2">
          <Button type="button" variant="ghost" onClick={onClose}>
            {t("Close")}
          </Button>
          <Button type="button" disabled={busy || !backup} onClick={() => void run()}>
            {t("Import")}
          </Button>
        </div>
      </div>
    </Drawer>
  );
}
