import { Badge, Button, EmptyState, Spinner, toast } from "@braedonsaunders/appkit-ui";
import { BellRing, RefreshCw, Trash2 } from "lucide-react";
import { useState } from "react";
import { Link } from "react-router-dom";
import { Artwork } from "../../../components/Artwork";
import { ErrorPanel } from "../../../components/feedback";
import { Section } from "../../../components/layout";
import { getLocale, t, tx } from "../../../i18n";
import { apiFetch, errorMessage } from "../../../lib/api";
import { useAsync } from "../../../lib/hooks";
import type { DiscoverySettings } from "./WatchlistDefaults";

type Item = {
  title: { key: string | number; title: string; released?: string; posterPath?: string };
  sources: string[];
  availableAt?: string;
  checkedAt?: string;
};

const known = (value?: string) => Boolean(value && !value.startsWith("0001"));
const day = (value?: string) =>
  known(value) ? new Date(value!).toLocaleDateString(getLocale()) : t("Date not announced");

/** One viewer's watched-for titles: sync, check and remove (viewer profile › Watchlist). */
export function ViewerWatchlist({ viewerId }: { viewerId: number }) {
  const settings = useAsync(() => apiFetch<DiscoverySettings>("/discovery/settings"), []);
  const items = useAsync(
    (signal) => apiFetch<Item[]>(`/discovery/users/${viewerId}/`, { signal }),
    [viewerId],
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
  const refreshWith = (action: "sync" | "check", done: string) =>
    run(action, async () => {
      items.setData(
        await apiFetch<Item[]>(`/discovery/users/${viewerId}/${action}`, {
          method: "POST",
          body: "{}",
        }),
      );
      toast.success(done);
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
            disabled={busy !== null || !enabled}
            onClick={() => void refreshWith("sync", t("Watchlist synced"))}
          >
            <RefreshCw size={14} /> {t("Sync viewer")}
          </Button>
          <Button
            variant="outline"
            size="sm"
            disabled={busy !== null || !enabled}
            onClick={() => void refreshWith("check", t("Availability checked"))}
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
              <Link to="/notifications" className="font-medium text-fg underline">
                {t("Turn it on in Notifications")}
              </Link>
            ),
          })}
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
                  src={
                    item.title.posterPath
                      ? `https://image.tmdb.org/t/p/w92${item.title.posterPath}`
                      : undefined
                  }
                  alt=""
                  title={item.title.title}
                  className="h-12 w-8 shrink-0 rounded"
                />
                <div className="min-w-0 flex-1">
                  <p className="truncate text-sm font-medium text-fg">{item.title.title}</p>
                  <p className="truncate text-xs text-fg-muted">
                    {available
                      ? t("Available since {date}", { date: day(item.availableAt) })
                      : t("Waiting for a stream · {date}", { date: day(item.title.released) })}
                    {item.sources.length ? ` · ${item.sources.join(", ")}` : ""}
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
