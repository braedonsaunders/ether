import { localized, t } from "../../i18n";
import { ErrorPanel, LoadingRows } from "../../components/feedback";
import { DomainPage, useRouteTab } from "../../components/layout";
import { apiFetch } from "../../lib/api";
import { useAsync, useInterval } from "../../lib/hooks";
import { Channels } from "./Channels";
import { Deliveries } from "./Deliveries";
import type { NotificationsData } from "./types";
import { Protection } from "./Protection";
import { WeeklyReport } from "./WeeklyReport";

const TABS = localized(
  () =>
    [
      { key: "channels", label: t("Channels") },
      { key: "protection", label: t("Server protection") },
      { key: "report", label: t("Weekly report") },
      { key: "deliveries", label: t("Delivery history") },
    ] as const,
);

export default function Notifications() {
  const tab = useRouteTab(TABS);
  const notices = useAsync(async () => {
    const n = await apiFetch<NotificationsData>("/notifications/");
    return {
      ...n,
      channels: n.channels ?? [],
      events: n.events ?? [],
      deliveries: n.deliveries ?? [],
    };
  }, []);
  useInterval(() => void notices.reload(), 30_000);

  const data = notices.data;
  const body =
    tab === "protection" ? (
      <Protection active={data?.crashWatcher} />
    ) : tab === "report" ? (
      <WeeklyReport notifications={data} />
    ) : notices.error && !data ? (
      <ErrorPanel error={notices.error} onRetry={() => void notices.reload()} />
    ) : !data ? (
      <LoadingRows rows={3} />
    ) : tab === "channels" ? (
      <Channels data={data} onChanged={notices.reload} />
    ) : (
      <Deliveries data={data} />
    );

  return (
    <DomainPage
      title={t("Notifications")}
      description={t(
        "Know when something needs you. Send server, playback and source alerts to your devices.",
      )}
      base="/notifications"
      tabs={TABS.map((tb) =>
        tb.key === "channels" && data?.channels.length
          ? { ...tb, count: data.channels.length }
          : tb,
      )}
      active={tab}
    >
      {body}
    </DomainPage>
  );
}
