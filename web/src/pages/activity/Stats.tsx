import { DashboardMetricCard, DashboardPanel } from "@braedonsaunders/appkit-dashboard/primitives";
import { CheckCircle2, Clock, HardDriveDownload, PlayCircle } from "lucide-react";
import { useState } from "react";
import { TimeBars } from "../../components/charts";
import { ErrorPanel, LoadingRows } from "../../components/feedback";
import { Segmented } from "../../components/fields";
import { localized, t } from "../../i18n";
import { fixed, formatBytes, formatNumber } from "../../lib/format";
import type { Bucket, Summary } from "./types";

type Breakdown = "daily" | "hourly" | "platforms" | "titles" | "users";

const BREAKDOWNS = localized<{ value: Breakdown; label: string; title: string }[]>(() => [
  { value: "daily", label: t("By day"), title: t("Plays by day") },
  { value: "hourly", label: t("By hour"), title: t("Hour of day (UTC)") },
  { value: "platforms", label: t("Devices"), title: t("Devices") },
  { value: "titles", label: t("Titles"), title: t("Popular titles") },
  { value: "users", label: t("Viewers"), title: t("Viewers") },
]);

/** Ranked horizontal bars for categorical breakdowns (devices, titles, viewers). */
function RankedBars({ items }: { items: Bucket[] }) {
  const max = Math.max(1, ...items.map((x) => x.plays));
  return (
    <ul className="space-y-2.5">
      {items.slice(0, 31).map((b, i) => (
        <li key={`${b.label}-${i}`} className="space-y-1">
          <div className="flex justify-between gap-4 text-xs">
            <span className="truncate text-fg">{b.label || t("Unknown")}</span>
            <span className="tabular-nums text-fg-muted">{b.plays}</span>
          </div>
          <div className="h-1.5 overflow-hidden rounded-full bg-bg-subtle ring-1 ring-inset ring-border-subtle">
            <div
              className="h-full rounded-full bg-primary"
              style={{ width: `${(b.plays / max) * 100}%` }}
            />
          </div>
        </li>
      ))}
    </ul>
  );
}

/** Viewing totals and breakdowns for the selected period and viewer. */
export function ViewingStats({
  summary,
  error,
  onRetry,
}: {
  summary: Summary | undefined;
  error: unknown;
  onRetry: () => void;
}) {
  const [breakdown, setBreakdown] = useState<Breakdown>("daily");
  if (error && !summary) return <ErrorPanel error={error} onRetry={onRetry} />;
  if (!summary) return <LoadingRows rows={3} />;
  const view = BREAKDOWNS.find((b) => b.value === breakdown)!;
  const items = summary[breakdown] ?? [];
  const timeline = breakdown === "daily" || breakdown === "hourly";
  return (
    <>
      <div className="grid grid-cols-2 gap-3 lg:grid-cols-4">
        <DashboardMetricCard
          label={t("Plays")}
          value={formatNumber(summary.plays, 0)}
          icon={<PlayCircle size={18} />}
        />
        <DashboardMetricCard
          label={t("Hours watched")}
          value={fixed(summary.watchedMs / 3600000, 1)}
          icon={<Clock size={18} />}
          tone="info"
        />
        <DashboardMetricCard
          label={t("Completed")}
          value={formatNumber(summary.completed, 0)}
          icon={<CheckCircle2 size={18} />}
          tone="success"
        />
        <DashboardMetricCard
          label={t("Transferred")}
          value={formatBytes(summary.bytes)}
          icon={<HardDriveDownload size={18} />}
          tone="warning"
        />
      </div>
      <DashboardPanel
        title={view.title}
        actions={
          <Segmented
            ariaLabel={t("Viewing breakdown")}
            size="sm"
            value={breakdown}
            onChange={setBreakdown}
            options={BREAKDOWNS.map((b) => ({ value: b.value, label: b.label }))}
          />
        }
      >
        {!items.length ? (
          <p className="py-6 text-center text-sm text-fg-muted">
            {t("No recorded plays in this period.")}
          </p>
        ) : timeline ? (
          <TimeBars
            ariaLabel={view.title}
            data={items.map((b, i) => ({
              key: `${b.label}-${i}`,
              label: b.label || t("Unknown"),
              value: b.plays,
              detail: t("{hours} h", { hours: fixed(b.watchedMs / 3600000, 1) }),
            }))}
          />
        ) : (
          <RankedBars items={items} />
        )}
      </DashboardPanel>
      <p className="text-xs text-fg-muted">
        {t(
          "Calendar totals use UTC. Watched time leaves out gaps longer than 60 seconds and simultaneous sessions that can't be matched to a device.",
        )}
      </p>
    </>
  );
}
