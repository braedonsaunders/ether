import {
  Badge,
  Button,
  Input,
  Select,
  SettingsSection,
  Spinner,
  toast,
} from "@braedonsaunders/appkit-ui";
import { useEffect, useState } from "react";
import { Drawer } from "../../components/Drawer";
import { ErrorPanel, MutationError } from "../../components/feedback";
import { Field, SecretInput } from "../../components/fields";
import { IntegrationAppCard } from "../../components/IntegrationAppCard";
import { PlexMark } from "../../components/integration-brands";
import { useLocation } from "react-router-dom";
import { SettingsRow } from "../../components/SettingsRow";
import { t, tn } from "../../i18n";
import { formatDateTime } from "../../lib/format";
import { api, apiFetch } from "../../lib/api";
import { useAsync } from "../../lib/hooks";

import { listPlexServers, type PMSServer } from "../../lib/plex-servers";

interface PMSAccount {
  id: number;
  name: string;
  owner: boolean;
  userId: number;
}

interface PMSUnmatched {
  direction: string;
  ref: string;
  title: string;
  reason: string;
  seenAt: number;
}

type ServerDraft = { id?: number; name: string; url: string; token: string };
type MappingDraft = {
  server: PMSServer;
  account: PMSAccount;
  userId: number;
  direction: number;
  interval: number;
  token: string;
  hasToken: boolean;
};

const INTERVALS = [5, 10, 30, 60];

function problemText(code: string): string {
  switch (code) {
    case "token":
      return t("The server refused its token. Edit the server and paste a new token.");
    case "account_token":
      return t("The server refused this person's token. Paste a new one.");
    case "unreachable":
      return t("Ether can't reach this server. Check that it's running and the address is right.");
    case "rate_limited":
      return t("The server asked Ether to slow down. Sync resumes on its own.");
    case "server":
      return t("The server reported an error. Ether will try again.");
    case "forbidden":
      return t("The server didn't allow this. Check that the token is the server owner's.");
    case "":
      return "";
    default:
      return t("Sync stopped on an unexpected error. Ether will try again.");
  }
}

function reasonText(code: string): string {
  switch (code) {
    case "no_match":
      return t("No TMDB, IMDb or TVDB match");
    case "kind":
      return t("Not a movie or episode");
    case "not_on_server":
      return t("Not on this server");
    case "no_plex_id":
      return t("No Plex identity yet");
    default:
      return t("Not matched");
  }
}

function directionText(direction: number): string {
  switch (direction) {
    case 1:
      return t("Ether → Plex");
    case 2:
      return t("Plex → Ether");
    case 3:
      return t("Both");
    default:
      return t("Off");
  }
}

function errorText(e: unknown): string {
  const code = (e as { code?: string })?.code;
  switch (code) {
    case "token":
      return t("The server refused this token.");
    case "not_owner":
      return t("This token works, but it isn't the server owner's.");
    case "unreachable":
      return t("Ether can't reach this server. Check that it's running and the address is right.");
    case "duplicate":
      return t("This server, or this viewer, is already set up here.");
    case "different_server":
      return t("That address is a different Plex Media Server. Add it as a new server instead.");
    case "needs_token":
      return t("Sending to Plex for this person needs their own token for this server.");
    case "address":
      return t("Enter the server address, like http://192.168.1.10:32400");
    case "limit":
      return t("You've reached the limit for Plex Media Servers.");
    default:
      return t("Plex Media Server settings were not saved.");
  }
}

const when = (seconds: number) => (seconds ? formatDateTime(new Date(seconds * 1000)) : t("Never"));

/** Uses the overview persistence queue and global switch lock. */
export function PlexServersCard({
  enabled,
  busy,
  disabled,
  onToggle,
}: {
  enabled: boolean;
  busy: boolean;
  disabled: boolean;
  onToggle: (next: boolean) => void;
}) {
  const { pathname } = useLocation();
  const servers = useAsync((signal) => listPlexServers(signal), [pathname]);
  const list = servers.data ?? [];
  const attention = list.some((s) => s.problem || s.viewers.some((v) => v.problem));
  return (
    <IntegrationAppCard
      identity={{ id: "plexservers", name: t("Plex watch sync") }}
      logo={<PlexMark />}
      description={t("Sync watched marks with other Plex Media Servers you run alongside Ether.")}
      enabled={enabled}
      busy={busy || servers.loading}
      disabled={disabled || !!servers.error}
      onToggle={onToggle}
      status={
        !enabled ? (
          <Badge variant="secondary">{t("Off")}</Badge>
        ) : servers.error ? (
          <Badge variant="warning">{t("Needs attention")}</Badge>
        ) : list.length === 0 ? (
          <Badge variant="secondary">{t("Not set up")}</Badge>
        ) : attention ? (
          <Badge variant="warning">{t("Needs attention")}</Badge>
        ) : (
          <Badge variant="success">{t("On")}</Badge>
        )
      }
      configureHref="/services/plexservers"
      configureLabel={list.length ? t("Manage Plex watch sync") : t("Set up Plex watch sync")}
    />
  );
}

/** Integrations › Plex Media Servers: server-to-server watch sync. */
export function PlexServers({ enabled }: { enabled: boolean }) {
  const { pathname } = useLocation();
  const servers = useAsync((signal) => listPlexServers(signal), [pathname]);
  const viewers = useAsync(() => api.viewers(), []);
  const [server, setServer] = useState<ServerDraft | null>(null);
  const [mapping, setMapping] = useState<MappingDraft | null>(null);
  const [accounts, setAccounts] = useState<Record<number, PMSAccount[] | "error">>({});
  const [unmatched, setUnmatched] = useState<{
    server: PMSServer;
    viewer: number;
    items: PMSUnmatched[];
    more: boolean;
  } | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>(null);

  const replace = (next: PMSServer) =>
    servers.setData((servers.data ?? []).map((s) => (s.id === next.id ? next : s)));
  const viewerName = (id: number) => {
    const v = (viewers.data ?? []).find((x) => x.id === id);
    return v ? v.title || v.username : t("Unknown viewer");
  };

  const loadAccounts = async (srv: PMSServer) => {
    try {
      const list = await apiFetch<PMSAccount[]>(`/plex-servers/${srv.id}/accounts`);
      setAccounts((old) => ({ ...old, [srv.id]: list }));
    } catch {
      setAccounts((old) => ({ ...old, [srv.id]: "error" }));
    }
  };

  const saveServer = async (draft: ServerDraft) => {
    setBusy(true);
    setError(null);
    try {
      const body = JSON.stringify({ name: draft.name, url: draft.url, token: draft.token });
      const next = draft.id
        ? await apiFetch<PMSServer>(`/plex-servers/${draft.id}`, { method: "PUT", body })
        : await apiFetch<PMSServer>("/plex-servers", { method: "POST", body });
      if (draft.id) replace(next);
      else servers.setData([...(servers.data ?? []), next]);
      setServer(null);
      void loadAccounts(next);
      toast.success(t("Plex Media Server saved"));
    } catch (e) {
      setError(e);
    } finally {
      setBusy(false);
    }
  };

  const removeServer = async (id: number) => {
    if (!window.confirm(t("Remove this server? Watched marks already synced stay where they are.")))
      return;
    setBusy(true);
    try {
      await apiFetch(`/plex-servers/${id}`, { method: "DELETE" });
      servers.setData((servers.data ?? []).filter((s) => s.id !== id));
      setServer(null);
    } catch (e) {
      setError(e);
    } finally {
      setBusy(false);
    }
  };

  const saveMapping = async (draft: MappingDraft, userId = draft.userId) => {
    setBusy(true);
    setError(null);
    try {
      const body: Record<string, unknown> = {
        userId,
        direction: draft.direction,
        interval: draft.interval,
      };
      if (draft.token) body.token = draft.token;
      const next = await apiFetch<PMSServer>(
        `/plex-servers/${draft.server.id}/accounts/${draft.account.id}`,
        { method: "PUT", body: JSON.stringify(body) },
      );
      replace(next);
      setAccounts((old) => {
        const list = old[draft.server.id];
        if (!Array.isArray(list)) return old;
        return {
          ...old,
          [draft.server.id]: list.map((a) => (a.id === draft.account.id ? { ...a, userId } : a)),
        };
      });
      setMapping(null);
    } catch (e) {
      setError(e);
    } finally {
      setBusy(false);
    }
  };

  const syncNow = async (srv: PMSServer) => {
    setBusy(true);
    try {
      replace(await apiFetch<PMSServer>(`/plex-servers/${srv.id}/sync`, { method: "POST" }));
    } catch {
      toast.error(t("Plex Media Server sync didn't run"));
    } finally {
      setBusy(false);
    }
  };

  const openUnmatched = async (srv: PMSServer, viewer: number, after = "") => {
    setBusy(true);
    try {
      const items = await apiFetch<PMSUnmatched[]>(
        `/plex-servers/${srv.id}/unmatched?viewer=${viewer}&after=${encodeURIComponent(after)}`,
      );
      setUnmatched((old) => ({
        server: srv,
        viewer,
        items: after && old ? [...old.items, ...items] : items,
        more: items.length === 100,
      }));
    } catch {
      toast.error(t("Unmatched titles could not be loaded"));
    } finally {
      setBusy(false);
    }
  };

  // Each server's people are listed live from that server, once per visit.
  const serverIDs = (servers.data ?? []).map((s) => s.id).join(",");
  useEffect(() => {
    for (const srv of servers.data ?? []) {
      if (accounts[srv.id] === undefined) void loadAccounts(srv);
    }
    // loadAccounts only sets state; re-running on accounts would refetch failures.
  }, [serverIDs]);

  const openMapping = (srv: PMSServer, account: PMSAccount) => {
    const m = srv.viewers.find((v) => v.accountId === account.id);
    setError(null);
    setMapping({
      server: srv,
      account,
      userId: m?.userId ?? 0,
      direction: m?.direction ?? (account.owner ? 3 : 2),
      interval: m?.interval ?? 10,
      token: "",
      hasToken: !!m && m.canSend && !account.owner,
    });
  };

  if (servers.error)
    return (
      <ErrorPanel
        error={new Error(t("Plex Media Servers could not be loaded"))}
        onRetry={() => void servers.reload()}
      />
    );
  if (!servers.data)
    return (
      <div className="py-6 text-center">
        <Spinner size={18} label={t("Loading Plex Media Servers")} />
      </div>
    );

  const list = servers.data;
  const canSend = mapping ? mapping.account.owner || mapping.hasToken || !!mapping.token : false;
  return (
    <div className="space-y-5">
      <SettingsSection
        description={t(
          "Add a Plex Media Server you run, then choose which of its people match which viewers. Watched and unwatched marks, and resume points sent to Plex, follow each person's direction. Nothing syncs for people you don't match.",
        )}
        footer={
          <Button
            size="sm"
            disabled={busy}
            onClick={() => {
              setError(null);
              setServer({ name: "", url: "", token: "" });
            }}
          >
            {t("Add server")}
          </Button>
        }
      >
        {list.length === 0 ? (
          <p className="px-5 py-4 text-sm text-fg-muted">{t("No Plex Media Servers yet.")}</p>
        ) : null}
      </SettingsSection>
      {list.map((srv) => {
        const accts = accounts[srv.id];
        return (
          <SettingsSection key={srv.id}>
            <h2 className="flex flex-wrap items-center gap-2 px-5 py-4 text-sm font-semibold">
              {srv.name}
              {srv.problem ? (
                <Badge variant="warning">{t("Needs attention")}</Badge>
              ) : (
                <Badge variant="success">{t("Connected")}</Badge>
              )}
              <span className="font-normal text-fg-muted">
                {srv.url}
                {srv.version ? ` · ${srv.version}` : ""}
              </span>
              <span className="ms-auto flex gap-2">
                <Button
                  size="sm"
                  variant="outline"
                  disabled={busy || !enabled}
                  onClick={() => void syncNow(srv)}
                >
                  {t("Sync now")}
                </Button>
                <Button
                  size="sm"
                  variant="outline"
                  disabled={busy}
                  onClick={() => {
                    setError(null);
                    setServer({ id: srv.id, name: srv.name, url: srv.url, token: "" });
                  }}
                >
                  {t("Edit")}
                </Button>
              </span>
            </h2>
            {srv.problem ? (
              <p className="px-5 pb-3 text-sm text-warning">{problemText(srv.problem)}</p>
            ) : null}
            {accts === "error" ? (
              <p className="px-5 pb-4 text-sm text-fg-muted">
                {t("This server's people could not be listed.")}{" "}
                <Button size="sm" variant="ghost" onClick={() => void loadAccounts(srv)}>
                  {t("Try again")}
                </Button>
              </p>
            ) : !accts ? (
              <div className="px-5 pb-4">
                <Spinner size={14} label={t("Loading people")} />
              </div>
            ) : (
              accts.map((account) => {
                const m = srv.viewers.find((v) => v.accountId === account.id);
                return (
                  <SettingsRow
                    key={account.id}
                    title={
                      <span className="flex items-center gap-2">
                        {account.name || t("Plex account {id}", { id: account.id })}
                        {account.owner ? (
                          <Badge variant="secondary">{t("Server owner")}</Badge>
                        ) : null}
                      </span>
                    }
                    description={
                      m ? (
                        <span className="space-y-0.5">
                          <span className="block">
                            {t("{viewer} · {direction} · every {interval}", {
                              viewer: viewerName(m.userId),
                              direction: directionText(m.direction),
                              interval: tn(m.interval, "{count} minute", "{count} minutes"),
                            })}
                          </span>
                          <span className="block">
                            {t("Last read {last} · Last sent {sent} · Next {next}", {
                              last: when(m.lastImport),
                              sent: when(m.lastSend),
                              next: m.direction ? when(m.nextSync) : t("Off"),
                            })}
                          </span>
                          {m.problem ? (
                            <span className="block text-warning">{problemText(m.problem)}</span>
                          ) : null}
                        </span>
                      ) : (
                        t("Not synced")
                      )
                    }
                    control={
                      <span className="flex flex-wrap gap-2">
                        {m && m.unmatched > 0 ? (
                          <Button
                            size="sm"
                            variant="ghost"
                            disabled={busy}
                            onClick={() => void openUnmatched(srv, m.userId)}
                          >
                            {tn(m.unmatched, "{count} unmatched title", "{count} unmatched titles")}
                          </Button>
                        ) : null}
                        <Button
                          size="sm"
                          variant="outline"
                          disabled={busy}
                          onClick={() => openMapping(srv, account)}
                        >
                          {m ? t("Change") : t("Match to a viewer")}
                        </Button>
                      </span>
                    }
                  />
                );
              })
            )}
          </SettingsSection>
        );
      })}

      <Drawer
        open={!!server}
        onClose={() => {
          if (!busy) setServer(null);
        }}
        title={server?.id ? t("Edit Plex Media Server") : t("Add server")}
        footer={
          <div className="flex w-full justify-between gap-2">
            {server?.id ? (
              <Button
                variant="ghost"
                size="sm"
                disabled={busy}
                onClick={() => void removeServer(server.id!)}
              >
                {t("Remove server")}
              </Button>
            ) : (
              <span />
            )}
            <span className="flex gap-2">
              <Button variant="ghost" size="sm" disabled={busy} onClick={() => setServer(null)}>
                {t("Cancel")}
              </Button>
              <Button
                size="sm"
                disabled={busy || !server?.url.trim() || (!server?.id && !server?.token.trim())}
                onClick={() => server && void saveServer(server)}
              >
                {busy ? t("Checking the server…") : t("Save")}
              </Button>
            </span>
          </div>
        }
      >
        {server ? (
          <div className="space-y-4">
            {error ? <MutationError error={new Error(errorText(error))} /> : null}
            <Field label={t("Name")} htmlFor="pms-name">
              <Input
                id="pms-name"
                value={server.name}
                placeholder={t("Living room server")}
                onChange={(e) => setServer({ ...server, name: e.target.value })}
              />
            </Field>
            <Field
              label={t("Address")}
              htmlFor="pms-url"
              hint={t("The address you'd use on your network, with its port.")}
            >
              <Input
                id="pms-url"
                value={server.url}
                placeholder="http://192.168.1.10:32400"
                onChange={(e) => setServer({ ...server, url: e.target.value })}
              />
            </Field>
            <Field
              label={t("Server owner's Plex token")}
              htmlFor="pms-token"
              hint={
                server.id
                  ? t("Leave blank to keep the saved token. Ether never shows it again.")
                  : t(
                      "In Plex Web, open any title's ⋯ menu, choose Get Info, then View XML. The token is the X-Plex-Token at the end of the address. Ether stores it encrypted and never shows it again.",
                    )
              }
            >
              <SecretInput
                id="pms-token"
                value={server.token}
                onChange={(token) => setServer({ ...server, token })}
              />
            </Field>
          </div>
        ) : null}
      </Drawer>

      <Drawer
        open={!!mapping}
        onClose={() => {
          if (!busy) setMapping(null);
        }}
        title={
          mapping
            ? t("Sync {name}", {
                name: mapping.account.name || t("Plex account {id}", { id: mapping.account.id }),
              })
            : ""
        }
        footer={
          <div className="flex w-full justify-between gap-2">
            {mapping && mapping.server.viewers.some((v) => v.accountId === mapping.account.id) ? (
              <Button
                variant="ghost"
                size="sm"
                disabled={busy}
                onClick={() => mapping && void saveMapping(mapping, 0)}
              >
                {t("Stop syncing")}
              </Button>
            ) : (
              <span />
            )}
            <span className="flex gap-2">
              <Button variant="ghost" size="sm" disabled={busy} onClick={() => setMapping(null)}>
                {t("Cancel")}
              </Button>
              <Button
                size="sm"
                disabled={busy || !mapping?.userId || ((mapping.direction & 1) !== 0 && !canSend)}
                onClick={() => mapping && void saveMapping(mapping)}
              >
                {t("Save")}
              </Button>
            </span>
          </div>
        }
      >
        {mapping ? (
          <div className="space-y-4">
            {error ? <MutationError error={new Error(errorText(error))} /> : null}
            <Field label={t("Ether viewer")} htmlFor="pms-viewer">
              <Select
                id="pms-viewer"
                value={mapping.userId}
                onChange={(e) => setMapping({ ...mapping, userId: Number(e.currentTarget.value) })}
              >
                <option value={0}>{t("Choose a viewer")}</option>
                {(viewers.data ?? []).map((v) => (
                  <option key={v.id} value={v.id}>
                    {v.title || v.username}
                  </option>
                ))}
              </Select>
            </Field>
            <Field
              label={t("Sync direction")}
              htmlFor="pms-direction"
              hint={t("Turning a direction on starts from now. Past history isn't copied.")}
            >
              <Select
                id="pms-direction"
                value={mapping.direction}
                onChange={(e) =>
                  setMapping({ ...mapping, direction: Number(e.currentTarget.value) })
                }
              >
                {[0, 1, 2, 3].map((d) => (
                  <option key={d} value={d}>
                    {directionText(d)}
                  </option>
                ))}
              </Select>
            </Field>
            <Field label={t("Check every")} htmlFor="pms-interval">
              <Select
                id="pms-interval"
                value={mapping.interval}
                onChange={(e) =>
                  setMapping({ ...mapping, interval: Number(e.currentTarget.value) })
                }
              >
                {INTERVALS.map((n) => (
                  <option value={n} key={n}>
                    {tn(n, "{count} minute", "{count} minutes")}
                  </option>
                ))}
              </Select>
            </Field>
            {!mapping.account.owner ? (
              <Field
                label={t("This person's Plex token for this server")}
                htmlFor="pms-account-token"
                hint={
                  mapping.hasToken
                    ? t("Saved. Leave blank to keep it.")
                    : t(
                        "Only needed to send to Plex: Plex marks titles watched for whoever's token is used. Reading what they watched works with the owner's token.",
                      )
                }
              >
                <SecretInput
                  id="pms-account-token"
                  value={mapping.token}
                  onChange={(token) => setMapping({ ...mapping, token })}
                />
              </Field>
            ) : null}
          </div>
        ) : null}
      </Drawer>

      <Drawer
        open={!!unmatched}
        onClose={() => setUnmatched(null)}
        title={t("Unmatched titles")}
        description={t(
          "Titles are matched only by TMDB, IMDb or TVDB identity, never by name. These were skipped and are checked again later.",
        )}
      >
        {unmatched ? (
          <div className="space-y-2">
            <ul className="divide-y divide-border text-sm">
              {unmatched.items.map((u) => (
                <li key={`${u.direction}:${u.ref}`} className="py-2">
                  <div className="font-medium">{u.title || u.ref}</div>
                  <div className="text-fg-muted">
                    {u.direction === "in"
                      ? t("From Plex: {reason}", { reason: reasonText(u.reason) })
                      : t("To Plex: {reason}", { reason: reasonText(u.reason) })}
                  </div>
                </li>
              ))}
            </ul>
            {unmatched.more ? (
              <Button
                size="sm"
                variant="outline"
                disabled={busy}
                onClick={() => {
                  const last = unmatched.items[unmatched.items.length - 1];
                  if (!last) return;
                  void openUnmatched(
                    unmatched.server,
                    unmatched.viewer,
                    `${last.direction}:${last.ref}`,
                  );
                }}
              >
                {t("Load more")}
              </Button>
            ) : null}
          </div>
        ) : null}
      </Drawer>
    </div>
  );
}
