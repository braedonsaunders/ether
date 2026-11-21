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
import {
  Copy,
  ExternalLink,
  MonitorSmartphone,
  Plus,
  RefreshCw,
  Search,
  UserCheck,
} from "lucide-react";
import { useNavigate } from "react-router-dom";
import { Drawer } from "../../components/Drawer";
import { copyText } from "../../components/CopyButton";
import { t, tn } from "../../i18n";
import { api, apiFetch, errorMessage } from "../../lib/api";
import type { Viewer } from "../../lib/contract";

export type PlexRelation = "owner" | "home" | "shared" | "pending" | "friend" | "historical";

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
  plexUsername?: string;
  plexRelation?: PlexRelation;
  plexSources?: string[];
}

export interface PlexCandidateServer {
  serverId: number;
  serverName: string;
  plexDirectoryTotal?: number;
  filteredOutCount?: number;
  accounts: PlexCandidateAccount[];
}

type CandidateFilter =
  "all" | "unmapped" | "shared" | "home" | "pending" | "friend" | "community" | "historical";

const hasSource = (acct: PlexCandidateAccount, source: string) =>
  Boolean(acct.plexSources?.includes(source));
const isAcceptedShare = (acct: PlexCandidateAccount) =>
  acct.plexRelation === "shared" ||
  hasSource(acct, "ether_share") ||
  hasSource(acct, "pms_share") ||
  hasSource(acct, "accepted_share");
const isHomeUser = (acct: PlexCandidateAccount) =>
  acct.plexRelation === "home" || hasSource(acct, "home");
const isPendingInvite = (acct: PlexCandidateAccount) =>
  acct.plexRelation === "pending" || hasSource(acct, "pending_invite");
const isFriend = (acct: PlexCandidateAccount) =>
  acct.plexRelation === "friend" || hasSource(acct, "friend");
const isCommunityFriend = (acct: PlexCandidateAccount) => hasSource(acct, "community_friend");
const isHistorical = (acct: PlexCandidateAccount) => acct.plexRelation === "historical";

function matchesFilter(acct: PlexCandidateAccount, filter: CandidateFilter): boolean {
  switch (filter) {
    case "unmapped":
      return !acct.matchedViewerId;
    case "shared":
      return isAcceptedShare(acct);
    case "home":
      return isHomeUser(acct);
    case "pending":
      return isPendingInvite(acct);
    case "friend":
      return isFriend(acct);
    case "community":
      return isCommunityFriend(acct);
    case "historical":
      return isHistorical(acct);
    default:
      return true;
  }
}

function matchesSearch(acct: PlexCandidateAccount, q: string): boolean {
  if (!q) return true;
  return (
    acct.accountName.toLowerCase().includes(q) ||
    Boolean(acct.plexUsername && acct.plexUsername.toLowerCase().includes(q)) ||
    Boolean(acct.matchedViewerName && acct.matchedViewerName.toLowerCase().includes(q)) ||
    Boolean(acct.matchedViewerUsername && acct.matchedViewerUsername.toLowerCase().includes(q)) ||
    Boolean(acct.suggestedViewerName && acct.suggestedViewerName.toLowerCase().includes(q))
  );
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
  const [activeFilter, setActiveFilter] = useState<CandidateFilter>("all");
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
        status?: string;
        sharedPlex?: boolean;
        shareError?: string;
      }>("/viewers/import-plex", {
        method: "POST",
        body: JSON.stringify({
          serverId: acct.serverId,
          accountId: acct.accountId,
          name: acct.accountName,
        }),
      });
      if (res.shareError) {
        toast.warning(
          t("Viewer {name} added, but Plex library access failed: {err}", {
            name: res.name,
            err: res.shareError,
          }),
        );
      } else if (res.sharedPlex) {
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
      const res = await apiFetch<{
        ok: boolean;
        viewerId: number;
        serverId: number;
        accountId: number;
        sharedPlex?: boolean;
        shareError?: string;
      }>("/viewers/match-plex", {
        method: "POST",
        body: JSON.stringify({
          serverId: acct.serverId,
          accountId: acct.accountId,
          viewerId,
        }),
      });
      if (res.shareError) {
        toast.warning(
          t("Account matched, but Plex library access failed: {err}", { err: res.shareError }),
        );
      } else {
        toast.success(t("Account matched to viewer"));
      }
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
      const results = await apiFetch<
        Array<{
          id?: number;
          name: string;
          inviteUrl?: string;
          serverId: number;
          accountId: number;
          status: "imported" | "matched" | "skipped" | "failed";
          reason?: string;
          sharedPlex?: boolean;
          shareError?: string;
          error?: string;
        }>
      >("/viewers/import-plex/all", {
        method: "POST",
      });
      const succeeded = results.filter((r) => r.status === "imported" || r.status === "matched");
      const failed = results.filter((r) => r.status === "failed");
      const shareErrors = results.filter((r) => r.shareError);
      if (failed.length > 0) {
        toast.error(
          tn(
            failed.length,
            "Failed to import {count} viewer from Plex",
            "Failed to import {count} viewers from Plex",
          ),
        );
      }
      if (succeeded.length > 0) {
        toast.success(
          tn(
            succeeded.length,
            "Imported {count} viewer from Plex",
            "Imported {count} viewers from Plex",
          ),
        );
      }
      if (shareErrors.length > 0) {
        toast.warning(
          tn(
            shareErrors.length,
            "Plex library access could not be granted for {count} viewer",
            "Plex library access could not be granted for {count} viewers",
          ),
        );
      }
      const newInvites: Record<string, string> = {};
      for (const item of results) {
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

  const hasPlexBreakdown = allAccounts.some((a) => Boolean(a.plexRelation));
  const filteredOutTotal = servers.reduce((sum, s) => sum + (s.filteredOutCount ?? 0), 0);
  const directoryTotal = servers.reduce((max, s) => Math.max(max, s.plexDirectoryTotal ?? 0), 0);
  const plexMatchedCount = allAccounts.filter(
    (a) => a.plexRelation && a.plexRelation !== "historical",
  ).length;
  const acceptedShareCount = allAccounts.filter(isAcceptedShare).length;
  const homeUserCount = allAccounts.filter(isHomeUser).length;
  const pendingInviteCount = allAccounts.filter(isPendingInvite).length;
  const friendCount = allAccounts.filter(isFriend).length;
  const communityFriendCount = allAccounts.filter(isCommunityFriend).length;
  const historicalCount = allAccounts.filter(isHistorical).length;

  const filterChips: Array<{ value: CandidateFilter; label: string }> = [
    { value: "all", label: t("All ({count})", { count: allAccounts.length }) },
    ...(unmappedAccounts.length > 0
      ? [
          {
            value: "unmapped" as const,
            label: t("Unmapped ({count})", { count: unmappedAccounts.length }),
          },
        ]
      : []),
    ...(acceptedShareCount > 0
      ? [
          {
            value: "shared" as const,
            label: t("Accepted shares ({count})", { count: acceptedShareCount }),
          },
        ]
      : []),
    ...(homeUserCount > 0
      ? [
          {
            value: "home" as const,
            label: t("Home users ({count})", { count: homeUserCount }),
          },
        ]
      : []),
    ...(pendingInviteCount > 0
      ? [
          {
            value: "pending" as const,
            label: t("Pending invites ({count})", { count: pendingInviteCount }),
          },
        ]
      : []),
    ...(friendCount > 0
      ? [{ value: "friend" as const, label: t("Friends ({count})", { count: friendCount }) }]
      : []),
    ...(communityFriendCount > 0
      ? [
          {
            value: "community" as const,
            label: t("Community friends ({count})", { count: communityFriendCount }),
          },
        ]
      : []),
    ...(historicalCount > 0
      ? [
          {
            value: "historical" as const,
            label: t("PMS history only ({count})", { count: historicalCount }),
          },
        ]
      : []),
  ];

  const renderAccountRow = (srv: PlexCandidateServer, acct: PlexCandidateAccount) => {
    const key = `${acct.serverId}:${acct.accountId}`;
    const invite = createdInvites[key];
    const isBusy = busyItem === key || busyAll;

    return (
      <li key={acct.accountId} className="p-4 space-y-2">
        <div className="flex flex-wrap items-center justify-between gap-3">
          <div className="space-y-0.5">
            <div className="flex flex-wrap items-center gap-2">
              <span className="text-sm font-medium">
                {acct.accountName || t("Account {id}", { id: acct.accountId })}
              </span>
              {acct.matchedViewerUsername &&
              acct.matchedViewerUsername.toLowerCase() !== acct.accountName.toLowerCase() ? (
                <span className="text-xs text-fg-muted font-normal">
                  (@{acct.matchedViewerUsername})
                </span>
              ) : acct.plexUsername &&
                acct.plexUsername.toLowerCase() !== acct.accountName.toLowerCase() ? (
                <span className="text-xs text-fg-muted font-normal">(@{acct.plexUsername})</span>
              ) : null}
              {acct.isOwner ? <Badge variant="secondary">{t("Server owner")}</Badge> : null}
              {!acct.isOwner && isHomeUser(acct) ? <Badge variant="info">{t("Home")}</Badge> : null}
              {!acct.isOwner && hasSource(acct, "ether_share") ? (
                <Badge variant="success">{t("Shared on Ether")}</Badge>
              ) : null}
              {!acct.isOwner && hasSource(acct, "pms_share") ? (
                <Badge variant="secondary">
                  {t("Shared on {server}", { server: acct.serverName })}
                </Badge>
              ) : null}
              {!acct.isOwner &&
              !hasSource(acct, "ether_share") &&
              !hasSource(acct, "pms_share") &&
              (acct.plexRelation === "shared" || hasSource(acct, "accepted_share")) ? (
                <Badge variant="secondary">{t("Accepted share")}</Badge>
              ) : null}
              {!acct.isOwner && isPendingInvite(acct) ? (
                <Badge variant="warning">{t("Pending invite")}</Badge>
              ) : null}
              {!acct.isOwner && acct.plexRelation === "friend" ? (
                <Badge variant="secondary">
                  {isCommunityFriend(acct) ? t("Community friend") : t("Plex friend")}
                </Badge>
              ) : null}
              {!acct.isOwner && isHistorical(acct) ? (
                <Badge variant="default">{t("PMS history only")}</Badge>
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
                  acct.matchedViewerUsername.toLowerCase() !==
                    (acct.matchedViewerName || "").toLowerCase() ? (
                    <span className="text-fg-muted font-normal">
                      (@{acct.matchedViewerUsername})
                    </span>
                  ) : null}
                </span>
              ) : acct.suggestedViewerName ? (
                <span className="flex items-center gap-1 text-info">
                  {t("Suggested match: {name}", {
                    name: acct.suggestedViewerName,
                  })}
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
                <Button size="sm" disabled={isBusy} onClick={() => void importSingle(acct)}>
                  <Plus size={12} /> {t("Add as viewer")}
                </Button>

                <div className="w-48">
                  <Select
                    value={0}
                    disabled={isBusy}
                    aria-label={t("Match account to existing viewer")}
                    onChange={(e) => void matchExisting(acct, Number(e.currentTarget.value))}
                  >
                    <option value={0}>{t("Match existing…")}</option>
                    {viewers
                      .filter((v) => !srv.accounts.some((a) => a.matchedViewerId === v.id))
                      .map((v) => {
                        const label =
                          v.title &&
                          v.username &&
                          v.title.toLowerCase() !== v.username.toLowerCase()
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
              "Connect your Plex Media Server under Integrations › Plex Media Server Sync first to discover and import its users.",
            )}
            action={
              <Button
                size="sm"
                onClick={() => {
                  onClose();
                  navigate("/services/plexservers");
                }}
              >
                <ExternalLink size={14} /> {t("Go to Plex Media Server Sync")}
              </Button>
            }
          />
        ) : (
          <div className="space-y-6">
            <div className="relative">
              <Search
                className="absolute left-3 top-2.5 text-fg-muted pointer-events-none"
                size={16}
              />
              <Input
                className="pl-9"
                placeholder={t("Search by account name or Plex username…")}
                aria-label={t("Search by account name or Plex username")}
                value={searchQuery}
                onChange={(e) => setSearchQuery(e.target.value)}
              />
            </div>

            <div className="space-y-2.5 rounded-lg border border-border bg-bg-subtle p-3">
              <div className="flex flex-wrap items-center justify-between gap-3">
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
                  {busyAll
                    ? t("Importing…")
                    : t("Import all ({count})", { count: unmappedAccounts.length })}
                </Button>
              </div>
              {filteredOutTotal > 0 ? (
                <p className="text-xs text-fg-muted">
                  {t(
                    "Showing active Plex accounts with an email ({count} inactive or without email hidden)",
                    { count: filteredOutTotal },
                  )}
                </p>
              ) : null}
              {hasPlexBreakdown ? (
                <div className="flex flex-wrap items-center gap-x-3 gap-y-1 border-t border-border/60 pt-2.5 text-xs text-fg-muted">
                  <span>
                    {directoryTotal > 0
                      ? t("Plex.tv matched: {matched} ({directory} in directory)", {
                          matched: plexMatchedCount,
                          directory: directoryTotal,
                        })
                      : t("Plex.tv matched: {matched}", { matched: plexMatchedCount })}
                  </span>
                  <span>·</span>
                  <span>{t("Accepted shares: {count}", { count: acceptedShareCount })}</span>
                  {homeUserCount > 0 ? (
                    <>
                      <span>·</span>
                      <span>{t("Home users: {count}", { count: homeUserCount })}</span>
                    </>
                  ) : null}
                  {pendingInviteCount > 0 ? (
                    <>
                      <span>·</span>
                      <span>{t("Pending invites: {count}", { count: pendingInviteCount })}</span>
                    </>
                  ) : null}
                  {friendCount > 0 ? (
                    <>
                      <span>·</span>
                      <span>{t("Friends: {count}", { count: friendCount })}</span>
                    </>
                  ) : null}
                  {communityFriendCount > 0 ? (
                    <>
                      <span>·</span>
                      <span>
                        {t("Community friends: {count}", { count: communityFriendCount })}
                      </span>
                    </>
                  ) : null}
                  {historicalCount > 0 ? (
                    <>
                      <span>·</span>
                      <span>{t("PMS history only: {count}", { count: historicalCount })}</span>
                    </>
                  ) : null}
                </div>
              ) : null}
            </div>

            {hasPlexBreakdown ||
            (unmappedAccounts.length > 0 && unmappedAccounts.length < allAccounts.length) ? (
              <div
                role="group"
                aria-label={t("Account category")}
                className="flex flex-wrap gap-1.5"
              >
                {filterChips.map((chip) => (
                  <button
                    key={chip.value}
                    type="button"
                    aria-pressed={activeFilter === chip.value}
                    onClick={() => setActiveFilter(chip.value)}
                    className={
                      activeFilter === chip.value
                        ? "rounded-full border border-primary/40 bg-primary-subtle px-3 py-1 text-xs font-medium text-primary"
                        : "rounded-full border border-border bg-surface px-3 py-1 text-xs font-medium text-fg-muted hover:border-border-strong hover:text-fg"
                    }
                  >
                    {chip.label}
                  </button>
                ))}
              </div>
            ) : null}

            {servers.map((srv) => {
              const q = searchQuery.trim().toLowerCase();
              const visibleAccounts = srv.accounts.filter(
                (acct) => matchesFilter(acct, activeFilter) && matchesSearch(acct, q),
              );

              if (visibleAccounts.length === 0 && (q || activeFilter !== "all")) {
                return null;
              }

              const serverHasBreakdown = visibleAccounts.some((a) => Boolean(a.plexRelation));
              const sections =
                serverHasBreakdown && activeFilter === "all"
                  ? [
                      {
                        key: "owner-home",
                        title: t("Server owner & Home users"),
                        items: visibleAccounts.filter(
                          (a) =>
                            a.isOwner || a.plexRelation === "owner" || a.plexRelation === "home",
                        ),
                      },
                      {
                        key: "shared",
                        title: t("Accepted shares"),
                        items: visibleAccounts.filter(
                          (a) => !a.isOwner && a.plexRelation === "shared",
                        ),
                      },
                      {
                        key: "pending",
                        title: t("Pending invites"),
                        items: visibleAccounts.filter(
                          (a) => !a.isOwner && a.plexRelation === "pending",
                        ),
                      },
                      {
                        key: "friend",
                        title: t("Friends"),
                        items: visibleAccounts.filter(
                          (a) => !a.isOwner && a.plexRelation === "friend",
                        ),
                      },
                      {
                        key: "historical",
                        title: t("PMS history only"),
                        description: t(
                          "Historical accounts from this Plex server no longer in your Plex.tv friends or shares",
                        ),
                        items: visibleAccounts.filter(
                          (a) => !a.isOwner && a.plexRelation === "historical",
                        ),
                      },
                      {
                        key: "other",
                        title: t("Other accounts"),
                        items: visibleAccounts.filter((a) => !a.isOwner && !a.plexRelation),
                      },
                    ].filter((sec) => sec.items.length > 0)
                  : null;

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

                  {sections ? (
                    <div className="divide-y divide-border">
                      {sections.map((sec) => (
                        <div key={sec.key}>
                          <div className="bg-bg-subtle/60 px-4 py-2 border-b border-border flex flex-wrap items-center justify-between gap-2">
                            <div>
                              <p className="text-xs font-semibold uppercase tracking-wide text-fg-muted">
                                {sec.title}
                              </p>
                              {sec.description ? (
                                <p className="text-xs text-fg-muted">{sec.description}</p>
                              ) : null}
                            </div>
                            <span className="text-xs text-fg-muted">{sec.items.length}</span>
                          </div>
                          <ul className="divide-y divide-border">
                            {sec.items.map((acct) => renderAccountRow(srv, acct))}
                          </ul>
                        </div>
                      ))}
                    </div>
                  ) : (
                    <ul className="divide-y divide-border">
                      {visibleAccounts.map((acct) => renderAccountRow(srv, acct))}
                    </ul>
                  )}
                </div>
              );
            })}

            {servers.every(
              (srv) =>
                srv.accounts.filter(
                  (acct) =>
                    matchesFilter(acct, activeFilter) &&
                    matchesSearch(acct, searchQuery.trim().toLowerCase()),
                ).length === 0,
            ) &&
            (searchQuery.trim() || activeFilter !== "all") ? (
              <div className="py-8 text-center text-sm text-fg-muted">
                {searchQuery.trim()
                  ? t('No accounts match "{query}"', { query: searchQuery.trim() })
                  : t("No accounts match this filter")}
              </div>
            ) : null}
          </div>
        )}
      </div>
    </Drawer>
  );
}
