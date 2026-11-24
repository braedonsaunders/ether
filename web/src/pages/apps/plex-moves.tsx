import { Button } from "@braedonsaunders/appkit-ui";
import { ExternalLink } from "lucide-react";
import { useEffect, useState } from "react";
import { Drawer } from "../../components/Drawer";
import { CopyButton } from "../../components/CopyButton";
import { t } from "../../i18n";
import { apiFetch, errorMessage } from "../../lib/api";
import type { PlexServersStatus } from "./PlexServers";

/** One viewer's move, as POST /plex/servers/move answers it. */
export type PlexMoveResult = {
  userId: number;
  name: string;
  to: string;
  toName?: string;
  inviteUrl?: string;
  acceptUrl?: string;
  removeLater?: string[];
  error?: string;
};

/** The Plex servers sharing with each viewer (GET /plex/servers/members). */
export type PlexMembers = Record<string, { id: string; name: string }[]>;

export function loadPlexMembers(signal?: AbortSignal) {
  return apiFetch<PlexMembers>("/plex/servers/members", { signal }).catch(
    () => ({}) as PlexMembers,
  );
}

/**
 * Shares the viewers on another Plex server ("auto" picks the first with
 * room) and, with removeOld, takes them off the servers they were on.
 */
export async function movePlexViewers(userIds: number[], to: string, removeOld: boolean) {
  const res = await apiFetch<{ results: PlexMoveResult[] | null }>("/plex/servers/move", {
    method: "POST",
    body: JSON.stringify({ userIds, to, removeOld }),
  });
  return res.results ?? [];
}

/** Per-viewer outcome of a move, with the invite link to send each one. */
export function PlexMoveResults({ results }: { results: PlexMoveResult[] }) {
  if (!results.length) return null;
  const ok = results.filter((r) => !r.error);
  const links = ok
    .map((r) => `${r.name}: ${r.inviteUrl || r.acceptUrl || ""}`)
    .filter((line) => !line.endsWith(": "))
    .join("\n");
  return (
    <div className="space-y-2 px-5 py-3 text-sm">
      <p className="text-fg">
        {t("{ok} of {total} viewers invited on their new Plex server.", {
          ok: ok.length,
          total: results.length,
        })}{" "}
        {t(
          "Each one has to accept the invite in a Plex app or on plex.tv; their old server keeps working until then. Their watch history and settings are already there.",
        )}
      </p>
      {links ? (
        <div className="flex items-center gap-2">
          <CopyButton value={links} label={t("Copy all invite links")} />
        </div>
      ) : null}
      <ul className="divide-y divide-border rounded-lg border border-border">
        {results.map((r) => (
          <li
            key={r.userId}
            className="flex flex-wrap items-center justify-between gap-2 px-3 py-2"
          >
            <span className="min-w-0">
              <span className="font-medium text-fg">{r.name || `#${r.userId}`}</span>{" "}
              <span className={r.error ? "text-danger" : "text-fg-muted"}>
                {r.error
                  ? r.error
                  : [
                      t("Invited on {name}", { name: r.toName || r.to }),
                      r.removeLater?.length
                        ? t("leaves {names} once they accept", { names: r.removeLater.join(", ") })
                        : "",
                    ]
                      .filter(Boolean)
                      .join(" · ")}
              </span>
            </span>
            {!r.error && (r.inviteUrl || r.acceptUrl) ? (
              <a
                href={r.inviteUrl || r.acceptUrl}
                target="_blank"
                rel="noopener noreferrer"
                className="inline-flex items-center gap-1 text-primary hover:underline"
              >
                {r.inviteUrl ? t("Invite link") : t("Accept page")} <ExternalLink size={12} />
              </a>
            ) : null}
          </li>
        ))}
      </ul>
    </div>
  );
}

/** Move one viewer to another Plex server, with the invite link to send. */
export function PlexMovePanel({
  viewerId,
  open,
  onClose,
  onDone,
}: {
  viewerId: number;
  open: boolean;
  onClose: () => void;
  onDone: () => void;
}) {
  const [servers, setServers] = useState<PlexServersStatus | null>(null);
  const [on, setOn] = useState<{ id: string; name: string }[]>([]);
  const [to, setTo] = useState("auto");
  const [removeOld, setRemoveOld] = useState(true);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [results, setResults] = useState<PlexMoveResult[]>([]);
  useEffect(() => {
    if (!open) return;
    let cancelled = false;
    setResults([]);
    setError("");
    void Promise.all([apiFetch<PlexServersStatus>("/plex/servers"), loadPlexMembers()])
      .then(([s, m]) => {
        if (cancelled) return;
        setServers(s);
        setOn(m[String(viewerId)] ?? []);
      })
      .catch((e: unknown) => !cancelled && setError(errorMessage(e)));
    return () => {
      cancelled = true;
    };
  }, [open, viewerId]);
  async function submit() {
    setBusy(true);
    setError("");
    try {
      setResults(await movePlexViewers([viewerId], to, removeOld));
      onDone();
    } catch (e) {
      setError(errorMessage(e));
    } finally {
      setBusy(false);
    }
  }
  const choices = (servers?.servers ?? []).filter(
    (s) => s.state === "active" && !on.some((m) => m.id === s.id),
  );
  return (
    <Drawer open={open} onClose={onClose} title={t("Move to another Plex server")}>
      <div className="space-y-4 text-sm">
        <p className="text-fg-muted">
          {on.length
            ? t("Now on: {names}", { names: on.map((s) => s.name).join(", ") })
            : t("No Plex server is recorded for this viewer yet.")}
        </p>
        <select
          aria-label={t("Move to")}
          className="w-full rounded-md border border-border bg-surface px-2 py-1.5"
          value={to}
          onChange={(e) => setTo(e.target.value)}
        >
          <option value="auto">{t("The first Plex server with room")}</option>
          {choices.map((s) => (
            <option key={s.id} value={s.id} disabled={s.blocked || s.retired}>
              {s.name}
              {s.blocked ? ` (${t("blocked")})` : s.retired ? ` (${t("not taking viewers")})` : ""}
            </option>
          ))}
        </select>
        <label className="flex items-start gap-2">
          <input
            type="checkbox"
            className="mt-0.5"
            checked={removeOld}
            onChange={(e) => setRemoveOld(e.target.checked)}
          />
          <span>{t("Take them off their current Plex server once they accept")}</span>
        </label>
        <p className="text-xs text-fg-muted">
          {t("They get a new Plex invite to accept. Watch history and settings stay.")}
        </p>
        {error ? (
          <p role="alert" className="text-danger">
            {error}
          </p>
        ) : null}
        <PlexMoveResults results={results} />
        <div className="flex justify-end gap-2">
          <Button type="button" variant="ghost" onClick={onClose}>
            {t("Close")}
          </Button>
          <Button type="button" disabled={busy || !servers} onClick={() => void submit()}>
            {t("Move")}
          </Button>
        </div>
      </div>
    </Drawer>
  );
}
