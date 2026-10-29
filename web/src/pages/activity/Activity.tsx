import { useEffect, useState } from "react";
import { DomainPage, useRouteTab } from "../../components/layout";
import { localized, t } from "../../i18n";
import { api, apiFetch } from "../../lib/api";
import { normalizeSessions } from "../../lib/health";
import { useAsync, useInterval } from "../../lib/hooks";
import { PlayHistory, ViewingFilters } from "./History";
import { NowPlaying } from "./NowPlaying";
import { ViewingStats } from "./Stats";
import { viewingQuery, type Play, type Summary, type ViewingFilter } from "./types";

const TABS = localized(
  () =>
    [
      { key: "now", label: t("Now playing") },
      { key: "history", label: t("History") },
      { key: "stats", label: t("Stats") },
    ] as const,
);

/**
 * Activity is the screen owners keep on their phone, so it offers the
 * dashboard as an installable app (manifest + service worker).
 */
function useInstallableDashboard() {
  useEffect(() => {
    const link = document.createElement("link");
    link.rel = "manifest";
    link.href = "/ether/manifest.webmanifest";
    document.head.appendChild(link);
    if ("serviceWorker" in navigator)
      void navigator.serviceWorker
        .register("/ether/service-worker.js", { scope: "/ether/" })
        .catch(() => {});
    return () => link.remove();
  }, []);
}

export default function Activity() {
  const tab = useRouteTab(TABS);
  useInstallableDashboard();
  const [filter, setFilter] = useState<ViewingFilter>({ days: "30", user: "", year: "" });
  const query = viewingQuery(filter);
  const viewingTab = tab !== "now";

  const sessions = useAsync(
    async () =>
      normalizeSessions(
        (await apiFetch<{ value: unknown[] | null }>("/status/sessions")).value ?? [],
      ),
    [],
  );
  const viewers = useAsync(() => api.users(), []);
  const viewing = useAsync(async () => {
    if (!viewingTab) return null;
    const [summary, history] = await Promise.all([
      apiFetch<Summary>(`/stats/summary${query}`),
      apiFetch<Play[] | null>(`/stats/history${query}`),
    ]);
    return { summary, history: history ?? [] };
  }, [query, viewingTab]);

  useInterval(() => void sessions.reload(), tab === "now" ? 10_000 : 30_000);
  useInterval(() => void viewing.reload(), viewingTab ? 30_000 : null);

  const live = sessions.data?.length;
  return (
    <DomainPage
      title={t("Activity")}
      description={t("What's playing right now and what your viewers have watched.")}
      base="/activity"
      tabs={TABS.map((tab) => (tab.key === "now" && live ? { ...tab, count: live } : tab))}
      active={tab}
    >
      {tab === "now" ? (
        <NowPlaying state={sessions} />
      ) : (
        <>
          <ViewingFilters value={filter} onChange={setFilter} viewers={viewers.data ?? []} />
          {tab === "history" ? (
            <PlayHistory
              history={viewing.data?.history}
              error={viewing.error}
              onRetry={() => void viewing.reload()}
            />
          ) : (
            <ViewingStats
              summary={viewing.data?.summary}
              error={viewing.error}
              onRetry={() => void viewing.reload()}
            />
          )}
        </>
      )}
    </DomainPage>
  );
}
