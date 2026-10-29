import { Avatar, EmptyState, PagedTable, Select, Skeleton } from "@braedonsaunders/appkit-ui";
import { useState } from "react";
import { Users } from "lucide-react";
import { useNavigate } from "react-router-dom";
import { ErrorPanel } from "../../components/feedback";
import { DomainPage, type PageTab } from "../../components/layout";
import { loadViewers, viewerName } from "../../components/pickers";
import { api } from "../../lib/api";
import type { Viewer } from "../../lib/contract";
import { formatDateTime, formatNumber, parseTime, relativeTime } from "../../lib/format";
import { useAsync } from "../../lib/hooks";
import type { EtherUser } from "../../lib/types";
import { CreateViewer } from "./ManageViewers";
import { ViewerDefaults } from "./Defaults";
import { PlexAccessNote, ViewerStatus, viewerStatus } from "./ViewerStatus";

type Tab = "people" | "defaults";

const TABS: readonly (PageTab & { key: Tab })[] = [
  { key: "people", label: "People" },
  { key: "defaults", label: "Defaults" },
];

export default function ViewersList({ tab = "people" }: { tab?: Tab } = {}) {
  return (
    <DomainPage
      title="Viewers"
      description="Manage household viewers and imported history."
      base="/viewers"
      tabs={TABS}
      active={tab}
    >
      {tab === "defaults" ? <ViewerDefaults /> : <People />}
    </DomainPage>
  );
}

/** A short "what's limited" line from the viewer's user record. */
function limitsSummary(u: EtherUser | undefined): string {
  if (!u) return "—";
  const parts: string[] = [];
  if (u.streamLimit > 0)
    parts.push(`${u.streamLimit} ${u.streamLimit === 1 ? "stream" : "streams"}`);
  if (u.maxContentRating && !u.isOwner) parts.push(`Up to ${u.maxContentRating}`);
  return parts.length ? parts.join(" · ") : "Server defaults";
}

function People() {
  const navigate = useNavigate();
  const [status, setStatus] = useState("all");
  const state = useAsync(() => loadViewers(true), []);
  // Limits come from the user records; the list still works without them.
  const users = useAsync(() => api.users().catch(() => [] as EtherUser[]), []);
  const byId = new Map((users.data ?? []).map((u) => [u.id, u]));
  const viewers = state.data ?? [];
  const hasAccess = (v: Viewer) => ["owner", "active"].includes(viewerStatus(v));
  const filtered = viewers.filter(
    (v) =>
      status === "all" ||
      (status === "active"
        ? hasAccess(v)
        : status === "imported"
          ? viewerStatus(v) === "imported"
          : !hasAccess(v)),
  );
  if (state.error) return <ErrorPanel error={state.error} onRetry={() => void state.reload()} />;
  if (!state.data)
    return (
      <div className="space-y-2" role="status" aria-label="Loading viewers">
        {Array.from({ length: 4 }, (_, i) => (
          <Skeleton key={i} className="h-14 w-full rounded-lg" />
        ))}
      </div>
    );
  return (
    <div className="space-y-4">
      <CreateViewer
        onCreated={() => {
          void state.reload();
          void users.reload();
        }}
      />
      <div className="flex flex-wrap items-center justify-between gap-3">
        <PlexAccessNote />
        <Select
          aria-label="Viewer status"
          value={status}
          onChange={(e) => setStatus(e.target.value)}
          className="w-60"
        >
          <option value="all">All viewers ({viewers.length})</option>
          <option value="active">With access ({viewers.filter(hasAccess).length})</option>
          <option value="imported">
            Imported history ({viewers.filter((v) => viewerStatus(v) === "imported").length})
          </option>
          <option value="inactive">
            No current access ({viewers.filter((v) => !hasAccess(v)).length})
          </option>
        </Select>
      </div>
      <PagedTable<Viewer>
        rows={filtered}
        rowKey={(v) => String(v.id)}
        searchable
        pageSize={15}
        onRowClick={(v) => navigate(`/viewers/${v.id}`)}
        defaultSort={{ key: "plays", dir: "desc" }}
        empty={
          <EmptyState
            icon={<Users />}
            title={viewers.length ? "No viewers match this filter" : "No viewers yet"}
            description={
              viewers.length
                ? "Choose another status or search to see more people."
                : "Share this server from Plex.tv, or import viewing history."
            }
          />
        }
        labels={{ searchPlaceholder: "Search viewers…" }}
        columns={[
          {
            key: "name",
            header: "Viewer",
            search: (v) => `${v.title} ${v.username}`,
            sortValue: (v) => viewerName(v).toLowerCase(),
            cell: (v) => (
              <span className="flex items-center gap-3">
                <Avatar src={v.thumb} name={viewerName(v)} size={34} />
                <span className="min-w-0">
                  <span className="flex items-center gap-1.5 font-medium text-fg">
                    {viewerName(v)} <ViewerStatus viewer={v} />
                  </span>
                  <span className="block text-xs text-fg-muted">{v.username}</span>
                </span>
              </span>
            ),
          },
          {
            key: "active",
            header: "Last active",
            sortValue: (v) => parseTime(v.lastActive) ?? null,
            cell: (v) => (
              <span title={formatDateTime(v.lastActive)} className="text-fg-muted">
                {relativeTime(v.lastActive)}
              </span>
            ),
          },
          {
            key: "plays",
            header: "Plays · 30d",
            align: "right",
            sortValue: (v) => v.plays30d,
            cell: (v) => <span className="tabular-nums">{formatNumber(v.plays30d, 0)}</span>,
          },
          {
            key: "hours",
            header: "Hours · 30d",
            align: "right",
            sortValue: (v) => v.hoursWatched30d,
            cell: (v) => <span className="tabular-nums">{formatNumber(v.hoursWatched30d, 1)}</span>,
          },
          {
            key: "limits",
            header: "Limits",
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
