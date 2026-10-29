import {
  Badge,
  Button,
  EmptyState,
  PagedTable,
  confirmDialog,
  toast,
} from "@braedonsaunders/appkit-ui";
import { HeartPulse, Pin as PinIcon, PinOff, RefreshCw } from "lucide-react";
import { useState } from "react";
import { Link } from "react-router-dom";
import { ErrorPanel, LoadingRows } from "../../components/feedback";
import { t } from "../../i18n";
import { errorMessage } from "../../lib/api";
import { formatBytes, formatDateTime, relativeTime } from "../../lib/format";
import { useAsync } from "../../lib/hooks";
import { PIN_STATUS_LABEL, qualityLabel, releaseConsole, type Pin } from "../../lib/releases";
import { ReleaseSearch } from "./Search";
import { pagedTableLabels } from "../../i18n/appkit-labels";

/** Last outcome of a pin: playing, failing, not playable for an app, or not tried yet. */
export function PinStatusBadge({ pin }: { pin: Pin }) {
  const s = pin.lastStatus;
  if (!s) return <Badge variant="secondary">{t("Not tried yet")}</Badge>;
  const variant =
    s === "healthy" ? "success" : s === "unhealthy" ? "destructive" : ("warning" as const);
  return <Badge variant={variant}>{PIN_STATUS_LABEL[s]}</Badge>;
}

/** The Pinned page with a title's releases already searched. */
export function searchLink(key: string, title: string) {
  return `/releases/pinned?key=${encodeURIComponent(key)}&title=${encodeURIComponent(title)}`;
}

/** Find a release to pin, then the releases already pinned, newest first. */
export function PinnedTab() {
  const state = useAsync((signal) => releaseConsole.pins(signal), []);
  const [busy, setBusy] = useState<string | null>(null);

  const unpin = async (p: Pin) => {
    const ok = await confirmDialog({
      title: t("Unpin {title}?", { title: p.titleName }),
      message: t("Ether goes back to picking the best release automatically."),
      confirmLabel: t("Unpin"),
    });
    if (!ok) return;
    setBusy(p.titleKey);
    try {
      await releaseConsole.unpin(p.titleKey);
      toast.success(t("Pin removed"));
      await state.reload();
    } catch (e) {
      toast.error(t("Couldn't unpin"), { description: errorMessage(e) });
    } finally {
      setBusy(null);
    }
  };

  const check = async (p: Pin) => {
    setBusy(p.titleKey);
    try {
      const h = await releaseConsole.checkHealth(p.release.id, p.titleKey);
      if (h.healthy) toast.success(t("Pinned release is healthy"));
      else toast.error(t("Pinned release failed its health check"), { description: h.reason });
      await state.reload();
    } catch (e) {
      toast.error(t("Health check failed"), { description: errorMessage(e) });
    } finally {
      setBusy(null);
    }
  };

  return (
    <>
      <ReleaseSearch onPinsChanged={() => void state.reload()} />
      <div className="flex flex-wrap items-center justify-between gap-3">
        <p className="max-w-2xl text-sm text-fg-muted">
          {t(
            "A pinned release plays first for its title, ahead of the automatic ranking. When it fails, Ether falls back to the next best release and keeps the pin so you can see it.",
          )}
        </p>
        <Button variant="outline" size="sm" onClick={() => void state.reload()}>
          <RefreshCw size={14} /> {t("Refresh")}
        </Button>
      </div>
      {state.error ? (
        <ErrorPanel error={state.error} onRetry={() => void state.reload()} />
      ) : !state.data ? (
        <LoadingRows rows={3} />
      ) : (
        <PagedTable<Pin>
          rows={state.data}
          rowKey={(p) => p.titleKey}
          searchable
          pageSize={25}
          labels={pagedTableLabels({
            searchLabel: t("Search pins"),
            searchPlaceholder: t("Filter by title or release…"),
          })}
          defaultSort={{ key: "pinned", dir: "desc" }}
          empty={
            <EmptyState
              icon={<PinIcon />}
              title={t("No pinned releases")}
              description={t("Search a title and pin the release you want it to play.")}
              action={
                <Link to="/releases/search" className="text-sm text-primary underline">
                  {t("Search releases")}
                </Link>
              }
            />
          }
          columns={[
            {
              key: "title",
              header: t("Title"),
              search: (p) => `${p.titleName} ${p.release.name} ${p.note ?? ""}`,
              sortValue: (p) => p.titleName,
              cell: (p) => (
                <span className="block">
                  <Link
                    to={searchLink(p.titleKey, p.titleName)}
                    className="font-medium text-fg hover:underline"
                  >
                    {p.titleName || p.titleKey}
                  </Link>
                  {p.note ? <span className="block text-xs text-fg-muted">{p.note}</span> : null}
                </span>
              ),
            },
            {
              key: "release",
              header: t("Release"),
              cell: (p) => (
                <span className="block">
                  <span className="block break-all font-mono text-xs text-fg">
                    {p.release.name}
                  </span>
                  <span className="text-xs text-fg-muted">
                    {qualityLabel(p.release.info)} · {formatBytes(p.release.size)} ·{" "}
                    {p.release.indexer}
                  </span>
                </span>
              ),
            },
            {
              key: "pinned",
              header: t("Pinned"),
              sortValue: (p) => p.pinnedAt,
              cell: (p) => (
                <span className="text-fg-muted" title={formatDateTime(p.pinnedAt)}>
                  {relativeTime(p.pinnedAt)}
                </span>
              ),
            },
            {
              key: "status",
              header: t("Last status"),
              sortValue: (p) => p.lastStatus ?? "",
              cell: (p) => (
                <span className="block">
                  <PinStatusBadge pin={p} />
                  {p.lastReason ? (
                    <span className="mt-1 block text-xs text-fg-muted">{p.lastReason}</span>
                  ) : null}
                  {p.lastStatus ? (
                    <span className="block text-[11px] text-fg-subtle">
                      {relativeTime(p.lastCheckedAt)}
                    </span>
                  ) : null}
                </span>
              ),
            },
            {
              key: "actions",
              header: <span className="sr-only">{t("Actions")}</span>,
              align: "right",
              cell: (p) => (
                <span className="flex justify-end gap-1">
                  <Button
                    variant="ghost"
                    size="sm"
                    disabled={busy === p.titleKey}
                    aria-label={t("Check health of the pin for {title}", { title: p.titleName })}
                    onClick={() => void check(p)}
                  >
                    <HeartPulse size={14} /> {t("Check")}
                  </Button>
                  <Button
                    variant="ghost"
                    size="sm"
                    disabled={busy === p.titleKey}
                    aria-label={t("Unpin {title}", { title: p.titleName })}
                    onClick={() => void unpin(p)}
                  >
                    <PinOff size={14} /> {t("Unpin")}
                  </Button>
                </span>
              ),
            },
          ]}
        />
      )}
    </>
  );
}
