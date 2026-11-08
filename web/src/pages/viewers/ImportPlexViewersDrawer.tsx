import {
  Badge,
  Button,
  EmptyState,
  Input,
  Select,
  Spinner,
  toast,
} from "@braedonsaunders/appkit-ui";
import { useCallback, useEffect, useState } from "react";
import { Copy, ExternalLink, MonitorSmartphone, Plus, RefreshCw, Search, UserCheck } from "lucide-react";
import { useNavigate } from "react-router-dom";
import { Drawer } from "../../components/Drawer";
import { copyText } from "../../components/CopyButton";
import { t } from "../../i18n";
import { api, apiFetch, errorMessage } from "../../lib/api";
import type { Viewer } from "../../lib/contract";

export interface PlexCandidateAccount {
  serverId: number;
  serverName: string;
  accountId: number;
  accountName: string;
  isOwner: boolean;
  matchedViewerId: number;
  matchedViewerName?: string;
  matchedViewerUsername?: string;
  suggestedViewerId?: number;
  suggestedViewerName?: string;
}

export interface PlexCandidateServer {
  serverId: number;
  serverName: string;
  accounts: PlexCandidateAccount[];
}

export function ImportPlexViewersDrawer({
  open,
  onClose,
  onImported,
}: {
  open: boolean;
  onClose: () => void;
  onImported: () => void;
}) {
  const navigate = useNavigate();
  const [loading, setLoading] = useState(false);
  const [searchQuery, setSearchQuery] = useState("");
  const [servers, setServers] = useState<PlexCandidateServer[]>([]);
  const [viewers, setViewers] = useState<Viewer[]>([]);
  const [createdInvites, setCreatedInvites] = useState<Record<string, string>>({});
  const [busyItem, setBusyItem] = useState<string | null>(null);
  const [busyAll, setBusyAll] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const load = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const [candidateServers, viewerList] = await Promise.all([
        apiFetch<PlexCandidateServer[]>("/viewers/plex-candidates"),
        api.viewers().catch(() => [] as Viewer[]),
      ]);
      setServers(candidateServers || []);
      setViewers(viewerList || []);
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    if (open) {
      void load();
    }
  }, [open, load]);

  const allAccounts = servers.flatMap((s) => s.accounts);
  const unmappedAccounts = allAccounts.filter((a) => !a.matchedViewerId);

  const importSingle = async (acct: PlexCandidateAccount) => {
    const key = `${acct.serverId}:${acct.accountId}`;
    setBusyItem(key);
    try {
      const res = await apiFetch<{
        id: number;
        name: string;
        inviteUrl?: string;
        sharedPlex?: boolean;
      }>("/viewers/import-plex", {
        method: "POST",
        body: JSON.stringify({
          serverId: acct.serverId,
          accountId: acct.accountId,
          name: acct.accountName,
        }),
      });
      if (res.sharedPlex) {
        toast.success(
          t("Viewer {name} added and library access granted on Plex.tv", { name: res.name }),
        );
      } else {
        toast.success(t("Viewer {name} added and matched", { name: res.name }));
      }
      if (res.inviteUrl) {
        const fullUrl = new URL(res.inviteUrl, window.location.origin).href;
        setCreatedInvites((prev) => ({ ...prev, [key]: fullUrl }));
      }
      onImported();
      await load();
    } catch (err) {
      toast.error(errorMessage(err));
    } finally {
      setBusyItem(null);
    }
  };

  const matchExisting = async (acct: PlexCandidateAccount, viewerId: number) => {
    if (!viewerId) return;
    const key = `${acct.serverId}:${acct.accountId}`;
    setBusyItem(key);
    try {
      await apiFetch("/viewers/match-plex", {
        method: "POST",
        body: JSON.stringify({
          serverId: acct.serverId,
          accountId: acct.accountId,
          viewerId,
        }),
      });
      toast.success(t("Account matched to viewer"));
      onImported();
      await load();
    } catch (err) {
      toast.error(errorMessage(err));
    } finally {
      setBusyItem(null);
    }
  };

  const importAll = async () => {
    if (!unmappedAccounts.length) return;
    setBusyAll(true);
    try {
      const imported = await apiFetch<
        Array<{ id: number; name: string; inviteUrl?: string; serverId: number; accountId: number }>
      >("/viewers/import-plex/all", {
        method: "POST",
      });
      toast.success(t("Imported {count} viewers from Plex", { count: imported.length }));
      const newInvites: Record<string, string> = {};
      for (const item of imported) {
        if (item.inviteUrl) {
          const fullUrl = new URL(item.inviteUrl, window.location.origin).href;
          newInvites[`${item.serverId}:${item.accountId}`] = fullUrl;
        }
      }
      if (Object.keys(newInvites).length > 0) {
        setCreatedInvites((prev) => ({ ...prev, ...newInvites }));
      }
      onImported();
      await load();
    } catch (err) {
      toast.error(errorMessage(err));
    } finally {
      setBusyAll(false);
    }
  };

  return (
    <Drawer
      open={open}
      onClose={onClose}
      title={t("Import users from connected Plex servers")}
      size="lg"
    >
      <div className="space-y-5">
        <p className="text-sm text-fg-muted">
          {t(
            "Pull accounts from your connected Plex Media Servers to add them as Ether viewers and enable watch state synchronization.",
          )}
        </p>

        <div className="flex items-center justify-between gap-3 rounded-lg border border-border bg-bg-subtle p-3 text-xs text-fg-muted">
          <span>
            {t(
              "Libraries are automatically shared when imported. You can also view or manage access directly on Plex.tv.",
            )}
          </span>
          <a
            href="https://app.plex.tv/desktop/#!/settings/manage-library-access"
            target="_blank"
            rel="noopener noreferrer"
            className="inline-flex items-center gap-1 font-medium text-primary hover:underline whitespace-nowrap"
          >
            {t("Manage Library Access")}
            <ExternalLink size={12} />
          </a>
        </div>

        {loading ? (
          <div className="py-12 text-center">
            <Spinner size={20} label={t("Loading Plex accounts…")} />
          </div>
        ) : error ? (
          <div className="rounded-lg border border-danger/30 bg-danger-subtle p-4 text-sm text-danger space-y-2">
            <p>{error}</p>
            <Button size="sm" variant="outline" onClick={() => void load()}>
              <RefreshCw size={12} /> {t("Try again")}
            </Button>
          </div>
        ) : servers.length === 0 || allAccounts.length === 0 ? (
          <EmptyState
            icon={<MonitorSmartphone />}
            title={t("No connected Plex Media Servers found")}
            description={t(
              "Connect your Plex Media Server under Integrations › Plex watch sync first to discover and import its users.",
            )}
            action={
              <Button
                size="sm"
                onClick={() => {
                  onClose();
                  navigate("/services/plexservers");
                }}
              >
                <ExternalLink size={14} /> {t("Go to Plex watch sync")}
              </Button>
            }
          />
        ) : (
          <div className="space-y-6">
            <div className="relative">
              <Search className="absolute left-3 top-2.5 text-fg-muted pointer-events-none" size={16} />
              <Input
                className="pl-9"
                placeholder={t("Search by account name or Plex username…")}
                aria-label={t("Search by account name or Plex username")}
                value={searchQuery}
                onChange={(e) => setSearchQuery(e.target.value)}
              />
            </div>

            <div className="flex flex-wrap items-center justify-between gap-3 rounded-lg border border-border bg-bg-subtle p-3">
              <div>
                <p className="text-sm font-medium">
                  {t("{count} unmapped accounts available", { count: unmappedAccounts.length })}
                </p>
                <p className="text-xs text-fg-muted">
                  {t("Found across {count} connected servers", { count: servers.length })}
                </p>
              </div>
              <Button
                size="sm"
                disabled={busyAll || unmappedAccounts.length === 0}
                onClick={() => void importAll()}
              >
                {busyAll ? t("Importing…") : t("Import all ({count})", { count: unmappedAccounts.length })}
              </Button>
            </div>

            {servers.map((srv) => {
              const q = searchQuery.trim().toLowerCase();
              const visibleAccounts = srv.accounts.filter((acct) => {
                if (!q) return true;
                return (
                  acct.accountName.toLowerCase().includes(q) ||
                  (acct.matchedViewerName && acct.matchedViewerName.toLowerCase().includes(q)) ||
                  (acct.matchedViewerUsername && acct.matchedViewerUsername.toLowerCase().includes(q)) ||
                  (acct.suggestedViewerName && acct.suggestedViewerName.toLowerCase().includes(q))
                );
              });

              if (visibleAccounts.length === 0 && q) {
                return null;
              }

              return (
                <div key={srv.serverId} className="rounded-lg border border-border overflow-hidden">
                  <div className="bg-surface border-b border-border px-4 py-2.5 flex items-center justify-between">
                    <div className="flex items-center gap-2">
                      <MonitorSmartphone size={16} className="text-primary" />
                      <span className="text-sm font-semibold">{srv.serverName}</span>
                    </div>
                    <span className="text-xs text-fg-muted">
                      {t("{count} accounts", { count: srv.accounts.length })}
                    </span>
                  </div>

                  <ul className="divide-y divide-border">
                    {visibleAccounts.map((acct) => {
                      const key = `${acct.serverId}:${acct.accountId}`;
                      const invite = createdInvites[key];
                      const isBusy = busyItem === key || busyAll;

                      return (
                        <li key={acct.accountId} className="p-4 space-y-2">
                          <div className="flex flex-wrap items-center justify-between gap-3">
                            <div className="space-y-0.5">
                              <div className="flex items-center gap-2">
                                <span className="text-sm font-medium">
                                  {acct.accountName || t("Account {id}", { id: acct.accountId })}
                                </span>
                                {acct.matchedViewerUsername &&
                                acct.matchedViewerUsername.toLowerCase() !== acct.accountName.toLowerCase() ? (
                                  <span className="text-xs text-fg-muted font-normal">
                                    (@{acct.matchedViewerUsername})
                                  </span>
                                ) : null}
                                {acct.isOwner ? (
                                  <Badge variant="secondary">{t("Server owner")}</Badge>
                                ) : null}
                              </div>
                              <div className="text-xs text-fg-muted">
                                {acct.matchedViewerId ? (
                                  <span className="flex items-center gap-1 text-success">
                                    <UserCheck size={12} />
                                    {t("Matched to {name}", {
                                      name: acct.matchedViewerName || t("Viewer #{id}", { id: acct.matchedViewerId }),
                                    })}
                                    {acct.matchedViewerUsername &&
                                    acct.matchedViewerUsername.toLowerCase() !== (acct.matchedViewerName || "").toLowerCase() ? (
                                      <span className="text-fg-muted font-normal">
                                        (@{acct.matchedViewerUsername})
                                      </span>
                                    ) : null}
                                  </span>
                                ) : acct.suggestedViewerName ? (
                                  <span className="flex items-center gap-1 text-info">
                                    {t("Suggested match: {name}", { name: acct.suggestedViewerName })}
                                  </span>
                                ) : (
                                  <span>{t("Not added to Ether yet")}</span>
                                )}
                              </div>
                            </div>

                            <div className="flex flex-wrap items-center gap-2">
                              {acct.matchedViewerId ? (
                                <Badge variant="success">{t("Active")}</Badge>
                              ) : (
                                <>
                                  <Button
                                    size="sm"
                                    disabled={isBusy}
                                    onClick={() => void importSingle(acct)}
                                  >
                                    <Plus size={12} /> {t("Add as viewer")}
                                  </Button>

                                  <div className="w-48">
                                    <Select
                                      value={acct.suggestedViewerId || 0}
                                      disabled={isBusy}
                                      aria-label={t("Match account to existing viewer")}
                                      onChange={(e) => void matchExisting(acct, Number(e.currentTarget.value))}
                                    >
                                      <option value={0}>{t("Match existing…")}</option>
                                      {viewers
                                        .filter((v) => !srv.accounts.some((a) => a.matchedViewerId === v.id))
                                        .map((v) => {
                                          const label =
                                            v.title && v.username && v.title.toLowerCase() !== v.username.toLowerCase()
                                              ? `${v.title} (@${v.username})`
                                              : v.title || v.username;
                                          return (
                                            <option key={v.id} value={v.id}>
                                              {label}
                                            </option>
                                          );
                                        })}
                                    </Select>
                                  </div>
                                </>
                              )}
                            </div>
                          </div>

                          {invite ? (
                            <div className="flex items-center justify-between gap-2 rounded bg-bg-subtle p-2 text-xs">
                              <span className="truncate text-fg-muted">{invite}</span>
                              <Button
                                size="sm"
                                variant="outline"
                                onClick={() => {
                                  void copyText(invite);
                                  toast.success(t("Invite link copied"));
                                }}
                              >
                                <Copy size={12} /> {t("Copy link")}
                              </Button>
                            </div>
                          ) : null}
                        </li>
                      );
                    })}
                  </ul>
                </div>
              );
            })}

            {servers.every((srv) => {
              const q = searchQuery.trim().toLowerCase();
              return srv.accounts.filter((acct) => {
                if (!q) return true;
                return (
                  acct.accountName.toLowerCase().includes(q) ||
                  (acct.matchedViewerName && acct.matchedViewerName.toLowerCase().includes(q)) ||
                  (acct.matchedViewerUsername && acct.matchedViewerUsername.toLowerCase().includes(q)) ||
                  (acct.suggestedViewerName && acct.suggestedViewerName.toLowerCase().includes(q))
                );
              }).length === 0;
            }) && searchQuery.trim() ? (
              <div className="py-8 text-center text-sm text-fg-muted">
                {t("No accounts match \"{query}\"", { query: searchQuery.trim() })}
              </div>
            ) : null}
          </div>
        )}
      </div>
    </Drawer>
  );
}
