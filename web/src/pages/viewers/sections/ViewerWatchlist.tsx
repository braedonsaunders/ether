import { Badge, Button, EmptyState, Spinner, toast } from "@braedonsaunders/appkit-ui";
import { BellRing, RefreshCw, Trash2 } from "lucide-react";
import { useState } from "react";
import { Link } from "react-router-dom";
import { Artwork } from "../../../components/Artwork";
import { ErrorPanel } from "../../../components/feedback";
import { Section } from "../../../components/layout";
import { t, tx } from "../../../i18n";
import { apiFetch, errorMessage } from "../../../lib/api";
import { useAsync, useInterval } from "../../../lib/hooks";
import { relativeTime } from "../../../lib/format";
import { titlePosterURL } from "../../../lib/titles";
import type { DiscoverySettings } from "./WatchlistDefaults";

type Item = {
  title: { key: string | number; title: string; released?: string; posterPath?: string };
  sources: string[];
  availableAt?: string;
  checkedAt?: string;
};

const known = (value?: string) => Boolean(value && !value.startsWith("0001"));

const formatSources = (sources: string[]) =>
  Array.from(
    new Set(sources.map((s) => (s.startsWith("plex-community:") ? "plex-community" : s))),
  );

/** One viewer's watched-for titles: sync, check and remove (viewer profile › Watchlist). */
export function ViewerWatchlist({ viewerId }: { viewerId: number }) {
  const settings = useAsync(() => apiFetch<DiscoverySettings>("/discovery/settings"), []);
  const items = useAsync(
    (signal) => apiFetch<Item[]>(`/discovery/users/${viewerId}/`, { signal }),
    [viewerId],
  );
  type CheckStatus = {
    state: string;
    done: number;
    total: number;
    enabled: boolean;
    nextCheck: string;
  };
  const status = useAsync(
    (signal) => apiFetch<CheckStatus>(`/discovery/users/${viewerId}/check`, { signal }),
    [viewerId],
  );
  const checking = status.data?.state === "running";
  useInterval(
    () => {
      void status.reload();
      void items.reload();
    },
    checking ? 2000 : 30_000,
  );
  const [busy, setBusy] = useState<string | null>(null);
  const enabled = Boolean(settings.data?.enabled);

  const run = async (key: string, fn: () => Promise<void>) => {
    setBusy(key);
    try {
      await fn();
    } catch (e) {
      toast.error(errorMessage(e));
    } finally {
      setBusy(null);
    }
  };
  const refreshWith = (action: "sync" | "check") =>
    run(action, async () => {
      if (action === "sync") {
        type SyncJob = { id: number; state: string; plex: string; count: number };
        let job = await apiFetch<SyncJob>(`/discovery/users/${viewerId}/sync/jobs`, {
          method: "POST",
          body: "{}",
        });
        const deadline = Date.now() + 6 * 60_000;
        while ((job.state === "queued" || job.state === "running") && Date.now() < deadline) {
          await new Promise((resolve) => setTimeout(resolve, 1500));
          job = await apiFetch<SyncJob>(`/discovery/users/${viewerId}/sync/jobs/${job.id}`);
        }
        const result = { ...job, failed: job.state !== "completed" };
        items.setData(await apiFetch<Item[]>(`/discovery/users/${viewerId}/`));
        const message =
          result.failed && result.plex === "synced"
            ? t("Watchlist sync incomplete")
            : result.plex === "friend"
              ? t(
                  "This viewer's Plex watchlist can't be read: they're a friend you share with, not a Plex Home member. Use Friends' Watchlist RSS in Viewers › Watchlists, or have them link Trakt.",
                )
              : result.plex === "pin"
                ? t("Plex asked for this Home member's PIN")
                : result.plex === "disabled"
                  ? t("Plex watchlists are turned off in Viewers › Watchlists")
                  : result.plex === "synced"
                    ? t("Synced {count} titles", { count: result.count })
                    : t("Plex refused access to this Home member's watchlist");
        if (result.plex === "synced" && !result.failed) toast.success(message);
        else toast.info(message);
      } else {
        status.setData(
          await apiFetch<CheckStatus>(`/discovery/users/${viewerId}/check`, {
            method: "POST",
            body: "{}",
          }),
        );
      }
    });
  const remove = (item: Item) =>
    run(`remove-${item.title.key}`, async () => {
      await apiFetch(`/discovery/users/${viewerId}/items/${item.title.key}`, { method: "DELETE" });
      items.setData((list) => (list ?? []).filter((v) => v.title.key !== item.title.key));
    });

  const list = items.data ?? [];
  return (
    <Section
      title={t("Watchlist")}
      description={t(
        "Titles watched for this viewer. Each one shows on their Home once it can be streamed.",
      )}
      actions={
        <>
          <Button
            variant="outline"
            size="sm"
            disabled={busy !== null}
            onClick={() => void refreshWith("sync")}
          >
            <RefreshCw size={14} /> {t("Sync viewer")}
          </Button>
          <Button
            variant="outline"
            size="sm"
            disabled={busy !== null || !enabled || checking || !status.data?.enabled}
            onClick={() => void refreshWith("check")}
          >
            <BellRing size={14} /> {t("Check availability")}
          </Button>
        </>
      }
    >
      {settings.data && !enabled ? (
        <p className="text-sm text-fg-muted">
          {tx("Watchlist monitoring is off. {link} to sync and check this viewer's watchlist.", {
            link: (
              <Link to="/viewers" className="font-medium text-fg underline">
                {t("Turn it on in Viewers")}
              </Link>
            ),
          })}
        </p>
      ) : null}
      {status.error ? (
        <ErrorPanel error={status.error} onRetry={() => void status.reload()} />
      ) : null}
      {status.data?.enabled ? (
        <p className="text-sm text-fg-muted" aria-live="polite">
          {checking
            ? t("Checking availability · {done} of {total} titles", {
                done: status.data.done,
                total: status.data.total,
              })
            : t("Checked {checked} of {total} titles · next check {time}", {
                checked: list.filter((item) => known(item.checkedAt)).length,
                total: list.length,
                time: relativeTime(status.data.nextCheck),
              })}
        </p>
      ) : null}
      {status.data && ["failed", "interrupted"].includes(status.data.state) ? (
        <p role="status" className="text-sm text-fg-muted">
          {t("Availability check incomplete")}
        </p>
      ) : null}
      {items.error ? (
        <ErrorPanel error={items.error} onRetry={() => void items.reload()} />
      ) : !items.data ? (
        <div className="py-6 text-center">
          <Spinner size={18} label={t("Loading watchlist")} />
        </div>
      ) : list.length === 0 ? (
        <EmptyState
          title={t("Nothing on the watchlist")}
          description={t(
            "Titles appear here after a sync of this viewer's Plex or Trakt watchlist.",
          )}
        />
      ) : (
        <ul className="divide-y divide-border" aria-label={t("Watched-for titles")}>
          {list.map((item) => {
            const available = known(item.availableAt);
            return (
              <li key={String(item.title.key)} className="flex items-center gap-3 py-2.5">
                <Artwork
                  src={titlePosterURL(
                    {
                      ...item.title,
                      year: known(item.title.released)
                        ? Number(item.title.released?.slice(0, 4))
                        : undefined,
                    },
                    92,
                  )}
                  alt=""
                  title={item.title.title}
                  className="h-12 w-8 shrink-0 rounded"
                />
                <div className="min-w-0 flex-1">
                  <p className="truncate text-sm font-medium text-fg">{item.title.title}</p>
                  <p className="truncate text-xs text-fg-muted">
                    {available
                      ? t("Available")
                      : known(item.checkedAt)
                        ? t("No stream found yet · checked {time}", {
                            time: relativeTime(item.checkedAt),
                          })
                        : t("Not checked yet")}
                    {item.sources.length ? ` · ${formatSources(item.sources).join(", ")}` : ""}
                  </p>
                </div>
                {available ? <Badge variant="success">{t("Available")}</Badge> : null}
                <Button
                  variant="ghost"
                  size="sm"
                  aria-label={t("Remove {title}", { title: item.title.title })}
                  disabled={busy !== null}
                  onClick={() => void remove(item)}
                >
                  <Trash2 size={14} /> {t("Remove")}
                </Button>
              </li>
            );
          })}
        </ul>
      )}
    </Section>
  );
}
