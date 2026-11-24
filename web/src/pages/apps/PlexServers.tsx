import {
  Button,
  Input,
  SettingsSection,
  Spinner,
  Switch,
  confirmDialog,
  toast,
} from "@braedonsaunders/appkit-ui";
import { ExternalLink } from "lucide-react";
import { useEffect, useRef, useState } from "react";
import { StatusRow } from "../../components/AppStatusList";
import { SettingsRow } from "../../components/SettingsRow";
import { t, tx } from "../../i18n";
import { ApiError, apiFetch, errorMessage } from "../../lib/api";
import { useAsync } from "../../lib/hooks";
import {
  PlexMoveResults,
  loadPlexMembers,
  movePlexViewers,
  type PlexMoveResult,
} from "./plex-moves";

/** One Plex server as GET /plex/servers lists it. The main server has id "". */
export type PlexServerRow = {
  id: string;
  main: boolean;
  name: string;
  nameInherited: boolean;
  account?: string;
  state: "active" | "standby";
  running: boolean;
  claimed: boolean;
  seatsUsed: number;
  addedAt?: number;
  activatedAt?: number;
  retired?: boolean;
  blocked?: boolean;
  blockedReason?: string;
  problem?: string;
};

export type PlexServersStatus = {
  enabled: boolean;
  maxExtra: number;
  extraCount: number;
  seatsPerServer: number;
  seatsUsed: number;
  servers: PlexServerRow[] | null;
  standby: number;
  claimed: boolean;
  blocked?: string[] | null;
  room?: boolean;
};

type Pin = { id: string; code: string; authUrl: string };

function viewersLabel(row: PlexServerRow) {
  return t("{count} viewers", { count: row.seatsUsed });
}

function rowState(row: PlexServerRow) {
  if (row.blocked) return "unreachable";
  if (row.state === "standby") return "pending";
  if (!row.running || !row.claimed) return "warning";
  return "reachable";
}

function rowDetail(row: PlexServerRow) {
  const account = row.account ? t("Plex account {name}", { name: row.account }) : "";
  if (row.state === "standby") {
    return [account, t("Added, not started. Press Start when you want to use it.")]
      .filter(Boolean)
      .join(" · ");
  }
  const parts = [account, viewersLabel(row)];
  if (row.blocked) parts.push(row.blockedReason || t("plex.tv seems to have blocked this server."));
  if (row.retired) parts.push(t("Not taking new viewers"));
  if (!row.blocked && row.problem) parts.push(row.problem);
  if (!row.main && !row.running) parts.push(t("Not running"));
  return parts.filter(Boolean).join(" · ");
}

export function PlexServers({ onChanged }: { onChanged?: () => void }) {
  const status = useAsync(() => apiFetch<PlexServersStatus>("/plex/servers"), []);
  const [pin, setPin] = useState<Pin | null>(null);
  const [busy, setBusy] = useState("");
  const [notice, setNotice] = useState<{ tone: "info" | "warn"; text: string } | null>(null);
  const [renaming, setRenaming] = useState<{ id: string; name: string } | null>(null);
  const [seats, setSeats] = useState<string | null>(null);
  const [limit, setLimit] = useState<string | null>(null);
  const [moved, setMoved] = useState<PlexMoveResult[]>([]);
  const mounted = useRef(true);
  const pollRef = useRef<number | null>(null);
  const pinRun = useRef(0);

  useEffect(() => {
    mounted.current = true;
    return () => {
      mounted.current = false;
      pinRun.current++;
      if (pollRef.current) window.clearInterval(pollRef.current);
      pollRef.current = null;
    };
  }, []);

  async function refresh() {
    await status.reload();
    onChanged?.();
  }

  function stopPolling() {
    if (pollRef.current) window.clearInterval(pollRef.current);
    pollRef.current = null;
  }

  async function addServer() {
    const run = ++pinRun.current;
    const current = () => mounted.current && run === pinRun.current;
    stopPolling();
    setBusy("add");
    setNotice(null);
    try {
      const next = await apiFetch<Pin>("/plex/servers/pin", { method: "POST" });
      if (!current()) return;
      setPin(next);
      window.open(next.authUrl, "_blank", "noopener,noreferrer");
      let polling = false;
      pollRef.current = window.setInterval(() => {
        if (!current() || polling) return;
        polling = true;
        void apiFetch<{ done: boolean; server?: { account?: string; state?: string } }>(
          `/plex/servers/pin/${encodeURIComponent(next.id)}`,
        )
          .then(async ({ done, server }) => {
            if (!current() || !done) return;
            stopPolling();
            setPin(null);
            setNotice({
              tone: "info",
              text:
                server?.state === "active"
                  ? t("Plex account {name} added and started.", { name: server?.account ?? "" })
                  : t("Plex account {name} added. Press Start when you want to use it.", {
                      name: server?.account ?? "",
                    }),
            });
            await refresh();
          })
          .catch((error: unknown) => {
            if (!current()) return;
            stopPolling();
            setPin(null);
            setNotice({
              tone: "warn",
              text:
                error instanceof ApiError && error.code === "plex_account_in_use"
                  ? t(
                      "That Plex account already runs one of these servers. Sign in with another Plex account.",
                    )
                  : errorMessage(error),
            });
          })
          .finally(() => {
            polling = false;
          });
      }, 2000);
    } catch (error) {
      if (current()) setNotice({ tone: "warn", text: errorMessage(error) });
    } finally {
      if (current()) setBusy("");
    }
  }

  async function act(id: string, what: () => Promise<unknown>, done: string) {
    setBusy(id);
    setNotice(null);
    try {
      await what();
      if (!mounted.current) return;
      toast.success(done);
      await refresh();
    } catch (error) {
      if (mounted.current) setNotice({ tone: "warn", text: errorMessage(error) });
    } finally {
      if (mounted.current) setBusy("");
    }
  }

  async function moveAll(row: PlexServerRow) {
    const ok = await confirmDialog({
      title: t("Move viewers off {name}?", { name: row.name }),
      message: t(
        "Every viewer on {name} is invited on the next Plex server with room. Each one is taken off {name} once they accept the new invite; watch history and settings stay as they are.",
        { name: row.name },
      ),
      confirmLabel: t("Move viewers"),
    });
    if (!ok) return;
    setBusy("move:" + row.id);
    setNotice(null);
    setMoved([]);
    try {
      const members = await loadPlexMembers();
      const ids = Object.entries(members)
        .filter(([, servers]) => servers.some((s) => s.id === row.id))
        .map(([id]) => Number(id));
      if (!ids.length) {
        setNotice({ tone: "info", text: t("No viewers are recorded on this server yet.") });
        return;
      }
      // Stop new invites there first so "auto" never picks it again.
      if (!row.retired) {
        await apiFetch(`/plex/servers/${encodeURIComponent(row.id)}/retire`, {
          method: "POST",
          body: JSON.stringify({ retired: true }),
        });
      }
      const results = await movePlexViewers(ids, "auto", true);
      if (!mounted.current) return;
      setMoved(results);
      await refresh();
    } catch (error) {
      if (mounted.current) setNotice({ tone: "warn", text: errorMessage(error) });
    } finally {
      if (mounted.current) setBusy("");
    }
  }

  async function remove(row: PlexServerRow) {
    const ok = await confirmDialog({
      title: t("Remove Plex server"),
      message: t(
        "This signs {name} out of plex.tv. People it shares with lose this server in their Plex apps unless another server shares with them.",
        { name: row.name },
      ),
      confirmLabel: t("Remove"),
      tone: "danger",
    });
    if (!ok) return;
    await act(
      row.id,
      () => apiFetch(`/plex/servers/${encodeURIComponent(row.id)}`, { method: "DELETE" }),
      t("Plex server removed"),
    );
  }

  async function saveName() {
    if (!renaming) return;
    const { id, name } = renaming;
    await act(
      id,
      () =>
        apiFetch(`/plex/servers/${encodeURIComponent(id)}`, {
          method: "PATCH",
          body: JSON.stringify({ name: name.trim() }),
        }),
      t("Name saved"),
    );
    if (mounted.current) setRenaming(null);
  }

  async function savePlex(key: string, body: Record<string, unknown>) {
    await act(
      key,
      () => apiFetch("/config/plex", { method: "PUT", body: JSON.stringify(body) }),
      t("Settings saved"),
    );
  }

  async function saveSeats() {
    if (seats == null) return;
    await savePlex("seats", { seatsPerServer: Number(seats) || 0 });
    if (mounted.current) setSeats(null);
  }

  async function saveLimit() {
    if (limit == null) return;
    await savePlex("limit", { maxExtraServers: Number(limit) || 0 });
    if (mounted.current) setLimit(null);
  }

  async function setEnabled(on: boolean) {
    if (on) {
      const ok = await confirmDialog({
        title: t("Turn on extra Plex servers?"),
        message: t(
          "This lets you connect other Plex accounts you own to this Ether. Plex's terms allow personal, non-commercial use only: do not sell or resell access.",
        ),
        confirmLabel: t("Turn on"),
      });
      if (!ok) return;
    }
    await savePlex("enabled", { extraServersEnabled: on });
  }

  if (status.error) {
    return (
      <SettingsSection title={t("Plex servers")}>
        <p className="px-5 py-4 text-sm text-fg-muted">{errorMessage(status.error)}</p>
      </SettingsSection>
    );
  }
  const data = status.data;
  if (!data) {
    return (
      <div className="py-10 text-center">
        <Spinner size={20} label={t("Loading")} />
      </div>
    );
  }
  const rows = data.servers ?? [];
  const atLimit = data.extraCount >= data.maxExtra;
  const blockedRow = data.blocked?.length ? (
    <StatusRow
      state="unreachable"
      title={t("plex.tv seems to have blocked {names}", { names: data.blocked.join(", ") })}
      description={
        data.room
          ? t(
              "Press Move viewers on that server: its viewers are invited on another server with room and keep their watch history.",
            )
          : t(
              "No other Plex server has room. You can give viewers a local sign-in for Jellyfin, Emby and Silo (Viewers, select, Bulk actions).",
            )
      }
    />
  ) : null;

  return (
    <>
      <SettingsSection
        title={t("Extra Plex servers")}
        description={t(
          "Connect another Plex account you own to this Ether. Each one appears in Plex as its own server, with the same libraries and watch history. Off by default.",
        )}
      >
        <SettingsRow
          title={t("Use extra Plex servers")}
          description={t("Plex's terms allow personal, non-commercial use only.")}
          control={
            <Switch
              aria-label={t("Use extra Plex servers")}
              checked={data.enabled}
              disabled={busy !== ""}
              onChange={(e) => void setEnabled(e.target.checked)}
            />
          }
        />
        <SettingsRow
          title={t("Most extra Plex servers")}
          description={t("How many other Plex accounts can be connected. 2 unless you change it.")}
          control={
            <div className="flex items-center gap-2">
              <Input
                aria-label={t("Most extra Plex servers")}
                type="number"
                min={1}
                max={10}
                value={limit ?? String(data.maxExtra)}
                onChange={(e) => setLimit(e.target.value)}
              />
              {limit != null ? (
                <Button size="sm" disabled={busy !== ""} onClick={() => void saveLimit()}>
                  {t("Save")}
                </Button>
              ) : null}
            </div>
          }
        />
        {data.enabled ? null : blockedRow}
        {!data.enabled && notice ? (
          <p className="px-5 py-3 text-sm text-warning">{notice.text}</p>
        ) : null}
      </SettingsSection>
      {data.enabled ? (
        <>
          <SettingsSection
            title={t("Plex servers")}
            description={t(
              "Servers start only when you start them. New viewers go to the first started server with room.",
            )}
          >
            {blockedRow}
            {rows.map((row) => (
              <StatusRow
                key={row.id || "main"}
                state={rowState(row)}
                title={
                  row.main
                    ? t("{name} (main)", { name: row.name || t("Main Plex server") })
                    : row.name || row.id
                }
                description={rowDetail(row)}
                control={
                  <div className="flex flex-wrap gap-2">
                    {row.state === "active" ? (
                      <>
                        <Button
                          variant={row.blocked ? undefined : "outline"}
                          size="sm"
                          disabled={busy !== "" || row.seatsUsed === 0}
                          onClick={() => void moveAll(row)}
                        >
                          {busy === "move:" + row.id ? (
                            <Spinner size={13} label={t("Moving")} />
                          ) : null}
                          {t("Move viewers")}
                        </Button>
                        <Button
                          variant="outline"
                          size="sm"
                          disabled={busy !== ""}
                          onClick={() =>
                            void act(
                              row.id,
                              () =>
                                apiFetch(`/plex/servers/${encodeURIComponent(row.id)}/retire`, {
                                  method: "POST",
                                  body: JSON.stringify({ retired: !row.retired }),
                                }),
                              row.retired
                                ? t("Taking new viewers again")
                                : t("No new viewers go here"),
                            )
                          }
                        >
                          {row.retired ? t("Take new viewers") : t("Stop new viewers")}
                        </Button>
                      </>
                    ) : null}
                    {row.main ? null : (
                      <div className="flex flex-wrap gap-2">
                        {row.state === "standby" ? (
                          <Button
                            size="sm"
                            disabled={busy !== ""}
                            onClick={() =>
                              void act(
                                row.id,
                                () =>
                                  apiFetch(`/plex/servers/${encodeURIComponent(row.id)}/start`, {
                                    method: "POST",
                                  }),
                                t("Plex server started"),
                              )
                            }
                          >
                            {busy === row.id ? <Spinner size={13} label={t("Starting")} /> : null}
                            {t("Start now")}
                          </Button>
                        ) : null}
                        <Button
                          variant="outline"
                          size="sm"
                          disabled={busy !== ""}
                          onClick={() =>
                            setRenaming({ id: row.id, name: row.nameInherited ? "" : row.name })
                          }
                        >
                          {t("Rename")}
                        </Button>
                        <Button
                          variant="outline"
                          size="sm"
                          disabled={busy !== ""}
                          onClick={() => void remove(row)}
                        >
                          {t("Remove")}
                        </Button>
                      </div>
                    )}
                  </div>
                }
              />
            ))}
            <PlexMoveResults results={moved} />
            {renaming ? (
              <SettingsRow
                title={t("Name")}
                description={t("Leave empty to use the server name from Settings.")}
                control={
                  <div className="flex flex-wrap items-center gap-2">
                    <Input
                      aria-label={t("Name")}
                      value={renaming.name}
                      maxLength={64}
                      onChange={(e) => setRenaming({ ...renaming, name: e.target.value })}
                    />
                    <Button size="sm" disabled={busy !== ""} onClick={() => void saveName()}>
                      {t("Save")}
                    </Button>
                    <Button variant="outline" size="sm" onClick={() => setRenaming(null)}>
                      {t("Cancel")}
                    </Button>
                  </div>
                }
              />
            ) : null}
            <SettingsRow
              title={t("Add Plex server")}
              description={
                !data.claimed
                  ? t("Sign in and claim the main Plex server first.")
                  : atLimit
                    ? t("You have connected {count} of {max} extra Plex servers.", {
                        count: data.extraCount,
                        max: data.maxExtra,
                      })
                    : t(
                        "Sign in with another Plex account you own. You start it when you want to use it.",
                      )
              }
              control={
                <Button
                  size="sm"
                  disabled={!data.claimed || atLimit || busy !== "" || pin != null}
                  onClick={() => void addServer()}
                >
                  {t("Add Plex server")}
                </Button>
              }
            />
            {pin ? (
              <div className="space-y-3 px-5 py-4">
                <p className="text-sm text-fg">
                  {t(
                    "A plex.tv window opened. Sign in there with the other Plex account and allow Ether; this page continues on its own.",
                  )}
                </p>
                <div className="flex flex-wrap items-center gap-3 text-sm text-fg-muted">
                  <Button
                    variant="outline"
                    size="sm"
                    onClick={() => window.open(pin.authUrl, "_blank", "noopener,noreferrer")}
                  >
                    {t("Open plex.tv sign-in")} <ExternalLink size={13} />
                  </Button>
                  <span className="inline-flex items-center gap-2">
                    {t("Waiting for approval")} <Spinner size={13} label={t("Waiting")} />
                  </span>
                  <Button
                    variant="outline"
                    size="sm"
                    onClick={() => {
                      pinRun.current++;
                      stopPolling();
                      setPin(null);
                    }}
                  >
                    {t("Cancel")}
                  </Button>
                </div>
                {pin.code.length <= 8 ? (
                  <p className="text-sm text-fg-muted">
                    {tx("Or enter {code} at plex.tv/link.", {
                      code: <span className="font-mono font-semibold text-fg">{pin.code}</span>,
                    })}
                  </p>
                ) : null}
              </div>
            ) : null}
            {notice ? (
              <p
                className={`px-5 py-3 text-sm ${notice.tone === "warn" ? "text-warning" : "text-fg-muted"}`}
              >
                {notice.text}
              </p>
            ) : null}
          </SettingsSection>
          <SettingsSection title={t("Viewers per Plex server")}>
            <SettingsRow
              title={t("Viewers per Plex server")}
              description={t(
                "When a server has this many viewers, new viewers go to the next started server.",
              )}
              control={
                <div className="flex items-center gap-2">
                  <Input
                    aria-label={t("Viewers per Plex server")}
                    type="number"
                    min={1}
                    max={1000}
                    value={seats ?? String(data.seatsPerServer)}
                    onChange={(e) => setSeats(e.target.value)}
                  />
                  {seats != null ? (
                    <Button size="sm" disabled={busy !== ""} onClick={() => void saveSeats()}>
                      {t("Save")}
                    </Button>
                  ) : null}
                </div>
              }
            />
          </SettingsSection>
        </>
      ) : null}
    </>
  );
}
