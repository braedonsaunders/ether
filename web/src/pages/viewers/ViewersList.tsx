import {
  Avatar,
  Button,
  EmptyState,
  PagedTable,
  Skeleton,
  toast,
} from "@braedonsaunders/appkit-ui";
import { useState } from "react";
import { MonitorSmartphone, Plus, RefreshCw, Users } from "lucide-react";
import { useNavigate } from "react-router-dom";
import { useAppOptional } from "../../app-context";
import { ErrorPanel } from "../../components/feedback";
import { DomainPage } from "../../components/layout";
import { loadViewers, viewerName } from "../../components/pickers";
import { t, tn } from "../../i18n";
import { api, errorMessage } from "../../lib/api";
import { ratingLevelLabel } from "../../lib/contentRatings";
import type { Viewer } from "../../lib/contract";
import {
  formatDateTime,
  formatNumber,
  isPlainObject,
  parseTime,
  relativeTime,
} from "../../lib/format";
import { useAsync } from "../../lib/hooks";
import type { EtherUser } from "../../lib/types";
import { pagedTableLabels } from "../../i18n/appkit-labels";
import { isActiveViewer, isPlexSource, LifecycleBadges, lifecycleOf } from "./lifecycle";
import { AddViewerDrawer } from "./ManageViewers";
import { ImportPlexViewersDrawer } from "./ImportPlexViewersDrawer";
import { SignInLockouts } from "./sections/SignInLockouts";
import { WatchlistAlerts } from "./sections/WatchlistDefaults";
import { PlexAccessNote } from "./ViewerStatus";
import { BulkViewersDrawer, ImportViewersDrawer, exportViewers } from "./BulkViewers";
import { loadPlexMembers, type PlexMembers } from "../apps/plex-moves";

type PeopleFilter = "all" | "active" | "paused" | "plex";

export default function ViewersList() {
  const app = useAppOptional();
  const remote = app?.dashboard?.sections?.remote;
  const canSyncPlex =
    app?.boot.plexEnabled !== false && isPlainObject(remote) && remote.claimed === true;
  const [adding, setAdding] = useState(false);
  const [syncing, setSyncing] = useState(false);
  const [importing, setImporting] = useState(false);
  const [version, setVersion] = useState(0);
  const syncPlex = async () => {
    setSyncing(true);
    try {
      await api.syncPlexFriends();
      setVersion((v) => v + 1);
      void app?.refreshDashboard();
      toast.success(t("Shared users synced"));
    } catch (e) {
      toast.error(errorMessage(e));
    } finally {
      setSyncing(false);
    }
  };
  return (
    <DomainPage
      title={t("Viewers")}
      base="/viewers"
      actions={
        <div className="flex flex-wrap items-center gap-2">
          {canSyncPlex ? (
            <Button variant="outline" size="sm" disabled={syncing} onClick={() => void syncPlex()}>
              <RefreshCw size={14} /> {syncing ? t("Syncing…") : t("Sync shared users")}
            </Button>
          ) : null}
          <Button size="sm" variant="outline" onClick={() => setImporting(true)}>
            <MonitorSmartphone size={14} /> {t("Import from Plex")}
          </Button>
          <Button size="sm" onClick={() => setAdding(true)}>
            <Plus size={14} /> {t("Add viewer")}
          </Button>
        </div>
      }
    >
      <div className="space-y-6">
        <People key={version} />
        <WatchlistAlerts />
        <SignInLockouts />
      </div>
      <AddViewerDrawer
        open={adding}
        onClose={() => setAdding(false)}
        onCreated={() => setVersion((v) => v + 1)}
        onOpenPlexImport={() => {
          setAdding(false);
          setImporting(true);
        }}
      />
      <ImportPlexViewersDrawer
        open={importing}
        onClose={() => setImporting(false)}
        onImported={() => setVersion((v) => v + 1)}
      />
    </DomainPage>
  );
}

/** A short "what's limited" line from the viewer's user record. */
function limitsSummary(u: EtherUser | undefined): string {
  if (!u) return "—";
  const parts: string[] = [];
  if (u.streamLimit > 0) parts.push(tn(u.streamLimit, "{count} stream", "{count} streams"));
  if (u.maxContentRating && !u.isOwner)
    parts.push(
      t("Up to {rating}", {
        rating: ratingLevelLabel(u.maxContentRating).split(" · ").join("/"),
      }),
    );
  return parts.length ? parts.join(" · ") : t("Server defaults");
}

function People() {
  const navigate = useNavigate();
  const plexConnected = useAppOptional()?.boot.plexEnabled !== false;
  const [status, setStatus] = useState<PeopleFilter>("active");
  const state = useAsync(() => loadViewers(true), []);
  // Limits come from the user records; the list still works without them.
  const users = useAsync(() => api.users().catch(() => [] as EtherUser[]), []);
  const members = useAsync(
    () => (plexConnected ? loadPlexMembers() : Promise.resolve({} as PlexMembers)),
    [],
  );
  const [picked, setPicked] = useState<Set<number>>(new Set());
  const [bulkOpen, setBulkOpen] = useState(false);
  const [importOpen, setImportOpen] = useState(false);
  const [exporting, setExporting] = useState(false);
  const byId = new Map((users.data ?? []).map((u) => [u.id, u]));
  const viewers = state.data ?? [];
  const isPaused = (v: Viewer) => lifecycleOf(v).paused;
  const fromPlex = (v: Viewer) => isPlexSource(lifecycleOf(v).source);
  const filtered = viewers.filter((v) =>
    status === "all"
      ? true
      : status === "active"
        ? isActiveViewer(v)
        : status === "paused"
          ? isPaused(v)
          : fromPlex(v),
  );
  const filters: { value: PeopleFilter; label: string }[] = [
    {
      value: "active",
      label: t("Active ({count})", { count: viewers.filter(isActiveViewer).length }),
    },
    { value: "all", label: t("All ({count})", { count: viewers.length }) },
    { value: "paused", label: t("Paused ({count})", { count: viewers.filter(isPaused).length }) },
    ...(plexConnected
      ? [
          {
            value: "plex" as PeopleFilter,
            label: t("From Plex ({count})", { count: viewers.filter(fromPlex).length }),
          },
        ]
      : []),
  ];
  if (state.error) return <ErrorPanel error={state.error} onRetry={() => void state.reload()} />;
  if (!state.data)
    return (
      <div className="space-y-2" role="status" aria-label={t("Loading viewers")}>
        {Array.from({ length: 4 }, (_, i) => (
          <Skeleton key={i} className="h-14 w-full rounded-lg" />
        ))}
      </div>
    );
  const selected = viewers.filter((v) => picked.has(v.id));
  const togglePick = (id: number, on: boolean) =>
    setPicked((old) => {
      const next = new Set(old);
      if (on) next.add(id);
      else next.delete(id);
      return next;
    });
  const allPicked = filtered.length > 0 && filtered.every((v) => v.isOwner || picked.has(v.id));
  const reloadAll = () => {
    void state.reload();
    void users.reload();
    void members.reload();
  };
  const exportPicked = async () => {
    setExporting(true);
    try {
      const n = await exportViewers(selected.length ? selected : viewers, users.data ?? []);
      toast.success(t("Exported {count} viewers", { count: n }));
    } catch (e) {
      toast.error(errorMessage(e));
    } finally {
      setExporting(false);
    }
  };
  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-center gap-2">
        <Button size="sm" disabled={!selected.length} onClick={() => setBulkOpen(true)}>
          {t("Bulk actions ({count})", { count: selected.length })}
        </Button>
        <Button
          size="sm"
          variant="outline"
          disabled={exporting}
          onClick={() => void exportPicked()}
        >
          {selected.length ? t("Export selected") : t("Export all viewers")}
        </Button>
        <Button size="sm" variant="outline" onClick={() => setImportOpen(true)}>
          {t("Import backup")}
        </Button>
        {selected.length ? (
          <Button size="sm" variant="ghost" onClick={() => setPicked(new Set())}>
            {t("Clear selection")}
          </Button>
        ) : null}
      </div>
      <BulkViewersDrawer
        open={bulkOpen}
        onClose={() => setBulkOpen(false)}
        selected={selected}
        allViewers={viewers}
        users={users.data ?? []}
        onDone={reloadAll}
      />
      <ImportViewersDrawer
        open={importOpen}
        onClose={() => setImportOpen(false)}
        viewers={viewers}
        onDone={reloadAll}
      />
      <div className="flex flex-wrap items-center justify-between gap-3">
        <PlexAccessNote />
        <div role="group" aria-label={t("Viewer status")} className="flex flex-wrap gap-1.5">
          {filters.map((f) => (
            <button
              key={f.value}
              type="button"
              aria-pressed={status === f.value}
              onClick={() => setStatus(f.value)}
              className={
                status === f.value
                  ? "rounded-full border border-primary/40 bg-primary-subtle px-3 py-1 text-sm font-medium text-primary"
                  : "rounded-full border border-border bg-surface px-3 py-1 text-sm font-medium text-fg-muted hover:border-border-strong hover:text-fg"
              }
            >
              {f.label}
            </button>
          ))}
        </div>
      </div>
      <PagedTable<Viewer>
        rows={filtered}
        rowKey={(v) => String(v.id)}
        searchable
        pageSize={15}
        onRowClick={(v) => navigate(`/viewers/${v.id}`)}
        defaultSort={{ key: "name", dir: "asc" }}
        empty={
          <EmptyState
            icon={<Users />}
            title={viewers.length ? t("No viewers match this filter") : t("No viewers yet")}
            description={
              viewers.length
                ? t("Choose another status or search to see more people.")
                : plexConnected
                  ? t("Share this server from Plex.tv, or import viewing history.")
                  : t("Add a viewer to get them watching.")
            }
          />
        }
        labels={pagedTableLabels({ searchPlaceholder: t("Search viewers…") })}
        columns={[
          {
            key: "pick",
            header: (
              <input
                type="checkbox"
                aria-label={t("Select all shown")}
                checked={allPicked}
                onChange={(e) => {
                  const on = e.target.checked;
                  setPicked((old) => {
                    const next = new Set(old);
                    for (const v of filtered)
                      if (!v.isOwner) on ? next.add(v.id) : next.delete(v.id);
                    return next;
                  });
                }}
              />
            ),
            cell: (v) =>
              v.isOwner ? null : (
                <input
                  type="checkbox"
                  aria-label={t("Select {name}", { name: viewerName(v) })}
                  checked={picked.has(v.id)}
                  onClick={(e) => e.stopPropagation()}
                  onChange={(e) => togglePick(v.id, e.target.checked)}
                />
              ),
          },
          {
            key: "name",
            header: t("Viewer"),
            search: (v) => `${v.title} ${v.username}`,
            // The owner ("You") always leads the default name sort.
            sortValue: (v) => `${v.isOwner ? "0" : "1"}${viewerName(v).toLowerCase()}`,
            cell: (v) => (
              <span className="flex items-center gap-3">
                <Avatar src={v.thumb} name={viewerName(v)} size={34} />
                <span className="min-w-0">
                  <span className="flex flex-wrap items-center gap-1.5 font-medium text-fg">
                    {v.isOwner ? t("You") : viewerName(v)}{" "}
                    <LifecycleBadges viewer={v} plexConnected={plexConnected} />
                    {(members.data?.[String(v.id)] ?? []).map((s) => (
                      <span
                        key={s.id}
                        title={t("Plex server")}
                        className="rounded-full border border-border px-2 py-0.5 text-xs font-normal text-fg-muted"
                      >
                        {t("Plex: {name}", { name: s.name })}
                      </span>
                    ))}
                  </span>
                  <span className="block text-xs text-fg-muted">
                    {v.isOwner ? `${viewerName(v)} · ${v.username}` : v.username}
                  </span>
                </span>
              </span>
            ),
          },
          {
            key: "active",
            header: t("Last active"),
            sortValue: (v) => parseTime(v.lastActive) ?? null,
            cell: (v) => (
              <span title={formatDateTime(v.lastActive)} className="text-fg-muted">
                {relativeTime(v.lastActive)}
              </span>
            ),
          },
          {
            key: "plays",
            header: t("Plays · 30d"),
            align: "right",
            sortValue: (v) => v.plays30d,
            cell: (v) => <span className="tabular-nums">{formatNumber(v.plays30d, 0)}</span>,
          },
          {
            key: "hours",
            header: t("Hours · 30d"),
            align: "right",
            sortValue: (v) => v.hoursWatched30d,
            cell: (v) => <span className="tabular-nums">{formatNumber(v.hoursWatched30d, 1)}</span>,
          },
          {
            key: "limits",
            header: t("Limits"),
            cell: (v) => (
              <span className="text-fg-muted">
                {users.data ? limitsSummary(byId.get(v.id)) : ""}
              </span>
            ),
          },
        ]}
      />
    </div>
  );
}
